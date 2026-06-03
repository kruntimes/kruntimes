package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

	reaper.handleStaleRun(context.Background(), run)

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

	reaper.handleStaleRun(context.Background(), run)

	updated := getRun(t, c, run)
	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if updated.Status.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1", updated.Status.Attempt)
	}
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

	reaper.handleStaleRun(context.Background(), run)

	updated := getRun(t, c, run)
	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if updated.Status.Attempt != 3 {
		t.Fatalf("attempt = %d, want 3", updated.Status.Attempt)
	}
}

func newStaleReaperTest(t *testing.T, run *v1alpha1.Run) (*StaleRunReaper, client.Client, *v1alpha1.Run) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()

	return &StaleRunReaper{Client: c}, c, run
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
