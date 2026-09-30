package e2e

// Condition-driven wait helpers for Run, WorkflowRun, and PersistentWorkspace state transitions.
//
// Every helper here polls a live condition (API status or a caller predicate)
// and fails with diagnostics on timeout. The condition is always evaluated
// before the first sleep, so an already-satisfied state returns immediately
// instead of paying a fixed delay.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// waitPollInterval is the cadence for status polling. It is deliberately
// shorter than the 500ms the suite used previously: most transitions are
// observed within a second, so a finer poll materially lowers per-transition
// latency without a meaningful increase in API server load.
const waitPollInterval = 200 * time.Millisecond

// waitFor evaluates cond until it reports done or timeout elapses. The first
// evaluation happens immediately. describe is called only on timeout so it can
// report the most recent observed state, and onTimeout (optional) can enrich
// failure diagnostics before the test fails.
func waitFor(t *testing.T, timeout time.Duration, describe func() string, cond func(ctx context.Context) (bool, error), onTimeout ...func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastErr error
	for {
		done, err := cond(ctx)
		if err != nil {
			lastErr = err
		}
		if done {
			return
		}
		select {
		case <-ctx.Done():
			for _, hook := range onTimeout {
				if hook != nil {
					hook()
				}
			}
			if lastErr != nil {
				t.Fatalf("timed out waiting for %s: %v", describe(), lastErr)
			}
			t.Fatalf("timed out waiting for %s", describe())
		case <-time.After(waitPollInterval):
		}
	}
}

func waitForRun(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	waitForRunPhase(t, run, timeout, v1alpha1.RunSucceeded)
}

// waitForRunRunning waits until the scheduler assigned the Run to a pod and
// runtimed claimed it.
func waitForRunRunning(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("run %s to be Running on a pod (last phase=%s, pod=%s, msg=%s)", run.Name, run.Status.Phase, run.Status.AssignedPod, run.Status.Message)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return false, err
		}
		return run.Status.Phase == v1alpha1.RunRunning && run.Status.AssignedPod != "", nil
	})
}

// waitForWorkflowChildRun waits until a WorkflowRun job has materialized the
// child WorkflowRun it calls through "uses".
func waitForWorkflowChildRun(t *testing.T, workflowRun *v1alpha1.WorkflowRun, jobName string, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("WorkflowRun %s job %s to create its child WorkflowRun (status=%#v)", workflowRun.Name, jobName, workflowRun.Status)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			return false, err
		}
		return workflowRun.Status.Jobs[jobName].WorkflowRunName != "", nil
	})
}

func waitForWorkflowRunPhase(t *testing.T, workflowRun *v1alpha1.WorkflowRun, timeout time.Duration, expected v1alpha1.WorkflowPhase) {
	t.Helper()
	var lastPhase v1alpha1.WorkflowPhase
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("workflowrun %s to reach phase %s (last phase=%s, msg=%s)", workflowRun.Name, expected, lastPhase, workflowRun.Status.Message)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			return false, err
		}
		if workflowRun.Status.Phase != lastPhase {
			t.Logf("WorkflowRun %s: phase=%s", workflowRun.Name, workflowRun.Status.Phase)
			lastPhase = workflowRun.Status.Phase
		}
		switch workflowRun.Status.Phase {
		case expected:
			return true, nil
		case v1alpha1.WorkflowSucceeded, v1alpha1.WorkflowFailed, v1alpha1.WorkflowCancelled:
			t.Fatalf("expected phase=%s, got phase=%s, msg=%s", expected, workflowRun.Status.Phase, workflowRun.Status.Message)
		}
		return false, nil
	})
}

func waitForRunPhase(t *testing.T, run *v1alpha1.Run, timeout time.Duration, expected v1alpha1.RunPhase) {
	t.Helper()
	var lastPhase v1alpha1.RunPhase
	var lastAttempt int32
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("run %s to reach phase %s (last phase=%s, attempt=%d, msg=%s)", run.Name, expected, lastPhase, lastAttempt, run.Status.Message)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return false, err
		}
		if run.Status.Phase != lastPhase || run.Status.Attempt != lastAttempt {
			t.Logf("Run %s: phase=%s, attempt=%d (pod=%s)", run.Name, run.Status.Phase, run.Status.Attempt, run.Status.AssignedPod)
			for _, c := range run.Status.Conditions {
				t.Logf("  Condition: type=%s status=%s reason=%s", c.Type, c.Status, c.Reason)
			}
			lastPhase = run.Status.Phase
			lastAttempt = run.Status.Attempt
		}
		switch run.Status.Phase {
		case expected:
			return true, nil
		case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
			t.Fatalf("expected phase=%s, got phase=%s, msg=%s (attempt=%d)", expected, run.Status.Phase, run.Status.Message, run.Status.Attempt)
		}
		return false, nil
	})
}

func waitForAnyTerminalRunPhase(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("terminal phase for run %s (last phase=%s, msg=%s)", run.Name, run.Status.Phase, run.Status.Message)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return false, err
		}
		switch run.Status.Phase {
		case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
			return true, nil
		}
		return false, nil
	})
}

func waitForRunDeleted(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	var current v1alpha1.Run
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("run %s to be deleted (phase=%s completion=%v)", run.Name, current.Status.Phase, current.Status.CompletionTime)
	}, func(ctx context.Context) (bool, error) {
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), &current)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

// deleteRunAndWait removes a Run while its Runtime is still available.  This
// matters for Session and Function Runs: their registration-cleanup finalizer
// must successfully release the remote Runtime Server state before deletion.
func deleteRunAndWait(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	if err := k8sClient.Delete(context.Background(), run); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete Run %s: %v", run.Name, err)
	}
	waitForRunDeleted(t, run, timeout)
}

func waitForPendingRunMessage(t *testing.T, run *v1alpha1.Run, timeout time.Duration, message string) {
	t.Helper()
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("unassigned Pending Run with message containing %q (status=%#v)", message, run.Status)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return false, err
		}
		return run.Status.Phase == v1alpha1.RunPending && run.Status.AssignedPod == "" && strings.Contains(run.Status.Message, message), nil
	})
}

func waitForPersistentWorkspacePhase(t *testing.T, workspace *v1alpha1.PersistentWorkspace, timeout time.Duration, phase v1alpha1.PersistentWorkspacePhase) {
	t.Helper()
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("PersistentWorkspace %s to reach phase %s (status=%#v)", workspace.Name, phase, workspace.Status)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workspace), workspace); err != nil {
			return false, err
		}
		return workspace.Status.Phase == phase, nil
	})
}

func waitForPersistentWorkspaceDeleted(t *testing.T, workspace *v1alpha1.PersistentWorkspace, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("PersistentWorkspace %s/%s to be deleted", workspace.Namespace, workspace.Name)
	}, func(ctx context.Context) (bool, error) {
		var current v1alpha1.PersistentWorkspace
		err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workspace), &current)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

func waitForActionChildRun(t *testing.T, workflowRun *v1alpha1.WorkflowRun, jobName, actionStepName string, timeout time.Duration) string {
	t.Helper()
	var runName string
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("Action child Run for %s/%s (status=%#v)", jobName, actionStepName, workflowRun.Status)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			return false, err
		}
		for _, step := range workflowRun.Status.Jobs[jobName].Steps {
			for _, actionStep := range step.ActionSteps {
				if actionStep.Name == actionStepName && actionStep.RunName != "" {
					runName = actionStep.RunName
					return true, nil
				}
			}
		}
		return false, nil
	})
	return runName
}
