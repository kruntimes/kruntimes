package scheduler

import (
	"fmt"
	"math/big"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

// LeastLoaded selects the Pod with the lowest projected resource utilization.
type LeastLoaded struct{}

func (s *LeastLoaded) Name() string { return "least-loaded" }

func (s *LeastLoaded) Select(candidates []corev1.Pod, usageByPod map[string]corev1.ResourceList, run *v1alpha1.Run) (*corev1.Pod, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidate pods")
	}
	request, err := run.Spec.ResourceRequests()
	if err != nil {
		return nil, fmt.Errorf("get run resource requests: %w", err)
	}

	type podLoad struct {
		pod   *corev1.Pod
		score resourceScore
	}

	pods := make([]podLoad, 0, len(candidates))
	for i := range candidates {
		pod := &candidates[i]
		if pod.DeletionTimestamp != nil {
			continue
		}

		score, err := resourceCapacityScore(
			runtimepod.Capacity(pod, defaultRuntimePodCapacity()),
			usageByPod[pod.Name],
			request,
		)
		if err != nil {
			return nil, fmt.Errorf("score pod %q: %w", pod.Name, err)
		}
		pods = append(pods, podLoad{pod: pod, score: score})
	}

	if len(pods) == 0 {
		return nil, fmt.Errorf("no available pods")
	}

	sort.Slice(pods, func(i, j int) bool {
		if comparison := pods[i].score.compare(pods[j].score); comparison != 0 {
			return comparison < 0
		}
		return pods[i].pod.Name < pods[j].pod.Name
	})

	return pods[0].pod, nil
}

// resourceScore ranks a projected allocation by its dominant resource
// utilization, then by the sum of all utilization. Exact rational values keep
// ordering deterministic for arbitrary Kubernetes quantity formats.
type resourceScore struct {
	max *big.Rat
	sum *big.Rat
}

func resourceCapacityScore(capacity, usage, request corev1.ResourceList) (resourceScore, error) {
	score := resourceScore{max: new(big.Rat), sum: new(big.Rat)}
	for name, available := range capacity {
		if available.Sign() <= 0 {
			continue
		}
		used := usage[name].DeepCopy()
		used.Add(request[name])
		if used.Sign() <= 0 {
			continue
		}
		utilization, err := quantityRatio(used, available)
		if err != nil {
			return resourceScore{}, fmt.Errorf("resource %q: %w", name, err)
		}
		if utilization.Cmp(score.max) > 0 {
			score.max.Set(utilization)
		}
		score.sum.Add(score.sum, utilization)
	}
	return score, nil
}

func (s resourceScore) compare(other resourceScore) int {
	if comparison := s.max.Cmp(other.max); comparison != 0 {
		return comparison
	}
	return s.sum.Cmp(other.sum)
}

func quantityRatio(numerator, denominator resource.Quantity) (*big.Rat, error) {
	numeratorRat, ok := new(big.Rat).SetString(numerator.AsDec().String())
	if !ok {
		return nil, fmt.Errorf("parse quantity %q", numerator.String())
	}
	denominatorRat, ok := new(big.Rat).SetString(denominator.AsDec().String())
	if !ok || denominatorRat.Sign() <= 0 {
		return nil, fmt.Errorf("parse positive capacity %q", denominator.String())
	}
	return numeratorRat.Quo(numeratorRat, denominatorRat), nil
}
