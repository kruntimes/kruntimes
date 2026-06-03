package runtimed

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
)

func TestPrepareSource_NoSource(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	run := &v1alpha1.Run{}
	run.UID = "test-uid"
	workDir, err := prepareSource(run)
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(dir, string(run.UID))
	if workDir != expected {
		t.Errorf("expected %s, got %s", expected, workDir)
	}
	if _, err := os.Stat(workDir); err != nil {
		t.Errorf("workDir not created: %v", err)
	}
}

func TestPrepareSource_Inline(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "#!/bin/bash\necho hello"
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Entrypoint: "run.sh",
			Source:     &v1alpha1.CodeSource{Inline: &inline},
		},
	}
	run.UID = "test-uid"

	workDir, err := prepareSource(run)
	if err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(workDir, "run.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if string(data) != inline {
		t.Errorf("expected %q, got %q", inline, string(data))
	}
}

func TestPrepareSource_InlineDefaultEntrypoint(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "echo default"
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Source: &v1alpha1.CodeSource{Inline: &inline},
		},
	}
	run.UID = "test-uid"

	workDir, err := prepareSource(run)
	if err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(workDir, "script")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Errorf("expected default 'script' file: %v", err)
	}
}

func TestReadOutputs_Empty(t *testing.T) {
	outputs := readOutputs("")
	if outputs != nil {
		t.Error("expected nil for empty workingDir")
	}
}

func TestReadOutputs_Nonexistent(t *testing.T) {
	outputs := readOutputs("/nonexistent/path")
	if outputs != nil {
		t.Error("expected nil for nonexistent file")
	}
}

func TestReadOutputs_Valid(t *testing.T) {
	dir := t.TempDir()
	content := "key1=val1\nkey2=val2\n# comment\nkey3 = val3\n"
	_ = os.WriteFile(filepath.Join(dir, "outputs"), []byte(content), 0o644)

	outputs := readOutputs(dir)
	if len(outputs) != 3 {
		t.Fatalf("expected 3 outputs, got %d: %v", len(outputs), outputs)
	}
	if outputs["key1"] != "val1" {
		t.Errorf("key1: expected val1, got %s", outputs["key1"])
	}
	if outputs["key2"] != "val2" {
		t.Errorf("key2: expected val2, got %s", outputs["key2"])
	}
	if outputs["key3"] != "val3" {
		t.Errorf("key3: expected val3, got %s", outputs["key3"])
	}
}

func TestReadOutputs_SkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	content := "no_equal_sign\n=empty_key\nb=\n  \n"
	_ = os.WriteFile(filepath.Join(dir, "outputs"), []byte(content), 0o644)

	outputs := readOutputs(dir)
	// "no_equal_sign" has no "=", skipped. "=empty_key" yields key="" value="empty_key".
	// "b=" yields key="b" value="". Whitespace-only lines are skipped.
	if _, ok := outputs["b"]; !ok {
		t.Errorf("expected key 'b', got %v", outputs)
	}
}

func TestStatusAdapter(t *testing.T) {
	var _ = (*statusAdapter)(nil)
}

func TestTerminalPhaseForFailure(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		expected v1alpha1.RunPhase
	}{
		{"timeout", runretry.ReasonTimeout, v1alpha1.RunTimeout},
		{"runtime_error", runretry.ReasonRuntimeError, v1alpha1.RunFailed},
		{"prepare_source", runretry.ReasonPrepareSource, v1alpha1.RunFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := terminalPhaseForFailure(tt.reason); got != tt.expected {
				t.Fatalf("terminalPhaseForFailure(%s) = %s, want %s", tt.reason, got, tt.expected)
			}
		})
	}
}

func TestHandleFailure_NoRetry(t *testing.T) {
	// When maxAttempts=1 (default), handleFailure should call finishRun directly.
	// This test verifies the logic through shouldRetry.
	p := runretry.WithDefaults(nil) // maxAttempts=1
	// First execution (attempt=0 → curAttempt=1)
	if runretry.ShouldRetry(p, 1, runretry.ReasonRuntimeError) {
		t.Error("should not retry when maxAttempts=1")
	}
}

func TestHandleFailure_RetryAndBackoff(t *testing.T) {
	p := runretry.WithDefaults(&v1alpha1.RetryPolicy{
		MaxAttempts: 3,
		Backoff:     metav1.Duration{Duration: time.Second},
	})

	// First execution fails (attempt 1)
	if !runretry.ShouldRetry(p, 1, runretry.ReasonRuntimeError) {
		t.Error("should retry on attempt 1 of 3")
	}
	// Second execution fails (attempt 2)
	if !runretry.ShouldRetry(p, 2, runretry.ReasonRuntimeError) {
		t.Error("should retry on attempt 2 of 3")
	}
	// Third execution fails (attempt 3) — maxAttempts reached
	if runretry.ShouldRetry(p, 3, runretry.ReasonRuntimeError) {
		t.Error("should not retry on attempt 3 of 3")
	}

	// Verify backoff for each attempt
	if d := runretry.Backoff(p, 2); d != time.Second {
		t.Errorf("attempt 2 backoff: expected 1s, got %v", d)
	}
	if d := runretry.Backoff(p, 3); d != 2*time.Second {
		t.Errorf("attempt 3 backoff: expected 2s, got %v", d)
	}
}
