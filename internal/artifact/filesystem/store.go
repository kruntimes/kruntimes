package filesystem

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

const directoryContentType = artifact.DirectoryArchiveContentType

// Store persists artifacts below a PVC-backed filesystem root.
type Store struct {
	root             string
	volumeClaimName  string
	maxArtifactBytes int64
}

// New returns a filesystem artifact store rooted at root.
func New(root, volumeClaimName string) (*Store, error) {
	return NewWithLimit(root, volumeClaimName, artifact.DefaultMaxArtifactBytes)
}

// NewWithLimit returns a filesystem artifact store with a hard per-artifact limit.
func NewWithLimit(root, volumeClaimName string, maxArtifactBytes int64) (*Store, error) {
	if root == "" {
		return nil, fmt.Errorf("artifact store root is required")
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("artifact store root must be absolute")
	}
	if volumeClaimName == "" {
		return nil, fmt.Errorf("artifact volume claim name is required")
	}
	if maxArtifactBytes <= 0 {
		return nil, fmt.Errorf("max artifact bytes must be positive")
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("create artifact store root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact store root: %w", err)
	}
	return &Store{
		root:             filepath.Clean(resolved),
		volumeClaimName:  volumeClaimName,
		maxArtifactBytes: maxArtifactBytes,
	}, nil
}

// Put atomically copies a file or directory into its deterministic Run location.
func (s *Store) Put(ctx context.Context, run *v1alpha1.Run, localPath string, opts artifact.PutOptions) (v1alpha1.ArtifactRef, error) {
	if run == nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("run is required")
	}
	if err := artifact.ValidateName(opts.Name); err != nil {
		return v1alpha1.ArtifactRef{}, invalidArtifactError{err: err}
	}
	if err := validatePathComponent(run.Namespace, "run namespace"); err != nil {
		return v1alpha1.ArtifactRef{}, invalidArtifactError{err: err}
	}
	if err := validatePathComponent(string(run.UID), "run UID"); err != nil {
		return v1alpha1.ArtifactRef{}, invalidArtifactError{err: err}
	}

	info, err := os.Lstat(localPath)
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("stat artifact %q: %w", opts.Name, err)
	}
	actualType, err := artifactType(info)
	if err != nil {
		return v1alpha1.ArtifactRef{}, invalidArtifactError{err: fmt.Errorf("artifact %q: %w", opts.Name, err)}
	}
	if opts.Type != "" && opts.Type != actualType {
		return v1alpha1.ArtifactRef{}, invalidArtifactError{err: fmt.Errorf("artifact %q type %q does not match source type %q", opts.Name, opts.Type, actualType)}
	}

	relativePath := filepath.Join("namespaces", run.Namespace, "runs", string(run.UID), opts.Name)
	destination, err := s.resolve(relativePath)
	if err != nil {
		return v1alpha1.ArtifactRef{}, err
	}
	parent := filepath.Dir(destination)
	if err := s.mkdirAllNoSymlink(parent, 0o750); err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("create artifact parent: %w", err)
	}

	tempDir, err := os.MkdirTemp(parent, ".artifact-*")
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("create artifact temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	tempPath := filepath.Join(tempDir, "content")

	digest := sha256.New()
	budget := &copyBudget{max: s.maxArtifactBytes}
	var size int64
	contentType := opts.ContentType
	switch actualType {
	case v1alpha1.ArtifactTypeFile:
		size, contentType, err = copyRegularFile(ctx, localPath, tempPath, info.Mode().Perm(), digest, contentType, budget)
	case v1alpha1.ArtifactTypeDirectory:
		archive, archiveErr := artifact.ArchiveDirectory(ctx, localPath)
		if archiveErr != nil {
			err = archiveErr
			break
		}
		defer archive.Close()
		contentType = directoryContentType
		size, _, err = copyRegularFile(ctx, archive.Name(), tempPath, 0o600, digest, contentType, budget)
	default:
		err = fmt.Errorf("unsupported filesystem artifact type %q", actualType)
	}
	if err != nil {
		if errors.Is(err, errArtifactTooLarge) {
			return v1alpha1.ArtifactRef{}, invalidArtifactError{err: fmt.Errorf("artifact %q exceeds %d bytes", opts.Name, s.maxArtifactBytes)}
		}
		var invalid invalidArtifactError
		if errors.As(err, &invalid) {
			return v1alpha1.ArtifactRef{}, invalid
		}
		return v1alpha1.ArtifactRef{}, fmt.Errorf("copy artifact %q: %w", opts.Name, err)
	}

	// The deterministic destination makes a repeated Put replace the previous
	// complete value instead of creating duplicate artifact objects.
	if err := os.RemoveAll(destination); err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("replace artifact %q: %w", opts.Name, err)
	}
	if err := os.Rename(tempPath, destination); err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("publish artifact %q: %w", opts.Name, err)
	}

	ref := v1alpha1.ArtifactRef{
		Name:        opts.Name,
		Driver:      v1alpha1.ArtifactDriverFilesystem,
		Type:        actualType,
		SizeBytes:   size,
		Digest:      "sha256:" + hex.EncodeToString(digest.Sum(nil)),
		ContentType: contentType,
		CreatedAt:   metav1.Now(),
		Location: v1alpha1.ArtifactLocation{
			Filesystem: &v1alpha1.FilesystemArtifactLocation{
				Path:            filepath.ToSlash(relativePath),
				VolumeClaimName: s.volumeClaimName,
			},
		},
	}
	if err := artifact.ValidateRef(ref); err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("validate stored artifact reference: %w", err)
	}
	return ref, nil
}

