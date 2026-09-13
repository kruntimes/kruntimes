package runtimed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	"github.com/kruntimes/kruntimes/internal/runstatus"
	rlegpkg "github.com/kruntimes/kruntimes/internal/runtimed/rleg"
)

func TestPrepareSource_NoSource(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	run := &v1alpha1.Run{}
	run.UID = "test-uid"
	ar, err := prepareTestSource(run)
	if err != nil {
		t.Fatal(err)
	}
	workDir := ar.workDir
	expected := filepath.Join(dir, string(run.UID))
	if workDir != expected {
		t.Errorf("expected %s, got %s", expected, workDir)
	}
	if _, err := os.Stat(workDir); err != nil {
		t.Errorf("workDir not created: %v", err)
	}
}

func TestPrepareSource_ReferencedWorkspacePreservesSharedDirectory(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: "workspace-run-uid"},
		Spec: v1alpha1.RunSpec{
			Workspace: &v1alpha1.RunWorkspaceReference{Name: "build"},
			Mode:      v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
	}
	sharedDir := persistentWorkspacePath("build")
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sharedDir, "from-previous-run"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}

	ar, err := prepareTestSource(run)
	if err != nil {
		t.Fatal(err)
	}
	workDir := ar.workDir
	if workDir != sharedDir {
		t.Fatalf("workDir = %q, want shared workspace %q", workDir, sharedDir)
	}
	if _, err := os.Stat(filepath.Join(workDir, "from-previous-run")); err != nil {
		t.Fatalf("shared workspace content was not preserved: %v", err)
	}
}

func TestDispatchDurationSeconds(t *testing.T) {
	claimedAt := time.Now()
	run := &v1alpha1.Run{
		Status: v1alpha1.RunStatus{
			Conditions: []metav1.Condition{
				{
					Type:               runstatus.ConditionScheduled,
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(claimedAt.Add(-3 * time.Second)),
				},
			},
		},
	}

	got, ok := dispatchDurationSeconds(run, claimedAt)
	if !ok || got < 2.9 || got > 3.1 {
		t.Fatalf("dispatchDurationSeconds() = %f, %v; want about 3s", got, ok)
	}

	run.Status.Conditions[0].LastTransitionTime = metav1.NewTime(claimedAt.Add(time.Second))
	if _, ok := dispatchDurationSeconds(run, claimedAt); ok {
		t.Fatal("dispatchDurationSeconds() ok = true for negative duration")
	}

	run.Status.Conditions = nil
	if _, ok := dispatchDurationSeconds(run, claimedAt); ok {
		t.Fatal("dispatchDurationSeconds() ok = true without Scheduled condition")
	}
}

func TestPrepareSource_Inline(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "#!/bin/bash\necho hello"
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Source: &v1alpha1.CodeSource{Inline: &inline},
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "run.sh"},
			},
		},
	}
	run.UID = "test-uid"

	ar, err := prepareTestSource(run)
	if err != nil {
		t.Fatal(err)
	}
	workDir := ar.workDir

	if _, err := os.Stat(filepath.Join(workDir, "run.sh")); !os.IsNotExist(err) {
		t.Fatalf("inline source should ignore entrypoint, stat err = %v", err)
	}
	scriptPath := filepath.Join(workDir, "script")
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
			Mode:   v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
	}
	run.UID = "test-uid"

	ar, err := prepareTestSource(run)
	if err != nil {
		t.Fatal(err)
	}
	workDir := ar.workDir

	scriptPath := filepath.Join(workDir, "script")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Errorf("expected default 'script' file: %v", err)
	}
}

func TestPrepareSource_InlineInReferencedWorkspaceUsesRunLocalStaging(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "echo shared"
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: "workspace-inline-uid"},
		Spec: v1alpha1.RunSpec{
			Workspace: &v1alpha1.RunWorkspaceReference{Name: "build"},
			Source:    &v1alpha1.CodeSource{Inline: &inline},
			Mode:      v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
	}

	ar, err := prepareTestSource(run)
	if err != nil {
		t.Fatal(err)
	}
	workDir := ar.workDir
	if workDir != persistentWorkspacePath("build") {
		t.Fatalf("workDir = %q, want shared workspace", workDir)
	}
	stagedScript := filepath.Join(ar.sourceDir, "script")
	if data, err := os.ReadFile(stagedScript); err != nil || string(data) != inline {
		t.Fatalf("staged script = %q, %v; want %q", data, err, inline)
	}
	if _, err := os.Stat(filepath.Join(workDir, "script")); !os.IsNotExist(err) {
		t.Fatalf("shared workspace script should not be materialized at its root: %v", err)
	}
	entrypoint, args, err := runtimeExecutionInput(ar)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ar.sourceEntrypointPrefix, "script"); entrypoint != want || args != nil {
		t.Fatalf("runtime input = %q, %v; want %q, nil", entrypoint, args, want)
	}
}

func TestPrepareSource_FunctionInlineMaterializesInlinePath(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "def invoke(request):\n    return request\n"
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Source: &v1alpha1.CodeSource{
				Inline:     &inline,
				InlinePath: "app/handler.py",
			},
			Mode: v1alpha1.RunMode{
				Function: &v1alpha1.RunFunctionMode{Handler: "app.handler.invoke"},
			},
		},
	}
	run.UID = "test-function-uid"

	ar, err := prepareTestSource(run)
	if err != nil {
		t.Fatal(err)
	}
	workDir := ar.workDir

	data, err := os.ReadFile(filepath.Join(workDir, "app", "handler.py"))
	if err != nil {
		t.Fatalf("read inline function source: %v", err)
	}
	if string(data) != inline {
		t.Errorf("inline function source = %q, want %q", data, inline)
	}
	if _, err := os.Stat(filepath.Join(workDir, "script")); !os.IsNotExist(err) {
		t.Fatalf("function inline source should not create default script, stat err = %v", err)
	}
}

func TestPrepareSource_FunctionInlineRejectsEscapingInlinePath(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "def invoke(request):\n    return request\n"
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Source: &v1alpha1.CodeSource{Inline: &inline, InlinePath: "../handler.py"},
			Mode:   v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "handler.invoke"}},
		},
	}
	run.UID = "test-function-uid"

	if _, err := prepareTestSource(run); err == nil {
		t.Fatal("prepareSource succeeded for escaping inlinePath")
	}
	if _, err := os.Stat(filepath.Join(dir, "handler.py")); !os.IsNotExist(err) {
		t.Fatalf("escaping inlinePath created a file outside the Run directory: %v", err)
	}
}

func TestPrepareSource_InlineIgnoresEscapingEntrypoint(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	inline := "echo escape"
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Source: &v1alpha1.CodeSource{Inline: &inline},
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "../escape.sh"},
			},
		},
	}
	run.UID = "test-uid"

	ar, err := prepareTestSource(run)
	if err != nil {
		t.Fatalf("prepareSource: %v", err)
	}
	workDir := ar.workDir
	if _, err := os.Stat(filepath.Join(dir, "escape.sh")); !os.IsNotExist(err) {
		t.Fatalf("escape target should not exist, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, "script")); err != nil {
		t.Fatalf("inline script should be written to default script path: %v", err)
	}
}

func TestPrepareSource_ModeTaskRejectsEscapingEntrypoint(t *testing.T) {
	dir := t.TempDir()
	workspacePath = dir

	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{
					Entrypoint: "../escape.sh",
				},
			},
		},
	}
	run.UID = "test-uid"

	if _, err := prepareTestSource(run); err == nil {
		t.Fatal("prepareSource() error = nil, want escaping entrypoint error")
	}
}

func TestRuntimeExecutionInput_ModeTask(t *testing.T) {
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{
					Entrypoint: "scripts/run.sh",
					Args:       []string{"first", "second"},
				},
			},
		},
	}

	entrypoint, args, err := runtimeExecutionInput(newActiveRun(run, time.Time{}))
	if err != nil {
		t.Fatalf("runtimeExecutionInput: %v", err)
	}
	if entrypoint != "scripts/run.sh" {
		t.Fatalf("entrypoint = %q, want scripts/run.sh", entrypoint)
	}
	if len(args) != 2 || args[0] != "first" || args[1] != "second" {
		t.Fatalf("args = %v, want [first second]", args)
	}
}

