package scheduler

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runstatus"
)

// effectiveUsage combines observed assignments with local assumptions. It
// also removes assumptions once the informer has observed their corresponding
// assignment, terminal state, or a different assignment.
func (r *RunReconciler) effectiveUsage(actual map[string]corev1.ResourceList, runs []v1alpha1.Run) map[string]corev1.ResourceList {
	return r.assumptions.effectiveUsage(actual, runs)
}

func (r *RunReconciler) reserve(snapshot *schedulingSnapshot, pod *corev1.Pod) bool {
	reserved := r.assumptions.reserve(snapshot, pod, func(usage corev1.ResourceList) bool {
		return r.isRuntimePodAvailable(pod, snapshot.now, usage, snapshot.request)
	})
	if !reserved {
		r.metricsRecorder().observeReservationConflict(reservationConflictStageReserve)
	}
	return reserved
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
		if apierrors.IsConflict(err) {
			r.metricsRecorder().observeReservationConflict(reservationConflictStageBind)
		}
		return err
	}
	return nil
}

func (r *RunReconciler) observeRun(run *v1alpha1.Run) {
	r.assumptions.observe(run)
}

func (r *RunReconciler) releaseReservation(run *v1alpha1.Run) {
	r.assumptions.release(run)
}
