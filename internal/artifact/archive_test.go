package artifact

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArchiveDirectoryIsDeterministic(t *testing.T) {
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "result.txt"), []byte("result"), 0o640); err != nil {
		t.Fatal(err)
	}

	first := readArchive(t, source)
	second := readArchive(t, source)
	if !bytes.Equal(first, second) {
		t.Fatal("directory archive is not deterministic")
	}

	gzipReader, err := gzip.NewReader(bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	tarReader := tar.NewReader(gzipReader)
	found := false
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "nested/result.txt" {
			content, err := io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
			found = string(content) == "result"
		}
	}
	if !found {
		t.Fatal("archive missing nested/result.txt")
	}
}

func readArchive(t *testing.T, source string) []byte {
	t.Helper()
	archive, err := ArchiveDirectory(t.Context(), source)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	content, err := io.ReadAll(archive)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
