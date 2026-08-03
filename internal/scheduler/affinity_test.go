package scheduler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestRunAffinityFiltersActualTargetsAndScoresPreferences(t *testing.T) {
	run := affinityTestRun("next", map[string]string{"stage": "test"}, &v1alpha1.RunAffinity{
		RunAffinity: &v1alpha1.RunAffinityRules{
			RequiredDuringSchedulingIgnoredDuringExecution:  []v1alpha1.RunAffinityTerm{affinityTestTerm("stage", "build")},
			PreferredDuringSchedulingIgnoredDuringExecution: []v1alpha1.WeightedRunAffinityTerm{{Weight: 10, RunAffinityTerm: affinityTestTerm("zone", "blue")}},
		},
		RunAntiAffinity: &v1alpha1.RunAffinityRules{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{affinityTestTerm("blocked", "true")},
		},
	})
	affinity, err := newRunAffinity(run, []affinityTarget{
		{podName: "pod-a", labels: map[string]string{"stage": "build", "zone": "blue"}},
		{podName: "pod-b", labels: map[string]string{"stage": "build", "blocked": "true"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !affinity.filter("pod-a").feasible || affinity.filter("pod-b").feasible || affinity.filter("pod-c").feasible {
		t.Fatalf("required filter results are incorrect")
	}
	candidates := affinity.preferredCandidates([]corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}}, {ObjectMeta: metav1.ObjectMeta{Name: "pod-c"}}})
	if len(candidates) != 1 || candidates[0].Name != "pod-a" {
		t.Fatalf("preferred candidates = %#v, want pod-a", candidates)
	}
}

func TestRunAffinityBootstrapRequiresMatchingRunLabel(t *testing.T) {
	term := affinityTestTerm("cohort", "build")
	matching := affinityTestRun("first", map[string]string{"cohort": "build"}, &v1alpha1.RunAffinity{RunAffinity: &v1alpha1.RunAffinityRules{RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{term}}})
	affinity, err := newRunAffinity(matching, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !affinity.filter("pod-a").feasible {
		t.Fatal("matching first cohort member should seed required affinity")
	}

	nonMatching := affinityTestRun("other", map[string]string{"cohort": "test"}, matching.Spec.Affinity)
	affinity, err = newRunAffinity(nonMatching, nil)
	if err != nil {
		t.Fatal(err)
	}
	if affinity.filter("pod-a").feasible {
		t.Fatal("non-matching Run must not seed required affinity")
	}
}

func TestAssumedReservationAffinityTarget(t *testing.T) {
	cache := &assumedReservationCache{}
	run := affinityTestRun("build", map[string]string{"stage": "build"}, nil)
	run.UID = types.UID("build-uid")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "runtime-a"}}
	if !cache.reserve(cacheSnapshot(run, runResourceRequest(1), *run), pod, func(corev1.ResourceList) bool { return true }) {
		t.Fatal("reserve = false")
	}
	targets := cache.affinityTargets([]v1alpha1.Run{*run})
	if len(targets) != 1 || targets[0].podName != pod.Name || targets[0].labels["stage"] != "build" {
		t.Fatalf("targets = %#v", targets)
	}
}

func affinityTestRun(name string, labels map[string]string, affinity *v1alpha1.RunAffinity) *v1alpha1.Run {
	return &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels}, Spec: v1alpha1.RunSpec{Affinity: affinity}}
}

func affinityTestTerm(key, value string) v1alpha1.RunAffinityTerm {
	return v1alpha1.RunAffinityTerm{TopologyKey: v1alpha1.RunAffinityTopologyRuntimePod, LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{key: value}}}
}