func prepareTestSource(run *v1alpha1.Run) (*activeRun, error) {
	ar := newActiveRun(run, time.Time{})
	return ar, prepareSource(ar)
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

func TestMergeInvocationOutputs(t *testing.T) {
	merged, err := mergeInvocationOutputs(
		map[string]string{"from-adapter": "value"},
		map[string]string{"from-file": "value"},
	)
	if err != nil {
		t.Fatalf("merge invocation outputs: %v", err)
	}
	if len(merged) != 2 || merged["from-adapter"] != "value" || merged["from-file"] != "value" {
		t.Fatalf("merged outputs = %#v", merged)
	}

	if _, err := mergeInvocationOutputs(map[string]string{"duplicate": "adapter"}, map[string]string{"duplicate": "file"}); err == nil {
		t.Fatal("merge invocation outputs succeeded for duplicate key")
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

func TestScheduleRetryForgetsFailedTaskExecution(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "retry", Namespace: "default", UID: "retry-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: "pod-a", Attempt: 1},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{Client: k8sClient, runtimeCli: runtimeClient}
	policy := runretry.WithDefaults(&v1alpha1.RetryPolicy{MaxAttempts: 3})

	if _, err := c.scheduleRetry(t.Context(), newActiveRun(run, time.Now()), 1, policy, runretry.ReasonRuntimeError, "failed"); err != nil {
		t.Fatalf("scheduleRetry: %v", err)
	}
	if len(runtimeClient.forgetRequests) != 1 || runtimeClient.forgetRequests[0].Id != string(run.UID) {
		t.Fatalf("Forget requests = %#v, want Run UID %q", runtimeClient.forgetRequests, run.UID)
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

func TestReconcileScheduledWaitsForLocalRuntimeHealth(t *testing.T) {
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "waiting", UID: "waiting-uid"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunScheduled},
	}
	c := &Controller{runtimeCli: &fakeRuntimeClient{healthErr: status.Error(codes.Unavailable, "starting")}}

	result, err := c.reconcileScheduled(t.Context(), run)
	if err != nil {
		t.Fatalf("reconcileScheduled: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, time.Second)
	}
	if run.Status.Phase != v1alpha1.RunScheduled {
		t.Fatalf("phase = %s, want Scheduled", run.Status.Phase)
	}
	if c.activeRunCount() != 0 {
		t.Fatalf("activeRunCount = %d, want 0", c.activeRunCount())
	}
}

func TestTryClaimActiveRunKeepsSessionExclusive(t *testing.T) {
	newRun := func(uid string, session bool) *activeRun {
		run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid)}}
		if session {
			run.Spec.Mode.Session = &v1alpha1.RunSessionMode{}
		}
		return newActiveRun(run, time.Now())
	}

	t.Run("Session waits for an active Run", func(t *testing.T) {
		c := &Controller{Workers: 2}
		if !c.tryClaimActiveRun(newRun("task", false)) {
			t.Fatal("claim normal Run")
		}
		if c.tryClaimActiveRun(newRun("session", true)) {
			t.Fatal("Session claimed alongside a normal Run")
		}
	})

	t.Run("normal Run waits for an active Session", func(t *testing.T) {
		c := &Controller{Workers: 2}
		if !c.tryClaimActiveRun(newRun("session", true)) {
			t.Fatal("claim Session Run")
		}
		if c.tryClaimActiveRun(newRun("task", false)) {
			t.Fatal("normal Run claimed alongside a Session")
		}
	})
}

func TestRunFilterAllowsRLEGGenericEventsForAssignedRuns(t *testing.T) {
	c := &Controller{PodName: "runtime-pod", RuntimeNamespace: "default"}
	filter := c.runFilter()

	assigned := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "assigned", Namespace: "default"},
		Status:     v1alpha1.RunStatus{AssignedPod: "runtime-pod"},
	}
	if !filter.Generic(event.GenericEvent{Object: assigned}) {
		t.Fatal("assigned Run generic event was filtered")
	}

	otherPod := assigned.DeepCopy()
	otherPod.Name = "other-pod"
	otherPod.Status.AssignedPod = "other-runtime-pod"
	if filter.Generic(event.GenericEvent{Object: otherPod}) {
		t.Fatal("Run assigned to another pod was accepted")
	}

	otherNamespace := assigned.DeepCopy()
	otherNamespace.Name = "other-namespace"
	otherNamespace.Namespace = "other"
	if filter.Generic(event.GenericEvent{Object: otherNamespace}) {
		t.Fatal("Run from another namespace was accepted")
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
			Runtime:     "bash",
			Termination: &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationImmediate},
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
		PodName:    "pod-a",
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

func TestReconcileRunningAppliesAsynchronousExecutionStartFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-artifact",
			Namespace: "default",
			UID:       "missing-artifact-uid",
		},
		Spec: v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "runtime-pod",
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	c := &Controller{
		Client:     k8sClient,
		PodName:    "runtime-pod",
		runtimeCli: &fakeRuntimeClient{statusErr: status.Error(codes.NotFound, "execution not found")},
	}
	ar := newActiveRun(run, time.Now())
	c.activeRuns.Store(string(run.UID), ar)
	ar.finishExecutionStart(&executionStartFailure{
		reason:  runretry.ReasonRuntimeExecute,
		message: "runtime Execute: open artifact input \"missing.txt\": no such file",
	})

	if _, err := c.reconcileRunningActive(t.Context(), ar); err != nil {
		t.Fatalf("reconcileRunningActive: %v", err)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.Message, "open artifact input") {
		t.Fatalf("message = %q, want artifact input error", updated.Status.Message)
	}
}

// staleRunGetClient proves status transitions use the Run object supplied to
// Reconcile rather than fetching around the controller cache.
type staleRunGetClient struct {
	client.Client
	stale *v1alpha1.Run
}

func (c staleRunGetClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if run, ok := object.(*v1alpha1.Run); ok && c.stale != nil && key == client.ObjectKeyFromObject(c.stale) {
		c.stale.DeepCopyInto(run)
		return nil
	}
	return c.Client.Get(ctx, key, object, options...)
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
		PodName:    "pod-a",
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
	setTestWorkspace(t)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-outputs", Namespace: "default", UID: "invalid-outputs-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime:     "bash",
			RetryPolicy: &v1alpha1.RetryPolicy{MaxAttempts: 3},
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: "pod-a"},
	}
	ar := newActiveRun(run, time.Now())
	if err := os.MkdirAll(ar.cleanupDir, 0o755); err != nil {
		t.Fatalf("create Run-local directory: %v", err)
	}
	if err := os.WriteFile(ar.outputPath, []byte("invalid\n"), 0o644); err != nil {
		t.Fatalf("write outputs: %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	c := &Controller{Client: k8sClient, PodName: "pod-a"}
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
	c := &Controller{Client: k8sClient, PodName: "pod-a", ExecutionLogWriter: &output}
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
	c := &Controller{Client: k8sClient, PodName: "pod-a", ExecutionLogWriter: &output}
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
		PodName:    "pod-a",
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
		PodName:    "pod-a",
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

func TestReconcileRunningRecoveredEmitsRetainedLiveOutput(t *testing.T) {
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "recovered-live", Namespace: "default", UID: "recovered-live-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "pod-a",
			StartTime:   &metav1.Time{Time: time.Now().Add(-time.Second)},
		},
	}
	var output bytes.Buffer
	c := &Controller{
		PodName:            "pod-a",
		ExecutionLogWriter: &output,
		runtimeCli: &fakeRuntimeClient{status: &pb.StatusResponse{
			Id:     string(run.UID),
			State:  pb.ExecutionState_EXECUTION_STATE_RUNNING,
			Stdout: "retained after recovery\n",
		}},
	}

	if _, err := c.reconcileRunningRecovered(t.Context(), run); err != nil {
		t.Fatalf("reconcile recovered Run: %v", err)
	}
	if !strings.Contains(output.String(), "retained after recovery") {
		t.Fatalf("recovered live output = %q", output.String())
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
				PodName:    "pod-a",
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

func TestReconcileRunningActiveRequeuesWhileExecutionRunning(t *testing.T) {
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "active", Namespace: "default", UID: "active-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "pod-a",
			StartTime:   &metav1.Time{Time: time.Now()},
		},
	}
	var output bytes.Buffer
	c := &Controller{
		PodName:            "pod-a",
		ExecutionLogWriter: &output,
		runtimeCli: &fakeRuntimeClient{status: &pb.StatusResponse{
			Id:     string(run.UID),
			State:  pb.ExecutionState_EXECUTION_STATE_RUNNING,
			Stdout: "ready\n",
		}},
	}

	ar := &activeRun{run: run, start: time.Now()}
	ar.started.Store(true)
	result, err := c.reconcileRunningActive(t.Context(), ar)
	if err != nil {
		t.Fatalf("reconcileRunningActive: %v", err)
	}
	if result.RequeueAfter != rlegpkg.DefaultRelistInterval {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, rlegpkg.DefaultRelistInterval)
	}
	var line executionLogLine
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &line); err != nil {
		t.Fatalf("decode live log: %v; logs=%q", err, output.String())
	}
	if line.Stream != "stdout" || line.Message != "ready" || line.RunUID != string(run.UID) {
		t.Fatalf("live log = %#v", line)
	}
}

