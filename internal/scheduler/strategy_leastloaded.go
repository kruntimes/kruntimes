package scheduler

import (
	"fmt"
	"math/big"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

type leastLoadedScore struct{}

func newLeastLoadedScore(_ *RunReconciler, _ *schedulingSnapshot, _ *schedulingPreFilterState) (scorePlugin, error) {
	return &leastLoadedScore{}, nil
}

func (s *leastLoadedScore) Name() string {
	return "LeastLoaded"
}

// Score defers conversion to the shared 0..100 range to NormalizeScores. The
// complete utilization comparison is a two-part exact rational value, so a
// scalar raw score would lose the dominant-utilization tie-breaking semantics.
func (s *leastLoadedScore) Score(_ *schedulingSnapshot, _ *corev1.Pod) (int64, error) {
	return 0, nil
}

func (s *leastLoadedScore) NormalizeScores(snapshot *schedulingSnapshot, scores []podScore) error {
	byName := make(map[string]*corev1.Pod, len(snapshot.pods))
	for i := range snapshot.pods {
		byName[snapshot.pods[i].Name] = &snapshot.pods[i]
	}
	type rankedScore struct {
		index int
		score resourceScore
	}
	ranked := make([]rankedScore, len(scores))
	for i := range scores {
		pod := byName[scores[i].podName]
		if pod == nil {
			return fmt.Errorf("candidate pod %q is not in the scheduling snapshot", scores[i].podName)
		}
		resourceScore, err := resourceCapacityScore(
			runtimepod.Capacity(pod, defaultRuntimePodCapacity()),
			snapshot.usageByPod[pod.Name],
			snapshot.request,
		)
		if err != nil {
			return fmt.Errorf("score pod %q: %w", pod.Name, err)
		}
		ranked[i] = rankedScore{index: i, score: resourceScore}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return ranked[i].score.compare(ranked[j].score) < 0
	})
	if len(ranked) == 1 {
		scores[ranked[0].index].score = maxPodScore
		return nil
	}
	for start := 0; start < len(ranked); {
		end := start + 1
		for end < len(ranked) && ranked[end].score.compare(ranked[start].score) == 0 {
			end++
		}
		// Rank groups preserve ties and map the least-loaded group to 100 and
		// the most-loaded group to 0.
		normalized := int64(len(ranked)-1-start) * maxPodScore / int64(len(ranked)-1)
		for i := start; i < end; i++ {
			scores[ranked[i].index].score = normalized
		}
		start = end
	}
	return nil
}

// resourceScore ranks a projected allocation by its dominant resource
// utilization, then by the sum of all utilization. Exact rational values keep
// ordering deterministic for arbitrary Kubernetes quantity formats.
type resourceScore struct {
	max *big.Rat
	sum *big.Rat
}

// resourceCapacityScore scores the allocation that would result after assigning
// request. For every advertised resource it calculates
// (usage + request) / capacity. The score minimizes the highest ratio to avoid
// creating a resource bottleneck, then the sum of ratios to break that tie.
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
