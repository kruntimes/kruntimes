package filesystem

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

func TestStorePutOpenDeleteFile(t *testing.T) {
	store := newTestStore(t)
	source := filepath.Join(t.TempDir(), "report.txt")
	content := []byte("artifact content")
	if err := os.WriteFile(source, content, 0o640); err != nil {
		t.Fatal(err)
	}

	ref, err := store.Put(t.Context(), testRun(), source, artifact.PutOptions{
		Name: "report.txt",
		Type: v1alpha1.ArtifactTypeFile,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref.SizeBytes != int64(len(content)) {
		t.Fatalf("size = %d, want %d", ref.SizeBytes, len(content))
	}
	wantDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(content))
	if ref.Digest != wantDigest {
		t.Fatalf("digest = %q, want %q", ref.Digest, wantDigest)
	}
	if !strings.HasPrefix(ref.ContentType, "text/plain") {
		t.Fatalf("content type = %q, want text/plain", ref.ContentType)
	}
	if ref.Location.Filesystem == nil || ref.Location.Filesystem.VolumeClaimName != "artifacts-pvc" {
		t.Fatalf("filesystem location = %#v", ref.Location.Filesystem)
	}

	reader, err := store.Open(t.Context(), ref)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("content = %q, want %q", got, content)
	}
	if err := store.Delete(t.Context(), ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := store.Open(t.Context(), ref); err == nil {
		t.Fatal("Open succeeded after Delete")
	}
}

func TestStorePutDirectoryRecursively(t *testing.T) {
	store := newTestStore(t)
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("aaa"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "b.bin"), []byte{1, 2, 3, 4}, 0o600); err != nil {
		t.Fatal(err)
	}

	ref, err := store.Put(t.Context(), testRun(), source, artifact.PutOptions{
		Name: "bundle",
		Type: v1alpha1.ArtifactTypeDirectory,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref.SizeBytes <= 0 {
		t.Fatalf("size = %d, want positive archive size", ref.SizeBytes)
	}
	if ref.ContentType != directoryContentType {
		t.Fatalf("content type = %q", ref.ContentType)
	}
	if !strings.HasPrefix(ref.Digest, "sha256:") {
		t.Fatalf("digest = %q", ref.Digest)
	}
	stored := filepath.Join(store.root, filepath.FromSlash(ref.Location.Filesystem.Path))
	data, err := os.ReadFile(stored)
	if err != nil {
		t.Fatalf("read stored archive: %v", err)
	}
	if int64(len(data)) != ref.SizeBytes {
		t.Fatalf("stored archive size = %d, ref size = %d", len(data), ref.SizeBytes)
	}

	reader, err := store.Open(t.Context(), ref)
	if err != nil {
		t.Fatalf("Open directory: %v", err)
	}
	defer reader.Close()
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		t.Fatalf("open directory gzip: %v", err)
	}
	tarReader := tar.NewReader(gzipReader)
	found := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read directory tar: %v", err)
		}
		if header.Name == "nested/b.bin" {
			found = true
		}
	}
	if !found {
		t.Fatal("directory stream missing nested/b.bin")
	}
}

func TestStorePutIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	run := testRun()
	source := filepath.Join(t.TempDir(), "result")
	if err := os.WriteFile(source, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := store.Put(t.Context(), run, source, artifact.PutOptions{Name: "result", Type: v1alpha1.ArtifactTypeFile})
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	if err := os.WriteFile(source, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(t.Context(), run, source, artifact.PutOptions{Name: "result", Type: v1alpha1.ArtifactTypeFile})
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if first.Location.Filesystem.Path != second.Location.Filesystem.Path {
		t.Fatalf("paths differ: %q and %q", first.Location.Filesystem.Path, second.Location.Filesystem.Path)
	}
	reader, err := store.Open(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(reader)
	reader.Close()
	if string(data) != "second" {
		t.Fatalf("stored content = %q, want second", data)
	}
}

func TestStoreEnforcesArtifactLimitDuringCopy(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	store, err := NewWithLimit(root, "artifacts-pvc", 4)
	if err != nil {
		t.Fatalf("NewWithLimit: %v", err)
	}
	source := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(source, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = store.Put(t.Context(), testRun(), source, artifact.PutOptions{
		Name: "large",
		Type: v1alpha1.ArtifactTypeFile,
	})
	if err == nil {
		t.Fatal("Put accepted oversized artifact")
	}
	var invalid interface{ ArtifactInvalid() bool }
	if !errors.As(err, &invalid) || !invalid.ArtifactInvalid() {
		t.Fatalf("error = %v, want permanent artifact invalid error", err)
	}
	destination := filepath.Join(root, "namespaces", "default", "runs", "run-uid", "large")
	if _, statErr := os.Stat(destination); !os.IsNotExist(statErr) {
		t.Fatalf("oversized artifact was published, stat error = %v", statErr)
	}
}

func TestStoreRejectsSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	store := newTestStore(t)
	sourceDir := t.TempDir()
	target := filepath.Join(sourceDir, "target")
	if err := os.WriteFile(target, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sourceDir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), testRun(), link, artifact.PutOptions{Name: "link", Type: v1alpha1.ArtifactTypeFile}); err == nil {
		t.Fatal("Put accepted top-level symlink")
	}

	directory := filepath.Join(sourceDir, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(directory, "nested-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), testRun(), directory, artifact.PutOptions{Name: "directory", Type: v1alpha1.ArtifactTypeDirectory}); err == nil {
		t.Fatal("Put accepted nested symlink")
	}
}

func TestStoreRejectsSpecialFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO is not available on Windows")
	}
	store := newTestStore(t)
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), testRun(), fifo, artifact.PutOptions{Name: "pipe", Type: v1alpha1.ArtifactTypeFile}); err == nil {
		t.Fatal("Put accepted FIFO")
	}
}

func TestStoreRejectsEscapingAndForeignReferences(t *testing.T) {
	store := newTestStore(t)
	tests := []v1alpha1.ArtifactRef{
		{
			Name: "escape", Driver: v1alpha1.ArtifactDriverFilesystem, Type: v1alpha1.ArtifactTypeFile,
			Location: v1alpha1.ArtifactLocation{Filesystem: &v1alpha1.FilesystemArtifactLocation{Path: "../escape", VolumeClaimName: "artifacts-pvc"}},
		},
		{
			Name: "foreign", Driver: v1alpha1.ArtifactDriverFilesystem, Type: v1alpha1.ArtifactTypeFile,
			Location: v1alpha1.ArtifactLocation{Filesystem: &v1alpha1.FilesystemArtifactLocation{Path: "namespaces/default/runs/uid/foreign", VolumeClaimName: "other-pvc"}},
		},
	}
	for _, ref := range tests {
		if _, err := store.Open(t.Context(), ref); err == nil {
			t.Fatalf("Open accepted invalid ref %q", ref.Name)
		}
	}
}

func TestStoreDeleteRun(t *testing.T) {
	store := newTestStore(t)
	run := testRun()
	source := filepath.Join(t.TempDir(), "result")
	if err := os.WriteFile(source, []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.Put(t.Context(), run, source, artifact.PutOptions{Name: "result", Type: v1alpha1.ArtifactTypeFile})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteRun(t.Context(), run); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if _, err := store.Open(t.Context(), ref); err == nil {
		t.Fatal("artifact still exists after DeleteRun")
	}
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(filepath.Join(t.TempDir(), "store"), "artifacts-pvc")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return store
}

func testRun() *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run",
			Namespace: "default",
			UID:       "run-uid",
		},
	}
}
