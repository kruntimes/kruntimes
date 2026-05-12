package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/airconduct/kruntime/api/v1alpha1"
	"github.com/airconduct/kruntime/internal/agent"
	"github.com/airconduct/kruntime/internal/scheduler"
)

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
)

func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "charts", "kruntime", "crds")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		panic("failed to start testenv: " + err.Error())
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		panic("failed to create client: " + err.Error())
	}

	code := m.Run()

	if err := testEnv.Stop(); err != nil {
		panic("failed to stop testenv: " + err.Error())
	}

	os.Exit(code)
}

func TestSchedulerReconcile(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer k8sClient.Delete(context.Background(), ns)

	// Create runtime pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "golang-pod-",
			Namespace:    ns.Name,
			Labels:       map[string]string{"runtime": "golang-1.26"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent", Image: "busybox", Command: []string{"sleep", "999"}},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	if err := k8sClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Create pending task (status subresource prevents setting phase on Create)
	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-task-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.TaskSpec{
			Runtime:  "golang-1.26",
			Commands: []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	task.Status.Phase = v1alpha1.TaskPending
	if err := k8sClient.Status().Update(context.Background(), task); err != nil {
		t.Fatalf("set task pending: %v", err)
	}

	reconciler := &scheduler.TaskReconciler{
		Client:   k8sClient,
		Log:      ctrl.Log.WithName("test"),
		Strategy: &scheduler.LeastLoaded{},
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: task.Name, Namespace: task.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.Task
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(task), &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}

	if updated.Status.Phase != v1alpha1.TaskScheduled {
		t.Errorf("expected Scheduled, got %s", updated.Status.Phase)
	}
	if updated.Status.AssignedPod == "" {
		t.Error("expected assignedPod to be set")
	}
	t.Logf("Task %s scheduled to pod %s", updated.Name, updated.Status.AssignedPod)
}

func TestAgentClaimAndExecute(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer k8sClient.Delete(context.Background(), ns)

	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-task-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.TaskSpec{
			Runtime:  "golang-1.26",
			Commands: []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Set status to Scheduled + assigned
	task.Status.Phase = v1alpha1.TaskScheduled
	task.Status.AssignedPod = "test-agent-pod"
	if err := k8sClient.Status().Update(context.Background(), task); err != nil {
		t.Fatalf("update task status: %v", err)
	}

	workspaceDir := t.TempDir()
	ctrl := &agent.Controller{
		Client:   k8sClient,
		Hostname: "test-agent-pod",
		Executor: &agent.Executor{WorkspaceBase: workspaceDir},
		Workers:  1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		time.Sleep(2 * time.Second)
		cancel()
	}()

	if err := ctrl.Run(ctx); err != nil {
		t.Fatalf("controller run: %v", err)
	}

	var final v1alpha1.Task
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(task), &final); err != nil {
		t.Fatalf("get task: %v", err)
	}

	if final.Status.Phase != v1alpha1.TaskSucceeded {
		t.Errorf("expected Succeeded, got %s (message: %s)", final.Status.Phase, final.Status.Message)
	}
	t.Logf("Task %s completed: phase=%s", final.Name, final.Status.Phase)
}

func TestSchedulerNoMatchingPod(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer k8sClient.Delete(context.Background(), ns)

	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-task-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.TaskSpec{
			Runtime:  "nonexistent-runtime",
			Commands: []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	task.Status.Phase = v1alpha1.TaskPending
	if err := k8sClient.Status().Update(context.Background(), task); err != nil {
		t.Fatalf("set task pending: %v", err)
	}

	reconciler := &scheduler.TaskReconciler{
		Client:   k8sClient,
		Log:      ctrl.Log.WithName("test"),
		Strategy: &scheduler.LeastLoaded{},
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: task.Name, Namespace: task.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.Task
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(task), &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}

	if updated.Status.Phase != v1alpha1.TaskFailed {
		t.Errorf("expected Failed when no matching pod, got %s", updated.Status.Phase)
	}
	t.Logf("Task correctly failed: %s", updated.Status.Message)
}
