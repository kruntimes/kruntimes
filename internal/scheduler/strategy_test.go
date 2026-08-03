package scheduler

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

func TestLeastLoadedScoreRanksCompleteResourceUtilization(t *testing.T) {
	run := &v1alpha1.Run{Spec: v1alpha1.RunSpec{Resources: &v1alpha1.RunResourceRequirements{Requests: corev1.ResourceList{
		corev1.ResourceName("example.com/accelerator"): resource.MustParse("1"),
	}}}}
	request, err := run.Spec.ResourceRequests()
	if err != nil {
		t.Fatal(err)
	}
	candidates := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Annotations: map[string]string{
			runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "4",
			runtimepod.CapacityAnnotation("example.com/accelerator"):    "16",
		}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Annotations: map[string]string{
			runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "4",
			runtimepod.CapacityAnnotation("example.com/accelerator"):    "16",
		}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-c", Annotations: map[string]string{
			runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "4",
			runtimepod.CapacityAnnotation("example.com/accelerator"):    "16",
		}}},
	}
	snapshot := &schedulingSnapshot{
		run:     run,
		request: request,
		pods:    candidates,
		usageByPod: map[string]corev1.ResourceList{
			"pod-a": {
				corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(1, resource.DecimalSI),
				corev1.ResourceName("example.com/accelerator"):    *resource.NewQuantity(8, resource.DecimalSI),
			},
			"pod-b": {
				corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(1, resource.DecimalSI),
				corev1.ResourceName("example.com/accelerator"):    *resource.NewQuantity(1, resource.DecimalSI),
			},
		},
	}
	ranked, err := scoreAndRankPods(snapshot, candidates, []registeredScorePlugin{{plugin: &leastLoadedScore{}, weight: 1}})
	if err != nil {
		t.Fatalf("scoreAndRankPods: %v", err)
	}
	if got := strings.Join(podNames(ranked), ","); got != "pod-c,pod-b,pod-a" {
		t.Fatalf("ranked pods = %v, want [pod-c pod-b pod-a]", podNames(ranked))
	}
}

func TestLeastLoadedScoreKeepsEqualUtilizationTied(t *testing.T) {
	candidates := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-b"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-a"}},
	}
	run := &v1alpha1.Run{}
	request, err := run.Spec.ResourceRequests()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &schedulingSnapshot{run: run, request: request, pods: candidates}
	ranked, err := scoreAndRankPods(snapshot, candidates, []registeredScorePlugin{{plugin: &leastLoadedScore{}, weight: 1}})
	if err != nil {
		t.Fatalf("scoreAndRankPods: %v", err)
	}
	if got := strings.Join(podNames(ranked), ","); got != "pod-a,pod-b" {
		t.Fatalf("ranked pods = %v, want [pod-a pod-b]", podNames(ranked))
	}
}
