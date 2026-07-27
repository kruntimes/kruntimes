package runtimepod

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestCapacityAnnotations(t *testing.T) {
	rt := &v1alpha1.Runtime{
		Spec: v1alpha1.RuntimeSpec{
			Capacity: &v1alpha1.RuntimeCapacity{
				Resources: corev1.ResourceList{
					corev1.ResourceName(v1alpha1.RuntimeResourceRuns): resource.MustParse("3"),
					corev1.ResourceName("gpu"):                        resource.MustParse("1"),
				},
			},
		},
	}

	annotations := CapacityAnnotations(rt)
	if annotations[CapacityAnnotation(v1alpha1.RuntimeResourceRuns)] != "3" {
		t.Fatalf("runs annotation = %q, want 3", annotations[CapacityAnnotation(v1alpha1.RuntimeResourceRuns)])
	}
	if annotations[CapacityAnnotation("gpu")] != "1" {
		t.Fatalf("gpu annotation = %q, want 1", annotations[CapacityAnnotation("gpu")])
	}
}

func TestRunsCapacity(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		fallback    int32
		want        int32
	}{
		{name: "missing", fallback: 1, want: 1},
		{name: "valid", annotations: map[string]string{CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "4"}, fallback: 1, want: 4},
		{name: "invalid", annotations: map[string]string{CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "bad"}, fallback: 2, want: 2},
		{name: "zero", annotations: map[string]string{CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "0"}, fallback: 2, want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations}}
			if got := RunsCapacity(pod, tt.fallback); got != tt.want {
				t.Fatalf("RunsCapacity() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCapacityAndFits(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "4",
		CapacityAnnotation("example.com/gpu"):            "2",
	}}}
	capacity := Capacity(pod, 1)
	gpuCapacity := capacity[corev1.ResourceName("example.com/gpu")]
	if got := gpuCapacity.Value(); got != 2 {
		t.Fatalf("gpu capacity = %d, want 2", got)
	}

	requests := corev1.ResourceList{
		corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(1, resource.DecimalSI),
		corev1.ResourceName("example.com/gpu"):            *resource.NewQuantity(1, resource.DecimalSI),
	}
	if !Fits(capacity, corev1.ResourceList{
		corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(3, resource.DecimalSI),
		corev1.ResourceName("example.com/gpu"):            *resource.NewQuantity(1, resource.DecimalSI),
	}, requests) {
		t.Fatal("expected complete resource request to fit")
	}
	if Fits(capacity, corev1.ResourceList{
		corev1.ResourceName("example.com/gpu"): *resource.NewQuantity(2, resource.DecimalSI),
	}, requests) {
		t.Fatal("expected exhausted custom resource not to fit")
	}
	if Fits(capacity, nil, corev1.ResourceList{corev1.ResourceName("example.com/fpga"): *resource.NewQuantity(1, resource.DecimalSI)}) {
		t.Fatal("expected request without capacity annotation not to fit")
	}
}

func TestRuntimedReadyCondition(t *testing.T) {
	now := metav1.Now()
	pod := &corev1.Pod{}

	SetRuntimedReadyCondition(pod, corev1.ConditionTrue, "Heartbeat", "fresh", now)
	if !IsRuntimedReady(pod) {
		t.Fatal("expected runtimed ready")
	}
	cond := FindRuntimedReadyCondition(pod)
	if cond == nil {
		t.Fatal("expected runtimed ready condition")
	}
	if cond.Type != v1alpha1.RuntimePodRuntimedReadyCondition {
		t.Fatalf("condition type = %s", cond.Type)
	}
	if !FreshRuntimedReady(pod, now.Time, time.Second) {
		t.Fatal("expected fresh runtimed condition")
	}
	if FreshRuntimedReady(pod, now.Add(2*time.Second), time.Second) {
		t.Fatal("expected stale runtimed condition")
	}

	SetRuntimedReadyCondition(pod, corev1.ConditionFalse, "Stopping", "stopped", now)
	if IsRuntimedReady(pod) {
		t.Fatal("expected runtimed not ready")
	}
}
