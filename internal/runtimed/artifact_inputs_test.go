package runtimed

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

func TestStageArtifactInputFile(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "inputs", "report.txt")
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := stageArtifactInput(t.Context(), bytes.NewBufferString("report"), v1alpha1.ArtifactRef{Type: v1alpha1.ArtifactTypeFile}, destination, 1024); err != nil {
		t.Fatalf("stageArtifactInput: %v", err)
	}
	content, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != "report" {
		t.Fatalf("content = %q, want report", got)
	}
}

func TestStageArtifactInputDirectoryRejectsTraversal(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "inputs")
	if err := stageArtifactInput(t.Context(), directoryArchive(t, "../escape", "bad", 0o600), v1alpha1.ArtifactRef{Type: v1alpha1.ArtifactTypeDirectory}, destination, 1024); err == nil {
		t.Fatal("stageArtifactInput accepted a traversal entry")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("destination should be removed, stat error = %v", err)
	}
}

func TestStageArtifactInputDirectoryExtractsFiles(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "inputs")
	if err := stageArtifactInput(t.Context(), directoryArchive(t, "nested/report.txt", "report", 0o600), v1alpha1.ArtifactRef{Type: v1alpha1.ArtifactTypeDirectory}, destination, 1024); err != nil {
		t.Fatalf("stageArtifactInput: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(destination, "nested", "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != "report" {
		t.Fatalf("content = %q, want report", got)
	}
	info, err := os.Stat(filepath.Join(destination, "nested", "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o640); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
}

func TestStageArtifactInputDirectoryPreservesOwnerExecuteBit(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "inputs")
	if err := stageArtifactInput(t.Context(), directoryArchive(t, "hack/verify.sh", "#!/bin/sh\necho verified\n", 0o755), v1alpha1.ArtifactRef{Type: v1alpha1.ArtifactTypeDirectory}, destination, 1024); err != nil {
		t.Fatalf("stageArtifactInput: %v", err)
	}
	info, err := os.Stat(filepath.Join(destination, "hack", "verify.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o740); got != want {
		t.Fatalf("mode = %o, want %o", got, want)
	}
}

func TestArtifactInputDestinationRejectsExistingSymlinkParent(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := artifactInputDestination(root, "link/report.txt"); err == nil {
		t.Fatal("artifactInputDestination accepted a symlink parent")
	}
}

func TestStageArtifactInputsUsesRunWorkingDirectory(t *testing.T) {
	workDir := t.TempDir()
	ref := testArtifactRef("report.txt", int64(len("report")))
	run := artifactTestRun()
	run.Spec.ArtifactInputs = []v1alpha1.ArtifactInput{{Ref: ref, Path: "inputs/report.txt"}}
	controller := &Controller{ArtifactStore: &artifactInputStore{content: []byte("report")}}
	if err := controller.stageArtifactInputs(t.Context(), &activeRun{run: run, workDir: workDir}); err != nil {
		t.Fatalf("stageArtifactInputs: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(workDir, "inputs", "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(content); got != "report" {
		t.Fatalf("content = %q, want report", got)
	}
}

func directoryArchive(t *testing.T, name, content string, mode int64) *bytes.Reader {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buffer.Bytes())
}

type artifactInputStore struct{ content []byte }

func (s *artifactInputStore) Put(context.Context, *v1alpha1.Run, string, artifact.PutOptions) (v1alpha1.ArtifactRef, error) {
	return v1alpha1.ArtifactRef{}, nil
}

func (s *artifactInputStore) Open(context.Context, v1alpha1.ArtifactRef) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.content)), nil
}

func (s *artifactInputStore) Delete(context.Context, v1alpha1.ArtifactRef) error { return nil }
