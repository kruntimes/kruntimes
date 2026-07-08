package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const actionReadyCondition = "Ready"

// ActionReconciler owns reusable Action definition status.
type ActionReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=actions,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=actions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *ActionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var action v1alpha1.Action
	if err := r.Get(ctx, req.NamespacedName, &action); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !action.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	base := action.DeepCopy()
	apimeta.SetStatusCondition(&action.Status.Conditions, metav1.Condition{
		Type:               actionReadyCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "Action definition accepted; execution is handled by WorkflowRun",
		ObservedGeneration: action.Generation,
	})
	if err := r.Status().Patch(ctx, &action, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch action status: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the Action reconciler.
func (r *ActionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Action{}).
		Complete(r)
}