func TestEmitExecutionOutputDeltaBuffersPartialLinesAndAvoidsDuplicates(t *testing.T) {
	run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: "live", Namespace: "default", UID: "live-uid"}, Spec: v1alpha1.RunSpec{Runtime: "bash"}}
	ar := &activeRun{run: run}
	var output bytes.Buffer
	c := &Controller{PodName: "pod-a", ExecutionLogWriter: &output}

	c.emitExecutionOutputDelta(ar, executionOutput{stdout: "first\npartial", stderr: "warning\n"}, false)
	c.emitExecutionOutputDelta(ar, executionOutput{stdout: "first\npartial line\n", stderr: "warning\n"}, false)
	c.emitExecutionOutputDelta(ar, executionOutput{stdout: "first\npartial line\n", stderr: "warning\nlast"}, true)

	var got []string
	for _, encoded := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var line executionLogLine
		if err := json.Unmarshal([]byte(encoded), &line); err != nil {
			t.Fatalf("decode log %q: %v", encoded, err)
		}
		got = append(got, line.Stream+":"+line.Message)
	}
	want := []string{"stdout:first", "stderr:warning", "stdout:partial line", "stderr:last"}
	if !slices.Equal(got, want) {
		t.Fatalf("live log records = %#v, want %#v", got, want)
	}
}

func TestEmitExecutionOutputDeltaReemitsRetainedOutputAfterRuntimeReset(t *testing.T) {
	run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: "reset", Namespace: "default", UID: "reset-uid"}, Spec: v1alpha1.RunSpec{Runtime: "bash"}}
	ar := &activeRun{run: run}
	var output bytes.Buffer
	c := &Controller{PodName: "pod-a", ExecutionLogWriter: &output}

	c.emitExecutionOutputDelta(ar, executionOutput{stdout: "before reset\n"}, false)
	// A replacement Runtime Server can retain a different buffer under the same
	// execution ID. Prefer at-least-once projection to losing its new output.
	c.emitExecutionOutputDelta(ar, executionOutput{stdout: "after reset\n"}, false)

	if !strings.Contains(output.String(), "before reset") || !strings.Contains(output.String(), "after reset") {
		t.Fatalf("reset output = %q", output.String())
	}
}

func TestActiveRunRequeueAfterUsesSoonerDeadline(t *testing.T) {
	got := activeRunRequeueAfter(&activeRun{deadline: time.Now().Add(25 * time.Millisecond)})
	if got <= 0 || got > rlegpkg.DefaultRelistInterval {
		t.Fatalf("activeRunRequeueAfter = %s, want positive duration <= %s", got, rlegpkg.DefaultRelistInterval)
	}
}

func TestSessionRunBecomesReadyFromReconciledRuntimeStatus(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunRunning,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "runtime-pod-uid",
			StartTime:      &metav1.Time{Time: time.Now()},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	sessionClient := &fakeSessionRuntimeClient{status: &pb.SessionStatus{State: pb.SessionState_SESSION_STATE_READY}}
	c := &Controller{
		Client:            k8sClient,
		PodName:           "runtime-pod",
		GatewayURL:        "http://kruntimes-gateway.platform.svc/",
		sessionCli:        sessionClient,
		ArtifactStore:     &fakeArtifactStore{},
		ArtifactStoreSpec: artifactTestStoreSpec(),
	}
	ar := newActiveRun(run, time.Now())
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileSessionRegistration(t.Context(), ar); err != nil {
		t.Fatalf("reconcileSessionRegistration: %v", err)
	}
	if sessionClient.registerRequest != nil {
		t.Fatal("RegisterSession was called although the Runtime Server was already ready")
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunReady {
		t.Fatalf("phase = %s, want Ready", updated.Status.Phase)
	}
	if updated.Status.Endpoint == nil || updated.Status.Endpoint.Protocol != v1alpha1.RunEndpointProtocolHTTP || updated.Status.Endpoint.URL != "http://kruntimes-gateway.platform.svc/v1/namespaces/default/runtimes/bash/sessions/session-uid" {
		t.Fatalf("endpoint = %#v", updated.Status.Endpoint)
	}
	if condition := meta.FindStatusCondition(updated.Status.Conditions, runstatus.ConditionReady); condition == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %#v, want true", condition)
	}
}

func TestSessionRegistrationAsyncDoesNotUpdateRunStatus(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunRunning,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "runtime-pod-uid",
			StartTime:      &metav1.Time{Time: time.Now()},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	sessionClient := &fakeSessionRuntimeClient{
		statusErr:      status.Error(codes.NotFound, "session not found"),
		registerCalled: make(chan struct{}),
	}
	c := &Controller{
		Client:     k8sClient,
		PodName:    "runtime-pod",
		sessionCli: sessionClient,
		rlegCh:     make(chan event.GenericEvent, 1),
	}
	ar := newActiveRun(run, time.Now())
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileSessionRegistration(t.Context(), ar); err != nil {
		t.Fatalf("reconcileSessionRegistration: %v", err)
	}
	select {
	case <-sessionClient.registerCalled:
	case <-time.After(time.Second):
		t.Fatal("RegisterSession was not called")
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunRunning {
		t.Fatalf("phase = %s, want Running before a later reconcile observes Ready", updated.Status.Phase)
	}
	select {
	case <-c.rlegCh:
	case <-time.After(time.Second):
		t.Fatal("registration completion did not enqueue the Run")
	}
}

func TestFunctionRunBecomesReadyFromReconciledRuntimeStatus(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	registration := &pb.FunctionRegistration{RunUid: string(run.UID), RegistrationId: "registration-1"}
	functionClient := &fakeFunctionRuntimeClient{status: &pb.FunctionStatusResponse{
		Registration: registration,
		State:        pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY,
	}}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient, GatewayURL: "https://gateway.default.svc", GatewayCABundle: []byte("test-ca")}
	ar := newActiveRun(run, time.Now())
	ar.finishFunctionRegistration(registration, nil)
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileFunctionRegistration(t.Context(), ar); err != nil {
		t.Fatalf("reconcileFunctionRegistration: %v", err)
	}
	if functionClient.registerRequest != nil {
		t.Fatal("RegisterFunction was called although the Runtime Server was already ready")
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunReady {
		t.Fatalf("phase = %s, want Ready", updated.Status.Phase)
	}
	if condition := meta.FindStatusCondition(updated.Status.Conditions, runstatus.ConditionReady); condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "FunctionRegistered" {
		t.Fatalf("Ready condition = %#v, want FunctionRegistered true", condition)
	}
	if endpoint := updated.Status.Endpoint; endpoint == nil || endpoint.Protocol != v1alpha1.RunEndpointProtocolHTTPS || endpoint.URL != "https://gateway.default.svc/v1/namespaces/default/runtimes/python/functions/function-uid:invoke" || string(endpoint.CABundle) != "test-ca" {
		t.Fatalf("endpoint = %#v", endpoint)
	}
}

func TestFunctionRegistrationAsyncDoesNotUpdateRunStatus(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	functionClient := &fakeFunctionRuntimeClient{registerCalled: make(chan struct{})}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient, rlegCh: make(chan event.GenericEvent, 1)}
	ar := newActiveRun(run, time.Now())
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileFunctionRegistration(t.Context(), ar); err != nil {
		t.Fatalf("reconcileFunctionRegistration: %v", err)
	}
	select {
	case <-functionClient.registerCalled:
	case <-time.After(time.Second):
		t.Fatal("RegisterFunction was not called")
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunRunning {
		t.Fatalf("phase = %s, want Running before a later reconcile observes Ready", updated.Status.Phase)
	}
	select {
	case <-c.rlegCh:
	case <-time.After(time.Second):
		t.Fatal("registration completion did not enqueue the Run")
	}
}

func TestReadyFunctionRecoversRegistrationAfterRuntimedRestart(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	run.Status.Phase = v1alpha1.RunReady
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	functionClient := &fakeFunctionRuntimeClient{registerCalled: make(chan struct{})}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient, rlegCh: make(chan event.GenericEvent, 1)}

	if _, err := c.reconcileReadyFunction(t.Context(), run); err != nil {
		t.Fatalf("reconcileReadyFunction: %v", err)
	}
	select {
	case <-functionClient.registerCalled:
	case <-time.After(time.Second):
		t.Fatal("RegisterFunction was not called to recover the Ready Run")
	}
	if functionClient.registerRequest == nil || functionClient.registerRequest.RegistrationAttempt != 1 {
		t.Fatalf("recovery registration request = %#v, want initial attempt", functionClient.registerRequest)
	}
	if c.activeRunCount() != 1 {
		t.Fatalf("activeRunCount = %d, want 1 after recovery", c.activeRunCount())
	}
}

