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

func TestWorkflowReconcilerAcceptsWorkflowDefinition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", Generation: 2},
		Spec: v1alpha1.WorkflowSpec{
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
		WithObjects(workflow).
		WithStatusSubresource(&v1alpha1.Workflow{}).
		Build()

	reconciler := &WorkflowReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflow.Namespace,
		Name:      workflow.Name,
	}}
	if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("reconcile workflow: %v", err)
	}

	var updated v1alpha1.Workflow
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflow), &updated); err != nil {
		t.Fatalf("get workflow: %v", err)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, workflowReadyCondition)
	if cond == nil {
		t.Fatalf("missing %s condition", workflowReadyCondition)
	}
	if cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != workflow.Generation {
		t.Fatalf("condition = %#v, want true observed generation %d", cond, workflow.Generation)
	}
}
