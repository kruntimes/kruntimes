package workflowtemplate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestValidateCallGraph(t *testing.T) {
	tests := []struct {
		name      string
		workflows map[string]v1alpha1.Workflow
		root      string
		wantError string
	}{
		{
			name: "acyclic nested calls",
			root: "release",
			workflows: map[string]v1alpha1.Workflow{
				"release": workflowWithCalls("release", "build", "deploy"),
				"build":   workflowWithCalls("build", "verify"),
				"deploy":  workflowWithCalls("deploy"),
				"verify":  workflowWithCalls("verify"),
			},
		},
		{
			name: "self cycle",
			root: "release",
			workflows: map[string]v1alpha1.Workflow{
				"release": workflowWithCalls("release", "release"),
			},
			wantError: "workflow call cycle: release -> release",
		},
		{
			name: "multi workflow cycle",
			root: "release",
			workflows: map[string]v1alpha1.Workflow{
				"release": workflowWithCalls("release", "build"),
				"build":   workflowWithCalls("build", "deploy"),
				"deploy":  workflowWithCalls("deploy", "release"),
			},
			wantError: "workflow call cycle: release -> build -> deploy -> release",
		},
		{
			name: "missing workflow",
			root: "release",
			workflows: map[string]v1alpha1.Workflow{
				"release": workflowWithCalls("release", "missing"),
			},
			wantError: `get workflow "missing": not found`,
		},
		{
			name:      "maximum nesting depth",
			root:      "workflow-0",
			workflows: nestedWorkflows(MaxCallDepth + 1),
			wantError: "workflow call depth exceeds maximum 8: workflow-0 -> workflow-1 -> workflow-2 -> workflow-3 -> workflow-4 -> workflow-5 -> workflow-6 -> workflow-7 -> workflow-8",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCallGraph(context.Background(), tt.root, func(_ context.Context, name string) (*v1alpha1.Workflow, error) {
				workflow, ok := tt.workflows[name]
				if !ok {
					return nil, errors.New("not found")
				}
				return workflow.DeepCopy(), nil
			})
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("ValidateCallGraph() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("ValidateCallGraph() error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func workflowWithCalls(name string, calls ...string) v1alpha1.Workflow {
	jobs := map[string]v1alpha1.JobSpec{
		"inline": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "run", Run: "echo ready"}}},
	}
	for i, call := range calls {
		jobs[fmt.Sprintf("call-%d", i)] = v1alpha1.JobSpec{Uses: call}
	}
	return v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1alpha1.WorkflowSpec{Jobs: jobs},
	}
}

func nestedWorkflows(count int) map[string]v1alpha1.Workflow {
	workflows := make(map[string]v1alpha1.Workflow, count)
	for i := range count {
		name := fmt.Sprintf("workflow-%d", i)
		if i == count-1 {
			workflows[name] = workflowWithCalls(name)
			continue
		}
		workflows[name] = workflowWithCalls(name, fmt.Sprintf("workflow-%d", i+1))
	}
	return workflows
}
