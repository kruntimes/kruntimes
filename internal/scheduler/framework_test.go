package scheduler

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

func TestPlanSchedulingCycle(t *testing.T) {
	now := time.Now()
	run := &v1alpha1.Run{Spec: v1alpha1.RunSpec{Runtime: "bash"}}
	request, err := run.Spec.ResourceRequests()
	if err != nil {
		t.Fatalf("resource requests: %v", err)
	}
	reconciler := &RunReconciler{
		Strategy:                    &LeastLoaded{},
		RuntimedHeartbeatStaleAfter: time.Minute,
	}

	plan, err := reconciler.planSchedulingCycle(&schedulingSnapshot{
		run:     run,
		request: request,
		now:     now,
	})
	if err != nil {
		t.Fatalf("plan empty snapshot: %v", err)
	}
	if plan.action != schedulingPlanWait || plan.selected != nil {
		t.Fatalf("empty plan = %#v, want Wait without a selected Pod", plan)
	}

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "runtime-a",
			Annotations: map[string]string{
				runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "2",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue, LastProbeTime: metav1.NewTime(now)},
			},
		},
	}
	plan, err = reconciler.planSchedulingCycle(&schedulingSnapshot{
		run:     run,
		request: request,
		pods:    []corev1.Pod{pod},
		now:     now,
	})
	if err != nil {
		t.Fatalf("plan schedulable snapshot: %v", err)
	}
	if plan.action != schedulingPlanBind || plan.selected == nil || plan.selected.Name != pod.Name {
		t.Fatalf("schedulable plan = %#v, want Bind for %q", plan, pod.Name)
	}
}

func TestPlanSchedulingCycleHonorsRunAffinity(t *testing.T) {
	now := time.Now()
	run := affinityTestRun("next", map[string]string{"stage": "test"}, &v1alpha1.RunAffinity{
		RunAffinity: &v1alpha1.RunAffinityRules{
			RequiredDuringSchedulingIgnoredDuringExecution:  []v1alpha1.RunAffinityTerm{affinityTestTerm("stage", "build")},
			PreferredDuringSchedulingIgnoredDuringExecution: []v1alpha1.WeightedRunAffinityTerm{{Weight: 1, RunAffinityTerm: affinityTestTerm("zone", "blue")}},
		},
	})
	run.Spec.Runtime = "bash"
	request, err := run.Spec.ResourceRequests()
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &RunReconciler{Strategy: &LeastLoaded{}, RuntimedHeartbeatStaleAfter: time.Minute}
	pods := []corev1.Pod{readyAffinityPod("runtime-a", now), readyAffinityPod("runtime-b", now)}

	plan, err := reconciler.planSchedulingCycle(&schedulingSnapshot{
		run:     run,
		request: request,
		pods:    pods,
		now:     now,
		affinityTargets: []affinityTarget{
			{podName: "runtime-a", labels: map[string]string{"stage": "build", "zone": "blue"}},
			{podName: "runtime-b", labels: map[string]string{"stage": "build"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.action != schedulingPlanBind || plan.selected == nil || plan.selected.Name != "runtime-a" {
		t.Fatalf("plan = %#v, want Bind on preferred affinity pod runtime-a", plan)
	}
}

func TestPlanSchedulingCycleReportsAffinityRejection(t *testing.T) {
	now := time.Now()
	run := affinityTestRun("next", map[string]string{"stage": "test"}, &v1alpha1.RunAffinity{
		RunAffinity: &v1alpha1.RunAffinityRules{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{affinityTestTerm("stage", "build")},
		},
	})
	run.Spec.Runtime = "bash"
	request, err := run.Spec.ResourceRequests()
	if err != nil {
		t.Fatal(err)
	}
	reconciler := &RunReconciler{Strategy: &LeastLoaded{}, RuntimedHeartbeatStaleAfter: time.Minute}
	plan, err := reconciler.planSchedulingCycle(&schedulingSnapshot{
		run:             run,
		request:         request,
		pods:            []corev1.Pod{readyAffinityPod("runtime-a", now)},
		now:             now,
		affinityTargets: []affinityTarget{{podName: "runtime-a", labels: map[string]string{"stage": "test"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.action != schedulingPlanWait || plan.rejections[filterReasonRunAffinity] != 1 {
		t.Fatalf("plan = %#v, want one RunAffinity rejection", plan)
	}
	if got := plan.waitingMessage(run.Spec.Runtime); got != `waiting for available runtime pods satisfying required Run affinity for runtime "bash"` {
		t.Fatalf("waiting message = %q", got)
	}
}

func TestRegisteredFilterPlugins(t *testing.T) {
	reconciler := &RunReconciler{}
	snapshot := &schedulingSnapshot{run: &v1alpha1.Run{}}
	preFilter, err := reconciler.preFilter(snapshot)
	if err != nil {
		t.Fatalf("preFilter: %v", err)
	}
	plugins, err := reconciler.registeredFilterPlugins(snapshot, preFilter)
	if err != nil {
		t.Fatalf("registeredFilterPlugins: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("registered plugins = %d, want 2", len(plugins))
	}
	if plugins[0].Name() != "RuntimePodAvailability" || plugins[1].Name() != "RunAffinity" {
		t.Fatalf("registered plugin order = %q, %q", plugins[0].Name(), plugins[1].Name())
	}
}

func readyAffinityPod(name string, now time.Time) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "2"}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			{Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue, LastProbeTime: metav1.NewTime(now)},
		}},
	}
}
