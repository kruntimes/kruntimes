package runstatus

import (
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	ConditionRunning   = "Running"
	ConditionCompleted = "Completed"
)

// SetTerminal applies the common status fields and lifecycle conditions for a
// terminal Run transition.
func SetTerminal(run *v1alpha1.Run, phase v1alpha1.RunPhase, reason, message string, now metav1.Time) {
	run.Status.Phase = phase
	run.Status.Message = message
	run.Status.CompletionTime = &now
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: ConditionRunning, Status: metav1.ConditionFalse, Reason: reason, Message: message,
	})
	meta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type: ConditionCompleted, Status: metav1.ConditionFalse, Reason: reason, Message: message,
	})
}
