package e2e

// Runtime pool and one-shot Run execution scenarios: lifecycle, timeouts, retries, and stale pod handling.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestRuntimeReadyReplicasTracksRuntimedAvailability(t *testing.T) {
	runtimeName := "bash-readiness"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)
	// A fresh E2E installation may wait for controller leader election before
	// creating the Deployment, so include that startup time in the initial wait.
	waitForRuntimeReadyReplicas(t, runtimeName, 1, 90*time.Second)

	podName := runtimePodName(t, runtimeName)
	previousRestartCount := runtimedRestartCount(t, podName)
	killRuntimed(t, podName)
	waitForRuntimeReadyReplicas(t, runtimeName, 0, 45*time.Second)

	waitForRuntimedRestart(t, podName, previousRestartCount)
	waitForRuntimeReadyReplicas(t, runtimeName, 1, 60*time.Second)
}

func TestFullRunLifecycle(t *testing.T) {
	t.Parallel()
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	const stdout = "hello-not-in-run-status"
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    taskMode("echo " + stdout),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (runtime=bash)", run.Name)
	waitForRun(t, run, 30*time.Second)
	if run.Status.Message != "execution completed" {
		t.Fatalf("success message = %q, want stable summary", run.Status.Message)
	}
	if strings.Contains(run.Status.Message, stdout) {
		t.Fatalf("success message contains stdout: %q", run.Status.Message)
	}
	t.Logf("Run completed successfully: %s", run.Status.Message)
}

func TestRunTimeout(t *testing.T) {
	t.Parallel()
	runtimeName := "bash-timeout"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-timeout-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("sleep 10; echo should_not_print"),
			Timeout: &metav1.Duration{Duration: time.Second},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (timeout)", run.Name)

	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunTimeout)

	if run.Status.Attempt != 1 {
		t.Fatalf("expected one attempt, got %d", run.Status.Attempt)
	}
	if !strings.Contains(run.Status.Message, "timeout") {
		t.Fatalf("expected timeout message, got %q", run.Status.Message)
	}
	if run.Status.CompletionTime == nil {
		t.Fatal("expected completion time for timed out run")
	}

	running := findRunCondition(run, "Running")
	if running == nil {
		t.Fatal("expected Running condition")
	}
	if running.Status != metav1.ConditionFalse || running.Reason != "Timeout" {
		t.Fatalf("expected Running=False reason=Timeout, got status=%s reason=%s", running.Status, running.Reason)
	}

	completed := findRunCondition(run, "Completed")
	if completed == nil {
		t.Fatal("expected Completed condition")
	}
	if completed.Status != metav1.ConditionFalse || completed.Reason != "Timeout" {
		t.Fatalf("expected Completed=False reason=Timeout, got status=%s reason=%s", completed.Status, completed.Reason)
	}

	t.Logf("Run timed out correctly: %s", run.Status.Message)
}

func TestCompletedRunTTLGCDeletesFinishedRun(t *testing.T) {
	t.Parallel()
	runtimeName := "bash-ttl-gc"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)
	ttlSeconds := int32(2)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-ttl-gc-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime:                 runtimeName,
			Mode:                    taskMode("echo ttl-gc"),
			TTLSecondsAfterFinished: &ttlSeconds,
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (ttl gc)", run.Name)

	waitForRun(t, run, 30*time.Second)
	if run.Status.CompletionTime == nil {
		t.Fatal("expected completion time before TTL GC")
	}
	waitForRunDeleted(t, run, 30*time.Second)
}

func TestRuntimedRecoversRunningRunAfterRestart(t *testing.T) {
	runtimeName := "bash-recovery"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-recovery-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("sleep 20; echo recovered"),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (runtimed recovery)", run.Name)

	waitForRunRunning(t, run, 30*time.Second)
	waitForRunCondition(t, run, runstatus.ConditionRuntimeAccepted, 30*time.Second)

	beforeRestart := runtimedRestartCount(t, run.Status.AssignedPod)
	killRuntimed(t, run.Status.AssignedPod)
	waitForRuntimedRestart(t, run.Status.AssignedPod, beforeRestart)

	waitForRun(t, run, 60*time.Second)
	if run.Status.Message != "execution completed" {
		t.Fatalf("success message = %q, want stable summary", run.Status.Message)
	}
}

func TestPythonInlineRun(t *testing.T) {
	t.Parallel()
	ensureRuntime(t, "python", pythonRuntimeImage(), 9092)

	inline := `print("hello from python")`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-py-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "python",
			Source:  &v1alpha1.CodeSource{Inline: &inline},
			Mode:    taskMode(),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Python Run %s", run.Name)
	waitForRun(t, run, 30*time.Second)
	t.Logf("Python Run completed successfully: %s", run.Status.Message)
}

