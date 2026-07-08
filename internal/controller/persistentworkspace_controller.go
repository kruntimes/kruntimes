package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	persistentWorkspaceAcceptedCondition = "Accepted"
	persistentWorkspaceRuntimeCondition  = "RuntimeReady"
)

// PersistentWorkspaceReconciler owns PersistentWorkspace lifecycle state.
type PersistentWorkspaceReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=persistentworkspaces,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=persistentworkspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *PersistentWorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var workspace v1alpha1.PersistentWorkspace
	if err := r.Get(ctx, req.NamespacedName, &workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workspace.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	base := workspace.DeepCopy()
	if workspace.Status.Phase == "" {
		workspace.Status.Phase = v1alpha1.PersistentWorkspacePending
	}
	workspace.Status.Runtime = workspace.Spec.Runtime
	apimeta.SetStatusCondition(&workspace.Status.Conditions, metav1.Condition{
		Type:               persistentWorkspaceAcceptedCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "PersistentWorkspace spec accepted; binding is not implemented yet",
		ObservedGeneration: workspace.Generation,
	})

	var runtimeResource v1alpha1.Runtime
	key := client.ObjectKey{Namespace: workspace.Namespace, Name: workspace.Spec.Runtime}
	if err := r.Get(ctx, key, &runtimeResource); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get runtime %s: %w", key, err)
		}
		apimeta.SetStatusCondition(&workspace.Status.Conditions, metav1.Condition{
			Type:               persistentWorkspaceRuntimeCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "RuntimeNotFound",
			Message:            fmt.Sprintf("Runtime %q does not exist", workspace.Spec.Runtime),
			ObservedGeneration: workspace.Generation,
		})
	} else {
		apimeta.SetStatusCondition(&workspace.Status.Conditions, metav1.Condition{
			Type:               persistentWorkspaceRuntimeCondition,
			Status:             metav1.ConditionTrue,
			Reason:             "RuntimeFound",
			Message:            fmt.Sprintf("Runtime %q exists; waiting for workspace binding implementation", runtimeResource.Name),
			ObservedGeneration: workspace.Generation,
		})
	}

	if err := r.Status().Patch(ctx, &workspace, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch persistent workspace status: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *PersistentWorkspaceReconciler) workspacesForRuntime(ctx context.Context, obj client.Object) []reconcile.Request {
	var list v1alpha1.PersistentWorkspaceList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		workspace := &list.Items[i]
		if workspace.Spec.Runtime != obj.GetName() {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	}
	return requests
}

// SetupWithManager registers the PersistentWorkspace reconciler.
func (r *PersistentWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.PersistentWorkspace{}).
		Watches(&v1alpha1.Runtime{}, handler.EnqueueRequestsFromMapFunc(r.workspacesForRuntime)).
		Complete(r)
}
