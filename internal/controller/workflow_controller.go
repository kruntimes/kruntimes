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

const workflowReadyCondition = "Ready"

// WorkflowReconciler owns reusable Workflow definition status.
type WorkflowReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=workflows,verbs=get;list;watch
// +kubebuilder:rbac:groups=kruntimes.io,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var workflow v1alpha1.Workflow
	if err := r.Get(ctx, req.NamespacedName, &workflow); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workflow.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	base := workflow.DeepCopy()
	apimeta.SetStatusCondition(&workflow.Status.Conditions, metav1.Condition{
		Type:               workflowReadyCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "Workflow definition accepted; execution is handled by WorkflowRun",
		ObservedGeneration: workflow.Generation,
	})
	if err := r.Status().Patch(ctx, &workflow, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch workflow status: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the Workflow reconciler.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Workflow{}).
		Complete(r)
}
