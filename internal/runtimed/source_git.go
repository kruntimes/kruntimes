package runtimed

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	gitCloneTimeout          = 30 * time.Second
	gitCheckoutTimeout       = 15 * time.Second
	gitCommandOutputLimit    = 64 << 10
	gitSourceSizeLimitBytes  = 100 << 20
	gitOutputTruncatedMarker = "\n[git output truncated]\n"
)

var gitCommandContext = exec.CommandContext //nolint:gochecknoglobals

func prepareGitSource(runDir string, sourceURL string, commitSHA string) (string, error) {
	if err := validateGitSourceURL(sourceURL); err != nil {
		return "", err
	}

	cloneDir := filepath.Join(runDir, "repo")
	args := []string{"clone", sourceURL, cloneDir}
	if commitSHA == "" {
		args = append(args, "--depth=1")
	}
	if err := runGitCommand(runDir, gitCloneTimeout, "git clone", args...); err != nil {
		return "", err
	}
	if commitSHA != "" {
		if err := runGitCommand(cloneDir, gitCheckoutTimeout, "git checkout", "checkout", commitSHA); err != nil {
			return "", err
		}
	}
	if err := enforceDirectorySizeLimit(cloneDir, gitSourceSizeLimitBytes); err != nil {
		return "", fmt.Errorf("git source size limit exceeded: %w", err)
	}
	return cloneDir, nil
}

func validateGitSourceURL(sourceURL string) error {
	if sourceURL == "" {
		return fmt.Errorf("repoURL is required")
	}

	if strings.HasPrefix(sourceURL, "git@") {
		if strings.Contains(sourceURL, ":") {
			return nil
		}
		return fmt.Errorf("repoURL uses unsupported SSH syntax")
	}

	u, err := url.Parse(sourceURL)
	if err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "https", "ssh":
			return nil
		default:
			return fmt.Errorf("repoURL scheme %q is not allowed", u.Scheme)
		}
	}

	return fmt.Errorf("repoURL must use https or ssh")
}

func runGitCommand(dir string, timeout time.Duration, prefix string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := gitCommandContext(ctx, "git", args...)
	cmd.Dir = dir

	output := &limitedCommandOutput{limit: gitCommandOutputLimit}
	cmd.Stdout = output
	cmd.Stderr = output

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s: timed out after %s", prefix, timeout)
		}
		if text := output.String(); text != "" {
			return fmt.Errorf("%s: %w\n%s", prefix, err, text)
		}
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}

func enforceDirectorySizeLimit(root string, limit int64) error {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		if total > limit {
			return fmt.Errorf("directory size %d exceeds limit %d", total, limit)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

type limitedCommandOutput struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedCommandOutput) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return len(p), nil
	}
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(p) > remaining {
			_, _ = b.buffer.Write(p[:remaining])
			b.truncated = true
		} else {
			_, _ = b.buffer.Write(p)
		}
	} else {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedCommandOutput) String() string {
	if !b.truncated {
		return b.buffer.String()
	}
	return b.buffer.String() + gitOutputTruncatedMarker
}
