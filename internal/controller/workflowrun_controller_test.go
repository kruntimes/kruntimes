package controller

import (
	"context"
	"testing"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestWorkflowRunReconcilerAcceptsWorkflowRun(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", Generation: 3},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}},
				},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile workflowrun: %v", err)
	}

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowPending {
		t.Fatalf("phase = %q, want %q", updated.Status.Phase, v1alpha1.WorkflowPending)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, workflowRunAcceptedCondition)
	if cond == nil {
		t.Fatalf("missing %s condition", workflowRunAcceptedCondition)
	}
	if cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != workflowRun.Generation {
		t.Fatalf("condition = %#v, want true observed generation %d", cond, workflowRun.Generation)
	}
}
