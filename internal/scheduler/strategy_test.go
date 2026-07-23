package scheduler

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

func TestLeastLoaded_Select(t *testing.T) {
	tests := []struct {
		name    string
		pods    []corev1.Pod
		usage   map[string]corev1.ResourceList
		run     *v1alpha1.Run
		wantPod string
		wantErr bool
	}{
		{
			name:    "no candidate pods",
			pods:    nil,
			run:     &v1alpha1.Run{},
			wantErr: true,
		},
		{
			name: "single candidate",
			pods: []corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}},
			},
			run:     &v1alpha1.Run{},
			wantPod: "pod-a",
		},
		{
			name: "least loaded selected",
			pods: []corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}},
			},
			usage: map[string]corev1.ResourceList{
				"pod-a": {corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(2, resource.DecimalSI)},
			},
			run:     &v1alpha1.Run{},
			wantPod: "pod-b",
		},
		{
			name: "ready run consumes capacity",
			pods: []corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}},
			},
			usage: map[string]corev1.ResourceList{
				"pod-a": {corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(1, resource.DecimalSI)},
			},
			run:     &v1alpha1.Run{},
			wantPod: "pod-b",
		},
		{
			name: "selects by runs while preserving other resource usage",
			pods: []corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}},
			},
			usage: map[string]corev1.ResourceList{
				"pod-a": {
					corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(1, resource.DecimalSI),
					corev1.ResourceName("example.com/accelerator"):    *resource.NewQuantity(8, resource.DecimalSI),
				},
			},
			run:     &v1alpha1.Run{},
			wantPod: "pod-b",
		},
		{
			name: "most available capacity selected",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-a",
						Namespace: "default",
						Annotations: map[string]string{
							runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "1",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod-b",
						Namespace: "default",
						Annotations: map[string]string{
							runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "4",
						},
					},
				},
			},
			run:     &v1alpha1.Run{},
			wantPod: "pod-b",
		},
		{
			name: "skip terminating pod",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:              "pod-a",
						Namespace:         "default",
						Finalizers:        []string{"test"},
						DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
					},
				},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}},
			},
			run:     &v1alpha1.Run{},
			wantPod: "pod-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &LeastLoaded{}

			pod, err := s.Select(tt.pods, tt.usage, tt.run)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pod.Name != tt.wantPod {
				t.Errorf("selected pod = %q, want %q", pod.Name, tt.wantPod)
			}
		})
	}
}

func TestLeastLoaded_DeterministicTiebreak(t *testing.T) {
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}},
	}

	s := &LeastLoaded{}

	pod, err := s.Select(pods, nil, &v1alpha1.Run{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pod.Name != "pod-a" {
		t.Errorf("tiebreak should pick alphabetically first: got %q, want pod-a", pod.Name)
	}
}
