package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestRunSpecEffectiveTaskModeFields(t *testing.T) {
	spec := RunSpec{
		Mode: RunMode{
			Task: &RunTaskMode{
				Entrypoint: "mode.sh",
				Args:       []string{"mode"},
			},
		},
	}

	if got := spec.EffectiveEntrypoint(); got != "mode.sh" {
		t.Fatalf("EffectiveEntrypoint() = %q, want mode.sh", got)
	}
	if got := spec.EffectiveArgs(); len(got) != 1 || got[0] != "mode" {
		t.Fatalf("EffectiveArgs() = %v, want [mode]", got)
	}
	if got := spec.EffectiveHandler(); got != "" {
		t.Fatalf("EffectiveHandler() = %q, want empty handler", got)
	}
}

func TestRunSpecEffectiveFunctionModeFields(t *testing.T) {
	spec := RunSpec{
		Mode: RunMode{
			Function: &RunFunctionMode{
				Handler: "main.invoke",
			},
		},
	}

	if got := spec.EffectiveEntrypoint(); got != "" {
		t.Fatalf("EffectiveEntrypoint() = %q, want empty entrypoint", got)
	}
	if got := spec.EffectiveArgs(); len(got) != 0 {
		t.Fatalf("EffectiveArgs() = %v, want empty args", got)
	}
	if got := spec.EffectiveHandler(); got != "main.invoke" {
		t.Fatalf("EffectiveHandler() = %q, want main.invoke", got)
	}
}

func TestRunSpecResourceRequests(t *testing.T) {
	runs := corev1.ResourceName(RuntimeResourceRuns)
	gpu := corev1.ResourceName("example.com/gpu")

	defaults, err := RunSpec{}.ResourceRequests()
	if err != nil {
		t.Fatalf("default resource requests: %v", err)
	}
	defaultRuns := defaults[runs]
	if got := defaultRuns.Value(); got != 1 {
		t.Fatalf("default runs request = %d, want 1", got)
	}

	spec := RunSpec{Resources: &RunResourceRequirements{Requests: corev1.ResourceList{
		runs: resource.MustParse("3"),
		gpu:  resource.MustParse("1"),
	}}}
	requests, err := spec.ResourceRequests()
	if err != nil {
		t.Fatalf("resource requests: %v", err)
	}
	requestedRuns := requests[runs]
	if got := requestedRuns.Value(); got != 3 {
		t.Fatalf("runs request = %d, want 3", got)
	}
	requestedGPU := requests[gpu]
	if got := requestedGPU.Value(); got != 1 {
		t.Fatalf("gpu request = %d, want 1", got)
	}

	invalid := RunSpec{Resources: &RunResourceRequirements{Requests: corev1.ResourceList{
		gpu: resource.MustParse("500m"),
	}}}
	if _, err := invalid.ResourceRequests(); err == nil {
		t.Fatal("expected fractional resource request to be rejected")
	}
}
