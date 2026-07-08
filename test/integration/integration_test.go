package integration

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
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
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
			},
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
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
			},
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
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
			},
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
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
			},
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
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode:    v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	artifactRefs := []v1alpha1.ArtifactRef{
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
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
			return err
		}
		run.Status.ArtifactRefs = artifactRefs
		run.Status.Phase = v1alpha1.RunPending
		return k8sClient.Status().Update(ctx, run)
	}); err != nil {
		t.Fatalf("update valid artifact ref: %v", err)
	}

	if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(run), run); err != nil {
		t.Fatalf("get run before invalid update: %v", err)
	}
	run.Status.ArtifactRefs[0].Location.S3 = &v1alpha1.S3ArtifactLocation{
		Bucket: "artifacts",
		Key:    "report",
	}
	if err := k8sClient.Status().Update(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid mixed artifact locations error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidRunModeTaskEntrypoint(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-entrypoint", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "/escape"},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid run entrypoint error = %v, want Invalid", err)
	}
}

func TestCRDValidationAllowsIgnoredInlineRunModeTaskEntrypoint(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")
	inline := "echo inline"

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "ignored-entrypoint", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Source:  &v1alpha1.CodeSource{Inline: &inline},
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "/ignored"},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("inline run with ignored entrypoint should be valid: %v", err)
	}
}

func TestCRDValidationRejectsRunWithoutMode(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "missing-mode", Namespace: ns.Name},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
	}
	if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("missing run mode error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsRunModeWithBothTaskAndFunction(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "mixed-mode", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode: v1alpha1.RunMode{
				Task:     &v1alpha1.RunTaskMode{Args: []string{"echo hello"}},
				Function: &v1alpha1.RunFunctionMode{Handler: "main.invoke"},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("mixed run mode error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidRunModeTaskEntrypointTraversal(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-mode-entrypoint", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "../escape"},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid mode task entrypoint error = %v, want Invalid", err)
	}
}

func TestCRDValidationAllowsIgnoredInlineRunModeTaskEntrypointTraversal(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-run-validation-")
	inline := "echo inline"

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "ignored-mode-entrypoint", Namespace: ns.Name},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Source:  &v1alpha1.CodeSource{Inline: &inline},
			Mode: v1alpha1.RunMode{
				Task: &v1alpha1.RunTaskMode{Entrypoint: "/ignored"},
			},
		},
	}
	if err := k8sClient.Create(ctx, run); err != nil {
		t.Fatalf("inline run with ignored mode task entrypoint should be valid: %v", err)
	}
}

func TestCRDValidationRejectsInvalidWorkflowNeeds(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflow-validation-")

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "unknown-need", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "compile", Run: "echo build"}},
				},
				"deploy": {
					RunsOn: "bash",
					Needs:  []string{"missing"},
					Steps:  []v1alpha1.StepSpec{{Name: "ship", Run: "echo deploy"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, workflow); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid workflow needs error = %v, want Invalid", err)
	}
}

func TestCRDValidationAllowsWorkflowWithoutNeeds(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-workflow-no-needs-")

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "no-needs", Namespace: ns.Name},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "hello", Run: "echo hello"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, workflow); err != nil {
		t.Fatalf("create workflow without needs: %v", err)
	}
}

func TestCRDValidationRejectsUnsupportedWorkflowStepShape(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-step-validation-")

	tests := []struct {
		name string
		step v1alpha1.StepSpec
	}{
		{
			name: "uses-only",
			step: v1alpha1.StepSpec{Name: "compile", Uses: "future/action"},
		},
		{
			name: "run-and-uses",
			step: v1alpha1.StepSpec{Name: "compile", Run: "echo build", Uses: "future/action"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workflow := &v1alpha1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: tt.name, Namespace: ns.Name},
				Spec: v1alpha1.WorkflowSpec{
					Jobs: map[string]v1alpha1.JobSpec{
						"build": {
							RunsOn: "bash",
							Steps:  []v1alpha1.StepSpec{tt.step},
						},
					},
				},
			}
			if err := k8sClient.Create(ctx, workflow); !apierrors.IsInvalid(err) {
				t.Fatalf("invalid workflow step error = %v, want Invalid", err)
			}
		})
	}
}

func TestCRDValidationRejectsInvalidRuntimeImage(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-runtime-validation-")

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-image", Namespace: ns.Name},
		Spec: v1alpha1.RuntimeSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runtime", Image: ""}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, rt); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid runtime image error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidRuntimeServiceAccountName(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-runtime-sa-validation-")

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-service-account", Namespace: ns.Name},
		Spec: v1alpha1.RuntimeSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "Bad_Name",
					Containers:         []corev1.Container{{Name: "runtime", Image: "runtime:latest"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, rt); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid runtime serviceAccountName error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsMultipleRuntimeWorkspaceVolumeSources(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-runtime-workspace-validation-")

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-workspace", Namespace: ns.Name},
		Spec: v1alpha1.RuntimeSpec{
			Workspace: &v1alpha1.RuntimeWorkspaceSpec{
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "workspace",
					},
				},
			},
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "runtime", Image: "runtime:latest"}},
				},
			},
		},
	}
	if err := k8sClient.Create(ctx, rt); !apierrors.IsInvalid(err) {
		t.Fatalf("multiple runtime workspace volume sources error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidPersistentWorkspaceRuntime(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-persistent-workspace-runtime-validation-")

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-runtime", Namespace: ns.Name},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime: "bad/runtime",
		},
	}
	if err := k8sClient.Create(ctx, workspace); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid persistent workspace runtime error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidPersistentWorkspaceMode(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-persistent-workspace-mode-validation-")

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "invalid-mode", Namespace: ns.Name},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime:       "bash",
			Mode:          v1alpha1.PersistentWorkspaceMode("PVC"),
			CleanupPolicy: v1alpha1.PersistentWorkspaceDeleteAfterTTL,
		},
	}
	if err := k8sClient.Create(ctx, workspace); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid persistent workspace mode error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsActionWithoutSteps(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-action-steps-validation-")

	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: ns.Name},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{},
		},
	}
	if err := k8sClient.Create(ctx, action); !apierrors.IsInvalid(err) {
		t.Fatalf("empty action steps error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsInvalidActionInputType(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-action-input-validation-")

	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "bad-input", Namespace: ns.Name},
		Spec: v1alpha1.ActionSpec{
			Inputs: map[string]v1alpha1.ActionInputSpec{
				"version": {Type: v1alpha1.ActionInputType("number")},
			},
			Steps: []v1alpha1.StepSpec{{Name: "setup", Run: "echo setup"}},
		},
	}
	if err := k8sClient.Create(ctx, action); !apierrors.IsInvalid(err) {
		t.Fatalf("invalid action input type error = %v, want Invalid", err)
	}
}

func TestCRDValidationRejectsActionStepUses(t *testing.T) {
	ctx := context.Background()
	ns := testNamespace(t, "test-action-step-uses-validation-")

	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "nested-uses", Namespace: ns.Name},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: "another-action"}},
		},
	}
	if err := k8sClient.Create(ctx, action); !apierrors.IsInvalid(err) {
		t.Fatalf("action step uses error = %v, want Invalid", err)
	}
}

func testNamespace(t *testing.T, generateName string) *corev1.Namespace {
	t.Helper()
	ns := &corev1.Namespace{}
	ns.GenerateName = generateName
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), ns) })
	return ns
}
