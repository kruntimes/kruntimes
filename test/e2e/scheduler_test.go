package e2e

// Scheduler scenarios: placement, capacity, responsiveness, affinity, and cancellation.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestSchedulerResponsiveness(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-perf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    taskMode("echo hello"),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for {
		time.Sleep(200 * time.Millisecond)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}

		if run.Status.Phase != v1alpha1.RunPending {
			elapsed := time.Since(start)
			t.Logf("Run scheduled in %v (phase=%s, pod=%s)", elapsed, run.Status.Phase, run.Status.AssignedPod)
			return
		}

		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for scheduler to pick up run")
		default:
		}
	}
}

func TestSchedulerKeepsRunPendingWithoutRuntimePod(t *testing.T) {
	runtimeName := fmt.Sprintf("missing-runtime-%d", time.Now().UnixNano())
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-no-runtime-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("echo hello"),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), run) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}

		switch run.Status.Phase {
		case v1alpha1.RunFailed, v1alpha1.RunScheduled, v1alpha1.RunRunning, v1alpha1.RunSucceeded:
			t.Fatalf("expected run to stay Pending without runtime pods, got phase=%s pod=%s msg=%s",
				run.Status.Phase, run.Status.AssignedPod, run.Status.Message)
		}

		if run.Status.Phase == v1alpha1.RunPending &&
			run.Status.AssignedPod == "" &&
			strings.Contains(run.Status.Message, "waiting for available runtime pods") {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for pending run observation, phase=%s pod=%s msg=%s",
				run.Status.Phase, run.Status.AssignedPod, run.Status.Message)
		default:
		}
	}
}

func TestCancelPendingRunWithoutRuntimePod(t *testing.T) {
	runtimeName := fmt.Sprintf("missing-cancel-runtime-%d", time.Now().UnixNano())
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-cancel-pending-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("sleep 10"),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (pending cancel)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunPending && run.Status.Message != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for pending run, phase=%s msg=%s", run.Status.Phase, run.Status.Message)
		default:
		}
	}

	requestRunCancel(t, run)
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunCancelled)
	assertCancelledRun(t, run)
}

func TestCancelRunningRunDoesNotRetry(t *testing.T) {
	runtimeName := "bash-cancel-running"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-cancel-running-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("sleep 30; echo should_not_finish"),
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 3,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (running cancel)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for running run, phase=%s msg=%s", run.Status.Phase, run.Status.Message)
		default:
		}
	}

	requestRunCancel(t, run)
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunCancelled)
	assertCancelledRun(t, run)
	if run.Status.Attempt != 1 {
		t.Fatalf("cancelled run attempt = %d, want 1", run.Status.Attempt)
	}
}

func TestCancelNearCompletionHasStableTerminalPhase(t *testing.T) {
	runtimeName := "bash-cancel-boundary"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-cancel-boundary-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("sleep 1; echo boundary_done"),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (completion-boundary cancel)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		time.Sleep(100 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for running run, phase=%s msg=%s", run.Status.Phase, run.Status.Message)
		default:
		}
	}

	time.Sleep(900 * time.Millisecond)
	requestRunCancel(t, run)

	waitForAnyTerminalRunPhase(t, run, 30*time.Second)
	terminal := run.Status.Phase
	switch terminal {
	case v1alpha1.RunSucceeded:
	case v1alpha1.RunCancelled:
		assertCancelledRun(t, run)
	default:
		t.Fatalf("unexpected terminal phase after boundary cancel: %s msg=%s", terminal, run.Status.Message)
	}

	time.Sleep(2 * time.Second)
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
		t.Fatalf("get run after terminal: %v", err)
	}
	if run.Status.Phase != terminal {
		t.Fatalf("terminal phase changed from %s to %s", terminal, run.Status.Phase)
	}
}

func TestSchedulerReactivatesPendingRunWhenRuntimePodBecomesReady(t *testing.T) {
	runtimeName := fmt.Sprintf("scheduler-wakeup-%d", time.Now().UnixNano())
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-runtime-wakeup-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("echo runtime-ready"),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), run) }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get pending run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunPending &&
			strings.Contains(run.Status.Message, "waiting for available runtime pods") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for pending run observation, phase=%s msg=%s", run.Status.Phase, run.Status.Message)
		default:
		}
	}

	// The Runtime Pod create/ready events must reactivate this Run before the
	// scheduler's 30-second no-capacity polling fallback.
	started := time.Now()
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)
	waitForRun(t, run, 20*time.Second)
	if elapsed := time.Since(started); elapsed >= 30*time.Second {
		t.Fatalf("pending Run completed after %s, want Runtime Pod event wakeup before polling fallback", elapsed)
	}
}

func TestSchedulerKeepsRunPendingWhenRuntimeAtCapacity(t *testing.T) {
	runtimeName := "bash-capacity"
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	first := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-capacity-first-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("sleep 10; echo first"),
		},
	}
	if err := k8sClient.Create(context.Background(), first); err != nil {
		t.Fatalf("create first run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), first) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(first), first); err != nil {
			t.Fatalf("get first run: %v", err)
		}
		if first.Status.Phase == v1alpha1.RunRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for first run to start, phase=%s msg=%s", first.Status.Phase, first.Status.Message)
		default:
		}
	}

	second := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-capacity-second-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("echo second"),
		},
	}
	if err := k8sClient.Create(context.Background(), second); err != nil {
		t.Fatalf("create second run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), second) }()

	pendingCtx, pendingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pendingCancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(second), second); err != nil {
			t.Fatalf("get second run: %v", err)
		}
		if second.Status.Phase != "" && second.Status.Phase != v1alpha1.RunPending {
			t.Fatalf("expected second run to stay Pending while capacity is full, got phase=%s pod=%s msg=%s",
				second.Status.Phase, second.Status.AssignedPod, second.Status.Message)
		}
		if second.Status.AssignedPod != "" {
			t.Fatalf("expected second run to remain unassigned while capacity is full, got pod=%s", second.Status.AssignedPod)
		}
		select {
		case <-pendingCtx.Done():
			goto capacityObserved
		default:
		}
	}

capacityObserved:
	waitForRun(t, first, 20*time.Second)
	waitForRun(t, second, 30*time.Second)
}

func TestSchedulerInterRunAffinityBootstrap(t *testing.T) {
	runtimeName := "bash-affinity"
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 2)
	affinity := &v1alpha1.RunAffinity{RunAffinity: &v1alpha1.RunAffinityRules{
		RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{{
			TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"cohort": "build"}},
		}},
	}}
	first := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-affinity-first-", Namespace: testNamespace, Labels: map[string]string{"cohort": "build"}},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Affinity: affinity, Mode: taskMode("sleep 5; echo first")},
	}
	if err := k8sClient.Create(context.Background(), first); err != nil {
		t.Fatalf("create first affinity run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), first) }()
	waitForRunPhase(t, first, 20*time.Second, v1alpha1.RunRunning)

	second := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-affinity-second-", Namespace: testNamespace, Labels: map[string]string{"cohort": "build"}},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Affinity: affinity, Mode: taskMode("sleep 1; echo second")},
	}
	if err := k8sClient.Create(context.Background(), second); err != nil {
		t.Fatalf("create second affinity run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), second) }()
	waitForRun(t, second, 20*time.Second)
	if second.Status.AssignedPod != first.Status.AssignedPod {
		t.Fatalf("second Run assigned to %q, want affinity target %q", second.Status.AssignedPod, first.Status.AssignedPod)
	}
	waitForRun(t, first, 20*time.Second)
}
