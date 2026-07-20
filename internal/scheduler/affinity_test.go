package scheduler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestFilterCandidatesByRequiredRunAffinity(t *testing.T) {
	run := affinityRun("next", v1alpha1.RunAffinity{
		RunAffinity: &v1alpha1.RunAffinityRules{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{affinityTerm("stage", "build")},
		},
	})
	runs := []v1alpha1.Run{
		activeAffinityRun("build", "pod-a", map[string]string{"stage": "build"}),
		activeAffinityRun("other", "pod-b", map[string]string{"stage": "other"}),
	}
	candidates, err := filterCandidatesByRequiredRunAffinity(run, affinityPods(), runs)
	if err != nil {
		t.Fatalf("filter candidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Name != "pod-a" {
		t.Fatalf("candidates = %#v, want pod-a only", candidates)
	}
}

func TestFilterCandidatesByRequiredRunAntiAffinity(t *testing.T) {
	run := affinityRun("next", v1alpha1.RunAffinity{
		RunAntiAffinity: &v1alpha1.RunAffinityRules{
			RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{affinityTerm("stage", "build")},
		},
	})
	runs := []v1alpha1.Run{activeAffinityRun("build", "pod-a", map[string]string{"stage": "build"})}
	candidates, err := filterCandidatesByRequiredRunAffinity(run, affinityPods(), runs)
	if err != nil {
		t.Fatalf("filter candidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].Name != "pod-b" {
		t.Fatalf("candidates = %#v, want pod-b only", candidates)
	}
}

func TestPreferredRunAffinityScoresBeforeLeastLoaded(t *testing.T) {
	run := affinityRun("next", v1alpha1.RunAffinity{
		RunAffinity: &v1alpha1.RunAffinityRules{
			PreferredDuringSchedulingIgnoredDuringExecution: []v1alpha1.WeightedRunAffinityTerm{{
				Weight:          100,
				RunAffinityTerm: affinityTerm("stage", "build"),
			}},
		},
	})
	runs := []v1alpha1.Run{activeAffinityRun("build", "pod-a", map[string]string{"stage": "build"})}
	pod, err := (&LeastLoaded{}).Select(context.Background(), affinityPods(), run, runs)
	if err != nil {
		t.Fatalf("select pod: %v", err)
	}
	if pod.Name != "pod-a" {
		t.Fatalf("selected pod = %q, want preferred affinity pod-a", pod.Name)
	}
}

func TestMatchingRunPodsIgnoresTerminalAndSelfRuns(t *testing.T) {
	run := affinityRun("next", v1alpha1.RunAffinity{})
	term := affinityTerm("stage", "build")
	runs := []v1alpha1.Run{
		activeAffinityRun("next", "pod-a", map[string]string{"stage": "build"}),
		{
			ObjectMeta: metav1.ObjectMeta{Name: "finished", Namespace: "default", Labels: map[string]string{"stage": "build"}},
			Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunSucceeded, AssignedPod: "pod-b"},
		},
	}
	matches, err := matchingRunPods(run, term, runs)
	if err != nil {
		t.Fatalf("matching run pods: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("matches = %#v, want no terminal or self targets", matches)
	}
}

func affinityRun(name string, affinity v1alpha1.RunAffinity) *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1alpha1.RunSpec{Affinity: &affinity},
	}
}

func activeAffinityRun(name, pod string, labels map[string]string) v1alpha1.Run {
	return v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, AssignedPod: pod},
	}
}

func affinityTerm(label, value string) v1alpha1.RunAffinityTerm {
	return v1alpha1.RunAffinityTerm{
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{label: value}},
		TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
	}
}

func affinityPods() []corev1.Pod {
	return []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}},
	}
}
