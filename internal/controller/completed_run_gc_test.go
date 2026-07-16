package controller

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestCompletedRunGCDeletesExpiredTerminalRun(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	completedAt := metav1.NewTime(now.Add(-2 * time.Hour))
	ttlSeconds := int32(3600)
	gc, c, run := newCompletedRunGCTest(t, now, &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{TTLSecondsAfterFinished: &ttlSeconds},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunSucceeded,
			CompletionTime: &completedAt,
		},
	})

	if _, err := gc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.Run
	err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &updated)
	if err == nil {
		t.Fatalf("run still exists after expired TTL")
	}
}

func TestCompletedRunGCRequeuesUnexpiredTerminalRun(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	completedAt := metav1.NewTime(now.Add(-30 * time.Minute))
	ttlSeconds := int32(3600)
	gc, c, run := newCompletedRunGCTest(t, now, &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{TTLSecondsAfterFinished: &ttlSeconds},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunFailed,
			CompletionTime: &completedAt,
		},
	})

	result, err := gc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != 30*time.Minute {
		t.Fatalf("requeueAfter = %s, want 30m", result.RequeueAfter)
	}
	if _, err := getRunMaybe(t, c, run); err != nil {
		t.Fatalf("run was deleted before TTL expired: %v", err)
	}
}

func TestCompletedRunGCIgnoresNonTerminalRun(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	completedAt := metav1.NewTime(now.Add(-2 * time.Hour))
	ttlSeconds := int32(3600)
	gc, c, run := newCompletedRunGCTest(t, now, &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{TTLSecondsAfterFinished: &ttlSeconds},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunRunning,
			CompletionTime: &completedAt,
		},
	})

	if _, err := gc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := getRunMaybe(t, c, run); err != nil {
		t.Fatalf("non-terminal run was deleted: %v", err)
	}
}

func TestCompletedRunGCIgnoresReadyRun(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	completedAt := metav1.NewTime(now.Add(-2 * time.Hour))
	ttlSeconds := int32(3600)
	gc, c, run := newCompletedRunGCTest(t, now, &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-function", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{TTLSecondsAfterFinished: &ttlSeconds},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunReady,
			CompletionTime: &completedAt,
		},
	})

	if _, err := gc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := getRunMaybe(t, c, run); err != nil {
		t.Fatalf("Ready run was deleted: %v", err)
	}
}

func TestCompletedRunGCIgnoresTerminalRunWithoutCompletionTime(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	ttlSeconds := int32(3600)
	gc, c, run := newCompletedRunGCTest(t, now, &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{TTLSecondsAfterFinished: &ttlSeconds},
		Status: v1alpha1.RunStatus{
			Phase: v1alpha1.RunCancelled,
		},
	})

	if _, err := gc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := getRunMaybe(t, c, run); err != nil {
		t.Fatalf("terminal run without completionTime was deleted: %v", err)
	}
}

func TestCompletedRunGCIgnoresTerminalRunWithoutTTL(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	completedAt := metav1.NewTime(now.Add(-2 * time.Hour))
	gc, c, run := newCompletedRunGCTest(t, now, &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-a", Namespace: "default"},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunTimeout,
			CompletionTime: &completedAt,
		},
	})

	if _, err := gc.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := getRunMaybe(t, c, run); err != nil {
		t.Fatalf("terminal run without TTL was deleted: %v", err)
	}
}

func newCompletedRunGCTest(t *testing.T, now time.Time, run *v1alpha1.Run) (*CompletedRunGC, client.Client, *v1alpha1.Run) {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(run).
		Build()

	return &CompletedRunGC{
		Client: c,
		Now:    func() time.Time { return now },
	}, c, run
}

func getRunMaybe(t *testing.T, c client.Client, run *v1alpha1.Run) (v1alpha1.Run, error) {
	t.Helper()

	var updated v1alpha1.Run
	err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &updated)
	return updated, err
}
