package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// StaleRunReaper watches Running Runs and detects those assigned to
// dead or unhealthy Runtime Pods, then either resets them for retry
// or marks them as Failed.
type StaleRunReaper struct {
	client.Client
	Log                logr.Logger
	Recorder           record.EventRecorder
	StalenessThreshold time.Duration
}

// SetupWithManager registers the reaper with the controller manager.
func (r *StaleRunReaper) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Run{}).
		WithEventFilter(r.runningRunPredicate()).
		Complete(r)
}

func (r *StaleRunReaper) runningRunPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			run, ok := e.Object.(*v1alpha1.Run)
			return ok && run.Status.Phase == v1alpha1.RunRunning && run.Status.AssignedPod != ""
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			run, ok := e.ObjectNew.(*v1alpha1.Run)
			return ok && run.Status.Phase == v1alpha1.RunRunning && run.Status.AssignedPod != ""
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}

// Reconcile checks whether the assigned Pod is still alive.
func (r *StaleRunReaper) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var run v1alpha1.Run
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if run.Status.Phase != v1alpha1.RunRunning || run.Status.AssignedPod == "" {
		return ctrl.Result{}, nil
	}

	podName := run.Status.AssignedPod
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Name: podName, Namespace: run.Namespace}, &pod)
	if apierrors.IsNotFound(err) {
		r.Log.Info("pod not found, marking run as stale", "run", run.Name, "pod", podName)
		r.handleStaleRun(ctx, &run)
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check if the pod is unhealthy and has been for longer than the threshold.
	if r.isPodStale(&pod) {
		r.handleStaleRun(ctx, &run)
		return ctrl.Result{}, nil
	}

	// Pod is healthy — requeue after threshold.
	return ctrl.Result{RequeueAfter: r.StalenessThreshold}, nil
}

func (r *StaleRunReaper) isPodStale(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return true
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			if cond.Status == corev1.ConditionTrue {
				return false
			}
			return time.Since(cond.LastTransitionTime.Time) > r.StalenessThreshold
		}
	}
	// No Ready condition yet — pod is initializing, not stale.
	return false
}

func (r *StaleRunReaper) handleStaleRun(ctx context.Context, run *v1alpha1.Run) {
	reason := "PodGone"
	msg := "assigned pod was deleted"

	// Determine failure reason.
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Name: run.Status.AssignedPod, Namespace: run.Namespace}, &pod)
	if err == nil {
		if pod.DeletionTimestamp != nil {
			reason = "PodTerminating"
			msg = "assigned pod is terminating"
		} else {
			reason = "PodUnhealthy"
			msg = "assigned pod is not ready"
		}
	}

	podName := run.Status.AssignedPod
	policy := run.Spec.RetryPolicy
	if policy != nil && policy.MaxAttempts > 1 {
		// Reset for retry — scheduler will re-assign.
		run.Status.Phase = v1alpha1.RunPending
		run.Status.AssignedPod = ""
		run.Status.StartTime = nil
		if run.Status.Attempt == 0 {
			run.Status.Attempt = 1
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(run, corev1.EventTypeWarning, "StaleRunRetrying",
				"Pod %s is unhealthy (%s), rescheduling run for retry (attempt %d/%d)",
				podName, reason, run.Status.Attempt, policy.MaxAttempts)
		}
	} else {
		now := metav1.Now()
		run.Status.Phase = v1alpha1.RunFailed
		run.Status.Message = msg
		run.Status.CompletionTime = &now
		if run.Status.Attempt == 0 {
			run.Status.Attempt = 1
		}
		if r.Recorder != nil {
			r.Recorder.Eventf(run, corev1.EventTypeWarning, "StaleRunFailed",
				"Pod %s is unhealthy, marking run as failed: %s", podName, reason)
		}
	}

	if err := r.Status().Update(ctx, run); err != nil {
		r.Log.Error(err, "failed to update stale run", "run", run.Name)
	}
}
