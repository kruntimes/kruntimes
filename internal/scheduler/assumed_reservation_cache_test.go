package scheduler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestAssumedReservationCacheReserveAccountsForExistingAssumptions(t *testing.T) {
	cache := &assumedReservationCache{}
	pod := &corev1.Pod{}
	pod.Name = "runtime-a"
	first := reservationTestRun("first", "first-uid")
	second := reservationTestRun("second", "second-uid")
	request := runResourceRequest(1)

	if reserved := cache.reserve(cacheSnapshot(first, request, *first, *second), pod, func(usage corev1.ResourceList) bool {
		usedRuns := usage[corev1.ResourceName(v1alpha1.RuntimeResourceRuns)]
		return usedRuns.IsZero()
	}); !reserved {
		t.Fatal("reserve first Run = false, want true")
	}

	if reserved := cache.reserve(cacheSnapshot(second, request, *first, *second), pod, func(usage corev1.ResourceList) bool {
		usedRuns := usage[corev1.ResourceName(v1alpha1.RuntimeResourceRuns)]
		return usedRuns.CmpInt64(1) < 0
	}); reserved {
		t.Fatal("reserve second Run = true, want false after first assumption")
	}
}

func TestAssumedReservationCacheHandsOffToObservedAssignment(t *testing.T) {
	cache := &assumedReservationCache{}
	pod := &corev1.Pod{}
	pod.Name = "runtime-a"
	run := reservationTestRun("run-a", "run-a-uid")
	request := runResourceRequest(1)

	if reserved := cache.reserve(cacheSnapshot(run, request, *run), pod, func(corev1.ResourceList) bool { return true }); !reserved {
		t.Fatal("reserve Run = false, want true")
	}
	if got := cache.effectiveUsage(nil, []v1alpha1.Run{*run})[pod.Name][corev1.ResourceName(v1alpha1.RuntimeResourceRuns)]; got.CmpInt64(1) != 0 {
		t.Fatalf("assumed runs usage = %s, want 1", got.String())
	}

	run.Status.Phase = v1alpha1.RunScheduled
	run.Status.AssignedPod = pod.Name
	actual := map[string]corev1.ResourceList{pod.Name: request}
	usage := cache.effectiveUsage(actual, []v1alpha1.Run{*run})
	if got := usage[pod.Name][corev1.ResourceName(v1alpha1.RuntimeResourceRuns)]; got.CmpInt64(1) != 0 {
		t.Fatalf("effective runs usage = %s, want 1", got.String())
	}
}

func TestAssumedReservationCacheReleaseRemovesAssumption(t *testing.T) {
	cache := &assumedReservationCache{}
	pod := &corev1.Pod{}
	pod.Name = "runtime-a"
	run := reservationTestRun("run-a", "run-a-uid")

	if reserved := cache.reserve(cacheSnapshot(run, runResourceRequest(1), *run), pod, func(corev1.ResourceList) bool { return true }); !reserved {
		t.Fatal("reserve Run = false, want true")
	}
	cache.release(run)
	if _, ok := cache.effectiveUsage(nil, []v1alpha1.Run{*run})[pod.Name]; ok {
		t.Fatal("assumed reservation remains after release")
	}
}

func cacheSnapshot(run *v1alpha1.Run, request corev1.ResourceList, runs ...v1alpha1.Run) *schedulingSnapshot {
	return &schedulingSnapshot{
		run:              run,
		request:          request,
		actualUsageByPod: map[string]corev1.ResourceList{},
		runs:             runs,
	}
}

func runResourceRequest(runs int64) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(runs, resource.DecimalSI),
	}
}
