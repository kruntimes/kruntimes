package runtimed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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

func (c *Controller) collectArtifacts(ctx context.Context, run *v1alpha1.Run) ([]v1alpha1.ArtifactRef, error) {
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	refs := make([]v1alpha1.ArtifactRef, 0, len(entries))
	var totalBytes int64
	for _, entry := range entries {
		if err := artifact.ValidateName(entry.Name()); err != nil {
			return nil, artifactInvalidError{err: fmt.Errorf("invalid artifact %q: %w", entry.Name(), err)}
		}
		localPath := filepath.Join(stagingDir, entry.Name())
		size, artifactType, err := inspectArtifact(localPath, c.maxArtifactBytes())
		if err != nil {
			return nil, artifactInvalidError{err: fmt.Errorf("artifact %q: %w", entry.Name(), err)}
		}
		totalBytes += size
		if totalBytes > c.maxArtifactsBytes() {
			return nil, artifactInvalidError{err: fmt.Errorf("total artifact size exceeds %d bytes", c.maxArtifactsBytes())}
		}
		ref, err := c.ArtifactStore.Put(ctx, run, localPath, artifact.PutOptions{
			Name: entry.Name(),
			Type: artifactType,
		})
		if err != nil {
			return nil, fmt.Errorf("store artifact %q: %w", entry.Name(), err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
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