func TestScheduledFunctionAddsCleanupFinalizerBeforeRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	run.Status.Phase = v1alpha1.RunScheduled
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	c := &Controller{Client: k8sClient, PodName: "runtime-pod"}

	if _, err := c.reconcileScheduled(t.Context(), run); err != nil {
		t.Fatalf("reconcileScheduled: %v", err)
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if !slices.Contains(updated.Finalizers, v1alpha1.RunRegistrationCleanupFinalizer) {
		t.Fatalf("finalizers = %v, want registration cleanup finalizer", updated.Finalizers)
	}
	if updated.Status.Phase != v1alpha1.RunRunning {
		t.Fatalf("phase = %s, want Running after claim", updated.Status.Phase)
	}
}

func TestScheduledSessionAddsCleanupFinalizerBeforeRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash", Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunScheduled},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	c := &Controller{Client: k8sClient, PodName: "runtime-pod"}

	if _, err := c.reconcileScheduled(t.Context(), run); err != nil {
		t.Fatalf("reconcileScheduled: %v", err)
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if !slices.Contains(updated.Finalizers, v1alpha1.RunRegistrationCleanupFinalizer) {
		t.Fatalf("finalizers = %v, want registration cleanup finalizer", updated.Finalizers)
	}
}

func TestReadyFunctionCancellationUnregistersBeforeTerminalStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	run.Spec.Termination = &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationImmediate}
	run.Status.Phase = v1alpha1.RunReady
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	registration := &pb.FunctionRegistration{RunUid: string(run.UID), RegistrationId: "registration-1"}
	functionClient := &fakeFunctionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient}
	ar := newActiveRun(run, time.Now())
	ar.finishFunctionRegistration(registration, nil)
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileReadyFunction(t.Context(), run); err != nil {
		t.Fatalf("reconcileReadyFunction: %v", err)
	}
	if len(functionClient.unregisterRequests) != 1 || functionClient.unregisterRequests[0].GetRegistration().GetRegistrationId() != registration.GetRegistrationId() || !functionClient.unregisterRequests[0].GetCancelInFlight() {
		t.Fatalf("UnregisterFunction requests = %#v, want cancelled registration", functionClient.unregisterRequests)
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("phase = %s, want Cancelled after unregister", updated.Status.Phase)
	}
}

func TestDeletingFunctionRemovesCleanupFinalizerAfterUnregister(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	run.Finalizers = []string{v1alpha1.RunRegistrationCleanupFinalizer}
	now := metav1.Now()
	run.DeletionTimestamp = &now
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	registration := &pb.FunctionRegistration{RunUid: string(run.UID), RegistrationId: "registration-1"}
	functionClient := &fakeFunctionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient}
	ar := newActiveRun(run, time.Now())
	ar.finishFunctionRegistration(registration, nil)
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileDeletingRegistration(t.Context(), run); err != nil {
		t.Fatalf("reconcileDeletingRegistration: %v", err)
	}
	if len(functionClient.unregisterRequests) != 1 {
		t.Fatalf("UnregisterFunction requests = %#v, want one", functionClient.unregisterRequests)
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err == nil {
		t.Fatalf("Run still exists after removing its last finalizer: %#v", updated.Finalizers)
	}
}

func TestDeletingFunctionRecoversRegistrationBeforeRemovingFinalizer(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	run.Finalizers = []string{v1alpha1.RunRegistrationCleanupFinalizer}
	now := metav1.Now()
	run.DeletionTimestamp = &now
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	functionClient := &fakeFunctionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient}

	if _, err := c.reconcileDeletingRegistration(t.Context(), run); err != nil {
		t.Fatalf("reconcileDeletingRegistration: %v", err)
	}
	if functionClient.registerRequest == nil {
		t.Fatal("RegisterFunction was not called to recover the opaque registration ID")
	}
	if len(functionClient.unregisterRequests) != 1 {
		t.Fatalf("UnregisterFunction requests = %#v, want one", functionClient.unregisterRequests)
	}
}

func TestDeletingFunctionRetainsFinalizerWhenUnregisterFails(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	run.Finalizers = []string{v1alpha1.RunRegistrationCleanupFinalizer}
	now := metav1.Now()
	run.DeletionTimestamp = &now
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	registration := &pb.FunctionRegistration{RunUid: string(run.UID), RegistrationId: "registration-1"}
	functionClient := &fakeFunctionRuntimeClient{unregisterErr: status.Error(codes.Unavailable, "runtime unavailable")}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient}
	ar := newActiveRun(run, time.Now())
	ar.finishFunctionRegistration(registration, nil)
	c.activeRuns.Store(string(run.UID), ar)

	result, err := c.reconcileDeletingRegistration(t.Context(), run)
	if err == nil || result.RequeueAfter != time.Second {
		t.Fatalf("reconcileDeletingRegistration = %#v, %v; want error and 1s requeue", result, err)
	}
	if c.activeRunCount() != 1 {
		t.Fatalf("activeRunCount = %d, want 1 while finalizer cleanup is retried", c.activeRunCount())
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get Run with retained finalizer: %v", err)
	}
	if !slices.Contains(updated.Finalizers, v1alpha1.RunRegistrationCleanupFinalizer) {
		t.Fatalf("finalizers = %v, want registration cleanup finalizer retained", updated.Finalizers)
	}
}

func TestDeletingSessionClosesBeforeRemovingFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	now := metav1.Now()
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid", Finalizers: []string{v1alpha1.RunRegistrationCleanupFinalizer}, DeletionTimestamp: &now},
		Spec:       v1alpha1.RunSpec{Runtime: "bash", Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunReady, AssignedPod: "runtime-pod", AssignedPodUID: "runtime-pod-uid"},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	sessionClient := &fakeSessionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", sessionCli: sessionClient}
	ar := newActiveRun(run, time.Now())
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileDeletingRegistration(t.Context(), run); err != nil {
		t.Fatalf("reconcileDeletingRegistration: %v", err)
	}
	if len(sessionClient.closeRequests) != 1 || sessionClient.closeRequests[0].GetIdentity().GetRunUid() != string(run.UID) {
		t.Fatalf("CloseSession requests = %#v, want one for the deleting Session", sessionClient.closeRequests)
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err == nil {
		t.Fatalf("Run still exists after removing its last finalizer: %#v", updated.Finalizers)
	}
}

func TestReadyFunctionTotalTimeoutUnregistersBeforeTimeoutStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	totalTimeout := metav1.Duration{Duration: time.Second}
	run := functionLifecycleTestRun()
	run.Spec.Timeout = &totalTimeout
	run.Status.Phase = v1alpha1.RunReady
	run.Status.StartTime = &metav1.Time{Time: time.Now().Add(-2 * time.Second)}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	registration := &pb.FunctionRegistration{RunUid: string(run.UID), RegistrationId: "registration-1"}
	functionClient := &fakeFunctionRuntimeClient{status: &pb.FunctionStatusResponse{
		Registration: registration,
		State:        pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY,
	}}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient}
	ar := c.buildActiveRun(run)
	ar.finishFunctionRegistration(registration, nil)
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileReadyFunction(t.Context(), run); err != nil {
		t.Fatalf("reconcileReadyFunction: %v", err)
	}
	if len(functionClient.unregisterRequests) != 1 {
		t.Fatalf("UnregisterFunction requests = %#v, want one", functionClient.unregisterRequests)
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunTimeout || updated.Status.Message != "timeout after 1s" {
		t.Fatalf("status = %#v, want Timeout after total timeout", updated.Status)
	}
}

func TestReadyFunctionIdleTimeoutUnregistersBeforeTimeoutStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	idleTimeout := int32(1)
	run := functionLifecycleTestRun()
	run.Spec.Mode.Function.IdleTimeoutSeconds = &idleTimeout
	run.Status.Phase = v1alpha1.RunReady
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	registration := &pb.FunctionRegistration{RunUid: string(run.UID), RegistrationId: "registration-1"}
	functionClient := &fakeFunctionRuntimeClient{status: &pb.FunctionStatusResponse{
		Registration:         registration,
		State:                pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY,
		LastActivityUnixNano: time.Now().Add(-2 * time.Second).UnixNano(),
	}}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient}
	ar := c.buildActiveRun(run)
	ar.finishFunctionRegistration(registration, nil)
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileReadyFunction(t.Context(), run); err != nil {
		t.Fatalf("reconcileReadyFunction: %v", err)
	}
	if len(functionClient.unregisterRequests) != 1 {
		t.Fatalf("UnregisterFunction requests = %#v, want one", functionClient.unregisterRequests)
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunTimeout || updated.Status.Message != "function idle timeout exceeded" {
		t.Fatalf("status = %#v, want idle Timeout", updated.Status)
	}
}

