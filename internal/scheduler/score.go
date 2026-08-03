package scheduler

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

const (
	minPodScore int64 = 0
	maxPodScore int64 = 100
)

// scorePluginError aborts one scheduling cycle without making the Run terminal.
// Controller-runtime retries it through the normal rate-limited workqueue.
type scorePluginError struct {
	err error
}

func (e *scorePluginError) Error() string {
	return e.err.Error()
}

func (e *scorePluginError) Unwrap() error {
	return e.err
}

func isScorePluginError(err error) bool {
	var scoreErr *scorePluginError
	return errors.As(err, &scoreErr)
}

// scorePlugin scores one Pod that has passed every Filter plugin. It must not
// select a Pod, remove candidates, patch Kubernetes objects, or mutate
// reservation state. Larger scores are preferred after normalization.
type scorePlugin interface {
	Name() string
	Score(*schedulingSnapshot, *corev1.Pod) (int64, error)
}

// scoreNormalizer optionally transforms the scores produced by one plugin for
// every feasible Pod. Implementations must keep every score in 0..100.
type scoreNormalizer interface {
	NormalizeScores(*schedulingSnapshot, []podScore) error
}

type podScore struct {
	podName string
	score   int64
}

type scorePluginRegistration struct {
	factory scorePluginFactory
	weight  int64
}

// scorePluginFactory prepares a score plugin once per scheduling cycle.
type scorePluginFactory func(*RunReconciler, *schedulingSnapshot, *schedulingPreFilterState) (scorePlugin, error)

type registeredScorePlugin struct {
	plugin scorePlugin
	weight int64
}

var defaultScorePluginRegistrations = []scorePluginRegistration{
	{factory: newPreferredRunAffinityScore, weight: 1},
	{factory: newLeastLoadedScore, weight: 1},
}

func (r *RunReconciler) registeredScorePlugins(snapshot *schedulingSnapshot, preFilter *schedulingPreFilterState) ([]registeredScorePlugin, error) {
	registrations := r.scorePluginRegistrations
	if len(registrations) == 0 {
		registrations = defaultScorePluginRegistrations
	}
	plugins := make([]registeredScorePlugin, 0, len(registrations))
	for _, registration := range registrations {
		if registration.factory == nil {
			return nil, &scorePluginError{err: errors.New("initialize score plugin: nil factory")}
		}
		if registration.weight <= 0 {
			return nil, &scorePluginError{err: fmt.Errorf("initialize score plugin: invalid weight %d", registration.weight)}
		}
		plugin, err := registration.factory(r, snapshot, preFilter)
		if err != nil {
			return nil, &scorePluginError{err: fmt.Errorf("initialize score plugin: %w", err)}
		}
		if plugin == nil {
			return nil, &scorePluginError{err: errors.New("initialize score plugin: nil plugin")}
		}
		plugins = append(plugins, registeredScorePlugin{plugin: plugin, weight: registration.weight})
	}
	return plugins, nil
}

// scoreAndRankPods runs every registered Score plugin for every feasible Pod,
// normalizes each plugin's scores when requested, and sorts the aggregate
// weighted totals in descending order. Pod name provides a stable tie break.
func scoreAndRankPods(snapshot *schedulingSnapshot, candidates []corev1.Pod, plugins []registeredScorePlugin) ([]corev1.Pod, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidates to score")
	}
	totals := make([]int64, len(candidates))
	for _, registered := range plugins {
		scores := make([]podScore, len(candidates))
		for i := range candidates {
			score, err := registered.plugin.Score(snapshot, &candidates[i])
			if err != nil {
				return nil, &scorePluginError{err: fmt.Errorf("score plugin %q pod %q: %w", registered.plugin.Name(), candidates[i].Name, err)}
			}
			scores[i] = podScore{podName: candidates[i].Name, score: score}
		}
		if normalizer, ok := registered.plugin.(scoreNormalizer); ok {
			if err := normalizer.NormalizeScores(snapshot, scores); err != nil {
				return nil, &scorePluginError{err: fmt.Errorf("normalize score plugin %q: %w", registered.plugin.Name(), err)}
			}
		}
		for i := range scores {
			if scores[i].score < minPodScore || scores[i].score > maxPodScore {
				return nil, &scorePluginError{err: fmt.Errorf("score plugin %q pod %q returned %d outside %d..%d", registered.plugin.Name(), scores[i].podName, scores[i].score, minPodScore, maxPodScore)}
			}
			if scores[i].score > (math.MaxInt64-totals[i])/registered.weight {
				return nil, &scorePluginError{err: fmt.Errorf("score plugin %q overflows total score", registered.plugin.Name())}
			}
			totals[i] += scores[i].score * registered.weight
		}
	}

	type scoredPod struct {
		pod   corev1.Pod
		total int64
	}
	rankedScores := make([]scoredPod, len(candidates))
	for i := range candidates {
		rankedScores[i] = scoredPod{pod: candidates[i], total: totals[i]}
	}
	sort.Slice(rankedScores, func(i, j int) bool {
		if rankedScores[i].total != rankedScores[j].total {
			return rankedScores[i].total > rankedScores[j].total
		}
		return rankedScores[i].pod.Name < rankedScores[j].pod.Name
	})
	ranked := make([]corev1.Pod, len(rankedScores))
	for i := range rankedScores {
		ranked[i] = rankedScores[i].pod
	}
	return ranked, nil
}

func normalizeScoresByMaximum(scores []podScore) error {
	if len(scores) == 0 {
		return nil
	}
	rawScores := make([]int64, len(scores))
	for i := range scores {
		if scores[i].score < 0 {
			return fmt.Errorf("negative raw score for pod %q", scores[i].podName)
		}
		rawScores[i] = scores[i].score
	}
	maximum := slices.Max(rawScores)
	if maximum == 0 {
		return nil
	}
	for i := range scores {
		scores[i].score = scores[i].score * maxPodScore / maximum
	}
	return nil
}
