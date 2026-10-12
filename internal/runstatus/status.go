package runstatus

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	ConditionScheduled = "Scheduled"
	ConditionRunning   = "Running"
	// ConditionRuntimeAccepted confirms that the Runtime Server has created an
	// execution for a task Run. Running=True only means runtimed has claimed
	// the Run and may still be dispatching Execute.
	ConditionRuntimeAccepted = "RuntimeAccepted"
	ConditionReady           = "Ready"
	ConditionCompleted       = "Completed"
)

// SetTerminal applies the common status fields and lifecycle conditions for a
// terminal Run transition.
func SetTerminal(run *v1alpha1.Run, phase v1alpha1.RunPhase, reason, message string, now metav1.Time) {
	run.Status.Phase = phase
	run.Status.Message = message
	run.Status.CompletionTime = &now
	if meta.FindStatusCondition(run.Status.Conditions, ConditionReady) != nil {
		meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type: ConditionReady, Status: metav1.ConditionFalse, Reason: reason, Message: message,
		})
	}
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: ConditionRunning, Status: metav1.ConditionFalse, Reason: reason, Message: message,
	})
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: ConditionCompleted, Status: metav1.ConditionFalse, Reason: reason, Message: message,
	})
}
