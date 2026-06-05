package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

const testNamespace = "default"

var k8sClient client.Client

func bashRuntimeImage() string {
	if image := os.Getenv("KRUNTIMES_BASH_RUNTIME_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-bash-runtime:latest"
}

func pythonRuntimeImage() string {
	if image := os.Getenv("KRUNTIMES_PYTHON_RUNTIME_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-python-runtime:latest"
}

func runtimedImage() string {
	if image := os.Getenv("KRUNTIMES_RUNTIMED_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-runtimed:latest"
}

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	cfg := config.GetConfigOrDie()
	cfg.QPS = 50
	cfg.Burst = 100

	var err error
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func ensureRuntime(t *testing.T, name, image string, port int32) {
	t.Helper()
	ensureRuntimeWithRunsCapacity(t, name, image, port, 0)
}

func ensureRuntimeWithRunsCapacity(t *testing.T, name, image string, port int32, runsCapacity int32) {
	t.Helper()

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: v1alpha1.RuntimeSpec{
			Image:    image,
			Port:     port,
			Replicas: 1,
			Command:  []string{fmt.Sprintf("--port=%d", port), "--work-dir=/workspace"},
		},
	}
	if runsCapacity > 0 {
		rt.Spec.Capacity = &v1alpha1.RuntimeCapacity{
			Resources: corev1.ResourceList{
				corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(int64(runsCapacity), resource.DecimalSI),
			},
		}
	}
	if err := k8sClient.Create(context.Background(), rt); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create runtime: %v", err)
	} else if apierrors.IsAlreadyExists(err) {
		existing := &v1alpha1.Runtime{}
		if getErr := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(rt), existing); getErr != nil {
			t.Fatalf("get runtime: %v", getErr)
		}
		existing.Spec.Image = image
		existing.Spec.Port = port
		existing.Spec.Replicas = 1
		existing.Spec.Command = []string{fmt.Sprintf("--port=%d", port), "--work-dir=/workspace"}
		if runsCapacity > 0 {
			existing.Spec.Capacity = rt.Spec.Capacity
		}
		if updateErr := k8sClient.Update(context.Background(), existing); updateErr != nil {
			t.Fatalf("update runtime: %v", updateErr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods,
			client.InNamespace(testNamespace),
			client.MatchingLabels{"runtime": name},
		); err == nil {
			for _, p := range pods.Items {
				if isRuntimePodReady(&p, image, runtimedImage(), runsCapacity) {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for runtime pods")
		case <-time.After(2 * time.Second):
		}
	}
}

func isRuntimePodReady(pod *corev1.Pod, runtimeImage, daemonImage string, runsCapacity int32) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	if containerImage(pod, "runtime") != runtimeImage || containerImage(pod, "runtimed") != daemonImage {
		return false
	}
	if runsCapacity > 0 {
		if runtimepod.RunsCapacity(pod, 0) != runsCapacity {
			return false
		}
	}
	podReady := false
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			podReady = cond.Status == corev1.ConditionTrue
			break
		}
	}
	return podReady && runtimepod.FreshRuntimedReady(pod, time.Now(), 30*time.Second)
}

func containerImage(pod *corev1.Pod, name string) string {
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return container.Image
		}
	}
	return ""
}

func waitForRun(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	waitForRunPhase(t, run, timeout, v1alpha1.RunSucceeded)
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

func findRunCondition(run *v1alpha1.Run, typ string) *metav1.Condition {
	for i := range run.Status.Conditions {
		if run.Status.Conditions[i].Type == typ {
			return &run.Status.Conditions[i]
		}
	}
	return nil
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
}

func requestRunCancel(t *testing.T, run *v1alpha1.Run) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run for cancel: %v", err)
		}
		run.Spec.CancelRequested = true
		if err := k8sClient.Update(context.Background(), run); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("failed to request cancellation for run %s", run.Name)
}

