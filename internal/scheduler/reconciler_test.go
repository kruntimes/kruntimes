package scheduler

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
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
	"github.com/kruntimes/kruntimes/internal/runstatus"
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

func TestReconcileCancelsPendingRun(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-cancel",
			Namespace: "default",
		},
		Spec: v1alpha1.RunSpec{
			Runtime:         "missing",
			CancelRequested: true,
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunPending},
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

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: run.Name, Namespace: run.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.Run
	if err := client.Get(context.Background(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get updated run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("phase = %s, want Cancelled", updated.Status.Phase)
	}
	if updated.Status.CompletionTime == nil {
		t.Fatal("expected completion time")
	}
}

func TestReconcileRecordsScheduledCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}

	createdAt := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "run-scheduled",
			Namespace:         "default",
			CreationTimestamp: createdAt,
		},
		Spec: v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{
			Phase: v1alpha1.RunPending,
		},
	}
	podReady := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-a",
			Namespace: "default",
			UID:       "runtime-a-uid",
			Labels: map[string]string{
				"runtime": "bash",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{
					Type:          v1alpha1.RuntimePodRuntimedReadyCondition,
					Status:        corev1.ConditionTrue,
					LastProbeTime: podReady,
				},
			},
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run, pod).
		Build()

	reconciler := &RunReconciler{
		Client:   client,
		Log:      logr.Discard(),
		Strategy: &LeastLoaded{},
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: run.Name, Namespace: run.Namespace},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.Run
	if err := client.Get(context.Background(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get updated run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunScheduled {
		t.Fatalf("phase = %s, want Scheduled", updated.Status.Phase)
	}
	if updated.Status.AssignedPod != pod.Name {
		t.Fatalf("assignedPod = %q, want %q", updated.Status.AssignedPod, pod.Name)
	}
	if updated.Status.AssignedPodUID != string(pod.UID) {
		t.Fatalf("assignedPodUID = %q, want %q", updated.Status.AssignedPodUID, pod.UID)
	}
	condition := meta.FindStatusCondition(updated.Status.Conditions, runstatus.ConditionScheduled)
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "Assigned" {
		t.Fatalf("Scheduled condition = %#v, want true/Assigned", condition)
	}
}

func TestReconcileSchedulesRunToRequiredAffinityPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}

	now := metav1.Now()
	readyPod := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Labels:    map[string]string{"runtime": "bash"},
				Annotations: map[string]string{
					runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "2",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					{Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue, LastProbeTime: now},
				},
			},
		}
	}

	target := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", Labels: map[string]string{"stage": "build"}},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: "runtime-a"},
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Affinity: &v1alpha1.RunAffinity{RunAffinity: &v1alpha1.RunAffinityRules{
				RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"stage": "build"}},
					TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
				}},
			}},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run, target, readyPod("runtime-a"), readyPod("runtime-b")).
		Build()
	reconciler := &RunReconciler{Client: k8sClient, Log: logr.Discard(), Strategy: &LeastLoaded{}}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var updated v1alpha1.Run
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunScheduled || updated.Status.AssignedPod != "runtime-a" {
		t.Fatalf("status = %#v, want Scheduled on runtime-a", updated.Status)
	}
}

func TestReconcileFailsRunForInvalidRequiredAffinity(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}

	now := metav1.Now()
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-affinity", Namespace: "default"},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Affinity: &v1alpha1.RunAffinity{RunAffinity: &v1alpha1.RunAffinityRules{
				RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{{
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"stage": "build"}},
					TopologyKey:   "invalid.topology",
				}},
			}},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-a", Namespace: "default", Labels: map[string]string{"runtime": "bash"}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue, LastProbeTime: now},
			},
		},
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run, pod).
		Build()
	reconciler := &RunReconciler{Client: k8sClient, Log: logr.Discard(), Strategy: &LeastLoaded{}}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.Message, "run affinity evaluation failed") {
		t.Fatalf("message = %q, want affinity failure", updated.Status.Message)
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

func TestRunQueueDurationSeconds(t *testing.T) {
	scheduledAt := time.Now()
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.NewTime(scheduledAt.Add(-2 * time.Second)),
		},
	}
	got, ok := runQueueDurationSeconds(run, scheduledAt)
	if !ok || got < 1.9 || got > 2.1 {
		t.Fatalf("runQueueDurationSeconds() = %f, %v; want about 2s", got, ok)
	}

	run.CreationTimestamp = metav1.NewTime(scheduledAt.Add(time.Second))
	if _, ok := runQueueDurationSeconds(run, scheduledAt); ok {
		t.Fatal("runQueueDurationSeconds() ok = true for negative duration")
	}
}

