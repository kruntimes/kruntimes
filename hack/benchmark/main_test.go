package main

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runstatus"
)

func TestSummarize(t *testing.T) {
	got := summarize([]time.Duration{
		100 * time.Millisecond,
		10 * time.Millisecond,
		50 * time.Millisecond,
	})

	if got.Count != 3 {
		t.Fatalf("Count = %d, want 3", got.Count)
	}
	if got.MinMS != 10 {
		t.Fatalf("MinMS = %v, want 10", got.MinMS)
	}
	if got.P50MS != 50 {
		t.Fatalf("P50MS = %v, want 50", got.P50MS)
	}
	if got.MaxMS != 100 {
		t.Fatalf("MaxMS = %v, want 100", got.MaxMS)
	}
}

func TestPercentileInterpolates(t *testing.T) {
	values := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		30 * time.Millisecond,
	}
	got := percentile(values, 0.95)
	want := 29 * time.Millisecond
	if got != want {
		t.Fatalf("percentile = %v, want %v", got, want)
	}
}

func TestBuildReportSeparatesExecutionFromEndToEndLatency(t *testing.T) {
	createdAt := time.Unix(100, 0)
	startedAt := createdAt.Add(2 * time.Second)
	finishedAt := startedAt.Add(500 * time.Millisecond)
	observations := map[string]*runObservation{
		"run-1": {
			CreatedAt:   createdAt,
			ScheduledAt: createdAt.Add(time.Second),
			StartedAt:   startedAt,
			FinishedAt:  finishedAt,
			Phase:       v1alpha1.RunSucceeded,
		},
	}

	report := buildReport(
		options{Runs: 1, RuntimeName: "benchmark-bash"},
		"bench-test",
		createdAt,
		finishedAt,
		observations,
		nil,
		nil,
		nil,
		capacityReport{},
		nil,
	)

	if report.Latency.Execution.Count != 1 || report.Latency.Execution.P50MS != 500 {
		t.Fatalf("execution latency = %#v, want one 500ms sample", report.Latency.Execution)
	}
	if report.Latency.Complete.Count != 1 || report.Latency.Complete.P50MS != 2500 {
		t.Fatalf("complete latency = %#v, want one 2500ms sample", report.Latency.Complete)
	}
}

func TestObserveRunLifecycleUsesStatusTimestamps(t *testing.T) {
	createdAt := time.Unix(100, 0)
	scheduledAt := metav1.NewTime(createdAt.Add(100 * time.Millisecond))
	startedAt := metav1.NewTime(createdAt.Add(200 * time.Millisecond))
	completedAt := metav1.NewTime(createdAt.Add(500 * time.Millisecond))
	obs := &runObservation{CreatedAt: createdAt}
	run := &v1alpha1.Run{Status: v1alpha1.RunStatus{
		Phase:          v1alpha1.RunSucceeded,
		StartTime:      &startedAt,
		CompletionTime: &completedAt,
		Conditions: []metav1.Condition{{
			Type:               runstatus.ConditionScheduled,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: scheduledAt,
		}},
	}}

	observeRunLifecycle(obs, run)

	if !obs.ScheduledAt.Equal(scheduledAt.Time) || !obs.StartedAt.Equal(startedAt.Time) || !obs.FinishedAt.Equal(completedAt.Time) {
		t.Fatalf("observation = %#v, want status timestamps", obs)
	}
}
