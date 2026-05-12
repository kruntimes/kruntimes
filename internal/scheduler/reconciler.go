package scheduler

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/airconduct/kruntime/api/v1alpha1"
)

var (
	tasksScheduled = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kruntime_scheduler_sync_total",
			Help: "Total number of tasks processed by the scheduler.",
		},
		[]string{"runtime", "result"},
	)
	syncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "kruntime_scheduler_sync_duration_seconds",
			Help:    "Latency of task scheduling.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"runtime"},
	)
	noPodsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "kruntime_scheduler_no_pods_total",
			Help: "Total number of tasks that could not find a matching runtime pod.",
		},
		[]string{"runtime"},
	)
)

func init() {
	metrics.Registry.MustRegister(tasksScheduled, syncDuration, noPodsTotal)
}

// TaskReconciler watches Pending Tasks and assigns them to Runtime Pods.
type TaskReconciler struct {
	client.Client
	Log      logr.Logger
	Strategy Strategy
}

// +kubebuilder:rbac:groups=kruntime.airconduct.com,resources=tasks,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntime.airconduct.com,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("task", req.NamespacedName)

	var task v1alpha1.Task
	if err := r.Get(ctx, req.NamespacedName, &task); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get task: %w", err)
	}

	if task.Status.Phase != v1alpha1.TaskPending {
		return ctrl.Result{}, nil
	}

	log.Info("Scheduling task", "runtime", task.Spec.Runtime)
	start := time.Now()
	defer func() {
		syncDuration.WithLabelValues(task.Spec.Runtime).Observe(time.Since(start).Seconds())
	}()

	reqLabel, err := labels.NewRequirement("runtime", selection.Equals, []string{task.Spec.Runtime})
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
		if pod.Status.Phase == corev1.PodRunning && pod.DeletionTimestamp.IsZero() {
			candidates = append(candidates, pod)
		}
	}

	if len(candidates) == 0 {
		noPodsTotal.WithLabelValues(task.Spec.Runtime).Inc()
		log.Info("No available runtime pods", "runtime", task.Spec.Runtime)

		task.Status.Phase = v1alpha1.TaskFailed
		task.Status.Message = fmt.Sprintf("no available runtime pods for runtime %q", task.Spec.Runtime)
		if err := r.Status().Update(ctx, &task); err != nil {
			return ctrl.Result{}, fmt.Errorf("update task status: %w", err)
		}
		tasksScheduled.WithLabelValues(task.Spec.Runtime, "no_pods").Inc()
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	selected, err := r.Strategy.Select(ctx, r.Client, candidates, &task)
	if err != nil {
		noPodsTotal.WithLabelValues(task.Spec.Runtime).Inc()

		task.Status.Phase = v1alpha1.TaskFailed
		task.Status.Message = fmt.Sprintf("pod selection failed: %v", err)
		if err := r.Status().Update(ctx, &task); err != nil {
			return ctrl.Result{}, fmt.Errorf("update task status: %w", err)
		}
		tasksScheduled.WithLabelValues(task.Spec.Runtime, "selection_error").Inc()
		return ctrl.Result{}, nil
	}

	task.Status.AssignedPod = selected.Name
	task.Status.Phase = v1alpha1.TaskScheduled
	if err := r.Status().Update(ctx, &task); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("update task status: %w", err)
	}

	log.Info("Task scheduled", "pod", selected.Name)
	tasksScheduled.WithLabelValues(task.Spec.Runtime, "scheduled").Inc()
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Task{}).
		WithEventFilter(pendingTaskPredicate()).
		Complete(r)
}

func pendingTaskPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			t, ok := e.Object.(*v1alpha1.Task)
			return ok && t.Status.Phase == v1alpha1.TaskPending
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			t, ok := e.ObjectNew.(*v1alpha1.Task)
			return ok && t.Status.Phase == v1alpha1.TaskPending
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return false
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
	}
}
