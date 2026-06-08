package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// CompletedRunGC deletes terminal Runs after their retention TTL expires.
type CompletedRunGC struct {
	client.Client
	Log logr.Logger
	Now func() time.Time
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=runs,verbs=get;list;watch;delete

// SetupWithManager registers the GC with the controller manager.
func (g *CompletedRunGC) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Run{}).
		WithEventFilter(g.completedRunPredicate()).
		Complete(g)
}

func (g *CompletedRunGC) completedRunPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			run, ok := e.Object.(*v1alpha1.Run)
			return ok && isTerminalRunPhase(run.Status.Phase)
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			run, ok := e.ObjectNew.(*v1alpha1.Run)
			return ok && isTerminalRunPhase(run.Status.Phase)
		},
		DeleteFunc:  func(e event.DeleteEvent) bool { return false },
		GenericFunc: func(e event.GenericEvent) bool { return false },
	}
}

// Reconcile checks whether a completed Run has exceeded its retention TTL.
func (g *CompletedRunGC) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var run v1alpha1.Run
	if err := g.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !isTerminalRunPhase(run.Status.Phase) || run.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}
	if run.Status.CompletionTime == nil {
		return ctrl.Result{}, nil
	}

	ttl := runTTL(&run)
	if ttl <= 0 {
		return ctrl.Result{}, nil
	}

	expiresAt := run.Status.CompletionTime.Add(ttl)
	now := g.now()
	if now.Before(expiresAt) {
		return ctrl.Result{RequeueAfter: expiresAt.Sub(now)}, nil
	}

	if err := g.Delete(ctx, &run); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete completed run: %w", err)
	}
	if g.Log.GetSink() != nil {
		g.Log.Info("Deleted completed Run after retention TTL", "run", req.NamespacedName, "ttl", ttl.String())
	}
	return ctrl.Result{}, nil
}

func (g *CompletedRunGC) now() time.Time {
	if g.Now != nil {
		return g.Now()
	}
	return time.Now()
}

func runTTL(run *v1alpha1.Run) time.Duration {
	if run.Spec.TTLSecondsAfterFinished == nil || *run.Spec.TTLSecondsAfterFinished <= 0 {
		return 0
	}
	return time.Duration(*run.Spec.TTLSecondsAfterFinished) * time.Second
}

func isTerminalRunPhase(phase v1alpha1.RunPhase) bool {
	switch phase {
	case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
		return true
	default:
		return false
	}
}
