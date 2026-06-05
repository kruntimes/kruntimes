package runtimed

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
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

func TestReadOutputs_Empty(t *testing.T) {
	outputs := readOutputs("")
	if outputs != nil {
		t.Error("expected nil for empty workingDir")
	}
}

func TestReadOutputs_Nonexistent(t *testing.T) {
	outputs := readOutputs("/nonexistent/path")
	if outputs != nil {
		t.Error("expected nil for nonexistent file")
	}
}

func TestReadOutputs_Valid(t *testing.T) {
	dir := t.TempDir()
	content := "key1=val1\nkey2=val2\n# comment\nkey3 = val3\n"
	_ = os.WriteFile(filepath.Join(dir, "outputs"), []byte(content), 0o644)

	outputs := readOutputs(dir)
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

func TestReadOutputs_SkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	content := "no_equal_sign\n=empty_key\nb=\n  \n"
	_ = os.WriteFile(filepath.Join(dir, "outputs"), []byte(content), 0o644)

	outputs := readOutputs(dir)
	// "no_equal_sign" has no "=", skipped. "=empty_key" yields key="" value="empty_key".
	// "b=" yields key="b" value="". Whitespace-only lines are skipped.
	if _, ok := outputs["b"]; !ok {
		t.Errorf("expected key 'b', got %v", outputs)
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
	if updated.Status.Message != "done" {
		t.Fatalf("message = %q, want done", updated.Status.Message)
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

type fakeRuntimeClient struct {
	pb.RuntimeClient
	status    *pb.StatusResponse
	statusErr error
	list      *pb.ListResponse
	listErr   error
}

func (f *fakeRuntimeClient) Execute(context.Context, *pb.ExecuteRequest, ...grpc.CallOption) (*pb.ExecuteResponse, error) {
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

func (f *fakeRuntimeClient) Health(context.Context, *pb.HealthRequest, ...grpc.CallOption) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Healthy: true}, nil
}
