// Package toolcache manages a Runtime Pod-local, performance-only tool cache.
package toolcache

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	EnvironmentVariable = "KRUNTIME_TOOL_CACHE"
	StagingVariable     = "KRUNTIME_CACHE_STAGING"
)

// Root returns the Runtime Pod-local cache location on its shared workspace.
func Root(workspace string) string {
	return filepath.Join(workspace, ".kruntimes", "tool-cache")
}

// Prepare creates directories shared by all containers in a Runtime Pod. The
// cache intentionally is not a privilege boundary: Runtime code is trusted.
func Prepare(root string) error {
	for _, directory := range []string{root, filepath.Join(root, ".locks"), filepath.Join(root, ".staging"), filepath.Join(root, "bin")} {
		if err := os.MkdirAll(directory, 0o777); err != nil {
			return fmt.Errorf("create cache directory %q: %w", directory, err)
		}
		info, err := os.Stat(directory)
		if err != nil {
			return fmt.Errorf("stat cache directory %q: %w", directory, err)
		}
		if info.Mode().Perm()&0o777 != 0o777 {
			if err := os.Chmod(directory, 0o777); err != nil {
				return fmt.Errorf("set cache directory permissions %q: %w", directory, err)
			}
		}
	}
	return nil
}

// InstallHelper makes the runtimed executable available to the adjacent
// runtime container through the shared workspace. The helper is immutable for
// the lifetime of a Runtime Pod.
func InstallHelper(root, source string) (string, error) {
	if err := Prepare(root); err != nil {
		return "", err
	}
	destination := filepath.Join(root, "bin", "kruntime-cache")
	if info, err := os.Stat(destination); err == nil && !info.IsDir() {
		return destination, nil
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("stat cache helper: %w", err)
	}
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open runtimed executable for cache helper: %w", err)
	}
	defer input.Close()
	temporary, err := os.CreateTemp(filepath.Join(root, "bin"), ".kruntime-cache-")
	if err != nil {
		return "", fmt.Errorf("create cache helper temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := io.Copy(temporary, input); err != nil {
		temporary.Close()
		return "", fmt.Errorf("copy cache helper: %w", err)
	}
	if err := temporary.Chmod(0o755); err != nil {
		temporary.Close()
		return "", fmt.Errorf("set cache helper permissions: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close cache helper: %w", err)
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		if info, statErr := os.Stat(destination); statErr == nil && !info.IsDir() {
			return destination, nil
		}
		return "", fmt.Errorf("publish cache helper: %w", err)
	}
	return destination, nil
}

// Ensure populates key exactly once across concurrent processes. populate
// receives a private staging directory; its contents become the cache entry
// atomically only after it returns successfully.
func Ensure(ctx context.Context, root, key string, populate func(context.Context, string) error) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	if err := Prepare(root); err != nil {
		return "", err
	}

	entry := filepath.Join(root, filepath.FromSlash(key))
	lock, err := os.OpenFile(filepath.Join(root, ".locks", keyHash(key)+".lock"), os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return "", fmt.Errorf("open cache lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("lock cache entry %q: %w", key, err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if complete(entry) {
		return entry, nil
	}
	stagingParent := filepath.Join(root, ".staging")
	staging, err := os.MkdirTemp(stagingParent, "entry-")
	if err != nil {
		return "", fmt.Errorf("create cache entry staging directory: %w", err)
	}
	defer os.RemoveAll(staging)
	if err := populate(ctx, staging); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(staging, ".complete"), nil, 0o644); err != nil {
		return "", fmt.Errorf("mark cache entry complete: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(entry), 0o777); err != nil {
		return "", fmt.Errorf("create cache entry parent: %w", err)
	}
	if err := os.RemoveAll(entry); err != nil {
		return "", fmt.Errorf("remove incomplete cache entry: %w", err)
	}
	if err := os.Rename(staging, entry); err != nil {
		return "", fmt.Errorf("publish cache entry: %w", err)
	}
	return entry, nil
}

func complete(entry string) bool {
	info, err := os.Stat(filepath.Join(entry, ".complete"))
	return err == nil && !info.IsDir()
}

func validateKey(key string) error {
	if key == "." || !fs.ValidPath(key) {
		return fmt.Errorf("cache key %q must be a relative slash-separated path without . or ..", key)
	}
	first, _, _ := strings.Cut(key, "/")
	if first == "bin" || strings.HasPrefix(first, ".") {
		return fmt.Errorf("cache key %q uses reserved top-level path %q", key, first)
	}
	return nil
}

func keyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", sum[:])
}

// CommandEnvironment returns the environment passed to a cache population
// command. It replaces a conflicting staging directory supplied by the caller.
func CommandEnvironment(environment []string, staging string) []string {
	filtered := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if strings.HasPrefix(item, StagingVariable+"=") {
			continue
		}
		filtered = append(filtered, item)
	}
	return append(filtered, StagingVariable+"="+staging)
}
