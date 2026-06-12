package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	"github.com/kruntimes/kruntimes/internal/runstatus"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
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
		if err := r.handleStaleRun(ctx, &run, runretry.ReasonPodGone, "assigned pod was deleted"); err != nil {
			return ctrl.Result{}, fmt.Errorf("update stale run status: %w", err)
		}
		return ctrl.Result{}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	stale, reason, message := r.stalePodState(&pod, time.Now())
	if stale {
		if err := r.handleStaleRun(ctx, &run, reason, message); err != nil {
			return ctrl.Result{}, fmt.Errorf("update stale run status: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Pod is healthy — requeue after threshold.
	return ctrl.Result{RequeueAfter: r.StalenessThreshold}, nil
}

func (r *StaleRunReaper) stalePodState(pod *corev1.Pod, now time.Time) (bool, string, string) {
	if pod.DeletionTimestamp != nil {
		return true, runretry.ReasonPodTerminating, "assigned pod is terminating"
	}

	for _, cond := range pod.Status.Conditions {
		if cond.Type != corev1.PodReady {
			continue
		}
		if cond.Status != corev1.ConditionTrue {
			stale := r.StalenessThreshold <= 0 ||
				now.Sub(cond.LastTransitionTime.Time) > r.StalenessThreshold
			return stale, runretry.ReasonPodUnhealthy, "assigned pod is not ready"
		}
		if !runtimepod.FreshRuntimedReady(pod, now, r.StalenessThreshold) {
			return true, runretry.ReasonPodUnhealthy, "assigned pod runtimed heartbeat is stale"
		}
		return false, "", ""
	}

	// No Ready condition yet — pod is initializing, not stale.
	return false, "", ""
}

func (r *StaleRunReaper) handleStaleRun(ctx context.Context, run *v1alpha1.Run, reason, msg string) error {
	podName := run.Status.AssignedPod
	policy := runretry.WithDefaults(run.Spec.RetryPolicy)
	curAttempt := runretry.CurrentAttempt(run.Status.Attempt)
	if runretry.ShouldRetry(policy, curAttempt, reason) {
		// Reset for retry — scheduler will re-assign.
		nextAttempt := runretry.NextAttempt(run.Status.Attempt)
		run.Status.Attempt = nextAttempt
		run.Status.Phase = v1alpha1.RunPending
		run.Status.AssignedPod = ""
		run.Status.StartTime = nil
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type: "Running", Status: metav1.ConditionFalse, Reason: reason, Message: msg,
		})
	} else {
		now := metav1.Now()
		run.Status.Attempt = curAttempt
		runstatus.SetTerminal(run, v1alpha1.RunFailed, reason, msg, now)
	}

	if err := r.Status().Update(ctx, run); err != nil {
		return err
	}
	if r.Recorder == nil {
		return nil
	}
	if run.Status.Phase == v1alpha1.RunPending {
		r.Recorder.Eventf(run, corev1.EventTypeWarning, "StaleRunRetrying",
			"Pod %s is unhealthy (%s), rescheduling run for retry (attempt %d/%d, backoff %s)",
			podName, reason, run.Status.Attempt, policy.MaxAttempts, runretry.Backoff(policy, run.Status.Attempt))
	} else {
		r.Recorder.Eventf(run, corev1.EventTypeWarning, "StaleRunFailed",
			"Pod %s is unhealthy, marking run as failed: %s", podName, reason)
	}
	return nil
}
