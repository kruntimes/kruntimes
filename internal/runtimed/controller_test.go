package runtimed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
)

func TestPrepareSource_NoSource(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	run := &v1alpha1.Run{}
	run.UID = "test-uid"
	workDir, err := prepareSource(run)
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(dir, string(run.UID))
	if workDir != expected {
		t.Errorf("expected %s, got %s", expected, workDir)
	}
	if _, err := os.Stat(workDir); err != nil {
		t.Errorf("workDir not created: %v", err)
	}
}

func TestPrepareSource_Inline(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "#!/bin/bash\necho hello"
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Entrypoint: "run.sh",
			Source:     &v1alpha1.CodeSource{Inline: &inline},
		},
	}
	run.UID = "test-uid"

	workDir, err := prepareSource(run)
	if err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(workDir, "run.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if string(data) != inline {
		t.Errorf("expected %q, got %q", inline, string(data))
	}
}

func TestPrepareSource_InlineDefaultEntrypoint(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "echo default"
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Source: &v1alpha1.CodeSource{Inline: &inline},
		},
	}
	run.UID = "test-uid"

	workDir, err := prepareSource(run)
	if err != nil {
		t.Fatal(err)
	}

	scriptPath := filepath.Join(workDir, "script")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Errorf("expected default 'script' file: %v", err)
	}
}

func TestPrepareSource_RejectsEscapingEntrypoint(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "echo escape"
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Entrypoint: "../escape.sh",
			Source:     &v1alpha1.CodeSource{Inline: &inline},
		},
	}
	run.UID = "test-uid"

	if _, err := prepareSource(run); err == nil || !strings.Contains(err.Error(), "entrypoint") {
		t.Fatalf("prepareSource error = %v, want entrypoint validation error", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.sh")); !os.IsNotExist(err) {
		t.Fatalf("escape target should not exist, stat err = %v", err)
	}
}

func TestReadOutputs_Empty(t *testing.T) {
	outputs, err := readOutputs("")
	if err != nil {
		t.Fatalf("readOutputs: %v", err)
	}
	if outputs != nil {
		t.Error("expected nil for empty workingDir")
	}
}

func TestReadOutputs_Nonexistent(t *testing.T) {
	outputs, err := readOutputs(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("readOutputs: %v", err)
	}
	if outputs != nil {
		t.Error("expected nil for nonexistent file")
	}
}

func TestReadOutputs_Valid(t *testing.T) {
	dir := t.TempDir()
	content := "key1=val1\nkey2=val2\n# comment\nkey3 = val3\n"
	path := filepath.Join(dir, "outputs")
	_ = os.WriteFile(path, []byte(content), 0o644)

	outputs, err := readOutputs(path)
	if err != nil {
		t.Fatalf("readOutputs: %v", err)
	}
	if len(outputs) != 3 {
		t.Fatalf("expected 3 outputs, got %d: %v", len(outputs), outputs)
	}
	if outputs["key1"] != "val1" {
		t.Errorf("key1: expected val1, got %s", outputs["key1"])
	}
	if outputs["key2"] != "val2" {
		t.Errorf("key2: expected val2, got %s", outputs["key2"])
	}
	if outputs["key3"] != "val3" {
		t.Errorf("key3: expected val3, got %s", outputs["key3"])
	}
}

func TestReadOutputs_RejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outputs")
	_ = os.WriteFile(path, []byte("no_equal_sign\n"), 0o644)

	if _, err := readOutputs(path); err == nil || isOutputsTooLarge(err) {
		t.Fatalf("readOutputs error = %v, want invalid outputs error", err)
	}
}

func TestReadOutputs_DuplicateKeyLastWins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outputs")
	_ = os.WriteFile(path, []byte("version=v1\nversion=v2\n"), 0o644)

	outputs, err := readOutputs(path)
	if err != nil {
		t.Fatalf("readOutputs: %v", err)
	}
	if got := outputs["version"]; got != "v2" {
		t.Fatalf("version = %q, want v2", got)
	}
}

