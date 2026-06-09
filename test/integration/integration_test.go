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
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimed"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
	"github.com/kruntimes/kruntimes/internal/scheduler"
)

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
	testMgr   ctrl.Manager
	mgrCtx    context.Context
	mgrCancel context.CancelFunc
)

func TestMain(m *testing.M) {
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "charts", "kruntimes", "crds")},
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

	skipNameValidation := true
	testMgr, err = ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Controller: config.Controller{
			SkipNameValidation: &skipNameValidation,
		},
	})
	if err != nil {
		panic("failed to create manager: " + err.Error())
	}

	if err := (&scheduler.RunReconciler{
		Client:   testMgr.GetClient(),
		Log:      ctrl.Log.WithName("scheduler"),
		Strategy: &scheduler.LeastLoaded{},
	}).SetupWithManager(testMgr); err != nil {
		panic("failed to setup scheduler: " + err.Error())
	}

	if err := (&runtimed.Controller{
		Client:          testMgr.GetClient(),
		Log:             ctrl.Log.WithName("runtimed"),
		Hostname:        "test-runtimed-pod",
		RuntimeEndpoint: "localhost:19091",
		Workers:         1,
	}).SetupWithManager(testMgr); err != nil {
		panic("failed to setup runtimed: " + err.Error())
	}

	mgrCtx, mgrCancel = context.WithCancel(context.Background())
	defer mgrCancel()

	go func() {
		if err := testMgr.Start(mgrCtx); err != nil {
			panic("manager error: " + err.Error())
		}
	}()

	code := m.Run()

	mgrCancel()
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
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bash-pod-",
			Namespace:    ns.Name,
			Labels:       map[string]string{"runtime": "bash"},
			Annotations: map[string]string{
				runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns): "1",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "runtimed", Image: "busybox", Command: []string{"sleep", "999"}},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()},
		{
			Type:               v1alpha1.RuntimePodRuntimedReadyCondition,
			Status:             corev1.ConditionTrue,
			LastProbeTime:      metav1.Now(),
			LastTransitionTime: metav1.Now(),
		},
	}
	if err := k8sClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Wait for scheduler to assign the run.
	var updated v1alpha1.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if updated.Status.Phase == v1alpha1.RunScheduled && updated.Status.AssignedPod != "" {
			t.Logf("Task %s scheduled to pod %s", updated.Name, updated.Status.AssignedPod)
			return
		}
	}
	t.Errorf("expected Scheduled, got phase=%s assignedPod=%s", updated.Status.Phase, updated.Status.AssignedPod)
}

func TestRuntimedClaimAndExecute(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Re-fetch and update, retrying on conflict with scheduler.
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		run.Status.Phase = v1alpha1.RunScheduled
		run.Status.AssignedPod = "test-runtimed-pod"
		if err := k8sClient.Status().Update(context.Background(), run); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if run.Status.Phase != v1alpha1.RunScheduled {
		t.Fatalf("failed to set phase after retries")
	}

	// Wait for runtimed to pick up and fail (no gRPC runtime on localhost:19091).
	var final v1alpha1.Run
	for i := 0; i < 30; i++ {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &final); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if final.Status.Phase == v1alpha1.RunFailed {
			t.Logf("Task %s completed: phase=%s, msg=%s", final.Name, final.Status.Phase, final.Status.Message)
			return
		}
	}
	t.Errorf("expected Failed due to no runtime, got phase=%s msg=%s", final.Status.Phase, final.Status.Message)
}

func TestSchedulerKeepsPendingWhenNoMatchingPod(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "nonexistent-runtime",
			Args:    []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Wait for scheduler to observe the run without failing it.
	var updated v1alpha1.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if updated.Status.Phase == v1alpha1.RunFailed {
			t.Fatalf("expected Pending when no matching pod, got Failed: %s", updated.Status.Message)
		}
		if updated.Status.Phase == v1alpha1.RunPending && updated.Status.Message != "" {
			if updated.Status.AssignedPod != "" {
				t.Fatalf("expected no assigned pod, got %s", updated.Status.AssignedPod)
			}
			return
		}
	}
	t.Errorf("expected Pending when no matching pod, got %s: %s", updated.Status.Phase, updated.Status.Message)
}

func TestSchedulerSkipsNotReadyPod(t *testing.T) {
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create ns: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), ns) }()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "bash-pod-",
			Namespace:    ns.Name,
			Labels:       map[string]string{"runtime": "bash"},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "runtimed", Image: "busybox", Command: []string{"sleep", "999"}},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.Now()},
	}
	if err := k8sClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-run-",
			Namespace:    ns.Name,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	var updated v1alpha1.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &updated); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if updated.Status.Phase == v1alpha1.RunScheduled {
			t.Fatalf("expected NotReady pod to be skipped, got scheduled to %s", updated.Status.AssignedPod)
		}
		if updated.Status.Phase == v1alpha1.RunPending && updated.Status.Message != "" {
			if updated.Status.AssignedPod != "" {
				t.Fatalf("expected no assigned pod, got %s", updated.Status.AssignedPod)
			}
			return
		}
	}
	t.Errorf("expected Pending when matching pod is not ready, got %s: %s", updated.Status.Phase, updated.Status.Message)
}

func TestRunArtifactRefValidation(t *testing.T) {
	ctx := context.Background()
	ns := &corev1.Namespace{}
	ns.GenerateName = "test-artifact-ref-"
	if err := k8sClient.Create(ctx, ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	defer func() { _ = k8sClient.Delete(ctx, ns) }()

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "artifact-ref", Namespace: ns.Name},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	run.Status.ArtifactRefs = []v1alpha1.ArtifactRef{
		{
			Name:   "report",
			Driver: v1alpha1.ArtifactDriverFilesystem,
			Type:   v1alpha1.ArtifactTypeFile,
			Location: v1alpha1.ArtifactLocation{
				Filesystem: &v1alpha1.FilesystemArtifactLocation{
					Path:            "namespaces/default/runs/uid/report",
					VolumeClaimName: "artifacts",
				},
			},
			CreatedAt: metav1.Now(),
		},
	}
	run.Status.Phase = v1alpha1.RunPending
	if err := k8sClient.Status().Update(ctx, run); err != nil {
		t.Fatalf("update valid artifact ref: %v", err)
	}

	run.Status.ArtifactRefs[0].Location.S3 = &v1alpha1.S3ArtifactLocation{
		Bucket: "artifacts",
		Key:    "report",
	}
	if err := k8sClient.Status().Update(ctx, run); err == nil {
		t.Fatal("expected invalid mixed artifact locations to be rejected")
	}
}
