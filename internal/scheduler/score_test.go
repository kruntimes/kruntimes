package scheduler

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRegisteredScorePlugins(t *testing.T) {
	reconciler := &RunReconciler{}
	snapshot := &schedulingSnapshot{}
	plugins, err := reconciler.registeredScorePlugins(snapshot, &schedulingPreFilterState{})
	if err != nil {
		t.Fatalf("registeredScorePlugins: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("registered plugins = %d, want 2", len(plugins))
	}
	if plugins[0].plugin.Name() != "PreferredRunAffinity" || plugins[0].weight != 1 || plugins[1].plugin.Name() != "LeastLoaded" || plugins[1].weight != 1 {
		t.Fatalf("registered plugins = %#v, want PreferredRunAffinity/1 and LeastLoaded/1", plugins)
	}
}

func TestScoreAndRankPodsScoresEveryCandidateAndAppliesWeights(t *testing.T) {
	candidates := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-c"}},
	}
	first := &recordingScorePlugin{name: "first", scores: map[string]int64{"pod-a": 0, "pod-b": 100, "pod-c": 10}}
	second := &recordingScorePlugin{name: "second", scores: map[string]int64{"pod-a": 100, "pod-b": 0, "pod-c": 100}}
	ranked, err := scoreAndRankPods(&schedulingSnapshot{}, candidates, []registeredScorePlugin{
		{plugin: first, weight: 1},
		{plugin: second, weight: 2},
	})
	if err != nil {
		t.Fatalf("scoreAndRankPods: %v", err)
	}
	if got := podNames(ranked); strings.Join(got, ",") != "pod-c,pod-a,pod-b" {
		t.Fatalf("ranked pods = %v, want [pod-c pod-a pod-b]", got)
	}
	for _, plugin := range []*recordingScorePlugin{first, second} {
		if got := strings.Join(plugin.received, ","); got != "pod-a,pod-b,pod-c" {
			t.Fatalf("plugin %q received %v, want every candidate", plugin.name, plugin.received)
		}
	}
}

func TestScoreAndRankPodsNormalizesAndBreaksTiesByPodName(t *testing.T) {
	candidates := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-c"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
	}
	plugin := &maximumNormalizingScorePlugin{recordingScorePlugin: recordingScorePlugin{
		name: "normalized", scores: map[string]int64{"pod-a": 1, "pod-b": 2, "pod-c": 2},
	}}
	ranked, err := scoreAndRankPods(&schedulingSnapshot{}, candidates, []registeredScorePlugin{{plugin: plugin, weight: 1}})
	if err != nil {
		t.Fatalf("scoreAndRankPods: %v", err)
	}
	if got := podNames(ranked); strings.Join(got, ",") != "pod-b,pod-c,pod-a" {
		t.Fatalf("ranked pods = %v, want [pod-b pod-c pod-a]", got)
	}
}

func TestScoreAndRankPodsRejectsPluginAndNormalizationErrors(t *testing.T) {
	candidates := []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}}}
	for _, plugin := range []scorePlugin{
		&recordingScorePlugin{name: "broken", err: errors.New("test failure")},
		&invalidNormalizingScorePlugin{recordingScorePlugin: recordingScorePlugin{name: "invalid", scores: map[string]int64{"pod-a": 1}}},
	} {
		_, err := scoreAndRankPods(&schedulingSnapshot{}, candidates, []registeredScorePlugin{{plugin: plugin, weight: 1}})
		if err == nil || !strings.Contains(err.Error(), plugin.Name()) || !isScorePluginError(err) {
			t.Fatalf("error = %v, want score plugin error for %q", err, plugin.Name())
		}
	}
}

func TestRegisteredScorePluginsRejectsInvalidWeight(t *testing.T) {
	reconciler := &RunReconciler{scorePluginRegistrations: []scorePluginRegistration{{factory: func(*RunReconciler, *schedulingSnapshot, *schedulingPreFilterState) (scorePlugin, error) {
		return &recordingScorePlugin{name: "test"}, nil
	}, weight: 0}}}
	_, err := reconciler.registeredScorePlugins(&schedulingSnapshot{}, &schedulingPreFilterState{})
	if err == nil || !isScorePluginError(err) || !strings.Contains(err.Error(), "invalid weight") {
		t.Fatalf("error = %v, want invalid score plugin weight", err)
	}
}

type recordingScorePlugin struct {
	name     string
	scores   map[string]int64
	err      error
	received []string
}

func (s *recordingScorePlugin) Name() string {
	return s.name
}

func (s *recordingScorePlugin) Score(_ *schedulingSnapshot, pod *corev1.Pod) (int64, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.received = append(s.received, pod.Name)
	return s.scores[pod.Name], nil
}

type maximumNormalizingScorePlugin struct {
	recordingScorePlugin
}

func (s *maximumNormalizingScorePlugin) NormalizeScores(_ *schedulingSnapshot, scores []podScore) error {
	return normalizeScoresByMaximum(scores)
}

type invalidNormalizingScorePlugin struct {
	recordingScorePlugin
}

func (s *invalidNormalizingScorePlugin) NormalizeScores(_ *schedulingSnapshot, scores []podScore) error {
	for i := range scores {
		scores[i].score = maxPodScore + 1
	}
	return nil
}

func podNames(pods []corev1.Pod) []string {
	names := make([]string, len(pods))
	for i := range pods {
		names[i] = pods[i].Name
	}
	return names
}
