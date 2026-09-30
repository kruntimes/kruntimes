package e2e

// Condition-driven wait helpers for Run, WorkflowRun, and PersistentWorkspace state transitions.
//
// Every helper polls a live condition (API status or a caller predicate) and
// fails with diagnostics on timeout; none of them sleep before the first check.

import (
	"context"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func waitForRun(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	waitForRunPhase(t, run, timeout, v1alpha1.RunSucceeded)
}

func waitForWorkflowRunPhase(t *testing.T, workflowRun *v1alpha1.WorkflowRun, timeout time.Duration, expected v1alpha1.WorkflowPhase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastPhase v1alpha1.WorkflowPhase
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for workflowrun %s, last phase=%s, msg=%s", workflowRun.Name, lastPhase, workflowRun.Status.Message)
		default:
		}

		time.Sleep(500 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			t.Fatalf("get workflowrun: %v", err)
		}
		if workflowRun.Status.Phase != lastPhase {
			t.Logf("WorkflowRun %s: phase=%s", workflowRun.Name, workflowRun.Status.Phase)
			lastPhase = workflowRun.Status.Phase
		}
		switch workflowRun.Status.Phase {
		case expected:
			return
		case v1alpha1.WorkflowSucceeded, v1alpha1.WorkflowFailed, v1alpha1.WorkflowCancelled:
			t.Fatalf("expected phase=%s, got phase=%s, msg=%s", expected, workflowRun.Status.Phase, workflowRun.Status.Message)
		}
	}
}

func waitForRunPhase(t *testing.T, run *v1alpha1.Run, timeout time.Duration, expected v1alpha1.RunPhase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastPhase v1alpha1.RunPhase
	var lastAttempt int32
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run %s, last phase=%s, attempt=%d, msg=%s", run.Name, lastPhase, lastAttempt, run.Status.Message)
		default:
		}

		time.Sleep(500 * time.Millisecond)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
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
			return
		case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
			t.Fatalf("expected phase=%s, got phase=%s, msg=%s (attempt=%d)", expected, run.Status.Phase, run.Status.Message, run.Status.Attempt)
		}
	}
}

func waitForAnyTerminalRunPhase(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for terminal run %s, last phase=%s msg=%s", run.Name, run.Status.Phase, run.Status.Message)
		default:
		}

		time.Sleep(500 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		switch run.Status.Phase {
		case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
			return
		}
	}
}

func waitForRunDeleted(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		var current v1alpha1.Run
		err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &current)
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatalf("get run while waiting for delete: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run %s to be deleted, phase=%s completion=%v", run.Name, current.Status.Phase, current.Status.CompletionTime)
		case <-time.After(500 * time.Millisecond):
		}
	}
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
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run while waiting for Pending: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunPending && run.Status.AssignedPod == "" && strings.Contains(run.Status.Message, message) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Run status = %#v, want unassigned Pending Run with message containing %q", run.Status, message)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForPersistentWorkspacePhase(t *testing.T, workspace *v1alpha1.PersistentWorkspace, timeout time.Duration, phase v1alpha1.PersistentWorkspacePhase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workspace), workspace); err != nil {
			t.Fatalf("get PersistentWorkspace while waiting for %s: %v", phase, err)
		}
		if workspace.Status.Phase == phase {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("PersistentWorkspace = %#v, want phase %s", workspace.Status, phase)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForPersistentWorkspaceDeleted(t *testing.T, workspace *v1alpha1.PersistentWorkspace, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		var current v1alpha1.PersistentWorkspace
		err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workspace), &current)
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatalf("get PersistentWorkspace while waiting for deletion: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("PersistentWorkspace %s/%s was not deleted", workspace.Namespace, workspace.Name)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForActionChildRun(t *testing.T, workflowRun *v1alpha1.WorkflowRun, jobName, actionStepName string, timeout time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			t.Fatalf("get WorkflowRun while waiting for Action child: %v", err)
		}
		for _, step := range workflowRun.Status.Jobs[jobName].Steps {
			for _, actionStep := range step.ActionSteps {
				if actionStep.Name == actionStepName && actionStep.RunName != "" {
					return actionStep.RunName
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for Action child Run: %#v", workflowRun.Status)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