func TestFunctionRegistrationFailureResetsLocalHandleForRetry(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	run.Spec.RetryPolicy = &v1alpha1.RetryPolicy{MaxAttempts: 2}
	registration := &pb.FunctionRegistration{RunUid: string(run.UID), RegistrationId: "registration-1"}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	functionClient := &fakeFunctionRuntimeClient{status: &pb.FunctionStatusResponse{
		Registration: registration,
		State:        pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_FAILED,
		FatalError:   "registration failed",
	}}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient}
	ar := c.buildActiveRun(run)
	ar.finishFunctionRegistration(registration, nil)
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.reconcileFunctionRegistration(t.Context(), ar); err != nil {
		t.Fatalf("reconcileFunctionRegistration: %v", err)
	}
	if ar.functionRegistrationRef() != nil {
		t.Fatal("failed registration handle was retained instead of reset for retry")
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", updated.Status.Attempt)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, runstatus.ConditionRunning)
	if condition == nil || condition.Status != metav1.ConditionFalse {
		t.Fatalf("Running condition = %#v, want retry backoff", condition)
	}
}

func functionLifecycleTestRun() *v1alpha1.Run {
	inline := "def invoke(event):\n    return event\n"
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "function", Namespace: "default", UID: "function-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime: "python",
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "handler.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "handler.invoke"}},
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: "runtime-pod", AssignedPodUID: "runtime-pod-uid", StartTime: &metav1.Time{Time: time.Now()}},
	}
}

func TestShutdownDrainReleasesRegistrationsAndFinalizers(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	function := functionLifecycleTestRun()
	function.Status.Phase = v1alpha1.RunReady
	function.Finalizers = []string{v1alpha1.RunRegistrationCleanupFinalizer}
	session := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "session",
			Namespace:  "default",
			UID:        "session-uid",
			Finalizers: []string{v1alpha1.RunRegistrationCleanupFinalizer},
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunFinalizing,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "runtime-pod-uid",
		},
	}
	nonOwner := function.DeepCopy()
	nonOwner.Name = "other-owner"
	nonOwner.UID = "other-owner-uid"
	nonOwner.Status.AssignedPod = "other-pod"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(function, session, nonOwner).Build()
	functionClient := &fakeFunctionRuntimeClient{}
	sessionClient := &fakeSessionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient, sessionCli: sessionClient}
	functionActive := newActiveRun(function, time.Now())
	functionActive.finishFunctionRegistration(&pb.FunctionRegistration{RunUid: string(function.UID), RegistrationId: "function-registration"}, nil)
	c.activeRuns.Store(string(function.UID), functionActive)
	c.activeRuns.Store(string(session.UID), newActiveRun(session, time.Now()))

	c.drainActiveRunsOnShutdown(t.Context())

	if len(functionClient.unregisterRequests) != 1 || functionClient.unregisterRequests[0].GetRegistration().GetRegistrationId() != "function-registration" {
		t.Fatalf("UnregisterFunction requests = %#v, want the active function registration", functionClient.unregisterRequests)
	}
	if len(sessionClient.closeRequests) != 1 || sessionClient.closeRequests[0].GetIdentity().GetRunUid() != string(session.UID) {
		t.Fatalf("CloseSession requests = %#v, want the active session", sessionClient.closeRequests)
	}
	for _, run := range []*v1alpha1.Run{function, session} {
		var updated v1alpha1.Run
		if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
			t.Fatalf("get drained Run %q: %v", run.Name, err)
		}
		if slices.Contains(updated.Finalizers, v1alpha1.RunRegistrationCleanupFinalizer) {
			t.Fatalf("Run %q finalizers = %v, want registration finalizer removed", run.Name, updated.Finalizers)
		}
		if updated.Status.Phase != run.Status.Phase {
			t.Fatalf("Run %q phase = %s, want shutdown drain not to write status %s", run.Name, updated.Status.Phase, run.Status.Phase)
		}
	}
	var untouched v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(nonOwner), &untouched); err != nil {
		t.Fatalf("get non-owner Run: %v", err)
	}
	if !slices.Contains(untouched.Finalizers, v1alpha1.RunRegistrationCleanupFinalizer) {
		t.Fatalf("non-owner finalizers = %v, want registration finalizer retained", untouched.Finalizers)
	}
	if c.activeRunCount() != 0 {
		t.Fatalf("activeRunCount = %d, want 0 after shutdown drain", c.activeRunCount())
	}
}

func TestShutdownDrainRetainsFinalizerWhenRemoteCleanupFails(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := functionLifecycleTestRun()
	run.Status.Phase = v1alpha1.RunReady
	run.Finalizers = []string{v1alpha1.RunRegistrationCleanupFinalizer}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	functionClient := &fakeFunctionRuntimeClient{unregisterErr: status.Error(codes.Unavailable, "runtime is stopping")}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", functionCli: functionClient}
	ar := newActiveRun(run, time.Now())
	ar.finishFunctionRegistration(&pb.FunctionRegistration{RunUid: string(run.UID), RegistrationId: "function-registration"}, nil)
	c.activeRuns.Store(string(run.UID), ar)

	c.drainActiveRunsOnShutdown(t.Context())

	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get Run: %v", err)
	}
	if !slices.Contains(updated.Finalizers, v1alpha1.RunRegistrationCleanupFinalizer) {
		t.Fatalf("finalizers = %v, want registration finalizer retained after failed cleanup", updated.Finalizers)
	}
	if c.activeRunCount() != 1 {
		t.Fatalf("activeRunCount = %d, want 1 when cleanup failed", c.activeRunCount())
	}
}

func TestApplySessionReadyUsesCachedRun(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: "runtime-pod"},
	}
	directReader := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run.DeepCopy()).
		Build()
	stale := run.DeepCopy()
	stale.Status.Phase = v1alpha1.RunScheduled
	c := &Controller{
		Client:  staleRunGetClient{Client: directReader, stale: stale},
		PodName: "runtime-pod",
	}
	var cached v1alpha1.Run
	if err := directReader.Get(t.Context(), client.ObjectKeyFromObject(run), &cached); err != nil {
		t.Fatalf("get cached Run: %v", err)
	}
	ar := newActiveRun(&cached, time.Now())
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.applySessionReady(t.Context(), ar); err != nil {
		t.Fatalf("applySessionReady: %v", err)
	}
	var updated v1alpha1.Run
	if err := directReader.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunReady {
		t.Fatalf("phase = %s, want Ready", updated.Status.Phase)
	}
}

func TestReadySessionCancellationClosesRuntimeSession(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime:     "bash",
			Termination: &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationImmediate},
			Mode:        v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunReady,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "runtime-pod-uid",
			StartTime:      &metav1.Time{Time: time.Now()},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	sessionClient := &fakeSessionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", sessionCli: sessionClient}
	c.activeRuns.Store(string(run.UID), newActiveRun(run, time.Now()))

	if _, err := c.reconcileReady(t.Context(), run); err != nil {
		t.Fatalf("reconcileReady: %v", err)
	}
	if len(sessionClient.closeRequests) != 1 {
		t.Fatalf("CloseSession requests = %d, want 1", len(sessionClient.closeRequests))
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("phase = %s, want Cancelled", updated.Status.Phase)
	}
}

func TestReadySessionCancellationWaitsForRuntimeSessionClose(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime:     "bash",
			Termination: &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationImmediate},
			Mode:        v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunReady,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "runtime-pod-uid",
			StartTime:      &metav1.Time{Time: time.Now()},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	sessionClient := &fakeSessionRuntimeClient{closeErr: status.Error(codes.Unavailable, "runtime unavailable")}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", sessionCli: sessionClient}
	c.activeRuns.Store(string(run.UID), newActiveRun(run, time.Now()))

	result, err := c.reconcileReady(t.Context(), run)
	if err != nil {
		t.Fatalf("reconcileReady: %v", err)
	}
	if result.RequeueAfter != time.Second {
		t.Fatalf("RequeueAfter = %s, want 1s", result.RequeueAfter)
	}
	var pending v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &pending); err != nil {
		t.Fatalf("get pending Run: %v", err)
	}
	if pending.Status.Phase != v1alpha1.RunReady {
		t.Fatalf("phase after failed close = %s, want Ready", pending.Status.Phase)
	}

	sessionClient.closeErr = nil
	result, err = c.reconcileReady(t.Context(), &pending)
	if err != nil {
		t.Fatalf("reconcileReady after close recovery: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("RequeueAfter after close recovery = %s, want 0", result.RequeueAfter)
	}
	var cancelled v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &cancelled); err != nil {
		t.Fatalf("get cancelled Run: %v", err)
	}
	if cancelled.Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("phase after close recovery = %s, want Cancelled", cancelled.Status.Phase)
	}
	if len(sessionClient.closeRequests) != 2 {
		t.Fatalf("CloseSession requests = %d, want 2", len(sessionClient.closeRequests))
	}
}

