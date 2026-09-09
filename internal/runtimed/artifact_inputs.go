package runtimed

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

// stageArtifactInputs materializes generic Run inputs without exposing any
// Workflow semantics to runtimed. Artifact paths are always relative to the
// prepared Run working directory and may not traverse existing symlinks.
func (c *Controller) stageArtifactInputs(ctx context.Context, ar *activeRun) error {
	if ar == nil || ar.run == nil {
		return fmt.Errorf("active Run is required")
	}
	inputs := ar.run.Spec.ArtifactInputs
	if len(inputs) == 0 {
		return nil
	}
	if c.ArtifactStore == nil {
		return fmt.Errorf("artifact inputs require a configured artifact store")
	}
	for _, input := range inputs {
		if err := artifact.ValidateRef(input.Ref); err != nil {
			return fmt.Errorf("invalid artifact input %q: %w", input.Path, err)
		}
		destination, err := artifactInputDestination(ar.workDir, input.Path)
		if err != nil {
			return fmt.Errorf("artifact input %q: %w", input.Path, err)
		}
		reader, err := c.ArtifactStore.Open(ctx, input.Ref)
		if err != nil {
			return fmt.Errorf("open artifact input %q: %w", input.Ref.Name, err)
		}
		stageErr := stageArtifactInput(ctx, reader, input.Ref, destination, c.maxArtifactBytes())
		closeErr := reader.Close()
		if stageErr != nil {
			return fmt.Errorf("stage artifact input %q: %w", input.Ref.Name, stageErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close artifact input %q: %w", input.Ref.Name, closeErr)
		}
	}
	return nil
}

func artifactInputDestination(root, relative string) (string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("destination path must be relative")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("destination path escapes working directory")
	}
	destination := filepath.Join(root, clean)
	if err := ensureArtifactInputParent(root, filepath.Dir(destination)); err != nil {
		return "", err
	}
	if _, err := os.Lstat(destination); err == nil {
		return "", fmt.Errorf("destination already exists")
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect destination: %w", err)
	}
	return destination, nil
}

func stageArtifactInput(ctx context.Context, reader io.Reader, ref v1alpha1.ArtifactRef, destination string, maxBytes int64) error {
	switch ref.Type {
	case v1alpha1.ArtifactTypeFile, v1alpha1.ArtifactTypeArchive, v1alpha1.ArtifactTypeBlob:
		return stageArtifactFile(ctx, reader, destination, maxBytes)
	case v1alpha1.ArtifactTypeDirectory:
		return stageArtifactDirectory(ctx, reader, destination, maxBytes)
	default:
		return fmt.Errorf("unsupported artifact type %q", ref.Type)
	}
}

func stageArtifactFile(ctx context.Context, reader io.Reader, destination string, maxBytes int64) error {
	file, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return fmt.Errorf("create destination: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if err := copyArtifactInput(ctx, file, reader, maxBytes); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close destination: %w", err)
	}
	ok = true
	return nil
}

func stageArtifactDirectory(ctx context.Context, reader io.Reader, destination string, maxBytes int64) error {
	if err := os.Mkdir(destination, 0o750); err != nil {
		return fmt.Errorf("create destination directory: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(destination)
		}
	}()
	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("open directory archive: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	var total int64
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read directory archive: %w", err)
		}
		target, err := archiveEntryDestination(destination, header.Name)
		if err != nil {
			return err
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := ensureArtifactInputParent(destination, filepath.Dir(target)); err != nil {
				return err
			}
			if err := os.Mkdir(target, 0o750); err != nil && !os.IsExist(err) {
				return fmt.Errorf("create archive directory: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || total > maxBytes-header.Size {
				return fmt.Errorf("extracted artifact exceeds %d bytes", maxBytes)
			}
			if err := ensureArtifactInputParent(destination, filepath.Dir(target)); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, artifactInputFileMode(header.FileInfo().Mode()))
			if err != nil {
				return fmt.Errorf("create archive file: %w", err)
			}
			copyErr := copyArtifactInput(ctx, file, tarReader, maxBytes-total)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return fmt.Errorf("close archive file: %w", closeErr)
			}
			total += header.Size
		default:
			return fmt.Errorf("archive entry %q has unsupported type", header.Name)
		}
	}
	ok = true
	return nil
}

// artifactInputFileMode deliberately restores only the owner execute bit from
// a directory artifact. Artifact producers may run under a different UID or
// group than consumers, so preserving arbitrary read, write, group, or other
// permission bits would make the resulting access policy depend on producer
// metadata. The stable 0640 baseline retains the existing confidentiality and
// mutability boundary while allowing checked-in scripts to be invoked.
func artifactInputFileMode(source os.FileMode) os.FileMode {
	mode := os.FileMode(0o640)
	if source&0o100 != 0 {
		mode |= 0o100
	}
	return mode
}

func archiveEntryDestination(root, name string) (string, error) {
	clean := path.Clean(name)
	if name == "" || strings.HasPrefix(name, "/") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry %q escapes destination", name)
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}

func ensureArtifactInputParent(root, directory string) error {
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("destination escapes working directory")
	}
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "." || component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0o750); err != nil && !os.IsExist(err) {
				return fmt.Errorf("create destination parent: %w", err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect destination parent: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("destination parent contains non-directory path component")
		}
	}
	return nil
}

func copyArtifactInput(ctx context.Context, destination io.Writer, source io.Reader, maxBytes int64) error {
	if maxBytes <= 0 {
		return fmt.Errorf("artifact input size limit must be positive")
	}
	written, err := io.Copy(destination, &contextLimitedReader{ctx: ctx, reader: source, remaining: maxBytes + 1})
	if err != nil {
		return fmt.Errorf("copy artifact input: %w", err)
	}
	if written > maxBytes {
		return fmt.Errorf("artifact input exceeds %d bytes", maxBytes)
	}
	return nil
}

type contextLimitedReader struct {
	ctx       context.Context
	reader    io.Reader
	remaining int64
}

func (r *contextLimitedReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	n, err := r.reader.Read(buffer)
	r.remaining -= int64(n)
	return n, err
}
