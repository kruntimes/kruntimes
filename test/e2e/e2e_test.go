package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const testNamespace = "default"

var k8sClient client.Client

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
	if err := k8sClient.Create(context.Background(), rt); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create runtime: %v", err)
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
				if p.Status.Phase == corev1.PodRunning {
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

func waitForRun(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
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
		case v1alpha1.RunSucceeded:
			return
		case v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
			t.Fatalf("Run failed: phase=%s, msg=%s (attempt=%d)", run.Status.Phase, run.Status.Message, run.Status.Attempt)
		}
	}
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
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

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

func TestSchedulerResponsiveness(t *testing.T) {
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

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

func TestPythonInlineRun(t *testing.T) {
	ensureRuntime(t, "python", "kruntimes-python-runtime:latest", 9092)

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
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

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
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

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
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

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
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

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
	ensureRuntime(t, runtimeName, "kruntimes-bash-runtime:latest", 9091)

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
	ensureRuntime(t, runtimeName, "kruntimes-bash-runtime:latest", 9091)

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