// Open opens a stored regular-file artifact without following a final symlink.
func (s *Store) Open(_ context.Context, ref v1alpha1.ArtifactRef) (io.ReadCloser, error) {
	path, err := s.pathForRef(ref)
	if err != nil {
		return nil, err
	}
	if err := s.ensureNoSymlinkPath(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open artifact %q: %w", ref.Name, err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat artifact %q: %w", ref.Name, err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("artifact %q is not a regular file", ref.Name)
	}
	return file, nil
}

// Delete removes an artifact from the store.
func (s *Store) Delete(_ context.Context, ref v1alpha1.ArtifactRef) error {
	path, err := s.pathForRef(ref)
	if err != nil {
		return err
	}
	if err := s.ensureNoSymlinkPath(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("delete artifact %q: %w", ref.Name, err)
	}
	return nil
}

// DeleteRun removes all artifacts stored for a Run.
func (s *Store) DeleteRun(_ context.Context, run *v1alpha1.Run) error {
	if run == nil {
		return fmt.Errorf("run is required")
	}
	if err := validatePathComponent(run.Namespace, "run namespace"); err != nil {
		return err
	}
	if err := validatePathComponent(string(run.UID), "run UID"); err != nil {
		return err
	}
	path, err := s.resolve(filepath.Join("namespaces", run.Namespace, "runs", string(run.UID)))
	if err != nil {
		return err
	}
	if err := s.ensureNoSymlinkPath(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("delete Run artifacts: %w", err)
	}
	return nil
}

func (s *Store) pathForRef(ref v1alpha1.ArtifactRef) (string, error) {
	if err := artifact.ValidateRef(ref); err != nil {
		return "", err
	}
	if ref.Driver != v1alpha1.ArtifactDriverFilesystem || ref.Location.Filesystem == nil {
		return "", fmt.Errorf("artifact %q is not a filesystem artifact", ref.Name)
	}
	if claim := ref.Location.Filesystem.VolumeClaimName; claim != "" && claim != s.volumeClaimName {
		return "", fmt.Errorf("artifact %q belongs to volume claim %q", ref.Name, claim)
	}
	return s.resolve(filepath.FromSlash(ref.Location.Filesystem.Path))
}

