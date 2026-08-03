package scheduler

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestPlanSchedulingCycleRecordsFilterRejection(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name            string
		run             *v1alpha1.Run
		pods            []corev1.Pod
		affinityTargets []affinityTarget
		plugin          string
		reason          filterReason
		wantCount       int
	}{
		{
			name:      "unavailable Runtime Pod",
			run:       &v1alpha1.Run{Spec: v1alpha1.RunSpec{Runtime: "bash"}},
			pods:      []corev1.Pod{{}, {}},
			plugin:    "RuntimePodAvailability",
			reason:    filterReasonRuntimePodUnavailable,
			wantCount: 2,
		},
		{
			name: "unsatisfied required Run affinity",
			run: affinityTestRun("candidate", map[string]string{"stage": "test"}, &v1alpha1.RunAffinity{
				RunAffinity: &v1alpha1.RunAffinityRules{
					RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{affinityTestTerm("stage", "build")},
				},
			}),
			pods:            []corev1.Pod{readyAffinityPod("runtime-a", now)},
			affinityTargets: []affinityTarget{{podName: "runtime-a", labels: map[string]string{"stage": "test"}}},
			plugin:          "RunAffinity",
			reason:          filterReasonRunAffinity,
			wantCount:       1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run.Spec.Runtime = "bash"
			request, err := tt.run.Spec.ResourceRequests()
			if err != nil {
				t.Fatalf("resource requests: %v", err)
			}
			registry := prometheus.NewPedanticRegistry()
			reconciler := &RunReconciler{
				RuntimedHeartbeatStaleAfter: time.Minute,
				metrics:                     newSchedulerMetrics(registry),
			}
			plan, err := reconciler.planSchedulingCycle(&schedulingSnapshot{
				run:             tt.run,
				request:         request,
				pods:            tt.pods,
				affinityTargets: tt.affinityTargets,
				now:             now,
			})
			if err != nil {
				t.Fatalf("plan scheduling cycle: %v", err)
			}
			if plan.action != schedulingPlanWait {
				t.Fatalf("plan action = %q, want %q", plan.action, schedulingPlanWait)
			}

			want := `# HELP kruntimes_scheduler_filter_rejections_total Total Runtime Pod evaluations rejected by scheduler Filter plugins.
# TYPE kruntimes_scheduler_filter_rejections_total counter
kruntimes_scheduler_filter_rejections_total{plugin="` + tt.plugin + `",reason="` + string(tt.reason) + `"} ` + strconv.Itoa(tt.wantCount) + `
`
			if err := testutil.GatherAndCompare(
				registry,
				strings.NewReader(want),
				"kruntimes_scheduler_filter_rejections_total",
			); err != nil {
				t.Fatalf("filter rejection metric: %v", err)
			}
		})
	}
}
