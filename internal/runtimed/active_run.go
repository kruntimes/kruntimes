package runtimed

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// activeRun holds the execution-specific state derived once when runtimed
// claims or recovers a Run.
type activeRun struct {
	run       *v1alpha1.Run
	workDir   string
	sourceDir string
	// sourceEntrypointPrefix is the Run-local source path relative to workDir.
	// It is set only when a task shares a PersistentWorkspace workDir.
	sourceEntrypointPrefix string
	outputPath             string
	artifactDir            string
	cleanupDir             string
	deadline               time.Time
	start                  time.Time
	started                atomic.Bool
	prepared               atomic.Bool
	executionStartMu       sync.Mutex
	executionStart         *executionStartFailure
	executionStarting      bool
	sessionRegistrationMu  sync.Mutex
	sessionRegistration    *sessionRegistrationFailure
	sessionRegistering     bool
	sessionCloseMu         sync.Mutex
	sessionClosed          atomic.Bool
	functionRegistrationMu sync.Mutex
	functionRegistration   *functionRegistrationState
	functionRegistering    bool
	functionCloseMu        sync.Mutex
	functionClosed         atomic.Bool
	outputMu               sync.Mutex
	stdoutCursor           outputCursor
	stderrCursor           outputCursor
}

// outputCursor tracks one Runtime Status stream as runtimed projects it into
// structured container logs. Runtime Status returns a bounded cumulative
// buffer, so the cursor retains an unflushed trailing line as well as the
// number of source bytes already inspected.
type outputCursor struct {
	seen       int
	prefixHash [32]byte
	hasHash    bool
	pending    string
}

type functionRegistrationState struct {
	registration *pb.FunctionRegistration
	failure      *functionRegistrationFailure
}

type functionRegistrationFailure struct {
	reason  string
	message string
}

func (ar *activeRun) beginFunctionRegistration() bool {
	ar.functionRegistrationMu.Lock()
	defer ar.functionRegistrationMu.Unlock()
	if ar.functionRegistering {
		return false
	}
	ar.functionRegistering = true
	ar.functionRegistration = nil
	return true
}

func (ar *activeRun) finishFunctionRegistration(registration *pb.FunctionRegistration, failure *functionRegistrationFailure) {
	ar.functionRegistrationMu.Lock()
	defer ar.functionRegistrationMu.Unlock()
	ar.functionRegistering = false
	ar.functionRegistration = &functionRegistrationState{registration: registration, failure: failure}
}

func (ar *activeRun) functionRegistrationInFlight() bool {
	ar.functionRegistrationMu.Lock()
	defer ar.functionRegistrationMu.Unlock()
	return ar.functionRegistering
}

func (ar *activeRun) functionRegistrationRef() *pb.FunctionRegistration {
	ar.functionRegistrationMu.Lock()
	defer ar.functionRegistrationMu.Unlock()
	if ar.functionRegistration == nil || ar.functionRegistration.registration == nil {
		return nil
	}
	return ar.functionRegistration.registration
}

func (ar *activeRun) consumeFunctionRegistrationFailure() *functionRegistrationFailure {
	ar.functionRegistrationMu.Lock()
	defer ar.functionRegistrationMu.Unlock()
	if ar.functionRegistration == nil {
		return nil
	}
	failure := ar.functionRegistration.failure
	ar.functionRegistration.failure = nil
	return failure
}

// resetFunctionRegistration drops the local reference after the shared retry
// engine has advanced the registration attempt. The next reconciliation then
// issues RegisterFunction for that new attempt; a Runtime Server uses the
// attempt to fence any prior registration generation.
func (ar *activeRun) resetFunctionRegistration() {
	ar.functionRegistrationMu.Lock()
	defer ar.functionRegistrationMu.Unlock()
	ar.functionRegistering = false
	ar.functionRegistration = nil
	ar.functionClosed.Store(false)
}

// executionStartFailure is recorded by the asynchronous local execution
// startup. Reconcile consumes it and applies the normal Run retry semantics;
// the goroutine itself never mutates Kubernetes status.
type executionStartFailure struct {
	reason  string
	message string
}

func (ar *activeRun) beginExecutionStart() bool {
	ar.executionStartMu.Lock()
	defer ar.executionStartMu.Unlock()
	if ar.executionStarting {
		return false
	}
	ar.executionStarting = true
	ar.executionStart = nil
	ar.started.Store(false)
	return true
}

