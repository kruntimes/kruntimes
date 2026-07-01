package krt

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	cmd := NewRootCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"version"})

	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}

	got := output.String()
	for _, want := range []string{"krt dev", "commit: unknown", "built: unknown"} {
		if !strings.Contains(got, want) {
			t.Fatalf("version output = %q, want to contain %q", got, want)
		}
	}
}
