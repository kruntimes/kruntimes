package scheduler

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

type affinityTarget struct {
	podName string
	labels  map[string]string
}

type affinityRule struct {
	selector labels.Selector
	weight   int32
	pods     map[string]bool
	seedable bool
}

type runAffinity struct {
	required      []affinityRule
	requiredAnti  []affinityRule
	preferred     []affinityRule
	preferredAnti []affinityRule
}

func newRunAffinity(run *v1alpha1.Run, targets []affinityTarget) (runAffinity, error) {
	if run == nil || run.Spec.Affinity == nil {
		return runAffinity{}, nil
	}
	build := func(term v1alpha1.RunAffinityTerm, weight int32, allowSeed bool) (affinityRule, error) {
		if term.TopologyKey != v1alpha1.RunAffinityTopologyRuntimePod {
			return affinityRule{}, fmt.Errorf("unsupported run affinity topology key %q", term.TopologyKey)
		}
		if term.LabelSelector == nil {
			return affinityRule{}, fmt.Errorf("run affinity term has no label selector")
		}
		selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
		if err != nil {
			return affinityRule{}, fmt.Errorf("parse run affinity selector: %w", err)
		}
		pods := map[string]bool{}
		for _, target := range targets {
			if selector.Matches(labels.Set(target.labels)) {
				pods[target.podName] = true
			}
		}
		return affinityRule{selector: selector, weight: weight, pods: pods, seedable: allowSeed && len(pods) == 0 && selector.Matches(labels.Set(run.Labels))}, nil
	}
	var result runAffinity
	appendRules := func(rules *v1alpha1.RunAffinityRules, anti bool) error {
		if rules == nil {
			return nil
		}
		for _, term := range rules.RequiredDuringSchedulingIgnoredDuringExecution {
			rule, err := build(term, 0, !anti)
			if err != nil {
				return err
			}
			if anti {
				result.requiredAnti = append(result.requiredAnti, rule)
			} else {
				result.required = append(result.required, rule)
			}
		}
		for _, weighted := range rules.PreferredDuringSchedulingIgnoredDuringExecution {
			rule, err := build(weighted.RunAffinityTerm, weighted.Weight, false)
			if err != nil {
				return err
			}
			if anti {
				result.preferredAnti = append(result.preferredAnti, rule)
			} else {
				result.preferred = append(result.preferred, rule)
			}
		}
		return nil
	}
	if err := appendRules(run.Spec.Affinity.RunAffinity, false); err != nil {
		return runAffinity{}, err
	}
	if err := appendRules(run.Spec.Affinity.RunAntiAffinity, true); err != nil {
		return runAffinity{}, err
	}
	return result, nil
}

func (a runAffinity) matchesRequired(podName string) bool {
	for _, rule := range a.required {
		if !rule.pods[podName] && !rule.seedable {
			return false
		}
	}
	for _, rule := range a.requiredAnti {
		if rule.pods[podName] {
			return false
		}
	}
	return true
}

func (a runAffinity) preferredCandidates(candidates []corev1.Pod) []corev1.Pod {
	if len(candidates) < 2 || (len(a.preferred) == 0 && len(a.preferredAnti) == 0) {
		return candidates
	}
	var best int32
	scores := make([]int32, len(candidates))
	for i, pod := range candidates {
		for _, rule := range a.preferred {
			if rule.pods[pod.Name] {
				scores[i] += rule.weight
			}
		}
		for _, rule := range a.preferredAnti {
			if !rule.pods[pod.Name] {
				scores[i] += rule.weight
			}
		}
		if i == 0 || scores[i] > best {
			best = scores[i]
		}
	}
	selected := make([]corev1.Pod, 0, len(candidates))
	for i, pod := range candidates {
		if scores[i] == best {
			selected = append(selected, pod)
		}
	}
	return selected
}

func isActiveAffinityTarget(run *v1alpha1.Run) bool {
	if run == nil || run.Status.AssignedPod == "" {
		return false
	}
	switch run.Status.Phase {
	case v1alpha1.RunScheduled, v1alpha1.RunRunning, v1alpha1.RunReady:
		return true
	default:
		return false
	}
}
