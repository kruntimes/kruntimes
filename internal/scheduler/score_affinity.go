package scheduler

import "fmt"

import corev1 "k8s.io/api/core/v1"

type preferredRunAffinityScore struct {
	affinity runAffinity
}

func newPreferredRunAffinityScore(_ *RunReconciler, _ *schedulingSnapshot, preFilter *schedulingPreFilterState) (scorePlugin, error) {
	return &preferredRunAffinityScore{affinity: preFilter.affinity}, nil
}

func (s *preferredRunAffinityScore) Name() string {
	return "PreferredRunAffinity"
}

func (s *preferredRunAffinityScore) Score(_ *schedulingSnapshot, pod *corev1.Pod) (int64, error) {
	if pod == nil {
		return 0, fmt.Errorf("nil pod")
	}
	return s.affinity.preferredScore(pod.Name), nil
}

func (s *preferredRunAffinityScore) NormalizeScores(_ *schedulingSnapshot, scores []podScore) error {
	return normalizeScoresByMaximum(scores)
}
