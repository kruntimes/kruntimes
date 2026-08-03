package controller

import (
	"strings"
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestResolveExprSupportsInputsStepsAndJobs(t *testing.T) {
	ctx := &resolveContext{
		inputs: map[string]string{"version": "3.13"},
		steps:  map[string]map[string]string{"setup": {"path": "/tools/python"}},
		jobs:   map[string]map[string]string{"build": {"artifact": "dist.tgz"}},
	}
	got, err := resolveExpr("${{ inputs.version }} ${{ steps.setup.outputs.path }} ${{ jobs.build.outputs.artifact }}", ctx)
	if err != nil {
		t.Fatalf("resolve expression: %v", err)
	}
	if want := "3.13 /tools/python dist.tgz"; got != want {
		t.Fatalf("resolved expression = %q, want %q", got, want)
	}
}

func TestResolveExprRejectsUnavailableContextValue(t *testing.T) {
	_, err := resolveExpr("${{ steps.setup.outputs.path }}", &resolveContext{})
	if err == nil || !strings.Contains(err.Error(), "no outputs available") {
		t.Fatalf("resolve expression error = %v, want unavailable output", err)
	}
}

func TestResolveStepExecutionResolvesRunArgsAndEnv(t *testing.T) {
	step, err := resolveStepExecution(v1alpha1.StepSpec{
		Name: "publish",
		Run:  "publish ${{ jobs.build.outputs.artifact }}",
		Args: []string{"--version", "${{ inputs.version }}"},
		Env:  map[string]string{"PATH": "${{ steps.setup.outputs.path }}"},
	}, &resolveContext{
		inputs: map[string]string{"version": "3.13"},
		steps:  map[string]map[string]string{"setup": {"path": "/tools/python"}},
		jobs:   map[string]map[string]string{"build": {"artifact": "dist.tgz"}},
	})
	if err != nil {
		t.Fatalf("resolve step: %v", err)
	}
	if step.Run != "publish dist.tgz" || step.Args[1] != "3.13" || step.Env["PATH"] != "/tools/python" {
		t.Fatalf("resolved step = %#v", step)
	}
}
