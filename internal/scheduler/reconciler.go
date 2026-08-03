package scheduler

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	"github.com/kruntimes/kruntimes/internal/runstatus"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

const defaultRuntimedHeartbeatStaleAfter = 15 * time.Second

const runRuntimeIndexField = "spec.runtime"

// RunReconciler watches Pending Tasks and assigns them to Runtime Pods.
type RunReconciler struct {
	client.Client
	Log                         logr.Logger
	RuntimedHeartbeatStaleAfter time.Duration

	assumptions               assumedReservationCache
	filterPluginRegistrations []filterPluginRegistration
	scorePluginRegistrations  []scorePluginRegistration
	metrics                   *schedulerMetrics
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=runs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *RunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("run", req.NamespacedName)

	var run v1alpha1.Run
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get run: %w", err)
	}
	r.observeRun(&run)

	// Treat empty phase (CRD default not yet applied) as Pending.
	if run.Status.Phase != "" && run.Status.Phase != v1alpha1.RunPending {
		return ctrl.Result{}, nil
	}
	if run.Spec.Runtime == "" {
		return ctrl.Result{}, nil
	}
	if run.Spec.CancelRequested {
		return r.applyCancelled(ctx, &run)
	}
	request, err := run.Spec.ResourceRequests()
	if err != nil {
		return r.applyInvalidResources(ctx, &run, err)
	}
	log.Info("Scheduling run", "runtime", run.Spec.Runtime)
	start := time.Now()
	defer func() {
		r.metricsRecorder().syncDuration.WithLabelValues(run.Spec.Runtime).Observe(time.Since(start).Seconds())
	}()

	if retryDelay := pendingRetryDelay(&run); retryDelay > 0 {
		return ctrl.Result{RequeueAfter: retryDelay}, nil
	}

	snapshot, err := r.loadSchedulingSnapshot(ctx, &run, request)
	if err != nil {
		return ctrl.Result{}, err
	}
	plan, err := r.planSchedulingCycle(snapshot)
	if err != nil {
		if isScorePluginError(err) {
			return ctrl.Result{}, err
		}
		r.metricsRecorder().noPodsTotal.WithLabelValues(run.Spec.Runtime).Inc()
		run.Status.Phase = v1alpha1.RunFailed
		run.Status.Message = fmt.Sprintf("pod selection failed: %v", err)
		if err := r.Status().Update(ctx, &run); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, fmt.Errorf("update selection failure status: %w", err)
		}
		r.metricsRecorder().runsScheduled.WithLabelValues(run.Spec.Runtime, "selection_error").Inc()
		return ctrl.Result{}, nil
	}

	if plan.action == schedulingPlanWait {
		r.metricsRecorder().noPodsTotal.WithLabelValues(run.Spec.Runtime).Inc()
		log.Info("No available runtime pods", "runtime", run.Spec.Runtime)

		message := plan.message
		if run.Status.Phase != v1alpha1.RunPending || run.Status.Message != message {
			run.Status.Phase = v1alpha1.RunPending
			run.Status.Message = message
			if err := r.Status().Update(ctx, &run); err != nil {
				return ctrl.Result{}, fmt.Errorf("update run status: %w", err)
			}
		}
		r.metricsRecorder().runsScheduled.WithLabelValues(run.Spec.Runtime, "no_pods").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if plan.action != schedulingPlanBind || plan.selected == nil {
		return ctrl.Result{}, fmt.Errorf("apply scheduling plan: unsupported action %q", plan.action)
	}

	reserved := r.reserve(snapshot, plan.selected)
	if !reserved {
		return ctrl.Result{Requeue: true}, nil
	}
	if err := r.bind(ctx, &run, plan.selected); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("bind run: %w", err)
	}

	log.Info("Run scheduled", "pod", plan.selected.Name)
	r.metricsRecorder().runsScheduled.WithLabelValues(run.Spec.Runtime, "scheduled").Inc()
	r.observeRunQueueDuration(&run, time.Now())
	return ctrl.Result{}, nil
}

func (r *RunReconciler) observeRunQueueDuration(run *v1alpha1.Run, scheduledAt time.Time) {
	if seconds, ok := runQueueDurationSeconds(run, scheduledAt); ok {
		r.metricsRecorder().runQueueDuration.WithLabelValues(run.Spec.Runtime).Observe(seconds)
	}
}

