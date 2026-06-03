package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
)

var (
	runsScheduled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kruntimes_scheduler_sync_total",
			Help: "Total number of tasks processed by the scheduler.",
		},
		[]string{"runtime", "result"},
	)
	syncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kruntimes_scheduler_sync_duration_seconds",
			Help:    "Latency of run scheduling.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"runtime"},
	)
	noPodsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kruntimes_scheduler_no_pods_total",
			Help: "Total number of tasks that could not find a matching runtime pod.",
		},
		[]string{"runtime"},
	)
)

func init() {
	metrics.Registry.MustRegister(runsScheduled, syncDuration, noPodsTotal)
}

// RunReconciler watches Pending Tasks and assigns them to Runtime Pods.
type RunReconciler struct {
	client.Client
	Log      logr.Logger
	Strategy Strategy
}

// +kubebuilder:rbac:groups=kruntimes.kruntimes.com,resources=runs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.kruntimes.com,resources=runs/status,verbs=get;update;patch
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

	// Treat empty phase (CRD default not yet applied) as Pending.
	if run.Status.Phase != "" && run.Status.Phase != v1alpha1.RunPending {
		return ctrl.Result{}, nil
	}
	if run.Spec.Runtime == "" {
		return ctrl.Result{}, nil
	}

	log.Info("Scheduling run", "runtime", run.Spec.Runtime)
	start := time.Now()
	defer func() {
		syncDuration.WithLabelValues(run.Spec.Runtime).Observe(time.Since(start).Seconds())
	}()

	if retryDelay := pendingRetryDelay(&run); retryDelay > 0 {
		return ctrl.Result{RequeueAfter: retryDelay}, nil
	}

	reqLabel, err := labels.NewRequirement("runtime", selection.Equals, []string{run.Spec.Runtime})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build label requirement: %w", err)
	}
	sel := labels.NewSelector().Add(*reqLabel)

	var pods corev1.PodList
	if err := r.List(ctx, &pods, &client.ListOptions{
		Namespace:     req.Namespace,
		LabelSelector: sel,
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("list runtime pods: %w", err)
	}

	var candidates []corev1.Pod
	for _, pod := range pods.Items {
		if isPodSchedulable(&pod) {
			candidates = append(candidates, pod)
		}
	}

	if len(candidates) == 0 {
		noPodsTotal.WithLabelValues(run.Spec.Runtime).Inc()
		log.Info("No available runtime pods", "runtime", run.Spec.Runtime)

		message := fmt.Sprintf("waiting for available runtime pods for runtime %q", run.Spec.Runtime)
		if run.Status.Phase != v1alpha1.RunPending || run.Status.Message != message {
			run.Status.Phase = v1alpha1.RunPending
			run.Status.Message = message
			if err := r.Status().Update(ctx, &run); err != nil {
				return ctrl.Result{}, fmt.Errorf("update run status: %w", err)
			}
		}
		runsScheduled.WithLabelValues(run.Spec.Runtime, "no_pods").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	selected, err := r.Strategy.Select(ctx, r.Client, candidates, &run)
	if err != nil {
		noPodsTotal.WithLabelValues(run.Spec.Runtime).Inc()

		run.Status.Phase = v1alpha1.RunFailed
		run.Status.Message = fmt.Sprintf("pod selection failed: %v", err)
		if err := r.Status().Update(ctx, &run); err != nil {
			return ctrl.Result{}, fmt.Errorf("update run status: %w", err)
		}
		runsScheduled.WithLabelValues(run.Spec.Runtime, "selection_error").Inc()
		return ctrl.Result{}, nil
	}

	run.Status.AssignedPod = selected.Name
	run.Status.Phase = v1alpha1.RunScheduled
	if err := r.Status().Update(ctx, &run); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("update run status: %w", err)
	}

	log.Info("Run scheduled", "pod", selected.Name)
	runsScheduled.WithLabelValues(run.Spec.Runtime, "scheduled").Inc()
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
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Run{}).
		WithEventFilter(pendingRunPredicate()).
		Complete(r)
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
