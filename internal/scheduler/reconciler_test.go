package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

func TestReconcileKeepsRunPendingWhenNoRuntimePodAvailable(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-a",
			Namespace: "default",
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()

	reconciler := &RunReconciler{
		Client:   client,
		Log:      logr.Discard(),
		Strategy: &LeastLoaded{},
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      run.Name,
			Namespace: run.Namespace,
		},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want 30s", result.RequeueAfter)
	}

	var updated v1alpha1.Run
	if err := client.Get(context.Background(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get updated run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunPending {
		t.Fatalf("phase = %s, want %s", updated.Status.Phase, v1alpha1.RunPending)
	}
	if updated.Status.AssignedPod != "" {
		t.Fatalf("assignedPod = %q, want empty", updated.Status.AssignedPod)
	}
	if !strings.Contains(updated.Status.Message, "waiting for available runtime pods") {
		t.Fatalf("message = %q, want waiting message", updated.Status.Message)
	}
}

func TestIsPodSchedulableRequiresReadyRunningPod(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{
			name: "running and ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			want: true,
		},
		{
			name: "running but not ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionFalse},
					},
				},
			},
			want: false,
		},
		{
			name: "running without ready condition",
			pod: corev1.Pod{
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: false,
		},
		{
			name: "ready but not running",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
		{
			name: "terminating",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Finalizers:        []string{"test"},
					DeletionTimestamp: &now,
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPodSchedulable(&tt.pod); got != tt.want {
				t.Fatalf("isPodSchedulable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPendingRetryDelay(t *testing.T) {
	run := &v1alpha1.Run{
		Spec: v1alpha1.RunSpec{
			RetryPolicy: &v1alpha1.RetryPolicy{
				Backoff: metav1.Duration{Duration: time.Minute},
			},
		},
		Status: v1alpha1.RunStatus{
			Phase:   v1alpha1.RunPending,
			Attempt: 2,
			Conditions: []metav1.Condition{
				{
					Type:               "Running",
					Status:             metav1.ConditionFalse,
					Reason:             "PodGone",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	if delay := pendingRetryDelay(run); delay <= 0 {
		t.Fatalf("pendingRetryDelay() = %s, want positive delay", delay)
	}

	run.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * time.Minute))
	if delay := pendingRetryDelay(run); delay > 0 {
		t.Fatalf("pendingRetryDelay() = %s, want no delay after backoff expires", delay)
	}
}

func TestIsRuntimePodAvailableRequiresRuntimedReadyAndCapacity(t *testing.T) {
	now := metav1.Now()
	reconciler := &RunReconciler{
		RuntimedHeartbeatStaleAfter: time.Minute,
	}

	basePod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "2",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{
					Type:          v1alpha1.RuntimePodRuntimedReadyCondition,
					Status:        corev1.ConditionTrue,
					LastProbeTime: now,
				},
			},
		},
	}

	if !reconciler.isRuntimePodAvailable(basePod.DeepCopy(), now.Time, 1) {
		t.Fatal("expected runtime pod with fresh heartbeat and capacity to be available")
	}
	if reconciler.isRuntimePodAvailable(basePod.DeepCopy(), now.Time, 2) {
		t.Fatal("expected runtime pod at capacity to be unavailable")
	}

	missingHeartbeat := basePod.DeepCopy()
	missingHeartbeat.Status.Conditions = missingHeartbeat.Status.Conditions[:1]
	if reconciler.isRuntimePodAvailable(missingHeartbeat, now.Time, 0) {
		t.Fatal("expected runtime pod without runtimed heartbeat to be unavailable")
	}

	staleHeartbeat := basePod.DeepCopy()
	staleHeartbeat.Status.Conditions[1].LastProbeTime = metav1.NewTime(now.Add(-2 * time.Minute))
	if reconciler.isRuntimePodAvailable(staleHeartbeat, now.Time, 0) {
		t.Fatal("expected runtime pod with stale runtimed heartbeat to be unavailable")
	}
}
