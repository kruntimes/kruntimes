package runtimed

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	workspaceHomeDirectory = ".home"
	workspaceTempDirectory = ".tmp"
)

// prepareWorkspaceEnvironment supplies filesystem-backed environment defaults
// for Runs that use a PersistentWorkspace. The function is intentionally
// workflow-agnostic: a WorkflowRun job receives one PersistentWorkspace, but
// users can also create one directly.
//
// For a WorkflowRun, this means sequential job steps share HOME and TMPDIR,
// while different jobs receive separate values. These runtime-owned defaults
// deliberately override Run.Spec.Env so a step cannot accidentally write to
// an image-level home or container-global temporary directory.
func prepareWorkspaceEnvironment(run *v1alpha1.Run, workingDir string, env map[string]string) error {
	if run == nil || run.Spec.Workspace == nil {
		return nil
	}
	if workingDir == "" {
		return fmt.Errorf("workspace working directory is required")
	}

	home := filepath.Join(workingDir, workspaceHomeDirectory)
	temporary := filepath.Join(workingDir, workspaceTempDirectory)
	for _, directory := range []string{home, temporary} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create workflow job directory %q: %w", directory, err)
		}
	}
	env["HOME"] = home
	env["TMPDIR"] = temporary
	return nil
}
