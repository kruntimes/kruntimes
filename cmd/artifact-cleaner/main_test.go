package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestCleanFilesystemRun(t *testing.T) {
	root := t.TempDir()
	runPath := filepath.Join(root, "namespaces", "workloads", "runs", "run-uid")
	if err := os.MkdirAll(runPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runPath, "artifact"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := clean(t.Context(), cleanupConfig{
		namespace: "workloads", runUID: "run-uid",
		driver:         v1alpha1.ArtifactDriverFilesystem,
		filesystemRoot: root, volumeClaim: "artifacts-pvc",
	})
	if err != nil {
		t.Fatalf("clean: %v", err)
	}
	if _, err := os.Stat(runPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Run artifact path still exists: %v", err)
	}
	if err := clean(t.Context(), cleanupConfig{
		namespace: "workloads", runUID: "run-uid",
		driver:         v1alpha1.ArtifactDriverFilesystem,
		filesystemRoot: root, volumeClaim: "artifacts-pvc",
	}); err != nil {
		t.Fatalf("idempotent clean: %v", err)
	}
}

func TestCleanRequiresRunIdentity(t *testing.T) {
	if err := clean(t.Context(), cleanupConfig{driver: v1alpha1.ArtifactDriverFilesystem}); err == nil {
		t.Fatal("clean accepted an empty Run identity")
	}
}
