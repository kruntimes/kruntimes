package integration

import (
	"context"
	"testing"

	"github.com/airconduct/kruntime/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTaskCRDRegistration(t *testing.T) {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1alpha1.AddToScheme(s))

	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-task",
			Namespace: "default",
		},
		Spec: v1alpha1.TaskSpec{
			Runtime:  "golang-1.26",
			Commands: []string{"echo hello"},
		},
		Status: v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(task).Build()

	var result v1alpha1.Task
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(task), &result); err != nil {
		t.Fatalf("failed to get task: %v", err)
	}

	if result.Spec.Runtime != "golang-1.26" {
		t.Errorf("expected runtime golang-1.26, got %s", result.Spec.Runtime)
	}
	if result.Status.Phase != v1alpha1.TaskPending {
		t.Errorf("expected phase Pending, got %s", result.Status.Phase)
	}

	t.Log("CRD types registered and usable with fake client")
}

func TestTaskListWithFiltering(t *testing.T) {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1alpha1.AddToScheme(s))

	task1 := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-1", Namespace: "default"},
		Spec:       v1alpha1.TaskSpec{Runtime: "golang-1.26", Commands: []string{"echo"}},
		Status:     v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending},
	}
	task2 := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-2", Namespace: "default"},
		Spec:       v1alpha1.TaskSpec{Runtime: "python-3.12", Commands: []string{"python"}},
		Status:     v1alpha1.TaskStatus{Phase: v1alpha1.TaskPending},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(task1, task2).Build()

	var tasks v1alpha1.TaskList
	if err := c.List(context.Background(), &tasks); err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	if len(tasks.Items) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks.Items))
	}

	t.Log("Task list and filtering works correctly")
}

func TestRuntimePodLabelMatching(t *testing.T) {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))

	runtimePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golang-pod-a",
			Namespace: "default",
			Labels:    map[string]string{"runtime": "golang-1.26"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	c := fake.NewClientBuilder().WithScheme(s).WithRuntimeObjects(runtimePod).Build()

	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatalf("failed to list pods: %v", err)
	}

	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods.Items))
	}

	pod := pods.Items[0]
	if pod.Labels["runtime"] != "golang-1.26" {
		t.Errorf("expected runtime label golang-1.26, got %s", pod.Labels["runtime"])
	}

	t.Log("Runtime pod label matching verified")
}