func TestRunInvalidOutputsDoesNotRetry(t *testing.T) {
	t.Parallel()
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-invalid-outputs-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    taskMode(`printf 'invalid\n' > "$KRUNTIME_OUTPUTS"`),
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 3,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunFailed)
	assertOutputsFailure(t, run, "OutputsInvalid")
}

func TestRunOversizedOutputsDoesNotRetry(t *testing.T) {
	t.Parallel()
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-oversized-outputs-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode: taskMode(
				`printf 'value=' > "$KRUNTIME_OUTPUTS"; head -c 8193 /dev/zero | tr '\0' x >> "$KRUNTIME_OUTPUTS"`,
			),
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 3,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunFailed)
	assertOutputsFailure(t, run, "OutputsTooLarge")
}

func TestRunRetry(t *testing.T) {
	t.Parallel()
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	// Script that fails the first 2 times, succeeds on the 3rd.
	// Uses a counter file in the workspace (which persists across retries).
	inline := `#!/bin/bash
COUNTER_FILE=retry_count
if [ -f "$COUNTER_FILE" ]; then
  count=$(cat "$COUNTER_FILE")
else
  count=0
fi
count=$((count + 1))
echo "$count" > "$COUNTER_FILE"
if [ "$count" -lt 3 ]; then
  echo "attempt $count, failing intentionally"
  exit 1
fi
echo "succeeded on attempt $count"
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-retry-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Source:  &v1alpha1.CodeSource{Inline: &inline},
			Mode:    taskMode(),
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 5,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (retry test)", run.Name)
	waitForRun(t, run, 30*time.Second)
	if run.Status.Attempt < 3 {
		t.Fatalf("expected at least 3 attempts, got %d", run.Status.Attempt)
	}
	t.Logf("Run succeeded after %d attempts: %s", run.Status.Attempt, run.Status.Message)
}

func TestStaleRunNoRetry(t *testing.T) {
	t.Parallel()
	runtimeName := "bash-stale-no-retry"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-stale-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("sleep 300"),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (stale, no retry)", run.Name)

	// Wait for Run to be Running on a pod.
	waitForRunRunning(t, run, 30*time.Second)
	t.Logf("Run running on pod %s", run.Status.AssignedPod)

	// Delete the assigned pod.
	podName := run.Status.AssignedPod
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNamespace}}
	if err := k8sClient.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod %s: %v", podName, err)
	}
	t.Logf("Deleted pod %s", podName)

	// Wait for stale reaper to detect and fail the Run.
	var lastPhase v1alpha1.RunPhase
	waitFor(t, 60*time.Second, func() string {
		return fmt.Sprintf("stale detection for run %s (last phase=%s)", run.Name, lastPhase)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return false, err
		}
		if run.Status.Phase != lastPhase {
			t.Logf("Run %s: phase=%s, attempt=%d (pod=%s)", run.Name, run.Status.Phase, run.Status.Attempt, run.Status.AssignedPod)
			lastPhase = run.Status.Phase
		}
		switch run.Status.Phase {
		case v1alpha1.RunFailed:
			t.Logf("Run correctly marked Failed after pod deletion: %s", run.Status.Message)
			return true, nil
		case v1alpha1.RunSucceeded:
			t.Error("expected Failed, got Succeeded")
			return true, nil
		}
		return false, nil
	})
}

func TestStaleRunWithRetry(t *testing.T) {
	t.Parallel()
	runtimeName := "bash-stale-retry"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-stale-retry-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("sleep 300"),
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 3,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (stale, with retry)", run.Name)

	// Wait for Run to be Running.
	waitForRunRunning(t, run, 30*time.Second)
	t.Logf("Run running on pod %s", run.Status.AssignedPod)

	// Delete the assigned pod.
	podName := run.Status.AssignedPod
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNamespace}}
	if err := k8sClient.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod %s: %v", podName, err)
	}
	t.Logf("Deleted pod %s", podName)

	// Wait for stale reaper to reset for retry, then scheduler re-assigns.
	waitFor(t, 90*time.Second, func() string {
		return fmt.Sprintf("run %s to be retried (phase=%s attempt=%d)", run.Name, run.Status.Phase, run.Status.Attempt)
	}, func(ctx context.Context) (bool, error) {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return false, err
		}
		if run.Status.Phase == v1alpha1.RunRunning && run.Status.Attempt >= 1 {
			t.Logf("Run retried on pod %s (attempt=%d)", run.Status.AssignedPod, run.Status.Attempt)
			return true, nil
		}
		return false, nil
	})
}
