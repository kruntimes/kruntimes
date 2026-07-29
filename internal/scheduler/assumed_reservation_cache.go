package scheduler

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// assumedReservationCache owns assignments made after planning and before the
// Run informer observes the persisted assignment. It keeps cleanup, resource
// accounting, availability checks, and insertion in one critical section.
type assumedReservationCache struct {
	mu           sync.Mutex
	reservations map[reservationKey]assumedReservation
}

// reservationKey identifies an assumed assignment by immutable Run UID. The
// namespace is retained because the scheduler serves multiple namespaces.
type reservationKey struct {
	namespace string
	runUID    types.UID
}

type assumedReservation struct {
	runName string
	podName string
	request corev1.ResourceList
}

func reservationKeyFor(run *v1alpha1.Run) reservationKey {
	return reservationKey{namespace: run.Namespace, runUID: run.UID}
}

func (c *assumedReservationCache) effectiveUsage(actual map[string]corev1.ResourceList, runs []v1alpha1.Run) map[string]corev1.ResourceList {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.syncLocked(runs)
	return mergeUsage(actual, c.usageLocked())
}

func (c *assumedReservationCache) reserve(snapshot *schedulingSnapshot, pod *corev1.Pod, available func(corev1.ResourceList) bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.syncLocked(snapshot.runs)
	usage := mergeUsage(snapshot.actualUsageByPod, c.usageLocked())
	if !available(usage[pod.Name]) {
		return false
	}
	c.assumeLocked(snapshot.run, pod.Name, snapshot.request)
	return true
}

func (c *assumedReservationCache) observe(run *v1alpha1.Run) {
	c.mu.Lock()
	defer c.mu.Unlock()

	key := reservationKeyFor(run)
	if _, ok := c.reservations[key]; ok && (run.Status.AssignedPod != "" || !isPendingRun(run)) {
		delete(c.reservations, key)
	}
}

func (c *assumedReservationCache) release(run *v1alpha1.Run) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reservations, reservationKeyFor(run))
}

func (c *assumedReservationCache) assumeLocked(run *v1alpha1.Run, podName string, request corev1.ResourceList) {
	if c.reservations == nil {
		c.reservations = make(map[reservationKey]assumedReservation)
	}
	c.reservations[reservationKeyFor(run)] = assumedReservation{
		runName: run.Name,
		podName: podName,
		request: cloneResourceList(request),
	}
}

func (c *assumedReservationCache) syncLocked(runs []v1alpha1.Run) {
	if len(c.reservations) == 0 {
		return
	}
	byKey := make(map[reservationKey]*v1alpha1.Run, len(runs))
	for i := range runs {
		byKey[reservationKeyFor(&runs[i])] = &runs[i]
	}
	for key, reservation := range c.reservations {
		run := byKey[key]
		if run == nil || !isPendingRun(run) || run.Status.AssignedPod != "" || run.Name != reservation.runName {
			delete(c.reservations, key)
		}
	}
}

func (c *assumedReservationCache) usageLocked() map[string]corev1.ResourceList {
	usage := make(map[string]corev1.ResourceList)
	for _, reservation := range c.reservations {
		addUsage(usage, reservation.podName, reservation.request)
	}
	return usage
}

func isPendingRun(run *v1alpha1.Run) bool {
	return run.Status.Phase == "" || run.Status.Phase == v1alpha1.RunPending
}

func mergeUsage(first, second map[string]corev1.ResourceList) map[string]corev1.ResourceList {
	usage := make(map[string]corev1.ResourceList, len(first)+len(second))
	for podName, resources := range first {
		usage[podName] = cloneResourceList(resources)
	}
	for podName, resources := range second {
		addUsage(usage, podName, resources)
	}
	return usage
}

func addUsage(usage map[string]corev1.ResourceList, podName string, request corev1.ResourceList) {
	resources := usage[podName]
	if resources == nil {
		resources = corev1.ResourceList{}
		usage[podName] = resources
	}
	for resourceName, quantity := range request {
		total := resources[resourceName].DeepCopy()
		total.Add(quantity)
		resources[resourceName] = total
	}
}

func cloneResourceList(resources corev1.ResourceList) corev1.ResourceList {
	if resources == nil {
		return nil
	}
	clone := make(corev1.ResourceList, len(resources))
	for name, quantity := range resources {
		clone[name] = quantity.DeepCopy()
	}
	return clone
}
