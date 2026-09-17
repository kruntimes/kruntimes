package controller

import (
	"reflect"
	"testing"

	v1alpha1 "github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestWorkflowEnvironmentOutputs(t *testing.T) {
	outputs := map[string]string{
		"result": v1alpha1.WorkflowEnvironmentOutputPrefix + "PATH",
		v1alpha1.WorkflowEnvironmentOutputPrefix + "GOBIN": "/workspace/.tools/bin",
		v1alpha1.WorkflowEnvironmentOutputPrefix + "PATH":  "/workspace/.tools/bin:/usr/bin",
	}

	got, err := workflowEnvironmentOutputs(outputs)
	if err != nil {
		t.Fatalf("workflowEnvironmentOutputs() error = %v", err)
	}
	want := map[string]string{
		"GOBIN": "/workspace/.tools/bin",
		"PATH":  "/workspace/.tools/bin:/usr/bin",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workflowEnvironmentOutputs() = %#v, want %#v", got, want)
	}
}

func TestWorkflowEnvironmentOutputsRejectInvalidName(t *testing.T) {
	_, err := workflowEnvironmentOutputs(map[string]string{
		v1alpha1.WorkflowEnvironmentOutputPrefix + "NOT-VALID": "value",
	})
	if err == nil {
		t.Fatal("workflowEnvironmentOutputs() error = nil, want invalid variable name error")
	}
}

func TestWorkflowStepEnvironment(t *testing.T) {
	status := v1alpha1.JobStatus{Steps: []v1alpha1.StepStatus{
		{
			Phase: v1alpha1.StepSucceeded,
			Outputs: map[string]string{
				v1alpha1.WorkflowEnvironmentOutputPrefix + "PATH": "/tools/go/bin:/usr/bin",
			},
		},
		{
			Phase: v1alpha1.StepSucceeded,
			Outputs: map[string]string{
				v1alpha1.WorkflowEnvironmentOutputPrefix + "GOBIN": "/workspace/bin",
			},
		},
	}}

	got, err := workflowStepEnvironment(status, jobStepRunTarget{stepIndex: 2})
	if err != nil {
		t.Fatalf("workflowStepEnvironment() error = %v", err)
	}
	want := map[string]string{"PATH": "/tools/go/bin:/usr/bin", "GOBIN": "/workspace/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workflowStepEnvironment() = %#v, want %#v", got, want)
	}
}

func TestWorkflowStepEnvironmentIncludesCompletedActionSteps(t *testing.T) {
	status := v1alpha1.JobStatus{Steps: []v1alpha1.StepStatus{{
		Phase: v1alpha1.StepRunning,
		ActionSteps: []v1alpha1.ActionStepStatus{
			{
				Phase: v1alpha1.StepSucceeded,
				Outputs: map[string]string{
					v1alpha1.WorkflowEnvironmentOutputPrefix + "GOROOT": "/cache/go/1.26.0",
				},
			},
			{Phase: v1alpha1.StepPending},
		},
	}}}

	got, err := workflowStepEnvironment(status, jobStepRunTarget{stepIndex: 0, actionStepIndex: 2})
	if err != nil {
		t.Fatalf("workflowStepEnvironment() error = %v", err)
	}
	want := map[string]string{"GOROOT": "/cache/go/1.26.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("workflowStepEnvironment() = %#v, want %#v", got, want)
	}
}

func TestPublicWorkflowOutputs(t *testing.T) {
	got := publicWorkflowOutputs(map[string]string{
		"result": v1alpha1.WorkflowEnvironmentOutputPrefix + "PATH",
		v1alpha1.WorkflowEnvironmentOutputPrefix + "PATH": "/tools/go/bin:/usr/bin",
	})
	want := map[string]string{"result": v1alpha1.WorkflowEnvironmentOutputPrefix + "PATH"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("publicWorkflowOutputs() = %#v, want %#v", got, want)
	}
}
