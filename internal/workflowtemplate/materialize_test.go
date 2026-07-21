package workflowtemplate

import (
	"strings"
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestMaterializeBindsAndRendersInputs(t *testing.T) {
	jobs, err := Materialize(v1alpha1.WorkflowSpec{
		Inputs: map[string]v1alpha1.WorkflowInputSpec{
			"ref":    {Required: true},
			"target": {Default: "linux-amd64"},
		},
		Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn:  "bash",
				Outputs: map[string]string{"target": "${{ inputs.target }}"},
				Steps: []v1alpha1.StepSpec{{
					Name: "compile",
					Run:  "make build REF=${{ inputs.ref }} TARGET=${{ inputs.target }}",
					Args: []string{"--ref=${{ inputs.ref }}"},
					Env:  map[string]string{"TARGET": "${{ inputs.target }}"},
				}},
			},
		},
	}, map[string]string{"ref": "main"})
	if err != nil {
		t.Fatalf("Materialize() error = %v", err)
	}
	job := jobs["build"]
	if job.Steps[0].Run != "make build REF=main TARGET=linux-amd64" || job.Steps[0].Args[0] != "--ref=main" || job.Steps[0].Env["TARGET"] != "linux-amd64" || job.Outputs["target"] != "linux-amd64" {
		t.Fatalf("materialized job = %#v", job)
	}
}

func TestMaterializeRejectsInvalidInputs(t *testing.T) {
	spec := v1alpha1.WorkflowSpec{
		Inputs: map[string]v1alpha1.WorkflowInputSpec{"ref": {Required: true}},
		Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
		},
	}
	for name, values := range map[string]map[string]string{
		"missing": nil,
		"unknown": {"ref": "main", "branch": "next"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Materialize(spec, values)
			if err == nil {
				t.Fatal("Materialize() error = nil")
			}
			if name == "missing" && !strings.Contains(err.Error(), `missing required input "ref"`) {
				t.Fatalf("error = %v", err)
			}
			if name == "unknown" && !strings.Contains(err.Error(), `unknown input "branch"`) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
