package scheduler

import (
	"context"
	"testing"

	"github.com/airconduct/kruntime/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestLeastLoaded_Select(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	tests := []struct {
		name       string
		pods       []corev1.Pod
		tasks      []v1alpha1.Task
		task       *v1alpha1.Task
		wantPod    string
		wantErr    bool
	}{
		{
			name:    "no candidate pods",
			pods:    nil,
			task:    &v1alpha1.Task{},
			wantErr: true,
		},
		{
			name: "single candidate",
			pods: []corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}},
			},
			task:    &v1alpha1.Task{},
			wantPod: "pod-a",
		},
		{
			name: "least loaded selected",
			pods: []corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}},
			},
			tasks: []v1alpha1.Task{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: "default"},
					Status:     v1alpha1.TaskStatus{Phase: v1alpha1.TaskRunning, AssignedPod: "pod-a"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "task-2", Namespace: "default"},
					Status:     v1alpha1.TaskStatus{Phase: v1alpha1.TaskRunning, AssignedPod: "pod-a"},
				},
			},
			task:    &v1alpha1.Task{},
			wantPod: "pod-b",
		},
		{
			name: "skip terminating pod",
			pods: []corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:       "pod-a",
						Namespace:  "default",
						Finalizers: []string{"test"},
						DeletionTimestamp: &metav1.Time{Time: metav1.Now().Time},
					},
				},
				{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}},
			},
			task:    &v1alpha1.Task{},
			wantPod: "pod-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []runtime.Object{}
			for i := range tt.pods {
				objs = append(objs, &tt.pods[i])
			}
			for i := range tt.tasks {
				objs = append(objs, &tt.tasks[i])
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
			s := &LeastLoaded{}

			pod, err := s.Select(context.Background(), client, tt.pods, tt.task)
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
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = v1alpha1.AddToScheme(scheme)

	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: "default"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "default"}},
	}

	client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects().Build()
	s := &LeastLoaded{}

	pod, err := s.Select(context.Background(), client, pods, &v1alpha1.Task{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pod.Name != "pod-a" {
		t.Errorf("tiebreak should pick alphabetically first: got %q, want pod-a", pod.Name)
	}
}
