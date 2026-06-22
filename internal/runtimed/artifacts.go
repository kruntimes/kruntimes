package runtimed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

func (c *Controller) prepareArtifactStaging(run *v1alpha1.Run) (string, error) {
	if c.ArtifactStore == nil {
		return "", nil
	}
	path := artifactStagingDir(run)
	if err := os.RemoveAll(path); err != nil {
		return "", fmt.Errorf("clear artifact staging directory: %w", err)
	}
	if err := os.MkdirAll(path, 0o750); err != nil {
		return "", fmt.Errorf("create artifact staging directory: %w", err)
	}
	return path, nil
}

func (c *Controller) collectArtifacts(ctx context.Context, run *v1alpha1.Run) (refs []v1alpha1.ArtifactRef, resultErr error) {
	if c.ArtifactStore == nil {
		return nil, nil
	}
	stagingDir := artifactStagingDir(run)
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read artifact staging directory: %w", err)
	}
	if len(entries) > artifact.MaxArtifactRefs {
		return nil, artifactInvalidError{err: fmt.Errorf("artifact count %d exceeds limit %d", len(entries), artifact.MaxArtifactRefs)}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	if err := c.ensureArtifactFinalizer(ctx, run); err != nil {
		return nil, err
	}

	uploadStarted := false
	defer func() {
		if resultErr == nil || !uploadStarted {
			return
		}
		if err := c.deleteRunArtifacts(ctx, run); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("rollback partial artifacts: %w", err))
		}
	}()

	refs = make([]v1alpha1.ArtifactRef, 0, len(entries))
	var totalBytes int64
	for _, entry := range entries {
		if err := artifact.ValidateName(entry.Name()); err != nil {
			return refs, artifactInvalidError{err: fmt.Errorf("invalid artifact %q: %w", entry.Name(), err)}
		}
		localPath := filepath.Join(stagingDir, entry.Name())
		_, artifactType, err := inspectArtifact(localPath, c.maxArtifactBytes())
		if err != nil {
			return refs, artifactInvalidError{err: fmt.Errorf("artifact %q: %w", entry.Name(), err)}
		}
		maxStoredBytes := min(c.maxArtifactBytes(), c.maxArtifactsBytes()-totalBytes)
		if maxStoredBytes <= 0 {
			return refs, artifactInvalidError{err: fmt.Errorf("total stored artifact size exceeds %d bytes", c.maxArtifactsBytes())}
		}
		uploadStarted = true
		ref, err := c.ArtifactStore.Put(ctx, run, localPath, artifact.PutOptions{
			Name:         entry.Name(),
			Type:         artifactType,
			MaxSizeBytes: maxStoredBytes,
		})
		if err != nil {
			if errors.Is(err, artifact.ErrSizeLimitExceeded) {
				return refs, artifactInvalidError{err: fmt.Errorf("store artifact %q: %w", entry.Name(), err)}
			}
			return refs, fmt.Errorf("store artifact %q: %w", entry.Name(), err)
		}
		if ref.SizeBytes > c.maxArtifactBytes() {
			return refs, artifactInvalidError{err: fmt.Errorf("stored artifact %q exceeds %d bytes", entry.Name(), c.maxArtifactBytes())}
		}
		totalBytes += ref.SizeBytes
		if totalBytes > c.maxArtifactsBytes() {
			return refs, artifactInvalidError{err: fmt.Errorf("total stored artifact size exceeds %d bytes", c.maxArtifactsBytes())}
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func (c *Controller) ensureArtifactFinalizer(ctx context.Context, run *v1alpha1.Run) error {
	if controllerutil.ContainsFinalizer(run, artifact.RunArtifactFinalizer) {
		return nil
	}
	if c.Client == nil {
		return nil
	}
	controllerutil.AddFinalizer(run, artifact.RunArtifactFinalizer)
	if err := c.Update(ctx, run); err != nil {
		return fmt.Errorf("add artifact cleanup finalizer: %w", err)
	}
	return nil
}

func (c *Controller) reconcileArtifactDeletion(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(run, artifact.RunArtifactFinalizer) {
		return ctrl.Result{}, nil
	}
	if err := c.deleteRunArtifacts(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(run, artifact.RunArtifactFinalizer)
	if err := c.Update(ctx, run); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove artifact cleanup finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (c *Controller) deleteRunArtifacts(ctx context.Context, run *v1alpha1.Run) error {
	if c.ArtifactStore == nil {
		return fmt.Errorf("artifact store is not configured")
	}
	if runStore, ok := c.ArtifactStore.(artifact.RunStore); ok {
		if err := runStore.DeleteRun(ctx, run); err != nil {
			return fmt.Errorf("delete Run artifacts: %w", err)
		}
		return nil
	}
	for _, ref := range run.Status.ArtifactRefs {
		if err := c.ArtifactStore.Delete(ctx, ref); err != nil {
			return fmt.Errorf("delete artifact %q: %w", ref.Name, err)
		}
	}
	return nil
}

func inspectArtifact(path string, maxBytes int64) (int64, v1alpha1.ArtifactType, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, "", err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return 0, "", fmt.Errorf("symbolic links are not allowed")
	case info.Mode().IsRegular():
		if info.Size() > maxBytes {
			return 0, "", fmt.Errorf("size %d exceeds %d bytes", info.Size(), maxBytes)
		}
		return info.Size(), v1alpha1.ArtifactTypeFile, nil
	case info.IsDir():
		var total int64
		err := filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			currentInfo, err := os.Lstat(current)
			if err != nil {
				return err
			}
			if currentInfo.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%s: symbolic links are not allowed", current)
			}
			if currentInfo.IsDir() {
				return nil
			}
			if !currentInfo.Mode().IsRegular() {
				return fmt.Errorf("%s: special files are not allowed", current)
			}
			total += currentInfo.Size()
			if total > maxBytes {
				return fmt.Errorf("size exceeds %d bytes", maxBytes)
			}
			return nil
		})
		return total, v1alpha1.ArtifactTypeDirectory, err
	default:
		return 0, "", fmt.Errorf("special files are not allowed")
	}
}

func (c *Controller) maxArtifactBytes() int64 {
	if c.MaxArtifactBytes > 0 {
		return c.MaxArtifactBytes
	}
	return artifact.DefaultMaxArtifactBytes
}

func (c *Controller) maxArtifactsBytes() int64 {
	if c.MaxArtifactsBytes > 0 {
		return c.MaxArtifactsBytes
	}
	return artifact.DefaultMaxArtifactsBytes
}

type artifactInvalidError struct {
	err error
}

func (e artifactInvalidError) Error() string { return e.err.Error() }
func (e artifactInvalidError) Unwrap() error { return e.err }

func isArtifactInvalid(err error) bool {
	var local artifactInvalidError
	if errors.As(err, &local) {
		return true
	}
	var storeError interface{ ArtifactInvalid() bool }
	return errors.As(err, &storeError) && storeError.ArtifactInvalid()
}

func artifactStagingDir(run *v1alpha1.Run) string {
	return filepath.Join(workspacePath, string(run.UID), "artifacts")
}