func TestReadOutputs_Limits(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{
			name:    "invalid utf8",
			content: []byte{'k', '=', 0xff, '\n'},
		},
		{
			name:    "key too large",
			content: []byte(strings.Repeat("k", artifact.MaxOutputKeyBytes+1) + "=v\n"),
		},
		{
			name:    "value too large",
			content: []byte("k=" + strings.Repeat("v", artifact.MaxOutputValueBytes+1) + "\n"),
		},
		{
			name: "too many keys",
			content: []byte(func() string {
				var b strings.Builder
				for i := 0; i <= artifact.MaxOutputKeys; i++ {
					b.WriteString("key")
					b.WriteString(strings.Repeat("x", i))
					b.WriteString("=v\n")
				}
				return b.String()
			}()),
		},
		{
			name: "total too large",
			content: []byte(func() string {
				var b strings.Builder
				for i := 0; i < 9; i++ {
					b.WriteString("key")
					b.WriteByte(byte('a' + i))
					b.WriteByte('=')
					b.WriteString(strings.Repeat("v", artifact.MaxOutputValueBytes))
					b.WriteByte('\n')
				}
				return b.String()
			}()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "outputs")
			if err := os.WriteFile(path, tt.content, 0o644); err != nil {
				t.Fatalf("write outputs: %v", err)
			}
			_, err := readOutputs(path)
			if err == nil {
				t.Fatal("readOutputs succeeded, want error")
			}
			if tt.name == "invalid utf8" {
				if isOutputsTooLarge(err) {
					t.Fatalf("error = %v, want invalid outputs", err)
				}
			} else if !isOutputsTooLarge(err) {
				t.Fatalf("error = %v, want outputs too large", err)
			}
		})
	}
}

func TestStatusAdapter(t *testing.T) {
	var _ = (*statusAdapter)(nil)
}

func TestTerminalPhaseForFailure(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		expected v1alpha1.RunPhase
	}{
		{"timeout", runretry.ReasonTimeout, v1alpha1.RunTimeout},
		{"runtime_error", runretry.ReasonRuntimeError, v1alpha1.RunFailed},
		{"prepare_source", runretry.ReasonPrepareSource, v1alpha1.RunFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := terminalPhaseForFailure(tt.reason); got != tt.expected {
				t.Fatalf("terminalPhaseForFailure(%s) = %s, want %s", tt.reason, got, tt.expected)
			}
		})
	}
}

func TestHandleFailure_NoRetry(t *testing.T) {
	// When maxAttempts=1 (default), handleFailure should call finishRun directly.
	// This test verifies the logic through shouldRetry.
	p := runretry.WithDefaults(nil) // maxAttempts=1
	// First execution (attempt=0 → curAttempt=1)
	if runretry.ShouldRetry(p, 1, runretry.ReasonRuntimeError) {
		t.Error("should not retry when maxAttempts=1")
	}
}

func TestHandleFailure_RetryAndBackoff(t *testing.T) {
	p := runretry.WithDefaults(&v1alpha1.RetryPolicy{
		MaxAttempts: 3,
		Backoff:     metav1.Duration{Duration: time.Second},
	})

	// First execution fails (attempt 1)
	if !runretry.ShouldRetry(p, 1, runretry.ReasonRuntimeError) {
		t.Error("should retry on attempt 1 of 3")
	}
	// Second execution fails (attempt 2)
	if !runretry.ShouldRetry(p, 2, runretry.ReasonRuntimeError) {
		t.Error("should retry on attempt 2 of 3")
	}
	// Third execution fails (attempt 3) — maxAttempts reached
	if runretry.ShouldRetry(p, 3, runretry.ReasonRuntimeError) {
		t.Error("should not retry on attempt 3 of 3")
	}

	// Verify backoff for each attempt
	if d := runretry.Backoff(p, 2); d != time.Second {
		t.Errorf("attempt 2 backoff: expected 1s, got %v", d)
	}
	if d := runretry.Backoff(p, 3); d != 2*time.Second {
		t.Errorf("attempt 3 backoff: expected 2s, got %v", d)
	}
}

func TestReconcileScheduledRespectsLocalCapacity(t *testing.T) {
	c := &Controller{Workers: 1}
	c.activeRuns.Store("existing", &activeRun{})

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name: "queued",
			UID:  "queued-uid",
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunScheduled},
	}

	result, err := c.reconcileScheduled(t.Context(), run)
	if err != nil {
		t.Fatalf("reconcileScheduled: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("RequeueAfter = %s, want 1s", result.RequeueAfter)
	}
	if run.Status.Phase != v1alpha1.RunScheduled {
		t.Fatalf("phase = %s, want Scheduled", run.Status.Phase)
	}
}

