package scheduler

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runstatus"
)

// reservationKey identifies an assumed assignment by the immutable Run UID.
// The namespace is retained because the scheduler serves multiple namespaces.
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

// effectiveUsage combines observed assignments with local assumptions. It
// also removes assumptions once the informer has observed their corresponding
// assignment, terminal state, or a different assignment.
func (r *RunReconciler) effectiveUsage(actual map[string]corev1.ResourceList, runs []v1alpha1.Run) map[string]corev1.ResourceList {
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	r.syncAssumptionsLocked(runs)
	return mergeUsage(actual, r.assumedUsageLocked())
}

func (r *RunReconciler) reserve(snapshot *schedulingSnapshot, pod *corev1.Pod) bool {
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()

	r.syncAssumptionsLocked(snapshot.runs)
	usage := mergeUsage(snapshot.actualUsageByPod, r.assumedUsageLocked())
	if !r.isRuntimePodAvailable(pod, snapshot.now, usage[pod.Name], snapshot.request) {
		return false
	}
	if r.assumed == nil {
		r.assumed = make(map[reservationKey]assumedReservation)
	}
	r.assumed[reservationKeyFor(snapshot.run)] = assumedReservation{
		runName: snapshot.run.Name,
		podName: pod.Name,
		request: cloneResourceList(snapshot.request),
	}
	return true
}

// bind persists an assumed assignment. The assumption remains in the local
// cache until an informer-observed Run assignment takes over its accounting.
func (r *RunReconciler) bind(ctx context.Context, run *v1alpha1.Run, pod *corev1.Pod) error {
	run.Status.AssignedPod = pod.Name
	run.Status.AssignedPodUID = string(pod.UID)
	run.Status.Phase = v1alpha1.RunScheduled
	scheduledAt := metav1.Now()
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               runstatus.ConditionScheduled,
		Status:             metav1.ConditionTrue,
		Reason:             "Assigned",
		Message:            "assigned to runtime pod " + pod.Name,
		LastTransitionTime: scheduledAt,
	})
	if err := r.Status().Update(ctx, run); err != nil {
		r.releaseReservation(run)
		return err
	}
	return nil
}

func (r *RunReconciler) observeRun(run *v1alpha1.Run) {
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	key := reservationKeyFor(run)
	_, ok := r.assumed[key]
	if !ok {
		return
	}
	if run.Status.AssignedPod != "" || !isPendingRun(run) {
		delete(r.assumed, key)
	}
}

func (r *RunReconciler) releaseReservation(run *v1alpha1.Run) {
	r.reservationMu.Lock()
	defer r.reservationMu.Unlock()
	delete(r.assumed, reservationKeyFor(run))
}

func (r *RunReconciler) syncAssumptionsLocked(runs []v1alpha1.Run) {
	if len(r.assumed) == 0 {
		return
	}
	byKey := make(map[reservationKey]*v1alpha1.Run, len(runs))
	for i := range runs {
		byKey[reservationKeyFor(&runs[i])] = &runs[i]
	}
	for key, reservation := range r.assumed {
		run := byKey[key]
		if run == nil || !isPendingRun(run) || run.Status.AssignedPod != "" || run.Name != reservation.runName {
			delete(r.assumed, key)
		}
	}
}

func (r *RunReconciler) assumedUsageLocked() map[string]corev1.ResourceList {
	usage := make(map[string]corev1.ResourceList)
	for _, reservation := range r.assumed {
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