func TestReadySessionDrainFinalizesAfterAcceptedOperations(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime:     "bash",
			Termination: &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationDrain},
			Mode:        v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunReady,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "runtime-pod-uid",
			StartTime:      &metav1.Time{Time: time.Now()},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	operations := NewSessionOperationQueue(1, time.Minute)
	if err := operations.Ensure(run, time.Now()); err != nil {
		t.Fatalf("ensure Session operation queue: %v", err)
	}
	sessionClient := &fakeSessionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", sessionCli: sessionClient, SessionOperations: operations}
	c.activeRuns.Store(string(run.UID), newActiveRun(run, time.Now()))

	if _, err := c.reconcileReady(t.Context(), run); err != nil {
		t.Fatalf("reconcileReady: %v", err)
	}
	var finalizing v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &finalizing); err != nil {
		t.Fatalf("get finalizing Run: %v", err)
	}
	if finalizing.Status.Phase != v1alpha1.RunFinalizing {
		t.Fatalf("phase = %s, want Finalizing", finalizing.Status.Phase)
	}
	if len(sessionClient.closeRequests) != 0 {
		t.Fatalf("CloseSession requests = %d before drain completion, want 0", len(sessionClient.closeRequests))
	}

	if _, err := c.reconcileFinalizing(t.Context(), &finalizing); err != nil {
		t.Fatalf("reconcileFinalizing: %v", err)
	}
	var completed v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &completed); err != nil {
		t.Fatalf("get completed Run: %v", err)
	}
	if completed.Status.Phase != v1alpha1.RunSucceeded {
		t.Fatalf("phase = %s, want Succeeded", completed.Status.Phase)
	}
	if len(sessionClient.closeRequests) != 1 {
		t.Fatalf("CloseSession requests = %d, want 1", len(sessionClient.closeRequests))
	}
}

func TestFinalizingSessionExportsArtifacts(t *testing.T) {
	setTestWorkspace(t)
	run, ar, c, k8sClient, sessionClient, store := newFinalizingSessionForArtifactTest(t, &fakeArtifactStore{})
	if err := os.MkdirAll(ar.artifactDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ar.artifactDir, "report.txt"), []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}

	if result, err := c.reconcileFinalizing(t.Context(), run); err != nil || !result.IsZero() {
		t.Fatalf("reconcileFinalizing = (%#v, %v), want success", result, err)
	}
	var completed v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status.Phase != v1alpha1.RunSucceeded || len(completed.Status.ArtifactRefs) != 1 || completed.Status.ArtifactRefs[0].Name != "report.txt" {
		t.Fatalf("completed status = %#v", completed.Status)
	}
	if len(sessionClient.closeRequests) != 1 {
		t.Fatalf("CloseSession requests = %d, want 1", len(sessionClient.closeRequests))
	}
	if len(store.puts) != 1 {
		t.Fatalf("artifact uploads = %d, want 1", len(store.puts))
	}
}

func TestFinalizingSessionRetriesTransientArtifactStoreFailure(t *testing.T) {
	setTestWorkspace(t)
	run, ar, c, k8sClient, sessionClient, store := newFinalizingSessionForArtifactTest(t, &fakeArtifactStore{failAt: 1})
	if err := os.MkdirAll(ar.artifactDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ar.artifactDir, "report.txt"), []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := c.reconcileFinalizing(t.Context(), run)
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("first reconcile = (%#v, %v), want requeue", result, err)
	}
	var retrying v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &retrying); err != nil {
		t.Fatal(err)
	}
	if retrying.Status.Phase != v1alpha1.RunFinalizing || len(retrying.Status.ArtifactRefs) != 0 {
		t.Fatalf("status after transient store failure = %#v", retrying.Status)
	}

	store.failAt = 0
	store.puts = nil
	if result, err := c.reconcileFinalizing(t.Context(), &retrying); err != nil || !result.IsZero() {
		t.Fatalf("retry reconcile = (%#v, %v), want success", result, err)
	}
	var completed v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status.Phase != v1alpha1.RunSucceeded || len(completed.Status.ArtifactRefs) != 1 {
		t.Fatalf("completed status = %#v", completed.Status)
	}
	if len(sessionClient.closeRequests) != 1 {
		t.Fatalf("CloseSession requests = %d, want one durable close", len(sessionClient.closeRequests))
	}
}

func TestFinalizingSessionRetriesRuntimeServerCloseBeforeArtifactExport(t *testing.T) {
	setTestWorkspace(t)
	run, ar, c, k8sClient, sessionClient, store := newFinalizingSessionForArtifactTest(t, &fakeArtifactStore{})
	sessionClient.closeErr = status.Error(codes.Unimplemented, "close session is not supported")
	if err := os.MkdirAll(ar.artifactDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ar.artifactDir, "report.txt"), []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := c.reconcileFinalizing(t.Context(), run)
	if err != nil || result.RequeueAfter <= 0 {
		t.Fatalf("first reconcile = (%#v, %v), want requeue", result, err)
	}
	var retrying v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &retrying); err != nil {
		t.Fatal(err)
	}
	if retrying.Status.Phase != v1alpha1.RunFinalizing || len(store.puts) != 0 {
		t.Fatalf("close failure exported artifacts or changed phase: %#v, uploads=%d", retrying.Status, len(store.puts))
	}

	sessionClient.closeErr = nil
	if result, err := c.reconcileFinalizing(t.Context(), &retrying); err != nil || !result.IsZero() {
		t.Fatalf("retry reconcile = (%#v, %v), want success", result, err)
	}
	var completed v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status.Phase != v1alpha1.RunSucceeded || len(completed.Status.ArtifactRefs) != 1 {
		t.Fatalf("completed status = %#v", completed.Status)
	}
	if len(sessionClient.closeRequests) != 2 {
		t.Fatalf("CloseSession requests = %d, want 2", len(sessionClient.closeRequests))
	}
}

func TestFinalizingSessionFailsForInvalidArtifact(t *testing.T) {
	setTestWorkspace(t)
	run, ar, c, k8sClient, _, store := newFinalizingSessionForArtifactTest(t, &fakeArtifactStore{})
	if err := os.MkdirAll(ar.artifactDir, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ar.artifactDir, "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ar.artifactDir, "invalid-link")); err != nil {
		t.Fatal(err)
	}

	if result, err := c.reconcileFinalizing(t.Context(), run); err != nil || !result.IsZero() {
		t.Fatalf("reconcileFinalizing = (%#v, %v), want terminal failure", result, err)
	}
	var failed v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &failed); err != nil {
		t.Fatal(err)
	}
	if failed.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", failed.Status.Phase)
	}
	if condition := findCondition(failed.Status.Conditions, "Completed"); condition == nil || condition.Reason != "ArtifactInvalid" {
		t.Fatalf("Completed condition = %#v, want ArtifactInvalid", condition)
	}
	if len(store.puts) != 0 {
		t.Fatalf("artifact uploads = %d, want 0", len(store.puts))
	}
}

func TestFinalizingSessionImmediateTerminationSkipsArtifactExport(t *testing.T) {
	setTestWorkspace(t)
	run, ar, c, k8sClient, sessionClient, store := newFinalizingSessionForArtifactTest(t, &fakeArtifactStore{})
	run.Spec.Termination.Mode = v1alpha1.RunTerminationImmediate
	if err := os.MkdirAll(ar.artifactDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ar.artifactDir, "report.txt"), []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}

	if result, err := c.reconcileFinalizing(t.Context(), run); err != nil || !result.IsZero() {
		t.Fatalf("reconcileFinalizing = (%#v, %v), want cancellation", result, err)
	}
	var cancelled v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &cancelled); err != nil {
		t.Fatal(err)
	}
	if cancelled.Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("phase = %s, want Cancelled", cancelled.Status.Phase)
	}
	if len(store.puts) != 0 {
		t.Fatalf("artifact uploads = %d, want 0", len(store.puts))
	}
	if len(sessionClient.closeRequests) != 1 {
		t.Fatalf("CloseSession requests = %d, want 1", len(sessionClient.closeRequests))
	}
}

