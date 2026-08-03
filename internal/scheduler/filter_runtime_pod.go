package scheduler

import corev1 "k8s.io/api/core/v1"

type runtimePodAvailabilityFilter struct {
	reconciler *RunReconciler
}

func newRuntimePodAvailabilityFilter(reconciler *RunReconciler, _ *schedulingSnapshot, _ *schedulingPreFilterState) (filterPlugin, error) {
	return &runtimePodAvailabilityFilter{reconciler: reconciler}, nil
}

func (f *runtimePodAvailabilityFilter) Name() string {
	return "RuntimePodAvailability"
}

func (f *runtimePodAvailabilityFilter) Filter(snapshot *schedulingSnapshot, pod *corev1.Pod) filterResult {
	if f.reconciler.isRuntimePodAvailable(pod, snapshot.now, snapshot.usageByPod[pod.Name], snapshot.request) {
		return filterResult{feasible: true}
	}
	return filterResult{reason: filterReasonRuntimePodUnavailable}
}