func runQueueDurationSeconds(run *v1alpha1.Run, scheduledAt time.Time) (float64, bool) {
	if run == nil || run.CreationTimestamp.IsZero() || scheduledAt.IsZero() {
		return 0, false
	}
	duration := scheduledAt.Sub(run.CreationTimestamp.Time)
	if duration < 0 {
		return 0, false
	}
	return duration.Seconds(), true
}

func (r *RunReconciler) applyCancelled(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	now := metav1.Now()
	runstatus.SetTerminal(run, v1alpha1.RunCancelled, runretry.ReasonCancelled, "cancelled by user", now)
	if err := r.Status().Update(ctx, run); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("update run status: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *RunReconciler) applyInvalidResources(ctx context.Context, run *v1alpha1.Run, validationErr error) (ctrl.Result, error) {
	runstatus.SetTerminal(run, v1alpha1.RunFailed, "InvalidResources", validationErr.Error(), metav1.Now())
	if err := r.Status().Update(ctx, run); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("update invalid resource status: %w", err)
	}
	return ctrl.Result{}, nil
}

func isPodSchedulable(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || !pod.DeletionTimestamp.IsZero() {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (r *RunReconciler) isRuntimePodAvailable(pod *corev1.Pod, now time.Time, usage, request corev1.ResourceList) bool {
	if !isPodSchedulable(pod) {
		return false
	}
	staleAfter := r.RuntimedHeartbeatStaleAfter
	if staleAfter <= 0 {
		staleAfter = defaultRuntimedHeartbeatStaleAfter
	}
	if !runtimepod.FreshRuntimedReady(pod, now, staleAfter) {
		return false
	}
	return runtimepod.Fits(
		runtimepod.Capacity(pod, defaultRuntimePodCapacity()),
		usage,
		request,
	)
}

func defaultRuntimePodCapacity() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(int64(v1alpha1.RuntimeDefaultRunsCapacity), resource.DecimalSI),
	}
}

func (r *RunReconciler) assignedRunUsage(ctx context.Context, namespace string) ([]v1alpha1.Run, map[string]corev1.ResourceList, error) {
	var runs v1alpha1.RunList
	if err := r.List(ctx, &runs, client.InNamespace(namespace)); err != nil {
		return nil, nil, err
	}

	usage := make(map[string]corev1.ResourceList)
	for _, run := range runs.Items {
		if run.Status.AssignedPod == "" {
			continue
		}
		switch run.Status.Phase {
		case v1alpha1.RunScheduled, v1alpha1.RunRunning, v1alpha1.RunReady:
			resources := usage[run.Status.AssignedPod]
			if resources == nil {
				resources = corev1.ResourceList{}
				usage[run.Status.AssignedPod] = resources
			}
			requests, err := run.Spec.ResourceRequests()
			if err != nil {
				continue
			}
			for resourceName, request := range requests {
				quantity := resources[resourceName].DeepCopy()
				quantity.Add(request)
				resources[resourceName] = quantity
			}
		}
	}
	return runs.Items, usage, nil
}

func pendingRetryDelay(run *v1alpha1.Run) time.Duration {
	if run.Status.Phase != v1alpha1.RunPending || run.Status.Attempt <= 1 {
		return 0
	}
	cond := meta.FindStatusCondition(run.Status.Conditions, "Running")
	if cond == nil || cond.Status != metav1.ConditionFalse {
		return 0
	}
	policy := runretry.WithDefaults(run.Spec.RetryPolicy)
	retryAt := cond.LastTransitionTime.Add(runretry.Backoff(policy, run.Status.Attempt))
	return time.Until(retryAt)
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *RunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.Run{}, runRuntimeIndexField, runRuntimeIndexValues); err != nil {
		return fmt.Errorf("index Runs by runtime: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Run{}, builder.WithPredicates(pendingRunPredicate())).
		Watches(
			&v1alpha1.Run{},
			handler.EnqueueRequestsFromMapFunc(r.pendingRunsForReleasedCapacity),
			builder.WithPredicates(runCapacityReleasedPredicate()),
		).
		Watches(
			&corev1.Pod{},
			handler.EnqueueRequestsFromMapFunc(r.pendingRunsForRuntimePod),
			builder.WithPredicates(runtimePodSchedulingPredicate()),
		).
		Complete(r)
}

func (r *RunReconciler) pendingRunsForReleasedCapacity(ctx context.Context, object client.Object) []reconcile.Request {
	run, ok := object.(*v1alpha1.Run)
	if !ok || run.Spec.Runtime == "" {
		return nil
	}
	return r.pendingRunsForRuntime(ctx, run.Namespace, run.Spec.Runtime, "released runtime capacity")
}

func (r *RunReconciler) pendingRunsForRuntimePod(ctx context.Context, object client.Object) []reconcile.Request {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return nil
	}
	runtimeName := pod.Labels["runtime"]
	if runtimeName == "" {
		return nil
	}
	return r.pendingRunsForRuntime(ctx, pod.Namespace, runtimeName, "runtime pod scheduling change")
}