func newFinalizingSessionForArtifactTest(t *testing.T, store *fakeArtifactStore) (*v1alpha1.Run, *activeRun, *Controller, client.Client, *fakeSessionRuntimeClient, *fakeArtifactStore) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime:     "bash",
			Termination: &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationDrain},
			Mode:        v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunFinalizing,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "runtime-pod-uid",
			StartTime:      &metav1.Time{Time: time.Now()},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	sessionClient := &fakeSessionRuntimeClient{}
	c := &Controller{
		Client:            k8sClient,
		PodName:           "runtime-pod",
		sessionCli:        sessionClient,
		ArtifactStore:     store,
		ArtifactStoreSpec: artifactTestStoreSpec(),
	}
	ar := newActiveRun(run, time.Now())
	c.activeRuns.Store(string(run.UID), ar)
	return run, ar, c, k8sClient, sessionClient, store
}

func TestFinalizingSessionClosesRuntimeSession(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime:     "bash",
			Termination: &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationDrain},
			Mode:        v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunFinalizing,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "runtime-pod-uid",
			StartTime:      &metav1.Time{Time: time.Now()},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	sessionClient := &fakeSessionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", sessionCli: sessionClient}
	ar := newActiveRun(run, time.Now())
	c.activeRuns.Store(string(run.UID), ar)

	if err := c.ensureActiveSessionClosed(t.Context(), ar); err != nil {
		t.Fatalf("ensureActiveSessionClosed: %v", err)
	}
	if len(sessionClient.closeRequests) != 1 {
		t.Fatalf("CloseSession requests = %d, want 1", len(sessionClient.closeRequests))
	}
}

func TestSessionCloseTimeoutUsesConfiguredValue(t *testing.T) {
	configured := 17 * time.Second
	if got := (&Controller{SessionCloseTimeout: configured}).sessionCloseTimeout(); got != configured {
		t.Fatalf("configured Session close timeout = %s, want %s", got, configured)
	}
	if got := (&Controller{}).sessionCloseTimeout(); got != executionCleanupTimeout {
		t.Fatalf("default Session close timeout = %s, want %s", got, executionCleanupTimeout)
	}
}

func TestReadySessionIdleExpiryClosesRuntimeSession(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	idleTimeout := int32(1)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{IdleTimeoutSeconds: &idleTimeout}},
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunReady, AssignedPod: "runtime-pod", AssignedPodUID: "runtime-pod-uid", StartTime: &metav1.Time{Time: time.Now()}},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	operations := NewSessionOperationQueue(0, 0)
	if err := operations.Ensure(run, time.Now().Add(-2*time.Second)); err != nil {
		t.Fatalf("start idle tracking: %v", err)
	}
	sessionClient := &fakeSessionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", sessionCli: sessionClient, SessionOperations: operations}
	c.activeRuns.Store(string(run.UID), newActiveRun(run, time.Now()))

	if _, err := c.reconcileReady(t.Context(), run); err != nil {
		t.Fatalf("reconcileReady: %v", err)
	}
	if len(sessionClient.closeRequests) != 1 {
		t.Fatalf("CloseSession requests = %d, want 1", len(sessionClient.closeRequests))
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunTimeout {
		t.Fatalf("phase = %s, want Timeout", updated.Status.Phase)
	}
}

func TestReadySessionTotalTimeoutTakesPrecedenceOverIdleTimeout(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	idleTimeout := int32(60)
	totalTimeout := metav1.Duration{Duration: time.Second}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Timeout: &totalTimeout,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{IdleTimeoutSeconds: &idleTimeout}},
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunReady, AssignedPod: "runtime-pod", AssignedPodUID: "runtime-pod-uid", StartTime: &metav1.Time{Time: time.Now().Add(-2 * time.Second)}},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	operations := NewSessionOperationQueue(0, 0)
	if err := operations.Ensure(run, time.Now()); err != nil {
		t.Fatalf("start idle tracking: %v", err)
	}
	sessionClient := &fakeSessionRuntimeClient{}
	c := &Controller{Client: k8sClient, PodName: "runtime-pod", sessionCli: sessionClient, SessionOperations: operations}
	c.activeRuns.Store(string(run.UID), c.buildActiveRun(run))

	if _, err := c.reconcileReady(t.Context(), run); err != nil {
		t.Fatalf("reconcileReady: %v", err)
	}
	if len(sessionClient.closeRequests) != 1 {
		t.Fatalf("CloseSession requests = %d, want 1", len(sessionClient.closeRequests))
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunTimeout {
		t.Fatalf("phase = %s, want Timeout", updated.Status.Phase)
	}
	if updated.Status.Message != "timeout after 1s" {
		t.Fatalf("message = %q, want total timeout", updated.Status.Message)
	}
}

func TestReadySessionRecoveryFailsWhenRuntimeSessionIsNotReady(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "session-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash", Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunReady,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "runtime-pod-uid",
			StartTime:      &metav1.Time{Time: time.Now()},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(run).WithObjects(run).Build()
	c := &Controller{
		Client:     k8sClient,
		PodName:    "runtime-pod",
		sessionCli: &fakeSessionRuntimeClient{status: &pb.SessionStatus{State: pb.SessionState_SESSION_STATE_CLOSED}},
	}

	if _, err := c.reconcileReady(t.Context(), run); err != nil {
		t.Fatalf("reconcileReady: %v", err)
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get updated Run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
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
		Client:  k8sClient,
		PodName: "pod-a",
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
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: "run-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "python"},
	}
	ar := newActiveRun(run, time.Time{})

	if err := c.startExecution(t.Context(), ar); err != nil {
		t.Fatalf("startExecution: %v", err)
	}
	if runtimeClient.executeOptions == 0 {
		t.Fatal("expected Execute to wait for the runtime connection to become ready")
	}
	if got := runtimeClient.executeRequest.Env[artifact.OutputsEnv]; got != ar.outputPath {
		t.Fatalf("%s = %q, want %q", artifact.OutputsEnv, got, ar.outputPath)
	}
}

func TestStartExecutionUsesRunLocalOutputsForReferencedWorkspace(t *testing.T) {
	setTestWorkspace(t)
	runtimeClient := &fakeRuntimeClient{}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: "workspace-output-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime:   "bash",
			Workspace: &v1alpha1.RunWorkspaceReference{Name: "build"},
			Mode:      v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
	}
	ar := newActiveRun(run, time.Time{})
	c := &Controller{runtimeCli: runtimeClient}

	if err := c.startExecution(t.Context(), ar); err != nil {
		t.Fatalf("startExecution: %v", err)
	}
	if got, want := runtimeClient.executeRequest.WorkingDir, ar.workDir; got != want {
		t.Fatalf("WorkingDir = %q, want shared workspace %q", got, want)
	}
	if got, want := runtimeClient.executeRequest.Env[artifact.OutputsEnv], ar.outputPath; got != want {
		t.Fatalf("%s = %q, want Run-local path %q", artifact.OutputsEnv, got, want)
	}
}

func TestStartExecutionInlineIgnoresEntrypointAndArgs(t *testing.T) {
	inline := "echo inline"
	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{runtimeCli: runtimeClient}
	ar := &activeRun{
		run: &v1alpha1.Run{
			ObjectMeta: metav1.ObjectMeta{UID: "run-uid"},
			Spec: v1alpha1.RunSpec{
				Runtime: "bash",
				Source:  &v1alpha1.CodeSource{Inline: &inline},
				Mode: v1alpha1.RunMode{
					Task: &v1alpha1.RunTaskMode{
						Entrypoint: "custom.sh",
						Args:       []string{"ignored"},
					},
				},
			},
		},
		workDir: t.TempDir(),
	}

	if err := c.startExecution(t.Context(), ar); err != nil {
		t.Fatalf("startExecution: %v", err)
	}
	if got := runtimeClient.executeRequest.Entrypoint; got != "script" {
		t.Fatalf("entrypoint = %q, want script", got)
	}
	if got := runtimeClient.executeRequest.Args; len(got) != 0 {
		t.Fatalf("args = %v, want empty for inline source", got)
	}
}

