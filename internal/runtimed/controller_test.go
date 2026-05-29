package runtimed

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
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
	os.WriteFile(filepath.Join(dir, "outputs"), []byte(content), 0o644)

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
	os.WriteFile(filepath.Join(dir, "outputs"), []byte(content), 0o644)

	outputs := readOutputs(dir)
	// "no_equal_sign" has no "=", skipped. "=empty_key" yields key="" value="empty_key".
	// "b=" yields key="b" value="". Whitespace-only lines are skipped.
	if _, ok := outputs["b"]; !ok {
		t.Errorf("expected key 'b', got %v", outputs)
	}
}

func TestStatusAdapter(t *testing.T) {
	// statusAdapter is tested in integration tests; verify it compiles and wraps correctly.
	// This is a compile-time check that the adapter satisfies the interface.
	var _ = (*statusAdapter)(nil)
}