func TestReconcileScheduledCancelBeforeClaim(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scheduled-cancel",
			Namespace: "default",
			UID:       "scheduled-cancel-uid",
		},
		Spec: v1alpha1.RunSpec{
			Runtime:         "bash",
			CancelRequested: true,
		},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunScheduled,
			AssignedPod: "pod-a",
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	c := &Controller{
		Client:     k8sClient,
		Hostname:   "pod-a",
		Recorder:   nil,
		runtimeCli: &fakeRuntimeClient{},
	}

	if _, err := c.reconcileScheduled(t.Context(), run); err != nil {
		t.Fatalf("reconcileScheduled: %v", err)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("phase = %s, want Cancelled", updated.Status.Phase)
	}
	if c.activeRunCount() != 0 {
		t.Fatalf("activeRunCount = %d, want 0", c.activeRunCount())
	}
}

func TestReconcileRunningRecoversMissingActiveRun(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running",
			Namespace: "default",
			UID:       "run-uid",
		},
		Spec: v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "pod-a",
			StartTime:   &metav1.Time{Time: time.Now().Add(-time.Second)},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	c := &Controller{
		Client:     k8sClient,
		Hostname:   "pod-a",
		runtimeCli: &fakeRuntimeClient{status: &pb.StatusResponse{Id: "run-uid", State: pb.ExecutionState_EXECUTION_STATE_SUCCEEDED, Stdout: "done"}},
	}

	result, err := c.reconcileRunning(t.Context(), run)
	if err != nil {
		t.Fatalf("reconcileRunning: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue: %s", result.RequeueAfter)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}
	if updated.Status.Message != "execution completed" {
		t.Fatalf("message = %q, want execution completed", updated.Status.Message)
	}
}