func TestStartExecutionEntrypointReceivesArgs(t *testing.T) {
	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{runtimeCli: runtimeClient}
	ar := &activeRun{
		run: &v1alpha1.Run{
			ObjectMeta: metav1.ObjectMeta{UID: "run-uid"},
			Spec: v1alpha1.RunSpec{
				Runtime: "bash",
				Mode: v1alpha1.RunMode{
					Task: &v1alpha1.RunTaskMode{
						Entrypoint: "scripts/run.sh",
						Args:       []string{"first", "second"},
					},
				},
			},
		},
		workDir: t.TempDir(),
	}

	if err := c.startExecution(t.Context(), ar); err != nil {
		t.Fatalf("startExecution: %v", err)
	}
	if got := runtimeClient.executeRequest.Entrypoint; got != "scripts/run.sh" {
		t.Fatalf("entrypoint = %q, want scripts/run.sh", got)
	}
	if got := runtimeClient.executeRequest.Args; strings.Join(got, ",") != "first,second" {
		t.Fatalf("args = %v, want [first second]", got)
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
	ar := newActiveRun(run, time.Time{})

	if err := c.startExecution(t.Context(), ar); err != nil {
		t.Fatalf("startExecution: %v", err)
	}
	want := ar.artifactDir
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
	ar := newActiveRun(run, time.Now())
	if err := os.MkdirAll(ar.cleanupDir, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ar.cleanupDir, "retained"), []byte("data"), 0o600); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{runtimeCli: runtimeClient}
	c.activeRuns.Store(string(run.UID), ar)

	c.cleanup(t.Context(), ar, v1alpha1.RunSucceeded)

	if len(runtimeClient.forgetRequests) != 1 || runtimeClient.forgetRequests[0].Id != string(run.UID) {
		t.Fatalf("Forget requests = %#v, want cleanup-uid", runtimeClient.forgetRequests)
	}
	if _, err := os.Stat(ar.cleanupDir); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists or stat failed unexpectedly: %v", err)
	}
	if c.activeRunCount() != 0 {
		t.Fatalf("active runs = %d, want 0", c.activeRunCount())
	}
}

func TestCleanupRetainsReferencedWorkspaceState(t *testing.T) {
	setTestWorkspace(t)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "cleanup-shared", Namespace: "default", UID: "cleanup-shared-uid"},
		Spec: v1alpha1.RunSpec{
			Runtime:   "bash",
			Workspace: &v1alpha1.RunWorkspaceReference{Name: "build"},
			Mode:      v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
	}
	ar := newActiveRun(run, time.Now())
	sharedDir := ar.workDir
	if err := os.MkdirAll(ar.cleanupDir, 0o755); err != nil {
		t.Fatalf("create run-local workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sharedDir, "retained"), []byte("data"), 0o600); err != nil {
		t.Fatalf("write shared workspace: %v", err)
	}

	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{runtimeCli: runtimeClient}
	c.activeRuns.Store(string(run.UID), ar)

	c.cleanup(t.Context(), ar, v1alpha1.RunSucceeded)

	if _, err := os.Stat(filepath.Join(sharedDir, "retained")); err != nil {
		t.Fatalf("shared workspace content was removed: %v", err)
	}
	if _, err := os.Stat(ar.cleanupDir); err != nil {
		t.Fatalf("Run-local state was removed from PersistentWorkspace: %v", err)
	}
}

func TestReconcileDeletedRunReleasesActiveRun(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	now := metav1.Now()
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "deleted",
			Namespace:         "default",
			UID:               "deleted-uid",
			DeletionTimestamp: &now,
			Finalizers:        []string{"test.finalizer"},
		},
		Spec:   v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunRunning},
	}
	ar := newActiveRun(run, time.Time{})
	if err := os.MkdirAll(ar.cleanupDir, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(run).
		Build()
	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{Client: k8sClient, runtimeCli: runtimeClient}
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if c.activeRunCount() != 0 {
		t.Fatalf("activeRunCount = %d, want 0", c.activeRunCount())
	}
	if len(runtimeClient.forgetRequests) != 1 || runtimeClient.forgetRequests[0].Id != string(run.UID) {
		t.Fatalf("Forget requests = %#v, want deleted-uid", runtimeClient.forgetRequests)
	}
	if _, err := os.Stat(ar.cleanupDir); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists or stat failed unexpectedly: %v", err)
	}
}

func TestReconcileNotFoundRunReleasesActiveRunByName(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "gone", Namespace: "default", UID: "gone-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
	}
	ar := newActiveRun(run, time.Time{})
	if err := os.MkdirAll(ar.cleanupDir, 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()
	runtimeClient := &fakeRuntimeClient{}
	c := &Controller{Client: k8sClient, runtimeCli: runtimeClient}
	c.activeRuns.Store(string(run.UID), ar)

	if _, err := c.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if c.activeRunCount() != 0 {
		t.Fatalf("activeRunCount = %d, want 0", c.activeRunCount())
	}
	if len(runtimeClient.forgetRequests) != 1 || runtimeClient.forgetRequests[0].Id != string(run.UID) {
		t.Fatalf("Forget requests = %#v, want gone-uid", runtimeClient.forgetRequests)
	}
	if _, err := os.Stat(ar.cleanupDir); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists or stat failed unexpectedly: %v", err)
	}
}

type fakeRuntimeClient struct {
	pb.RuntimeClient
	health         *pb.HealthResponse
	healthErr      error
	status         *pb.StatusResponse
	statusErr      error
	list           *pb.ListResponse
	listErr        error
	forgetErr      error
	forgetRequests []*pb.ForgetRequest
	executeOptions int
	executeRequest *pb.ExecuteRequest
}

type fakeSessionRuntimeClient struct {
	pb.SessionRuntimeClient
	registerRequest *pb.RegisterSessionRequest
	registerErr     error
	status          *pb.SessionStatus
	statusErr       error
	closeRequests   []*pb.CloseSessionRequest
	closeErr        error
	registerCalled  chan struct{}
}

type fakeFunctionRuntimeClient struct {
	pb.FunctionRuntimeClient
	registerRequest    *pb.RegisterFunctionRequest
	registerErr        error
	status             *pb.FunctionStatusResponse
	statusErr          error
	registerCalled     chan struct{}
	unregisterRequests []*pb.UnregisterFunctionRequest
	unregisterErr      error
}

func (f *fakeFunctionRuntimeClient) RegisterFunction(_ context.Context, request *pb.RegisterFunctionRequest, _ ...grpc.CallOption) (*pb.RegisterFunctionResponse, error) {
	f.registerRequest = request
	if f.registerCalled != nil {
		close(f.registerCalled)
	}
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	return &pb.RegisterFunctionResponse{Registration: &pb.FunctionRegistration{RunUid: request.RunUid, RegistrationId: "registration-1"}, State: pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY}, nil
}

func (f *fakeFunctionRuntimeClient) FunctionStatus(context.Context, *pb.FunctionStatusRequest, ...grpc.CallOption) (*pb.FunctionStatusResponse, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if f.status != nil {
		return f.status, nil
	}
	return &pb.FunctionStatusResponse{State: pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY}, nil
}

func (f *fakeFunctionRuntimeClient) UnregisterFunction(_ context.Context, request *pb.UnregisterFunctionRequest, _ ...grpc.CallOption) (*pb.UnregisterFunctionResponse, error) {
	f.unregisterRequests = append(f.unregisterRequests, request)
	if f.unregisterErr != nil {
		return nil, f.unregisterErr
	}
	return &pb.UnregisterFunctionResponse{Registration: request.Registration}, nil
}

func (f *fakeSessionRuntimeClient) RegisterSession(_ context.Context, request *pb.RegisterSessionRequest, _ ...grpc.CallOption) (*pb.SessionStatus, error) {
	f.registerRequest = request
	if f.registerCalled != nil {
		close(f.registerCalled)
	}
	if f.registerErr != nil {
		return nil, f.registerErr
	}
	return &pb.SessionStatus{Identity: request.Identity, State: pb.SessionState_SESSION_STATE_READY}, nil
}

func (f *fakeSessionRuntimeClient) GetSessionStatus(context.Context, *pb.GetSessionStatusRequest, ...grpc.CallOption) (*pb.SessionStatus, error) {
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if f.status != nil {
		return f.status, nil
	}
	return &pb.SessionStatus{State: pb.SessionState_SESSION_STATE_READY}, nil
}

func (f *fakeSessionRuntimeClient) CloseSession(_ context.Context, request *pb.CloseSessionRequest, _ ...grpc.CallOption) (*pb.CloseSessionResponse, error) {
	f.closeRequests = append(f.closeRequests, request)
	if f.closeErr != nil {
		return nil, f.closeErr
	}
	return &pb.CloseSessionResponse{Identity: request.Identity}, nil
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
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	if f.health != nil {
		return f.health, nil
	}
	return &pb.HealthResponse{Healthy: true}, nil
}