func TestPendingRunsForReleasedCapacity(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}

	released := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "released", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunSucceeded},
	}
	pendingSameRuntime := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-same-runtime", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunPending},
	}
	pendingEmptyPhase := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-empty-phase", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
	}
	pendingOtherRuntime := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-other-runtime", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{Runtime: "python"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunPending},
	}
	scheduledSameRuntime := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "scheduled-same-runtime", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunScheduled},
	}
	pendingOtherNamespace := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "pending-other-namespace", Namespace: "other"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunPending},
	}

	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(released, pendingSameRuntime, pendingEmptyPhase, pendingOtherRuntime, scheduledSameRuntime, pendingOtherNamespace).
		Build()
	reconciler := &RunReconciler{Client: k8sClient, Log: logr.Discard()}

	requests := reconciler.pendingRunsForReleasedCapacity(context.Background(), released)
	got := make([]string, 0, len(requests))
	for _, request := range requests {
		got = append(got, request.NamespacedName.String())
	}
	sort.Strings(got)

	want := []string{"default/pending-empty-phase", "default/pending-same-runtime"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v, want %v", got, want)
	}

	if requests := reconciler.pendingRunsForReleasedCapacity(context.Background(), &corev1.Pod{}); len(requests) != 0 {
		t.Fatalf("requests for non-Run object = %v, want none", requests)
	}
}

func TestRunCapacityReleasedPredicate(t *testing.T) {
	pred := runCapacityReleasedPredicate()

	tests := []struct {
		name     string
		oldPhase v1alpha1.RunPhase
		newPhase v1alpha1.RunPhase
		want     bool
	}{
		{name: "scheduled to succeeded", oldPhase: v1alpha1.RunScheduled, newPhase: v1alpha1.RunSucceeded, want: true},
		{name: "running to failed", oldPhase: v1alpha1.RunRunning, newPhase: v1alpha1.RunFailed, want: true},
		{name: "running to pending", oldPhase: v1alpha1.RunRunning, newPhase: v1alpha1.RunPending, want: true},
		{name: "ready to pending", oldPhase: v1alpha1.RunReady, newPhase: v1alpha1.RunPending, want: true},
		{name: "pending to scheduled", oldPhase: v1alpha1.RunPending, newPhase: v1alpha1.RunScheduled, want: false},
		{name: "ready stays ready", oldPhase: v1alpha1.RunReady, newPhase: v1alpha1.RunReady, want: false},
		{name: "running stays running", oldPhase: v1alpha1.RunRunning, newPhase: v1alpha1.RunRunning, want: false},
		{name: "terminal update", oldPhase: v1alpha1.RunSucceeded, newPhase: v1alpha1.RunFailed, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pred.Update(event.UpdateEvent{
				ObjectOld: &v1alpha1.Run{Status: v1alpha1.RunStatus{Phase: tt.oldPhase}},
				ObjectNew: &v1alpha1.Run{Status: v1alpha1.RunStatus{Phase: tt.newPhase}},
			})
			if got != tt.want {
				t.Fatalf("Update() = %v, want %v", got, tt.want)
			}
		})
	}

	if pred.Create(event.CreateEvent{Object: &v1alpha1.Run{Status: v1alpha1.RunStatus{Phase: v1alpha1.RunRunning}}}) {
		t.Fatal("Create() = true, want false")
	}
	if !pred.Delete(event.DeleteEvent{Object: &v1alpha1.Run{Status: v1alpha1.RunStatus{Phase: v1alpha1.RunRunning}}}) {
		t.Fatal("Delete() = false, want true for Running Run")
	}
	if !pred.Delete(event.DeleteEvent{Object: &v1alpha1.Run{Status: v1alpha1.RunStatus{Phase: v1alpha1.RunReady}}}) {
		t.Fatal("Delete() = false, want true for Ready Run")
	}
	if pred.Delete(event.DeleteEvent{Object: &v1alpha1.Run{Status: v1alpha1.RunStatus{Phase: v1alpha1.RunSucceeded}}}) {
		t.Fatal("Delete() = true, want false for terminal Run")
	}
	if pred.Generic(event.GenericEvent{Object: &v1alpha1.Run{Status: v1alpha1.RunStatus{Phase: v1alpha1.RunRunning}}}) {
		t.Fatal("Generic() = true, want false")
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
