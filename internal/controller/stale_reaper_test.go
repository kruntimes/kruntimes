package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
)

func TestStaleRunReaperRetriesUsingSharedRetryPolicy(t *testing.T) {
	reaper, c, run := newStaleReaperTest(t, &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "default"},
		Spec: v1alpha1.RunSpec{
			RetryPolicy: &v1alpha1.RetryPolicy{MaxAttempts: 3},
		},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "missing-pod",
		},
	})

	if err := reaper.handleStaleRun(context.Background(), run, runretry.ReasonPodGone, "assigned pod was deleted"); err != nil {
		t.Fatalf("handle stale run: %v", err)
	}

	updated := getRun(t, c, run)
	if updated.Status.Phase != v1alpha1.RunPending {
		t.Fatalf("phase = %s, want Pending", updated.Status.Phase)
	}
	if updated.Status.AssignedPod != "" {
		t.Fatalf("assignedPod = %q, want empty", updated.Status.AssignedPod)
	}
	if updated.Status.Attempt != 2 {
		t.Fatalf("attempt = %d, want 2", updated.Status.Attempt)
	}
	if cond := findCondition(updated.Status.Conditions, "Running"); cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != runretry.ReasonPodGone {
		t.Fatalf("Running condition = %#v, want false/%s", cond, runretry.ReasonPodGone)
	}
}

func TestStaleRunReaperHonorsRetryableReasons(t *testing.T) {
	reaper, c, run := newStaleReaperTest(t, &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "default"},
		Spec: v1alpha1.RunSpec{
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts:      3,
				RetryableReasons: []string{runretry.ReasonRuntimeError},
			},
		},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "missing-pod",
		},
	})

	if err := reaper.handleStaleRun(context.Background(), run, runretry.ReasonPodGone, "assigned pod was deleted"); err != nil {
		t.Fatalf("handle stale run: %v", err)
	}

	updated := getRun(t, c, run)
	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if updated.Status.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", updated.Status.Attempt)
	}
	assertFailedTerminalConditions(t, &updated, runretry.ReasonPodGone)
}

func TestStaleRunReaperDoesNotRetryWhenAttemptsExhausted(t *testing.T) {
	reaper, c, run := newStaleReaperTest(t, &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "default"},
		Spec: v1alpha1.RunSpec{
			RetryPolicy: &v1alpha1.RetryPolicy{MaxAttempts: 3},
		},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "missing-pod",
			Attempt:     3,
		},
	})

	if err := reaper.handleStaleRun(context.Background(), run, runretry.ReasonPodGone, "assigned pod was deleted"); err != nil {
		t.Fatalf("handle stale run: %v", err)
	}

	updated := getRun(t, c, run)
	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if updated.Status.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3", updated.Status.Attempt)
	}
	assertFailedTerminalConditions(t, &updated, runretry.ReasonPodGone)
}

func TestStaleRunReaperChecksPodAndRuntimedReadiness(t *testing.T) {
	now := time.Date(2026, time.June, 12, 12, 0, 0, 0, time.UTC)
	threshold := 30 * time.Second
	ready := metav1.NewTime(now.Add(-time.Minute))
	freshProbe := metav1.NewTime(now.Add(-10 * time.Second))
	staleProbe := metav1.NewTime(now.Add(-time.Minute))

	tests := []struct {
		name       string
		conditions []corev1.PodCondition
		wantStale  bool
		wantMsg    string
	}{
		{
			name: "both ready",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: ready},
				{Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue, LastProbeTime: freshProbe},
			},
		},
		{
			name: "stale runtimed heartbeat",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: ready},
				{Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue, LastProbeTime: staleProbe},
			},
			wantStale: true,
			wantMsg:   "assigned pod runtimed heartbeat is stale",
		},
		{
			name: "missing runtimed heartbeat",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: ready},
			},
			wantStale: true,
			wantMsg:   "assigned pod runtimed heartbeat is stale",
		},
		{
			name: "pod recently became unready",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(now.Add(-10 * time.Second))},
			},
		},
		{
			name: "pod remained unready",
			conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: staleProbe},
			},
			wantStale: true,
			wantMsg:   "assigned pod is not ready",
		},
	}

	reaper := &StaleRunReaper{StalenessThreshold: threshold}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{Conditions: tt.conditions}}
			stale, reason, msg := reaper.stalePodState(pod, now)
			if stale != tt.wantStale {
				t.Fatalf("stale = %t, want %t", stale, tt.wantStale)
			}
			if !tt.wantStale {
				return
			}
			if reason != runretry.ReasonPodUnhealthy {
				t.Fatalf("reason = %q, want %q", reason, runretry.ReasonPodUnhealthy)
			}
			if msg != tt.wantMsg {
				t.Fatalf("message = %q, want %q", msg, tt.wantMsg)
			}
		})
	}
}

func TestStaleRunReaperReturnsStatusUpdateError(t *testing.T) {
	statusErr := errors.New("status update failed")
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "default"},
		Status: v1alpha1.RunStatus{
			Phase:       v1alpha1.RunRunning,
			AssignedPod: "runtime-a",
		},
	}
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-a", Namespace: "default"},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{
				Type:          v1alpha1.RuntimePodRuntimedReadyCondition,
				Status:        corev1.ConditionTrue,
				LastProbeTime: metav1.NewTime(now.Add(-time.Minute)),
			},
		}},
	}

	c := fake.NewClientBuilder().
		WithScheme(staleReaperScheme(t)).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run, pod).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(
				_ context.Context,
				_ client.Client,
				subResourceName string,
				_ client.Object,
				_ ...client.SubResourceUpdateOption,
			) error {
				if subResourceName == "status" {
					return statusErr
				}
				return nil
			},
		}).
		Build()
	reaper := &StaleRunReaper{
		Client:             c,
		StalenessThreshold: 30 * time.Second,
	}

	_, err := reaper.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: run.Name, Namespace: run.Namespace},
	})
	if !errors.Is(err, statusErr) {
		t.Fatalf("Reconcile error = %v, want %v", err, statusErr)
	}
}

func newStaleReaperTest(t *testing.T, run *v1alpha1.Run) (*StaleRunReaper, client.Client, *v1alpha1.Run) {
	t.Helper()

	c := fake.NewClientBuilder().
		WithScheme(staleReaperScheme(t)).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()

	return &StaleRunReaper{Client: c}, c, run
}

func staleReaperScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}
	return scheme
}

func getRun(t *testing.T, c client.Client, run *v1alpha1.Run) v1alpha1.Run {
	t.Helper()

	var updated v1alpha1.Run
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	return updated
}

func findCondition(conditions []metav1.Condition, typ string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == typ {
			return &conditions[i]
		}
	}
	return nil
}

func assertFailedTerminalConditions(t *testing.T, run *v1alpha1.Run, reason string) {
	t.Helper()

	if run.Status.CompletionTime == nil {
		t.Fatal("completionTime is nil")
	}
	for _, typ := range []string{"Running", "Completed"} {
		cond := findCondition(run.Status.Conditions, typ)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != reason {
			t.Fatalf("%s condition = %#v, want false/%s", typ, cond, reason)
		}
	}
}
