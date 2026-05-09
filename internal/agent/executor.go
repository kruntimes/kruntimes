package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/airconduct/kruntime/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const maxOutputBytes = 64 * 1024

// ExecutionResult holds the outcome of a task execution.
type ExecutionResult struct {
	Phase   v1alpha1.TaskPhase
	Message string
}

// Executor runs task commands in an isolated working directory.
type Executor struct {
	WorkspaceBase string
}

// Execute runs all commands for the given task and returns the result.
func (e *Executor) Execute(ctx context.Context, task *v1alpha1.Task) ExecutionResult {
	workDir := filepath.Join(e.workspaceBase(), string(task.UID))
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return ExecutionResult{Phase: v1alpha1.TaskFailed, Message: fmt.Sprintf("create workspace: %v", err)}
	}
	defer os.RemoveAll(workDir)

	if task.Spec.RepoURL != "" {
		cloneDir := filepath.Join(workDir, "repo")
		if err := e.gitClone(ctx, task, cloneDir); err != nil {
			return ExecutionResult{Phase: v1alpha1.TaskFailed, Message: fmt.Sprintf("git clone: %v", err)}
		}
		workDir = cloneDir
	}

	var output strings.Builder
	for i, cmd := range task.Spec.Commands {
		start := time.Now()
		err := e.runCommand(ctx, workDir, cmd, task.Spec.Env, &output)
		elapsed := time.Since(start)

		if err != nil {
			output.WriteString(fmt.Sprintf("\n[command %d failed in %v: %v]\n", i, elapsed, err))
			return ExecutionResult{
				Phase:   v1alpha1.TaskFailed,
				Message: truncateOutput(output.String()),
			}
		}
	}

	return ExecutionResult{
		Phase:   v1alpha1.TaskSucceeded,
		Message: truncateOutput(output.String()),
	}
}

func (e *Executor) workspaceBase() string {
	if e.WorkspaceBase != "" {
		return e.WorkspaceBase
	}
	if base := os.Getenv("WORKSPACE_DIR"); base != "" {
		return base
	}
	return "/workspace"
}

func (e *Executor) gitClone(ctx context.Context, task *v1alpha1.Task, dir string) error {
	args := []string{"clone", "--depth", "1"}
	if task.Spec.CommitSHA != "" {
		args = append(args, "--branch", task.Spec.CommitSHA)
	}
	args = append(args, task.Spec.RepoURL, dir)

	cmd := exec.CommandContext(ctx, "git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}
	return nil
}

func (e *Executor) runCommand(ctx context.Context, workDir, cmdStr string, envVars []corev1.EnvVar, output *strings.Builder) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdStr)
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	for _, env := range envVars {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", env.Name, env.Value))
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	stdout.WriteTo(output)
	stderr.WriteTo(output)

	return err
}

func truncateOutput(s string) string {
	if len(s) > maxOutputBytes {
		return s[:maxOutputBytes] + "\n[... output truncated ...]"
	}
	return s
}
