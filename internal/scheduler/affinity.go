package scheduler

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func filterCandidatesByRequiredRunAffinity(run *v1alpha1.Run, candidates []corev1.Pod, runs []v1alpha1.Run) ([]corev1.Pod, error) {
	if run == nil || run.Spec.Affinity == nil {
		return candidates, nil
	}

	filtered := make([]corev1.Pod, 0, len(candidates))
	for i := range candidates {
		pod := &candidates[i]
		matches, err := satisfiesRequiredRunAffinity(run, pod, runs)
		if err != nil {
			return nil, err
		}
		if matches {
			filtered = append(filtered, *pod)
		}
	}
	return filtered, nil
}

func satisfiesRequiredRunAffinity(run *v1alpha1.Run, pod *corev1.Pod, runs []v1alpha1.Run) (bool, error) {
	if run == nil || run.Spec.Affinity == nil {
		return true, nil
	}
	if rules := run.Spec.Affinity.RunAffinity; rules != nil {
		for _, term := range rules.RequiredDuringSchedulingIgnoredDuringExecution {
			matches, err := matchingRunPods(run, term, runs)
			if err != nil {
				return false, err
			}
			if !matches[pod.Name] {
				return false, nil
			}
		}
	}
	if rules := run.Spec.Affinity.RunAntiAffinity; rules != nil {
		for _, term := range rules.RequiredDuringSchedulingIgnoredDuringExecution {
			matches, err := matchingRunPods(run, term, runs)
			if err != nil {
				return false, err
			}
			if matches[pod.Name] {
				return false, nil
			}
		}
	}
	return true, nil
}

func preferredRunAffinityScore(run *v1alpha1.Run, pod *corev1.Pod, runs []v1alpha1.Run) (int32, error) {
	if run == nil || run.Spec.Affinity == nil {
		return 0, nil
	}

	var score int32
	if rules := run.Spec.Affinity.RunAffinity; rules != nil {
		for _, weighted := range rules.PreferredDuringSchedulingIgnoredDuringExecution {
			matches, err := matchingRunPods(run, weighted.RunAffinityTerm, runs)
			if err != nil {
				return 0, err
			}
			if matches[pod.Name] {
				score += weighted.Weight
			}
		}
	}
	if rules := run.Spec.Affinity.RunAntiAffinity; rules != nil {
		for _, weighted := range rules.PreferredDuringSchedulingIgnoredDuringExecution {
			matches, err := matchingRunPods(run, weighted.RunAffinityTerm, runs)
			if err != nil {
				return 0, err
			}
			if !matches[pod.Name] {
				score += weighted.Weight
			}
		}
	}
	return score, nil
}

func matchingRunPods(run *v1alpha1.Run, term v1alpha1.RunAffinityTerm, runs []v1alpha1.Run) (map[string]bool, error) {
	if term.TopologyKey != v1alpha1.RunAffinityTopologyRuntimePod {
		return nil, fmt.Errorf("unsupported run affinity topology key %q", term.TopologyKey)
	}
	if term.LabelSelector == nil {
		return nil, fmt.Errorf("run affinity term has no label selector")
	}
	selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
	if err != nil {
		return nil, fmt.Errorf("parse run affinity label selector: %w", err)
	}

	matches := make(map[string]bool)
	for i := range runs {
		target := &runs[i]
		if target.Namespace != run.Namespace || isSameRun(run, target) || !isActiveAffinityTarget(target) {
			continue
		}
		if selector.Matches(labels.Set(target.Labels)) {
			matches[target.Status.AssignedPod] = true
		}
	}
	return matches, nil
}

func isSameRun(left, right *v1alpha1.Run) bool {
	return left.Namespace == right.Namespace && left.Name == right.Name
}

func isActiveAffinityTarget(run *v1alpha1.Run) bool {
	return run.Status.AssignedPod != "" && consumesRuntimeCapacity(run.Status.Phase)
}