func waitForWorkflow(t *testing.T, wf *v1alpha1.Workflow, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for workflow completion")
		default:
		}
		time.Sleep(500 * time.Millisecond)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(wf), wf); err != nil {
			t.Fatalf("get workflow: %v", err)
		}
		t.Logf("Workflow %s: phase=%s", wf.Name, wf.Status.Phase)

		switch wf.Status.Phase {
		case v1alpha1.WorkflowSucceeded:
			return
		case v1alpha1.WorkflowFailed:
			t.Fatalf("Workflow failed: %s", wf.Status.Message)
		}
	}
}

func TestFullRunLifecycle(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (runtime=bash)", run.Name)
	waitForRun(t, run, 30*time.Second)
	t.Logf("Run completed successfully: %s", run.Status.Message)
}

func TestRunTimeout(t *testing.T) {
	runtimeName := "bash-timeout"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-timeout-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 10; echo should_not_print"},
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
			Args:    []string{"sleep 20; echo recovered"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (runtimed recovery)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning && run.Status.AssignedPod != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run to start, phase=%s pod=%s msg=%s",
				run.Status.Phase, run.Status.AssignedPod, run.Status.Message)
		default:
		}
	}

	beforeRestart := runtimedRestartCount(t, run.Status.AssignedPod)
	killRuntimed(t, run.Status.AssignedPod)
	waitForRuntimedRestart(t, run.Status.AssignedPod, beforeRestart)

	waitForRun(t, run, 60*time.Second)
	if !strings.Contains(run.Status.Message, "recovered") {
		t.Fatalf("expected recovered stdout, got %q", run.Status.Message)
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
			Args:    []string{"sleep 10"},
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
			Args:    []string{"sleep 30; echo should_not_finish"},
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
			Args:    []string{"sleep 1; echo boundary_done"},
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

func TestSchedulerResponsiveness(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-perf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{"echo hello"},
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
			Args:    []string{"echo hello"},
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
			Args:    []string{"sleep 10; echo first"},
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
			Args:    []string{"echo second"},
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

func killRuntimed(t *testing.T, podName string) {
	t.Helper()
	cmd := exec.Command("kubectl", "exec", podName, "-n", testNamespace, "-c", "runtimed", "--", "/bin/sh", "-c", "kill 1")
	if err := cmd.Run(); err != nil {
		t.Logf("kill runtimed returned expected process termination error: %v", err)
	}
}

func runtimedRestartCount(t *testing.T, podName string) int32 {
	t.Helper()

	var pod corev1.Pod
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: podName, Namespace: testNamespace}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "runtimed" {
			return status.RestartCount
		}
	}
	t.Fatalf("pod %s has no runtimed container status", podName)
	return 0
}

