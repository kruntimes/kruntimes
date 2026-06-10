package artifact

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

const DirectoryArchiveContentType = "application/gzip"

// TemporaryArchive is a seekable tar.gz file removed when closed.
type TemporaryArchive struct {
	*os.File
	path string
}

func (a *TemporaryArchive) Close() error {
	closeErr := a.File.Close()
	removeErr := os.Remove(a.path)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}

// ArchiveDirectory creates a deterministic tar.gz stream for a directory.
func ArchiveDirectory(ctx context.Context, source string) (*TemporaryArchive, error) {
	rootInfo, err := os.Lstat(source)
	if err != nil {
		return nil, fmt.Errorf("stat directory: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("source is not a directory")
	}

	file, err := os.CreateTemp("", "kruntime-artifact-*.tar.gz")
	if err != nil {
		return nil, fmt.Errorf("create directory archive: %w", err)
	}
	archive := &TemporaryArchive{File: file, path: file.Name()}
	ok := false
	defer func() {
		if !ok {
			_ = archive.Close()
		}
	}()

	gzipWriter := gzip.NewWriter(file)
	gzipWriter.Header.ModTime = time.Unix(0, 0)
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)

	err = filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == source {
			return nil
		}

		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s: symbolic links are not allowed", path)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%s: special files are not allowed", path)
		}

		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if info.IsDir() {
			name += "/"
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = name
		header.ModTime = time.Unix(0, 0)
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Uid = 0
		header.Gid = 0
		header.Uname = ""
		header.Gname = ""
		header.PAXRecords = nil
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		input, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tarWriter, &contextReader{ctx: ctx, reader: input})
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return nil, fmt.Errorf("archive directory: %w", err)
	}
	if err := tarWriter.Close(); err != nil {
		return nil, fmt.Errorf("close tar archive: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, fmt.Errorf("close gzip archive: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind directory archive: %w", err)
	}
	ok = true
	return archive, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
