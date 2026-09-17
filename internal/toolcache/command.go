package toolcache

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Run executes the kruntime-cache command-line interface.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "ensure":
		return runEnsure(ctx, args[1:], stdout, stderr)
	case "fetch":
		return runFetch(ctx, args[1:])
	default:
		return usageError()
	}
}

func usageError() error {
	return fmt.Errorf("usage: kruntime-cache ensure <key> -- <command> [args...]\n       kruntime-cache fetch <key> <https-url> --extract=zip")
}

func runEnsure(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) < 3 || args[1] != "--" {
		return usageError()
	}
	root := os.Getenv(EnvironmentVariable)
	if root == "" {
		return fmt.Errorf("%s is required", EnvironmentVariable)
	}
	key, command := args[0], args[2:]
	_, err := Ensure(ctx, root, key, func(ctx context.Context, staging string) error {
		cmd := exec.CommandContext(ctx, command[0], command[1:]...)
		cmd.Env = CommandEnvironment(os.Environ(), staging)
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		cmd.Stdin = os.Stdin
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("populate cache entry %q: %w", key, err)
		}
		return nil
	})
	return err
}

func runFetch(ctx context.Context, args []string) error {
	if len(args) != 3 || args[2] != "--extract=zip" {
		return usageError()
	}
	root := os.Getenv(EnvironmentVariable)
	if root == "" {
		return fmt.Errorf("%s is required", EnvironmentVariable)
	}
	key, source := args[0], args[1]
	_, err := Ensure(ctx, root, key, func(ctx context.Context, staging string) error {
		return fetchZip(ctx, source, staging)
	})
	return err
}

func fetchZip(ctx context.Context, source, destination string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return fmt.Errorf("create download request: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("download cache entry: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("download cache entry: unexpected HTTP status %s", response.Status)
	}
	archive, err := os.CreateTemp(destination, "download-*.zip")
	if err != nil {
		return fmt.Errorf("create download archive: %w", err)
	}
	archiveName := archive.Name()
	defer os.Remove(archiveName)
	if _, err := io.Copy(archive, response.Body); err != nil {
		archive.Close()
		return fmt.Errorf("save download archive: %w", err)
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("close download archive: %w", err)
	}
	reader, err := zip.OpenReader(archiveName)
	if err != nil {
		return fmt.Errorf("open ZIP cache archive: %w", err)
	}
	defer reader.Close()
	for _, file := range reader.File {
		if err := extractZipFile(file, destination); err != nil {
			return err
		}
	}
	return nil
}

func extractZipFile(file *zip.File, destination string) error {
	name := strings.TrimSuffix(file.Name, "/")
	if name == "" {
		return nil
	}
	if !fs.ValidPath(name) {
		return fmt.Errorf("ZIP cache archive contains unsafe path %q", file.Name)
	}
	target := filepath.Join(destination, filepath.FromSlash(name))
	if file.FileInfo().IsDir() {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("create ZIP directory %q: %w", file.Name, err)
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create ZIP parent for %q: %w", file.Name, err)
	}
	input, err := file.Open()
	if err != nil {
		return fmt.Errorf("open ZIP file %q: %w", file.Name, err)
	}
	defer input.Close()
	mode := file.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create ZIP file %q: %w", file.Name, err)
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("extract ZIP file %q: %w", file.Name, err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close ZIP file %q: %w", file.Name, err)
	}
	return nil
}