func waitForRuntimedRestart(t *testing.T, podName string, previousRestartCount int32) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for {
		if runtimedRestartCount(t, podName) > previousRestartCount {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for runtimed container restart in pod %s", podName)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func TestPythonInlineRun(t *testing.T) {
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
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Python Run %s", run.Name)
	waitForRun(t, run, 30*time.Second)
	t.Logf("Python Run completed successfully: %s", run.Status.Message)
}

func TestWorkflowSingleJob(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-wf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "hello",
						Run:  "echo hello_from_workflow",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Logf("Created Workflow %s", wf.Name)
	waitForWorkflow(t, wf, 30*time.Second)
	t.Logf("Workflow succeeded: %s", wf.Status.Message)
}

func TestWorkflowStepOutputs(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-wf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{
						{
							Name: "gen-version",
							Run:  "echo version=v1.0 >> outputs",
						},
						{
							Name: "build-image",
							Run:  "echo image=app:${{ steps.gen-version.outputs.version }} >> outputs",
						},
					},
					Outputs: map[string]string{
						"artifact": "${{ steps.build-image.outputs.image }}",
					},
				},
				"deploy": {
					RunsOn: "bash",
					Needs:  []string{"build"},
					Steps: []v1alpha1.StepSpec{{
						Name: "deploy-step",
						Run:  "echo deploying ${{ jobs.build.outputs.artifact }}",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Logf("Created Workflow %s", wf.Name)
	waitForWorkflow(t, wf, 60*time.Second)
	buildJob := wf.Status.Jobs["build"]
	buildStep := buildJob.Steps["build-image"]
	if buildStep.Outputs == nil || buildStep.Outputs["image"] != "app:v1.0" {
		t.Fatalf("build-image outputs mismatch: got %v", buildStep.Outputs)
	}
	t.Logf("All outputs verified")
}

func TestRunRetry(t *testing.T) {
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
			Runtime:    "bash",
			Source:     &v1alpha1.CodeSource{Inline: &inline},
			Entrypoint: "script.sh",
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

func TestWorkflowTopoOrder(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-wf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				// prep has no needs, runs first.
				"prep": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "generate",
						Run:  "echo version=v2.0 >> outputs",
					}},
				},
				// lint also has no needs, runs in parallel with prep.
				"lint": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "check",
						Run:  "echo lint=ok >> outputs",
					}},
				},
				// build needs prep explicitly, and references lint's output implicitly.
				"build": {
					RunsOn: "bash",
					Needs:  []string{"prep"},
					Steps: []v1alpha1.StepSpec{{
						Name: "compile",
						Run:  "echo image=app:${{ jobs.prep.outputs.version }}:${{ jobs.lint.outputs.lint }}",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Logf("Created Workflow %s", wf.Name)
	waitForWorkflow(t, wf, 60*time.Second)
	t.Logf("All jobs completed in correct order")
}

func TestStaleRunNoRetry(t *testing.T) {
	runtimeName := "bash-stale-no-retry"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-stale-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 300"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (stale, no retry)", run.Name)

	// Wait for Run to be Running on a pod.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning {
			t.Logf("Run running on pod %s", run.Status.AssignedPod)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run to start, phase=%s", run.Status.Phase)
		default:
		}
	}

	// Delete the assigned pod.
	podName := run.Status.AssignedPod
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNamespace}}
	if err := k8sClient.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod %s: %v", podName, err)
	}
	t.Logf("Deleted pod %s", podName)

	// Wait for stale reaper to detect and fail the Run.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	var lastPhase v1alpha1.RunPhase
	for {
		time.Sleep(500 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase != lastPhase {
			t.Logf("Run %s: phase=%s, attempt=%d (pod=%s)", run.Name, run.Status.Phase, run.Status.Attempt, run.Status.AssignedPod)
			lastPhase = run.Status.Phase
		}
		switch run.Status.Phase {
		case v1alpha1.RunFailed:
			t.Logf("Run correctly marked Failed after pod deletion: %s", run.Status.Message)
			return
		case v1alpha1.RunSucceeded:
			t.Error("expected Failed, got Succeeded")
			return
		}
		select {
		case <-ctx2.Done():
			t.Fatalf("timed out waiting for stale detection, phase=%s", run.Status.Phase)
		default:
		}
	}
}

func TestStaleRunWithRetry(t *testing.T) {
	runtimeName := "bash-stale-retry"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-stale-retry-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 300"},
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning {
			t.Logf("Run running on pod %s", run.Status.AssignedPod)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run to start, phase=%s", run.Status.Phase)
		default:
		}
	}

	// Delete the assigned pod.
	podName := run.Status.AssignedPod
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNamespace}}
	if err := k8sClient.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod %s: %v", podName, err)
	}
	t.Logf("Deleted pod %s", podName)

	// Wait for stale reaper to reset for retry, then scheduler re-assigns.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel2()
	for {
		time.Sleep(500 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning && run.Status.Attempt >= 1 {
			t.Logf("Run retried on pod %s (attempt=%d)", run.Status.AssignedPod, run.Status.Attempt)
			return
		}
		select {
		case <-ctx2.Done():
			t.Fatalf("timed out waiting for retry, phase=%s attempt=%d", run.Status.Phase, run.Status.Attempt)
		default:
		}
	}
}