func (s *Store) resolve(relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", fmt.Errorf("artifact path must be relative")
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact path escapes store root")
	}
	path := filepath.Join(s.root, clean)
	rel, err := filepath.Rel(s.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact path escapes store root")
	}
	return path, nil
}

func (s *Store) ensureNoSymlinkPath(path string) error {
	relative, err := filepath.Rel(s.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("artifact path escapes store root")
	}
	current := s.root
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect artifact store path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact store path contains symlink %q", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("artifact store path component %q is not a directory", current)
		}
	}
	return nil
}

func (s *Store) mkdirAllNoSymlink(path string, mode os.FileMode) error {
	relative, err := filepath.Rel(s.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("artifact path escapes store root")
	}
	current := s.root
	if relative == "." {
		return nil
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, mode); err != nil && !os.IsExist(err) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact store path contains symlink %q", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("artifact store path component %q is not a directory", current)
		}
	}
	return nil
}

func validatePathComponent(value, field string) error {
	if value == "" || value == "." || value == ".." || filepath.Base(value) != value || strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%s %q is not a safe path component", field, value)
	}
	return nil
}

func artifactType(info os.FileInfo) (v1alpha1.ArtifactType, error) {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return "", fmt.Errorf("symbolic links are not allowed")
	case info.Mode().IsRegular():
		return v1alpha1.ArtifactTypeFile, nil
	case info.IsDir():
		return v1alpha1.ArtifactTypeDirectory, nil
	default:
		return "", fmt.Errorf("special files are not allowed")
	}
}

func copyRegularFile(ctx context.Context, source, destination string, mode os.FileMode, digest hash.Hash, contentType string, budget *copyBudget) (int64, string, error) {
	fd, err := syscall.Open(source, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return 0, "", err
	}
	input := os.NewFile(uintptr(fd), source)
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return 0, "", err
	}
	if !info.Mode().IsRegular() {
		return 0, "", invalidArtifactError{err: fmt.Errorf("source is not a regular file")}
	}

	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return 0, "", err
	}
	defer output.Close()

	var header [512]byte
	headerSize, readErr := io.ReadFull(input, header[:])
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return 0, "", readErr
	}
	if contentType == "" {
		contentType = detectContentType(source, header[:headerSize])
	}
	writer := io.MultiWriter(output, digest)
	limited := &budgetWriter{writer: writer, budget: budget}
	written, err := limited.Write(header[:headerSize])
	if err != nil {
		return 0, "", err
	}
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	copied, err := io.Copy(limited, &contextReader{ctx: ctx, reader: input})
	if err != nil {
		return 0, "", err
	}
	return int64(written) + copied, contentType, nil
}

var errArtifactTooLarge = errors.New("artifact exceeds size limit")

type copyBudget struct {
	max     int64
	written int64
}

type budgetWriter struct {
	writer io.Writer
	budget *copyBudget
}

func (w *budgetWriter) Write(p []byte) (int, error) {
	remaining := w.budget.max - w.budget.written
	if remaining <= 0 {
		return 0, errArtifactTooLarge
	}
	exceeded := int64(len(p)) > remaining
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := w.writer.Write(p)
	w.budget.written += int64(n)
	if err == nil && int64(n) < int64(len(p)) {
		err = io.ErrShortWrite
	}
	if err == nil && exceeded {
		err = errArtifactTooLarge
	}
	return n, err
}

type invalidArtifactError struct {
	err error
}

func (e invalidArtifactError) Error() string         { return e.err.Error() }
func (e invalidArtifactError) Unwrap() error         { return e.err }
func (e invalidArtifactError) ArtifactInvalid() bool { return true }

func detectContentType(path string, header []byte) string {
	if byExtension := mime.TypeByExtension(filepath.Ext(path)); byExtension != "" {
		return byExtension
	}
	if len(header) == 0 {
		return "application/octet-stream"
	}
	return http.DetectContentType(header)
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

var _ artifact.Store = (*Store)(nil)
var _ artifact.RunStore = (*Store)(nil)
