package scheduler

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

func TestReservationPreventsOvercommitBeforeBindIsObserved(t *testing.T) {
	now := time.Now()
	pod := reservationTestPod(now, "runtime-a", 1)
	first := reservationTestRun("first", "first-uid")
	second := reservationTestRun("second", "second-uid")
	reconciler := &RunReconciler{RuntimedHeartbeatStaleAfter: time.Minute}
	request, err := first.Spec.ResourceRequests()
	if err != nil {
		t.Fatalf("resource requests: %v", err)
	}

	firstSnapshot := &schedulingSnapshot{
		run:              first,
		request:          request,
		actualUsageByPod: map[string]corev1.ResourceList{},
		runs:             []v1alpha1.Run{*first, *second},
		now:              now,
	}
	if !reconciler.reserve(firstSnapshot, pod) {
		t.Fatal("reserve first Run = false, want true")
	}

	// The informer may still report the first Run as Pending while its Bind is
	// in flight. That observation must not discard its assumption.
	secondSnapshot := &schedulingSnapshot{
		run:              second,
		request:          request,
		actualUsageByPod: map[string]corev1.ResourceList{},
		runs:             []v1alpha1.Run{*first, *second},
		now:              now,
	}
	if reconciler.reserve(secondSnapshot, pod) {
		t.Fatal("reserve second Run = true, want false after first assumed assignment")
	}
}

func TestEffectiveUsageHandsOffAssumptionToObservedAssignment(t *testing.T) {
	now := time.Now()
	run := reservationTestRun("run-a", "run-a-uid")
	pod := reservationTestPod(now, "runtime-a", 2)
	reconciler := &RunReconciler{
		assumed: map[reservationKey]assumedReservation{
			reservationKeyFor(run): {
				runName: run.Name,
				podName: pod.Name,
				request: defaultRuntimePodCapacity(),
			},
		},
	}

	request, err := run.Spec.ResourceRequests()
	if err != nil {
		t.Fatalf("resource requests: %v", err)
	}
	run.Status.Phase = v1alpha1.RunScheduled
	run.Status.AssignedPod = pod.Name
	actual := map[string]corev1.ResourceList{pod.Name: request}
	usage := reconciler.effectiveUsage(actual, []v1alpha1.Run{*run})

	want := request[corev1.ResourceName(v1alpha1.RuntimeResourceRuns)]
	if got := usage[pod.Name][corev1.ResourceName(v1alpha1.RuntimeResourceRuns)]; got.Cmp(want) != 0 {
		t.Fatalf("effective runs usage = %s, want %s", got.String(), want.String())
	}
	if len(reconciler.assumed) != 0 {
		t.Fatalf("assumed reservations = %#v, want empty after observed assignment", reconciler.assumed)
	}
}

func TestReleaseReservation(t *testing.T) {
	run := reservationTestRun("run-a", "run-a-uid")
	reconciler := &RunReconciler{
		assumed: map[reservationKey]assumedReservation{
			reservationKeyFor(run): {runName: run.Name, podName: "runtime-a"},
		},
	}
	reconciler.releaseReservation(run)
	if len(reconciler.assumed) != 0 {
		t.Fatalf("assumed reservations = %#v, want empty", reconciler.assumed)
	}
}

func TestBindReleasesReservationOnStatusUpdateFailure(t *testing.T) {
	run := reservationTestRun("run-a", "run-a-uid")
	pod := reservationTestPod(time.Now(), "runtime-a", 1)
	reconciler := &RunReconciler{
		Client: statusUpdateErrorClient{err: errors.New("status update failed")},
		assumed: map[reservationKey]assumedReservation{
			reservationKeyFor(run): {runName: run.Name, podName: pod.Name},
		},
	}
	if err := reconciler.bind(context.Background(), run, pod); err == nil {
		t.Fatal("bind error = nil, want status update error")
	}
	if len(reconciler.assumed) != 0 {
		t.Fatalf("assumed reservations = %#v, want empty after bind failure", reconciler.assumed)
	}
}

type statusUpdateErrorClient struct {
	client.Client
	err error
}

func (c statusUpdateErrorClient) Status() client.SubResourceWriter {
	return statusUpdateErrorWriter{err: c.err}
}

type statusUpdateErrorWriter struct {
	client.SubResourceWriter
	err error
}

func (w statusUpdateErrorWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return w.err
}

func reservationTestRun(name string, uid types.UID) *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: uid},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunPending},
	}
}

func reservationTestPod(now time.Time, name string, capacity int64) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID(name + "-uid"),
			Annotations: map[string]string{
				runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): resource.NewQuantity(capacity, resource.DecimalSI).String(),
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue, LastProbeTime: metav1.NewTime(now)},
			},
		},
	}
}
