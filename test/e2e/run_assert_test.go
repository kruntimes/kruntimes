package e2e

// Run lifecycle assertions and termination requests shared by the execution and scheduler suites.

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runstatus"
)

func findRunCondition(run *v1alpha1.Run, typ string) *metav1.Condition {
	for i := range run.Status.Conditions {
		if run.Status.Conditions[i].Type == typ {
			return &run.Status.Conditions[i]
		}
	}
	return nil
}

func waitForRunCondition(t *testing.T, run *v1alpha1.Run, typ string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if condition := findRunCondition(run, typ); condition != nil && condition.Status == metav1.ConditionTrue {
			return
		}
		switch run.Status.Phase {
		case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
			t.Fatalf("Run %s reached %s before condition %s=True: %s", run.Name, run.Status.Phase, typ, run.Status.Message)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for Run %s condition %s=True", run.Name, typ)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func assertCancelledRun(t *testing.T, run *v1alpha1.Run) {
	t.Helper()
	if run.Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("phase = %s, want Cancelled", run.Status.Phase)
	}
	if run.Status.CompletionTime == nil {
		t.Fatal("expected completion time for cancelled run")
	}
	running := findRunCondition(run, "Running")
	if running == nil {
		t.Fatal("expected Running condition")
	}
	if running.Status != metav1.ConditionFalse || running.Reason != "Cancelled" {
		t.Fatalf("expected Running=False reason=Cancelled, got status=%s reason=%s", running.Status, running.Reason)
	}
	completed := findRunCondition(run, "Completed")
	if completed == nil {
		t.Fatal("expected Completed condition")
	}
	if completed.Status != metav1.ConditionFalse || completed.Reason != "Cancelled" {
		t.Fatalf("expected Completed=False reason=Cancelled, got status=%s reason=%s", completed.Status, completed.Reason)
	}
	if ready := findRunCondition(run, runstatus.ConditionReady); ready != nil && ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected terminal Ready condition to be false, got status=%s reason=%s", ready.Status, ready.Reason)
	}
}

func requestRunCancel(t *testing.T, run *v1alpha1.Run) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run for cancel: %v", err)
		}
		run.Spec.Termination = &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationImmediate}
		if err := k8sClient.Update(context.Background(), run); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("failed to request cancellation for run %s", run.Name)
}

func requestRunDrain(t *testing.T, run *v1alpha1.Run) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run for drain: %v", err)
		}
		run.Spec.Termination = &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationDrain}
		if err := k8sClient.Update(context.Background(), run); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("failed to request drain for run %s", run.Name)
}

func taskMode(args ...string) v1alpha1.RunMode {
	return v1alpha1.RunMode{
		Task: &v1alpha1.RunTaskMode{Args: args},
	}
}

func assertOutputsFailure(t *testing.T, run *v1alpha1.Run, reason string) {
	t.Helper()
	if run.Status.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 for non-retryable outputs failure", run.Status.Attempt)
	}
	if run.Status.CompletionTime == nil {
		t.Fatal("expected completion time")
	}
	running := findRunCondition(run, "Running")
	if running == nil || running.Status != metav1.ConditionFalse || running.Reason != reason {
		t.Fatalf("Running condition = %#v, want False/%s", running, reason)
	}
	completed := findRunCondition(run, "Completed")
	if completed == nil || completed.Status != metav1.ConditionFalse || completed.Reason != reason {
		t.Fatalf("Completed condition = %#v, want False/%s", completed, reason)
	}
}