func (r *RunReconciler) pendingRunsForRuntime(ctx context.Context, namespace, runtimeName, source string) []reconcile.Request {
	var runs v1alpha1.RunList
	if err := r.List(ctx, &runs, client.InNamespace(namespace), client.MatchingFields{runRuntimeIndexField: runtimeName}); err != nil {
		r.Log.Error(err, "unable to list pending runs", "source", source, "namespace", namespace, "runtime", runtimeName)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(runs.Items))
	for i := range runs.Items {
		pending := &runs.Items[i]
		if pending.Status.Phase != "" && pending.Status.Phase != v1alpha1.RunPending {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(pending)})
	}
	return requests
}

func runRuntimeIndexValues(object client.Object) []string {
	run, ok := object.(*v1alpha1.Run)
	if !ok || run.Spec.Runtime == "" {
		return nil
	}
	return []string{run.Spec.Runtime}
}

func runtimePodSchedulingPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return runtimePodName(e.Object) != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldPod, oldOK := e.ObjectOld.(*corev1.Pod)
			newPod, newOK := e.ObjectNew.(*corev1.Pod)
			if !oldOK || !newOK {
				return false
			}
			if runtimePodName(oldPod) == "" && runtimePodName(newPod) == "" {
				return false
			}
			return !reflect.DeepEqual(schedulingStateForRuntimePod(oldPod), schedulingStateForRuntimePod(newPod))
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return runtimePodName(e.Object) != ""
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

type runtimePodSchedulingState struct {
	runtimeName   string
	phase         corev1.PodPhase
	deleting      bool
	podReady      corev1.ConditionStatus
	runtimedReady *corev1.PodCondition
	capacity      map[string]string
}

func schedulingStateForRuntimePod(pod *corev1.Pod) runtimePodSchedulingState {
	state := runtimePodSchedulingState{
		runtimeName: runtimePodName(pod),
		phase:       pod.Status.Phase,
		deleting:    pod.DeletionTimestamp != nil,
		capacity:    map[string]string{},
	}
	for _, condition := range pod.Status.Conditions {
		switch condition.Type {
		case corev1.PodReady:
			state.podReady = condition.Status
		case v1alpha1.RuntimePodRuntimedReadyCondition:
			conditionCopy := condition
			state.runtimedReady = &conditionCopy
		}
	}
	for key, value := range pod.Annotations {
		if len(key) >= len(v1alpha1.RuntimePodCapacityAnnotationPrefix) && key[:len(v1alpha1.RuntimePodCapacityAnnotationPrefix)] == v1alpha1.RuntimePodCapacityAnnotationPrefix {
			state.capacity[key] = value
		}
	}
	return state
}

func runtimePodName(object client.Object) string {
	if object == nil {
		return ""
	}
	return object.GetLabels()["runtime"]
}

func pendingRunPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			t, ok := e.Object.(*v1alpha1.Run)
			return ok && (t.Status.Phase == "" || t.Status.Phase == v1alpha1.RunPending)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			t, ok := e.ObjectNew.(*v1alpha1.Run)
			return ok && (t.Status.Phase == "" || t.Status.Phase == v1alpha1.RunPending)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func runCapacityReleasedPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldRun, oldOK := e.ObjectOld.(*v1alpha1.Run)
			newRun, newOK := e.ObjectNew.(*v1alpha1.Run)
			return oldOK && newOK && consumesRuntimeCapacity(oldRun.Status.Phase) && !consumesRuntimeCapacity(newRun.Status.Phase)
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			run, ok := e.Object.(*v1alpha1.Run)
			return ok && consumesRuntimeCapacity(run.Status.Phase)
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}

func consumesRuntimeCapacity(phase v1alpha1.RunPhase) bool {
	switch phase {
	case v1alpha1.RunScheduled, v1alpha1.RunRunning, v1alpha1.RunReady:
		return true
	default:
		return false
	}
}