func TestApplySuccessRejectsInvalidOutputsWithoutRetry(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	workDir := t.TempDir()
	if err := os.WriteFile(outputsPath(workDir), []byte("invalid\n"), 0o644); err != nil {
		t.Fatalf("write outputs: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-outputs", Namespace: "default", UID: "invalid-outputs-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime:     "bash",
			RetryPolicy: &v1alpha1.RetryPolicy{MaxAttempts: 3},
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: "pod-a"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	c := &Controller{Client: k8sClient, Hostname: "pod-a"}
	ar := &activeRun{run: run, workDir: workDir, start: time.Now()}
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.applySuccess(t.Context(), ar, &pb.StatusResponse{Stdout: "done"}); err != nil {
		t.Fatalf("applySuccess: %v", err)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if updated.Status.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", updated.Status.Attempt)
	}
	completed := meta.FindStatusCondition(updated.Status.Conditions, "Completed")
	if completed == nil || completed.Reason != reasonOutputsInvalid {
		t.Fatalf("Completed condition = %#v, want reason %s", completed, reasonOutputsInvalid)
	}
	if c.activeRunCount() != 0 {
		t.Fatalf("active runs = %d, want 0", c.activeRunCount())
	}
}

func TestApplySuccessEmitsStructuredOutputAfterStatusUpdate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "logged", Namespace: "team-a", UID: "logged-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: "pod-a"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	var output bytes.Buffer
	c := &Controller{Client: k8sClient, Hostname: "pod-a", ExecutionLogWriter: &output}
	ar := &activeRun{run: run, workDir: t.TempDir(), start: time.Now()}
	largeStdout := "stdout-secret-" + strings.Repeat("x", maxStatusMessageBytes*2)

	if _, err := c.applySuccess(t.Context(), ar, &pb.StatusResponse{
		Stdout: largeStdout + "\nworld\n",
		Stderr: "warning\n",
	}); err != nil {
		t.Fatalf("applySuccess: %v", err)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status.Message != "execution completed" || strings.Contains(updated.Status.Message, "stdout-secret") {
		t.Fatalf("status message contains stdout: %q", updated.Status.Message)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("log lines = %d, want 3: %q", len(lines), output.String())
	}
	for i, line := range lines {
		var entry executionLogLine
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("unmarshal line %d: %v", i, err)
		}
		if entry.RunUID != "logged-uid" || entry.RunName != "logged" || entry.Namespace != "team-a" ||
			entry.Runtime != "bash" || entry.Pod != "pod-a" {
			t.Fatalf("unexpected metadata on line %d: %#v", i, entry)
		}
	}
}

func TestApplyFailureBoundsStatusMessageAndLogsFullStderr(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "failed", Namespace: "team-a", UID: "failed-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: "pod-a"},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	var output bytes.Buffer
	c := &Controller{Client: k8sClient, Hostname: "pod-a", ExecutionLogWriter: &output}
	ar := &activeRun{run: run, workDir: t.TempDir(), start: time.Now()}
	stderr := strings.Repeat("early-", 400) + "useful-tail"
	resp := &pb.StatusResponse{Stderr: stderr}

	if _, err := c.applyFailureWithOutput(
		t.Context(),
		ar,
		runretry.ReasonRuntimeError,
		summarizeRuntimeFailure(resp),
		outputFromStatus(resp),
	); err != nil {
		t.Fatalf("applyFailureWithOutput: %v", err)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if len(updated.Status.Message) > maxStatusMessageBytes {
		t.Fatalf("status message is %d bytes, want <= %d", len(updated.Status.Message), maxStatusMessageBytes)
	}
	if !strings.HasPrefix(updated.Status.Message, "runtime stderr: ... ") ||
		!strings.HasSuffix(updated.Status.Message, "useful-tail") {
		t.Fatalf("status message does not contain bounded stderr tail: %q", updated.Status.Message)
	}
	if strings.Contains(updated.Status.Message, strings.Repeat("early-", 200)) {
		t.Fatal("status message contains unbounded stderr")
	}
	if !strings.Contains(output.String(), stderr) {
		t.Fatal("structured log does not contain full stderr")
	}
}

func TestSummarizeRuntimeFailureCapsErrorMessage(t *testing.T) {
	message := "runtime-error-" + strings.Repeat("x", maxStatusMessageBytes*2)
	got := summarizeRuntimeFailure(&pb.StatusResponse{
		ErrorMessage: message,
		Stderr:       strings.Repeat("stderr", maxStatusMessageBytes),
	})
	if len(got) > maxStatusMessageBytes {
		t.Fatalf("message is %d bytes, want <= %d", len(got), maxStatusMessageBytes)
	}
	if !strings.HasPrefix(got, "runtime-error-") || !strings.HasSuffix(got, "...") {
		t.Fatalf("unexpected bounded error message: %q", got)
	}
	if strings.Contains(got, "stderr") {
		t.Fatalf("error message unexpectedly contains stderr: %q", got)
	}
}

func TestReconcileRunningFailsWhenRecoveredExecutionMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing",
			Namespace: "default",
			UID:       "missing-uid",
		},
		Spec: v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "pod-a",
			StartTime:   &metav1.Time{Time: time.Now().Add(-time.Second)},
		},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	c := &Controller{
		Client:     k8sClient,
		Hostname:   "pod-a",
		runtimeCli: &fakeRuntimeClient{statusErr: status.Error(codes.NotFound, "execution not found")},
	}

	if _, err := c.reconcileRunning(t.Context(), run); err != nil {
		t.Fatalf("reconcileRunning: %v", err)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if updated.Status.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", updated.Status.Attempt)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, "Running")
	if condition == nil || condition.Reason != runretry.ReasonExecutionLost {
		t.Fatalf("Running condition = %#v, want reason %s", condition, runretry.ReasonExecutionLost)
	}
}

func TestReconcileRunningRecoveredKeepsRunActiveOnTransientStatusError(t *testing.T) {
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "transient",
			Namespace: "default",
			UID:       "transient-uid",
		},
		Spec: v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "pod-a",
			StartTime:   &metav1.Time{Time: time.Now().Add(-time.Second)},
		},
	}
	c := &Controller{
		Hostname:   "pod-a",
		runtimeCli: &fakeRuntimeClient{statusErr: status.Error(codes.Unavailable, "runtime unavailable")},
	}

	if _, err := c.reconcileRunningRecovered(t.Context(), run); status.Code(err) != codes.Unavailable {
		t.Fatalf("reconcileRunningRecovered error = %v, want Unavailable", err)
	}
	if run.Status.Phase != v1alpha1.RunRunning {
		t.Fatalf("phase = %s, want Running", run.Status.Phase)
	}
	if run.Status.Attempt != 0 {
		t.Fatalf("attempt = %d, want unchanged", run.Status.Attempt)
	}
	if _, ok := c.activeRuns.Load(string(run.UID)); !ok {
		t.Fatal("transient Status error should keep the recovered Run under active monitoring")
	}
}

