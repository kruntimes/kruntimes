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

	requiredMatches, antiMatches, err := requiredRunAffinityMatchSets(run, runs)
	if err != nil {
		return nil, err
	}

	filtered := make([]corev1.Pod, 0, len(candidates))
	for i := range candidates {
		pod := &candidates[i]
		if satisfiesRequiredRunAffinity(pod, requiredMatches, antiMatches) {
			filtered = append(filtered, *pod)
		}
	}
	return filtered, nil
}

func requiredRunAffinityMatchSets(run *v1alpha1.Run, runs []v1alpha1.Run) ([]map[string]bool, []map[string]bool, error) {
	if run == nil || run.Spec.Affinity == nil {
		return nil, nil, nil
	}

	var requiredMatches, antiMatches []map[string]bool
	if rules := run.Spec.Affinity.RunAffinity; rules != nil {
		requiredMatches = make([]map[string]bool, 0, len(rules.RequiredDuringSchedulingIgnoredDuringExecution))
		for _, term := range rules.RequiredDuringSchedulingIgnoredDuringExecution {
			matches, err := matchingRunPods(run, term, runs)
			if err != nil {
				return nil, nil, err
			}
			requiredMatches = append(requiredMatches, matches)
		}
	}
	if rules := run.Spec.Affinity.RunAntiAffinity; rules != nil {
		antiMatches = make([]map[string]bool, 0, len(rules.RequiredDuringSchedulingIgnoredDuringExecution))
		for _, term := range rules.RequiredDuringSchedulingIgnoredDuringExecution {
			matches, err := matchingRunPods(run, term, runs)
			if err != nil {
				return nil, nil, err
			}
			antiMatches = append(antiMatches, matches)
		}
	}
	return requiredMatches, antiMatches, nil
}

func satisfiesRequiredRunAffinity(pod *corev1.Pod, requiredMatches, antiMatches []map[string]bool) bool {
	for _, matches := range requiredMatches {
		if !matches[pod.Name] {
			return false
		}
	}
	for _, matches := range antiMatches {
		if matches[pod.Name] {
			return false
		}
	}
	return true
}

type weightedRunAffinityMatches struct {
	weight int32
	pods   map[string]bool
}

func preferredRunAffinityMatchSets(run *v1alpha1.Run, runs []v1alpha1.Run) ([]weightedRunAffinityMatches, []weightedRunAffinityMatches, error) {
	if run == nil || run.Spec.Affinity == nil {
		return nil, nil, nil
	}

	var preferredMatches, antiMatches []weightedRunAffinityMatches
	if rules := run.Spec.Affinity.RunAffinity; rules != nil {
		preferredMatches = make([]weightedRunAffinityMatches, 0, len(rules.PreferredDuringSchedulingIgnoredDuringExecution))
		for _, weighted := range rules.PreferredDuringSchedulingIgnoredDuringExecution {
			matches, err := matchingRunPods(run, weighted.RunAffinityTerm, runs)
			if err != nil {
				return nil, nil, err
			}
			preferredMatches = append(preferredMatches, weightedRunAffinityMatches{weight: weighted.Weight, pods: matches})
		}
	}
	if rules := run.Spec.Affinity.RunAntiAffinity; rules != nil {
		antiMatches = make([]weightedRunAffinityMatches, 0, len(rules.PreferredDuringSchedulingIgnoredDuringExecution))
		for _, weighted := range rules.PreferredDuringSchedulingIgnoredDuringExecution {
			matches, err := matchingRunPods(run, weighted.RunAffinityTerm, runs)
			if err != nil {
				return nil, nil, err
			}
			antiMatches = append(antiMatches, weightedRunAffinityMatches{weight: weighted.Weight, pods: matches})
		}
	}
	return preferredMatches, antiMatches, nil
}

func preferredRunAffinityScoreForMatches(pod *corev1.Pod, preferredMatches, antiMatches []weightedRunAffinityMatches) int32 {
	var score int32
	for _, matches := range preferredMatches {
		if matches.pods[pod.Name] {
			score += matches.weight
		}
	}
	for _, matches := range antiMatches {
		if !matches.pods[pod.Name] {
			score += matches.weight
		}
	}
	return score
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
