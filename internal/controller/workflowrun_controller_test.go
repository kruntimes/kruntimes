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
				"build": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{
						{Name: "checkout", Run: "git status"},
						{Name: "package", Run: "make package"},
					},
				},
				"test": {
					RunsOn: "bash",
					Needs:  []string{"build"},
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
	build := updated.Status.Jobs["build"]
	if build.Phase != v1alpha1.JobPending {
		t.Fatalf("build phase = %q, want %q", build.Phase, v1alpha1.JobPending)
	}
	if len(build.Pre) != 0 {
		t.Fatalf("build pre = %v, want empty", build.Pre)
	}
	if len(build.Steps) != 2 || build.Steps[0].Name != "checkout" || build.Steps[1].Name != "package" {
		t.Fatalf("build steps = %#v, want checkout, package", build.Steps)
	}
	for _, step := range build.Steps {
		if step.Phase != v1alpha1.StepPending {
			t.Fatalf("step %s phase = %q, want %q", step.Name, step.Phase, v1alpha1.StepPending)
		}
	}
	testJob := updated.Status.Jobs["test"]
	if testJob.Phase != v1alpha1.JobWaiting {
		t.Fatalf("test phase = %q, want %q", testJob.Phase, v1alpha1.JobWaiting)
	}
	if len(testJob.Pre) != 1 || testJob.Pre[0] != "build" {
		t.Fatalf("test pre = %v, want [build]", testJob.Pre)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, workflowRunAcceptedCondition)
	if cond == nil {
		t.Fatalf("missing %s condition", workflowRunAcceptedCondition)
	}
	if cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != workflowRun.Generation {
		t.Fatalf("condition = %#v, want true observed generation %d", cond, workflowRun.Generation)
	}
}

func TestWorkflowRunReconcilerPreservesResolvedJobStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", Generation: 4},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}},
				},
			},
		},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowPending,
			Jobs: map[string]v1alpha1.JobStatus{
				"test": {
					Phase: v1alpha1.JobRunning,
					Pre:   []string{"prepare"},
					Steps: []v1alpha1.StepStatus{
						{Name: "unit", Phase: v1alpha1.StepRunning, RunName: "existing-run"},
					},
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
	status := updated.Status.Jobs["test"]
	if status.Phase != v1alpha1.JobRunning {
		t.Fatalf("job phase = %q, want %q", status.Phase, v1alpha1.JobRunning)
	}
	if len(status.Pre) != 1 || status.Pre[0] != "prepare" {
		t.Fatalf("job pre = %v, want [prepare]", status.Pre)
	}
	if len(status.Steps) != 1 || status.Steps[0].RunName != "existing-run" {
		t.Fatalf("steps = %#v, want existing run preserved", status.Steps)
	}
}