func TestReconcileRunningActiveDistinguishesStatusErrors(t *testing.T) {
	tests := []struct {
		name       string
		statusErr  error
		wantErr    codes.Code
		wantPhase  v1alpha1.RunPhase
		wantReason string
	}{
		{
			name:      "transient",
			statusErr: status.Error(codes.Unavailable, "runtime unavailable"),
			wantErr:   codes.Unavailable,
			wantPhase: v1alpha1.RunRunning,
		},
		{
			name:       "execution lost",
			statusErr:  status.Error(codes.NotFound, "execution not found"),
			wantErr:    codes.OK,
			wantPhase:  v1alpha1.RunFailed,
			wantReason: runretry.ReasonExecutionLost,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			if err := v1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("add scheme: %v", err)
			}
			run := &v1alpha1.Run{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "active",
					Namespace: "default",
					UID:       "active-uid",
				},
				Spec: v1alpha1.RunSpec{Runtime: "bash"},
				Status: v1alpha1.RunStatus{
					Phase:       v1alpha1.RunRunning,
					AssignedPod: "pod-a",
					StartTime:   &metav1.Time{Time: time.Now().Add(-time.Second)},
				},
			}
			k8sClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&v1alpha1.Run{}).
				WithObjects(run).
				Build()
			c := &Controller{
				Client:     k8sClient,
				Hostname:   "pod-a",
				runtimeCli: &fakeRuntimeClient{statusErr: tt.statusErr},
			}
			ar := c.addRecoveredRun(run)

			_, err := c.reconcileRunningActive(t.Context(), ar)
			if status.Code(err) != tt.wantErr {
				t.Fatalf("error = %v, want code %s", err, tt.wantErr)
			}

			var updated v1alpha1.Run
			if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
				t.Fatalf("get run: %v", err)
			}
			if updated.Status.Phase != tt.wantPhase {
				t.Fatalf("phase = %s, want %s", updated.Status.Phase, tt.wantPhase)
			}
			condition := meta.FindStatusCondition(updated.Status.Conditions, "Running")
			if tt.wantReason == "" {
				if condition != nil {
					t.Fatalf("Running condition changed on transient error: %#v", condition)
				}
			} else if condition == nil || condition.Reason != tt.wantReason {
				t.Fatalf("Running condition = %#v, want reason %s", condition, tt.wantReason)
			}
		})
	}
}

func TestRecoverActiveRunsOnceAddsRuntimeExecutions(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "default")

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "recover",
			Namespace: "default",
			UID:       "recover-uid",
		},
		Spec: v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "pod-a",
			StartTime:   &metav1.Time{Time: time.Now().Add(-time.Second)},
		},
	}
	otherPodRun := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", UID: "other-uid"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: "pod-b"},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(run, otherPodRun).
		Build()
	c := &Controller{
		Client:   k8sClient,
		Hostname: "pod-a",
		runtimeCli: &fakeRuntimeClient{list: &pb.ListResponse{Entries: []*pb.StatusResponse{
			{Id: "recover-uid", State: pb.ExecutionState_EXECUTION_STATE_RUNNING},
			{Id: "orphan-runtime-execution", State: pb.ExecutionState_EXECUTION_STATE_RUNNING},
		}}},
	}

	c.recoverActiveRunsOnce(t.Context())

	if c.activeRunCount() != 1 {
		t.Fatalf("activeRunCount = %d, want 1", c.activeRunCount())
	}
	if _, ok := c.activeRuns.Load("recover-uid"); !ok {
		t.Fatal("expected recover-uid to be active")
	}
	if _, ok := c.activeRuns.Load("other-uid"); ok {
		t.Fatal("did not expect other pod run to be active")
	}
}

