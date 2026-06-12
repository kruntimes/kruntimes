package runtimed

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestValidateGitSourceURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "https", url: "https://github.com/kruntimes/kruntimes.git"},
		{name: "ssh scheme", url: "ssh://git@github.com/kruntimes/kruntimes.git"},
		{name: "scp style", url: "git@github.com:kruntimes/kruntimes.git"},
		{name: "file scheme", url: "file:///tmp/repo.git", wantErr: true},
		{name: "git scheme", url: "git://github.com/kruntimes/kruntimes.git", wantErr: true},
		{name: "local path", url: "/tmp/repo.git", wantErr: true},
		{name: "relative path", url: "../repo.git", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGitSourceURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("validateGitSourceURL: %v", err)
			}
		})
	}
}

func TestRunGitCommandTimeout(t *testing.T) {
	restore := stubGitCommandContext(t, "sleep")
	defer restore()

	err := runGitCommand(t.TempDir(), 20*time.Millisecond, "git clone", "clone", "https://example.invalid/repo.git", "repo")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("runGitCommand error = %v, want timeout", err)
	}
}

func TestRunGitCommandOutputLimit(t *testing.T) {
	restore := stubGitCommandContext(t, "stderr-large")
	defer restore()

	err := runGitCommand(t.TempDir(), time.Second, "git clone", "clone", "https://example.invalid/repo.git", "repo")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), gitOutputTruncatedMarker) {
		t.Fatalf("error = %q, want truncation marker", err.Error())
	}
}

func TestEnforceDirectorySizeLimit(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("1234"), 0o644); err != nil {
		t.Fatalf("write small file: %v", err)
	}
	if err := enforceDirectorySizeLimit(root, 16); err != nil {
		t.Fatalf("enforceDirectorySizeLimit: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Repeat("x", 32)), 0o644); err != nil {
		t.Fatalf("write big file: %v", err)
	}
	if err := enforceDirectorySizeLimit(root, 16); err == nil {
		t.Fatal("expected size limit error")
	}
}

func TestPrepareSourceRejectsDisallowedRepoURL(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	run := testRunWithRepoURL("file:///tmp/repo.git")
	_, err := prepareSource(run)
	if err == nil || !strings.Contains(err.Error(), "repoURL") {
		t.Fatalf("prepareSource error = %v, want repoURL validation", err)
	}
}

func TestPrepareSourceRejectsOversizedRepository(t *testing.T) {
	restore := stubGitCommandContext(t, "create-large-repo")
	defer restore()

	dir := t.TempDir()
	workspacePath = dir

	run := testRunWithRepoURL("https://github.com/kruntimes/kruntimes.git")
	_, err := prepareSource(run)
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("prepareSource error = %v, want size limit", err)
	}
}

func testRunWithRepoURL(url string) *v1alpha1.Run {
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Source: &v1alpha1.CodeSource{RepoURL: url},
		},
	}
	run.UID = "test-uid"
	return run
}

func stubGitCommandContext(t *testing.T, mode string) func() {
	t.Helper()
	previous := gitCommandContext
	gitCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		cmdArgs := []string{"-test.run=TestGitHelperProcess", "--"}
		cmdArgs = append(cmdArgs, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], cmdArgs...)
		cmd.Env = append(os.Environ(), "GO_WANT_GIT_HELPER_PROCESS=1", "GIT_HELPER_MODE="+mode)
		return cmd
	}
	return func() {
		gitCommandContext = previous
	}
}

func TestGitHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_GIT_HELPER_PROCESS") != "1" {
		return
	}

	args := os.Args
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}

	switch os.Getenv("GIT_HELPER_MODE") {
	case "sleep":
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	case "stderr-large":
		fmt.Fprint(os.Stderr, strings.Repeat("x", gitCommandOutputLimit+128))
		os.Exit(1)
	case "create-large-repo":
		if len(args) < 3 || args[0] != "clone" {
			fmt.Fprint(os.Stderr, "unexpected clone args")
			os.Exit(2)
		}
		cloneDir := args[2]
		if err := os.MkdirAll(cloneDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "mkdir: %v", err)
			os.Exit(1)
		}
		content := strings.Repeat("x", int(gitSourceSizeLimitBytes)+1)
		if err := os.WriteFile(filepath.Join(cloneDir, "large.txt"), []byte(content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	default:
		fmt.Fprint(os.Stderr, "unknown helper mode")
		os.Exit(2)
	}
}
