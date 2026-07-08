package controller

import (
	"context"
	"slices"
	"testing"

	"github.com/go-logr/logr/testr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestPersistentWorkspaceReconcileRuntimeFound(t *testing.T) {
	ctx := context.Background()
	scheme := persistentWorkspaceTestScheme(t)
	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "default"},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime: "bash",
		},
	}
	runtimeResource := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PersistentWorkspace{}).
		WithObjects(workspace, runtimeResource).
		Build()
	reconciler := &PersistentWorkspaceReconciler{Client: client, Log: testr.New(t), Scheme: scheme}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ci", Namespace: "default"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got v1alpha1.PersistentWorkspace
	if err := client.Get(ctx, types.NamespacedName{Name: "ci", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if got.Status.Phase != v1alpha1.PersistentWorkspacePending {
		t.Fatalf("phase = %q, want Pending", got.Status.Phase)
	}
	if got.Status.Runtime != "bash" {
		t.Fatalf("status runtime = %q, want bash", got.Status.Runtime)
	}
	assertPersistentWorkspaceCondition(t, got.Status.Conditions, persistentWorkspaceAcceptedCondition, metav1.ConditionTrue, "Accepted")
	assertPersistentWorkspaceCondition(t, got.Status.Conditions, persistentWorkspaceRuntimeCondition, metav1.ConditionTrue, "RuntimeFound")
}

func TestPersistentWorkspaceReconcileRuntimeNotFound(t *testing.T) {
	ctx := context.Background()
	scheme := persistentWorkspaceTestScheme(t)
	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "default"},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime: "missing",
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.PersistentWorkspace{}).
		WithObjects(workspace).
		Build()
	reconciler := &PersistentWorkspaceReconciler{Client: client, Log: testr.New(t), Scheme: scheme}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "ci", Namespace: "default"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got v1alpha1.PersistentWorkspace
	if err := client.Get(ctx, types.NamespacedName{Name: "ci", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	assertPersistentWorkspaceCondition(t, got.Status.Conditions, persistentWorkspaceRuntimeCondition, metav1.ConditionFalse, "RuntimeNotFound")
}

func TestPersistentWorkspaceRuntimeWatchEnqueuesMatchingWorkspaces(t *testing.T) {
	ctx := context.Background()
	scheme := persistentWorkspaceTestScheme(t)
	matching := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "matching", Namespace: "default"},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: "bash"},
	}
	otherRuntime := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "other-runtime", Namespace: "default"},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: "python"},
	}
	otherNamespace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "other-namespace", Namespace: "other"},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: "bash"},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(matching, otherRuntime, otherNamespace).
		Build()
	reconciler := &PersistentWorkspaceReconciler{Client: client, Log: testr.New(t), Scheme: scheme}

	requests := reconciler.workspacesForRuntime(ctx, &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
	})
	if len(requests) != 1 {
		t.Fatalf("requests = %v, want exactly one", requests)
	}
	if got := requests[0].NamespacedName.String(); got != "default/matching" {
		t.Fatalf("request = %s, want default/matching", got)
	}
	if slices.ContainsFunc(requests, func(req ctrl.Request) bool { return req.Name == "other-runtime" || req.Name == "other-namespace" }) {
		t.Fatalf("requests include non-matching workspace: %v", requests)
	}
}

func persistentWorkspaceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return scheme
}

func assertPersistentWorkspaceCondition(t *testing.T, conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus, reason string) {
	t.Helper()
	condition := findCondition(conditions, conditionType)
	if condition == nil {
		t.Fatalf("condition %q not found in %#v", conditionType, conditions)
	}
	if condition.Status != status || condition.Reason != reason {
		t.Fatalf("condition %q = (%s, %s), want (%s, %s)", conditionType, condition.Status, condition.Reason, status, reason)
	}
}
