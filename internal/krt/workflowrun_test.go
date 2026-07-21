package krt

import (
	"strings"
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestParseWorkflowRunManifest(t *testing.T) {
	data := []byte(`
apiVersion: kruntimes.io/v1alpha1
kind: WorkflowRun
metadata:
  name: build
spec:
  jobs:
    build:
      runs-on: bash
      steps:
        - name: compile
          run: make build
`)

	workflowRun, err := parseWorkflowRun(data, "team-a")
	if err != nil {
		t.Fatalf("parseWorkflowRun() error = %v", err)
	}
	if workflowRun.Name != "build" {
		t.Fatalf("name = %q, want build", workflowRun.Name)
	}
	if workflowRun.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want team-a", workflowRun.Namespace)
	}
	if job := workflowRun.Spec.Jobs["build"]; job.RunsOn != "bash" || len(job.Steps) != 1 || job.Steps[0].Run != "make build" {
		t.Fatalf("spec = %#v, want inline build job", workflowRun.Spec)
	}
}

func TestConvertGitHubWorkflowToWorkflowRun(t *testing.T) {
	gh := &ghWorkflow{
		Jobs: map[string]ghJob{
			"test": {
				RunsOn: "bash",
				Steps:  []ghStep{{Name: "unit", Run: "make test"}},
			},
		},
	}

	workflowRun := convertWorkflowRun(gh, "team-a")
	if workflowRun.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want team-a", workflowRun.Namespace)
	}
	if workflowRun.GenerateName != "wfr-" {
		t.Fatalf("generateName = %q, want wfr-", workflowRun.GenerateName)
	}
	job := workflowRun.Spec.Jobs["test"]
	if job.RunsOn != "bash" || len(job.Steps) != 1 || job.Steps[0].Run != "make test" {
		t.Fatalf("job = %#v, want bash unit step", job)
	}
}

func TestParseWorkflowRunRejectsEmptyFile(t *testing.T) {
	_, err := parseWorkflowRun([]byte(`name: empty`), "team-a")
	if err == nil || !strings.Contains(err.Error(), "kind: WorkflowRun or GitHub Actions jobs") {
		t.Fatalf("parseWorkflowRun() error = %v, want jobs error", err)
	}
}

func TestParseWorkflowManifest(t *testing.T) {
	data := []byte(`
apiVersion: kruntimes.io/v1alpha1
kind: Workflow
metadata:
  name: build-and-test
spec:
  jobs:
    test:
      runs-on: bash
      steps:
        - name: unit
          run: make test
`)

	workflow, err := parseWorkflow(data, "team-a")
	if err != nil {
		t.Fatalf("parseWorkflow() error = %v", err)
	}
	if workflow.Name != "build-and-test" {
		t.Fatalf("name = %q, want build-and-test", workflow.Name)
	}
	if workflow.Namespace != "team-a" {
		t.Fatalf("namespace = %q, want team-a", workflow.Namespace)
	}
	if workflow.Spec.Jobs["test"].RunsOn != "bash" {
		t.Fatalf("workflow spec = %#v, want test job", workflow.Spec)
	}
}

func TestParseWorkflowInputs(t *testing.T) {
	inputs, err := parseWorkflowInputs([]string{"ref=main", "image=agent:v0.1.0"})
	if err != nil {
		t.Fatalf("parseWorkflowInputs() error = %v", err)
	}
	if inputs["ref"] != "main" || inputs["image"] != "agent:v0.1.0" {
		t.Fatalf("inputs = %#v, want ref and image", inputs)
	}

	_, err = parseWorkflowInputs([]string{"bad"})
	if err == nil || !strings.Contains(err.Error(), "key=value") {
		t.Fatalf("parseWorkflowInputs() error = %v, want key=value error", err)
	}
}

func TestWorkflowCommandShape(t *testing.T) {
	root := NewRootCmd()
	for _, args := range [][]string{
		{"wf", "create"},
		{"wf", "ls"},
		{"wf", "trigger"},
		{"wf", "delete"},
		{"wf", "run"},
		{"wf", "run", "ls"},
		{"wf", "run", "cancel"},
		{"wf", "run", "delete"},
	} {
		if cmd, _, err := root.Find(args); err != nil || cmd == nil {
			t.Fatalf("Find(%v) = %v, %v; want command", args, cmd, err)
		}
	}
	if cmd, _, err := root.Find([]string{"workflowrun"}); err == nil && cmd != root {
		t.Fatalf("top-level workflowrun command still exists: %v", cmd.CommandPath())
	}
}

func TestWorkflowRunTerminalPhasesRemainShared(t *testing.T) {
	if v1alpha1.WorkflowPending == "" {
		t.Fatalf("workflow phase constants should remain available for WorkflowRun status")
	}
}
