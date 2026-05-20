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

	// Create pending run (status subresource prevents setting phase on Create)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime:  "golang-1.26",
			Commands: []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create task: %v", err)
	}
	run.Status.Phase = v1alpha1.RunPending
	if err := k8sClient.Status().Update(context.Background(), run); err != nil {
		t.Fatalf("set run pending: %v", err)
	}

	reconciler := &scheduler.RunReconciler{
		Client:   k8sClient,
		Log:      ctrl.Log.WithName("test"),
		Strategy: &scheduler.LeastLoaded{},
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: run.Name, Namespace: run.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}

	if updated.Status.Phase != v1alpha1.RunScheduled {
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

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime:  "golang-1.26",
			Commands: []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Set status to Scheduled + assigned
	run.Status.Phase = v1alpha1.RunScheduled
	run.Status.AssignedPod = "test-agent-pod"
	if err := k8sClient.Status().Update(context.Background(), run); err != nil {
		t.Fatalf("update run status: %v", err)
	}

	ctrl := &agent.Controller{
		Client:          k8sClient,
		Hostname:        "test-agent-pod",
		RuntimeEndpoint: "localhost:19091",
		Workers:         1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		time.Sleep(2 * time.Second)
		cancel()
	}()

	if err := ctrl.Run(ctx); err != nil {
		t.Fatalf("controller run: %v", err)
	}

	var final v1alpha1.Run
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &final); err != nil {
		t.Fatalf("get task: %v", err)
	}

	// Agent fails because no gRPC runtime is listening on localhost:19091.
	if final.Status.Phase != v1alpha1.RunFailed {
		t.Errorf("expected Failed due to no runtime, got %s (message: %s)", final.Status.Phase, final.Status.Message)
	}
	t.Logf("Task %s completed: phase=%s, msg=%s", final.Name, final.Status.Phase, final.Status.Message)
}

func TestSchedulerNoMatchingPod(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer k8sClient.Delete(context.Background(), ns)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime:  "nonexistent-runtime",
			Commands: []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create task: %v", err)
	}
	run.Status.Phase = v1alpha1.RunPending
	if err := k8sClient.Status().Update(context.Background(), run); err != nil {
		t.Fatalf("set run pending: %v", err)
	}

	reconciler := &scheduler.RunReconciler{
		Client:   k8sClient,
		Log:      ctrl.Log.WithName("test"),
		Strategy: &scheduler.LeastLoaded{},
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: run.Name, Namespace: run.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha1.Run
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}

	if updated.Status.Phase != v1alpha1.RunFailed {
		t.Errorf("expected Failed when no matching pod, got %s", updated.Status.Phase)
	}
	t.Logf("Task correctly failed: %s", updated.Status.Message)
}
