package controller

import (
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestWorkflowSnapshotRoundTripsFrozenAction(t *testing.T) {
	snapshot := &workflowExecutionSnapshot{
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: "setup-python-tools"}}},
		}},
		Actions: map[string]workflowActionSnapshot{
			"build/setup": {
				Name: "setup-python-tools",
				Spec: v1alpha1.ActionSpec{
					Inputs:  map[string]v1alpha1.ActionInputSpec{"version": {Default: "3.13"}},
					Outputs: map[string]v1alpha1.ActionOutputSpec{"version": {Value: "${{ steps.install.outputs.version }}"}},
					Steps:   []v1alpha1.StepSpec{{Name: "install", Run: "echo install"}},
				},
			},
		},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	loaded, err := loadWorkflowSnapshot(&appsv1.ControllerRevision{Data: runtime.RawExtension{Raw: raw}})
	if err != nil {
		t.Fatalf("load snapshot: %v", err)
	}
	action, ok := loaded.Actions["build/setup"]
	if !ok {
		t.Fatalf("loaded actions = %#v, want build/setup", loaded.Actions)
	}
	if action.Name != "setup-python-tools" || action.Spec.Steps[0].Run != "echo install" {
		t.Fatalf("loaded Action = %#v, want frozen definition", action)
	}
	if action.Spec.Inputs["version"].Default != "3.13" {
		t.Fatalf("loaded Action inputs = %#v, want version default", action.Spec.Inputs)
	}
}