func TestStartExecutionWaitsForRuntimeReady(t *testing.T) {
	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{runtimeCli: runtimeClient}
	ar := &activeRun{
		run: &v1alpha1.Run{
			ObjectMeta: metav1.ObjectMeta{UID: "run-uid"},
			Spec:       v1alpha1.RunSpec{Runtime: "python"},
		},
		workDir: t.TempDir(),
	}

	if err := c.startExecution(t.Context(), ar); err != nil {
		t.Fatalf("startExecution: %v", err)
	}
	if runtimeClient.executeOptions == 0 {
		t.Fatal("expected Execute to wait for the runtime connection to become ready")
	}
	if got := runtimeClient.executeRequest.Env[artifact.OutputsEnv]; got != outputsPath(ar.workDir) {
		t.Fatalf("%s = %q, want %q", artifact.OutputsEnv, got, outputsPath(ar.workDir))
	}
}

func TestStartExecutionInjectsReservedArtifactDirectory(t *testing.T) {
	setTestWorkspace(t)
	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{
		runtimeCli:    runtimeClient,
		ArtifactStore: &fakeArtifactStore{},
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: "run-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime: "python",
			Env: []corev1.EnvVar{{
				Name:  artifact.ArtifactsDirEnv,
				Value: "/user/override",
			}},
		},
	}
	ar := &activeRun{run: run, workDir: filepath.Join(workspacePath, "run-uid")}

	if err := c.startExecution(t.Context(), ar); err != nil {
		t.Fatalf("startExecution: %v", err)
	}
	want := filepath.Join(workspacePath, "run-uid", "artifacts")
	if got := runtimeClient.executeRequest.Env[artifact.ArtifactsDirEnv]; got != want {
		t.Fatalf("%s = %q, want %q", artifact.ArtifactsDirEnv, got, want)
	}
}

func TestCleanupForgetsExecutionAndRemovesWorkspace(t *testing.T) {
	setTestWorkspace(t)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup", Namespace: "default", UID: "cleanup-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
	}
	runWorkspace := workspaceForRun(run)
	if err := os.MkdirAll(runWorkspace, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runWorkspace, "retained"), []byte("data"), 0o600); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{runtimeCli: runtimeClient}
	ar := &activeRun{run: run, workDir: runWorkspace, start: time.Now()}
	c.activeRuns.Store(string(run.UID), ar)

	c.cleanup(t.Context(), ar, v1alpha1.RunSucceeded)

	if len(runtimeClient.forgetRequests) != 1 || runtimeClient.forgetRequests[0].Id != string(run.UID) {
		t.Fatalf("Forget requests = %#v, want cleanup-uid", runtimeClient.forgetRequests)
	}
	if _, err := os.Stat(runWorkspace); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists or stat failed unexpectedly: %v", err)
	}
	if c.activeRunCount() != 0 {
		t.Fatalf("active runs = %d, want 0", c.activeRunCount())
	}
}

type fakeRuntimeClient struct {
	pb.RuntimeClient
	status         *pb.StatusResponse
	statusErr      error
	list           *pb.ListResponse
	listErr        error
	forgetErr      error
	forgetRequests []*pb.ForgetRequest
	executeOptions int
	executeRequest *pb.ExecuteRequest
}

func (f *fakeRuntimeClient) Execute(_ context.Context, req *pb.ExecuteRequest, opts ...grpc.CallOption) (*pb.ExecuteResponse, error) {
	f.executeOptions = len(opts)
	f.executeRequest = req
	return &pb.ExecuteResponse{}, nil
}

func (f *fakeRuntimeClient) Status(context.Context, *pb.StatusRequest, ...grpc.CallOption) (*pb.StatusResponse, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	return f.status, nil
}

func (f *fakeRuntimeClient) List(context.Context, *pb.ListRequest, ...grpc.CallOption) (*pb.ListResponse, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.list != nil {
		return f.list, nil
	}
	return &pb.ListResponse{}, nil
}

func (f *fakeRuntimeClient) Cancel(context.Context, *pb.CancelRequest, ...grpc.CallOption) (*pb.CancelResponse, error) {
	return &pb.CancelResponse{}, nil
}

func (f *fakeRuntimeClient) Forget(_ context.Context, req *pb.ForgetRequest, _ ...grpc.CallOption) (*pb.ForgetResponse, error) {
	f.forgetRequests = append(f.forgetRequests, req)
	if f.forgetErr != nil {
		return nil, f.forgetErr
	}
	return &pb.ForgetResponse{}, nil
}

func (f *fakeRuntimeClient) Health(context.Context, *pb.HealthRequest, ...grpc.CallOption) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Healthy: true}, nil
}
