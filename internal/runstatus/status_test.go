package runstatus

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestSetTerminalSetsCommonFieldsAndConditions(t *testing.T) {
	run := &v1alpha1.Run{}
	now := metav1.NewTime(time.Date(2026, time.June, 12, 12, 0, 0, 0, time.UTC))

	SetTerminal(run, v1alpha1.RunFailed, "PodGone", "assigned pod was deleted", now)

	if run.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", run.Status.Phase)
	}
	if run.Status.Message != "assigned pod was deleted" {
		t.Fatalf("message = %q", run.Status.Message)
	}
	if run.Status.CompletionTime == nil || !run.Status.CompletionTime.Equal(&now) {
		t.Fatalf("completionTime = %v, want %v", run.Status.CompletionTime, now)
	}
	for _, conditionType := range []string{ConditionRunning, ConditionCompleted} {
		condition := meta.FindStatusCondition(run.Status.Conditions, conditionType)
		if condition == nil ||
			condition.Status != metav1.ConditionFalse ||
			condition.Reason != "PodGone" ||
			condition.Message != "assigned pod was deleted" {
			t.Fatalf("%s condition = %#v", conditionType, condition)
		}
	}
}