func (ar *activeRun) finishExecutionStart(failure *executionStartFailure) {
	ar.executionStartMu.Lock()
	defer ar.executionStartMu.Unlock()
	ar.executionStarting = false
	ar.executionStart = failure
}

func (ar *activeRun) executionStartInFlight() bool {
	ar.executionStartMu.Lock()
	defer ar.executionStartMu.Unlock()
	return ar.executionStarting
}

func (ar *activeRun) consumeExecutionStartFailure() *executionStartFailure {
	ar.executionStartMu.Lock()
	defer ar.executionStartMu.Unlock()
	failure := ar.executionStart
	ar.executionStart = nil
	return failure
}

// sessionRegistrationFailure is recorded by the asynchronous local
// registration operation. Reconcile consumes it and applies the normal Run
// retry semantics; the goroutine itself never mutates Kubernetes status.
type sessionRegistrationFailure struct {
	reason  string
	message string
}

func (ar *activeRun) beginSessionRegistration() bool {
	ar.sessionRegistrationMu.Lock()
	defer ar.sessionRegistrationMu.Unlock()
	if ar.sessionRegistering {
		return false
	}
	ar.sessionRegistering = true
	ar.sessionRegistration = nil
	return true
}

func (ar *activeRun) finishSessionRegistration(failure *sessionRegistrationFailure) {
	ar.sessionRegistrationMu.Lock()
	defer ar.sessionRegistrationMu.Unlock()
	ar.sessionRegistering = false
	ar.sessionRegistration = failure
}

func (ar *activeRun) sessionRegistrationInFlight() bool {
	ar.sessionRegistrationMu.Lock()
	defer ar.sessionRegistrationMu.Unlock()
	return ar.sessionRegistering
}

func (ar *activeRun) consumeSessionRegistrationFailure() *sessionRegistrationFailure {
	ar.sessionRegistrationMu.Lock()
	defer ar.sessionRegistrationMu.Unlock()
	failure := ar.sessionRegistration
	ar.sessionRegistration = nil
	return failure
}

func newActiveRun(run *v1alpha1.Run, start time.Time) *activeRun {
	return newActiveRunWithWorkspace(run, start, workspacePath)
}

func newActiveRunWithWorkspace(run *v1alpha1.Run, start time.Time, workspaceRoot string) *activeRun {
	if run == nil {
		return &activeRun{start: start}
	}

	workspaceDir := filepath.Join(workspaceRoot, string(run.UID))
	if run.Spec.Workspace != nil {
		workspaceDir = persistentWorkspacePath(run.Spec.Workspace.Name)
	}

	cleanupDir := workspaceDir
	sourceEntrypointPrefix := ""
	if run.Spec.Workspace != nil {
		cleanupDir = filepath.Join(workspaceDir, ".kruntimes", "runs", string(run.UID))
		if run.Spec.Mode.Task != nil {
			sourceEntrypointPrefix = filepath.Join(".kruntimes", "runs", string(run.UID))
		}
	}

	sourceDir := cleanupDir
	if run.Spec.Mode.Function != nil && run.Spec.Workspace != nil {
		sourceDir = workspaceDir
	}
	workDir := sourceDir
	if sourceEntrypointPrefix != "" {
		workDir = workspaceDir
	} else if run.Spec.Source != nil && run.Spec.Source.RepoURL != "" {
		workDir = filepath.Join(sourceDir, "repo")
	}

	ar := &activeRun{
		run:                    run,
		start:                  start,
		workDir:                workDir,
		sourceDir:              sourceDir,
		sourceEntrypointPrefix: sourceEntrypointPrefix,
		outputPath:             filepath.Join(cleanupDir, "outputs"),
		artifactDir:            filepath.Join(cleanupDir, "artifacts"),
		cleanupDir:             cleanupDir,
	}
	if run.Spec.Timeout != nil && run.Status.StartTime != nil {
		ar.deadline = run.Status.StartTime.Add(run.Spec.Timeout.Duration)
	}
	return ar
}

// cleanupRunFiles removes the temporary workspace of an unshared Run. A
// referenced PersistentWorkspace owns its entire volume lifecycle, so runtimed
// must not remove any path below its mount.
func cleanupRunFiles(ar *activeRun) error {
	if ar == nil || ar.run == nil || ar.run.Spec.Workspace != nil || ar.cleanupDir == "" {
		return nil
	}
	if err := os.RemoveAll(ar.cleanupDir); err != nil {
		return fmt.Errorf("remove Run-local workspace: %w", err)
	}
	return nil
}
