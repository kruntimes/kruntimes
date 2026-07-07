package v1alpha1

import "testing"

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
