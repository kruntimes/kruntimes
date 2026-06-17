package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/krt"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

const testNamespace = "default"

var k8sClient client.Client
var restConfig *rest.Config
var coreClientset *kubernetes.Clientset

func bashRuntimeImage() string {
	if image := os.Getenv("KRUNTIMES_BASH_RUNTIME_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-bash-runtime:latest"
}

func pythonRuntimeImage() string {
	if image := os.Getenv("KRUNTIMES_PYTHON_RUNTIME_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-python-runtime:latest"
}

func runtimedImage() string {
	if image := os.Getenv("KRUNTIMES_RUNTIMED_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-runtimed:latest"
}

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	restConfig = config.GetConfigOrDie()
	restConfig.QPS = 50
	restConfig.Burst = 100

	var err error
	k8sClient, err = client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		os.Exit(1)
	}
	coreClientset, err = kubernetes.NewForConfig(restConfig)
	if err != nil {
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func ensureRuntime(t *testing.T, name, image string, port int32) {
	t.Helper()
	ensureRuntimeWithRunsCapacity(t, name, image, port, 0)
}

func ensureRuntimeWithRunsCapacity(t *testing.T, name, image string, port int32, runsCapacity int32) {
	t.Helper()

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: v1alpha1.RuntimeSpec{
			Image:     image,
			Port:      port,
			Replicas:  1,
			Command:   []string{fmt.Sprintf("--port=%d", port), "--work-dir=/workspace"},
			Resources: e2eRuntimeResources(),
		},
	}
	if runsCapacity > 0 {
		rt.Spec.Capacity = &v1alpha1.RuntimeCapacity{
			Resources: corev1.ResourceList{
				corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(int64(runsCapacity), resource.DecimalSI),
			},
		}
	}
	if err := k8sClient.Create(context.Background(), rt); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create runtime: %v", err)
	} else if apierrors.IsAlreadyExists(err) {
		existing := &v1alpha1.Runtime{}
		if getErr := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(rt), existing); getErr != nil {
			t.Fatalf("get runtime: %v", getErr)
		}
		existing.Spec.Image = image
		existing.Spec.Port = port
		existing.Spec.Replicas = 1
		existing.Spec.Command = []string{fmt.Sprintf("--port=%d", port), "--work-dir=/workspace"}
		existing.Spec.Resources = e2eRuntimeResources()
		if runsCapacity > 0 {
			existing.Spec.Capacity = rt.Spec.Capacity
		}
		if updateErr := k8sClient.Update(context.Background(), existing); updateErr != nil {
			t.Fatalf("update runtime: %v", updateErr)
		}
	}
	cleanupRuntime(t, name)

	waitForRuntimePod(t, name, image, runtimedImage(), runsCapacity, "runtime pods")
}

func ensureFilesystemRuntime(t *testing.T, name, claimName string) {
	t.Helper()
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: testNamespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), claim); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create artifact PVC: %v", err)
	}

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.RuntimeSpec{
			Image:     bashRuntimeImage(),
			Port:      9091,
			Replicas:  1,
			Command:   []string{"--port=9091", "--work-dir=/workspace"},
			Resources: e2eRuntimeResources(),
			ArtifactStore: &v1alpha1.RuntimeArtifactStoreSpec{
				Driver: v1alpha1.ArtifactDriverFilesystem,
				Filesystem: &v1alpha1.FilesystemArtifactStoreSpec{
					VolumeClaimName: claimName,
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), rt); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create filesystem runtime: %v", err)
		}
		existing := &v1alpha1.Runtime{}
		if getErr := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(rt), existing); getErr != nil {
			t.Fatalf("get filesystem runtime: %v", getErr)
		}
		existing.Spec = rt.Spec
		if updateErr := k8sClient.Update(context.Background(), existing); updateErr != nil {
			t.Fatalf("update filesystem runtime: %v", updateErr)
		}
	}
	cleanupRuntime(t, name)

	waitForRuntimePod(t, name, bashRuntimeImage(), runtimedImage(), 0, "filesystem runtime pod")
}

func e2eRuntimeResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("25m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
}

func cleanupRuntime(t *testing.T, name string) {
	t.Helper()
	if name == "bash" || name == "python" {
		return
	}
	t.Cleanup(func() {
		rt := &v1alpha1.Runtime{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
		if err := k8sClient.Delete(context.Background(), rt); err != nil && !apierrors.IsNotFound(err) {
			t.Logf("delete Runtime %s: %v", name, err)
		}
	})
}

func isRuntimePodReady(pod *corev1.Pod, runtimeImage, daemonImage string, runsCapacity int32) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	if containerImage(pod, "runtime") != runtimeImage || containerImage(pod, "runtimed") != daemonImage {
		return false
	}
	if runsCapacity > 0 {
		if runtimepod.RunsCapacity(pod, 0) != runsCapacity {
			return false
		}
	}
	podReady := false
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			podReady = cond.Status == corev1.ConditionTrue
			break
		}
	}
	return podReady && runtimepod.FreshRuntimedReady(pod, time.Now(), 30*time.Second)
}

func containerImage(pod *corev1.Pod, name string) string {
	for _, container := range pod.Spec.Containers {
		if container.Name == name {
			return container.Image
		}
	}
	return ""
}

func waitForRuntimePod(t *testing.T, name, runtimeImage, daemonImage string, runsCapacity int32, description string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var lastErr error
	for {
		var pods corev1.PodList
		err := k8sClient.List(ctx, &pods,
			client.InNamespace(testNamespace),
			client.MatchingLabels{"runtime": name},
		)
		if err == nil {
			for _, pod := range pods.Items {
				if isRuntimePodReady(&pod, runtimeImage, daemonImage, runsCapacity) {
					return
				}
			}
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			dumpRuntimeDiagnostics(t, name, runtimeImage, daemonImage, runsCapacity, lastErr)
			t.Fatalf("timed out waiting for %s", description)
		case <-time.After(2 * time.Second):
		}
	}
}

func dumpRuntimeDiagnostics(t *testing.T, name, runtimeImage, daemonImage string, runsCapacity int32, lastErr error) {
	t.Helper()
	t.Logf("Runtime %s diagnostics: expected runtime image=%s runtimed image=%s runsCapacity=%d", name, runtimeImage, daemonImage, runsCapacity)
	if lastErr != nil {
		t.Logf("last pod list error: %v", lastErr)
	}

	var rt v1alpha1.Runtime
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &rt); err != nil {
		t.Logf("get Runtime %s: %v", name, err)
	} else {
		t.Logf("Runtime %s: generation=%d replicas=%d readyReplicas=%d image=%s daemonImage=%s port=%d",
			name, rt.Generation, rt.Spec.Replicas, rt.Status.ReadyReplicas, rt.Spec.Image, rt.Spec.DaemonImage, rt.Spec.Port)
		for _, cond := range rt.Status.Conditions {
			t.Logf("  Runtime condition: type=%s status=%s reason=%s message=%s", cond.Type, cond.Status, cond.Reason, cond.Message)
		}
	}

	var deploy appsv1.Deployment
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "runtime-" + name}, &deploy); err != nil {
		t.Logf("get Deployment runtime-%s: %v", name, err)
	} else {
		t.Logf("Deployment %s: generation=%d observedGeneration=%d replicas=%d ready=%d available=%d unavailable=%d",
			deploy.Name, deploy.Generation, deploy.Status.ObservedGeneration, deploy.Status.Replicas, deploy.Status.ReadyReplicas, deploy.Status.AvailableReplicas, deploy.Status.UnavailableReplicas)
		for _, cond := range deploy.Status.Conditions {
			t.Logf("  Deployment condition: type=%s status=%s reason=%s message=%s", cond.Type, cond.Status, cond.Reason, cond.Message)
		}
	}

	var pods corev1.PodList
	if err := k8sClient.List(context.Background(), &pods, client.InNamespace(testNamespace), client.MatchingLabels{"runtime": name}); err != nil {
		t.Logf("list Runtime pods: %v", err)
		return
	}
	if len(pods.Items) == 0 {
		t.Log("Runtime pod list is empty")
	}
	for i := range pods.Items {
		logPodDiagnostics(t, &pods.Items[i])
	}
}

func logPodDiagnostics(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	t.Logf("Pod %s: phase=%s deletion=%v node=%s runtimeImage=%s runtimedImage=%s runsCapacity=%d",
		pod.Name, pod.Status.Phase, pod.DeletionTimestamp != nil, pod.Spec.NodeName,
		containerImage(pod, "runtime"), containerImage(pod, "runtimed"), runtimepod.RunsCapacity(pod, 0))
	for _, cond := range pod.Status.Conditions {
		t.Logf("  Pod condition: type=%s status=%s reason=%s message=%s lastProbe=%s lastTransition=%s",
			cond.Type, cond.Status, cond.Reason, cond.Message, cond.LastProbeTime.Time.Format(time.RFC3339), cond.LastTransitionTime.Time.Format(time.RFC3339))
	}
	for _, status := range pod.Status.ContainerStatuses {
		t.Logf("  Container %s: ready=%t restartCount=%d image=%s state=%s lastState=%s",
			status.Name, status.Ready, status.RestartCount, status.Image, formatContainerState(status.State), formatContainerState(status.LastTerminationState))
	}
	listPodEvents(t, pod)
}

func listPodEvents(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if coreClientset == nil {
		return
	}
	selector := fields.OneTermEqualSelector("involvedObject.name", pod.Name).String()
	events, err := coreClientset.CoreV1().Events(pod.Namespace).List(context.Background(), metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		t.Logf("  list pod events: %v", err)
		return
	}
	for _, event := range events.Items {
		t.Logf("  Event: type=%s reason=%s count=%d message=%s", event.Type, event.Reason, event.Count, event.Message)
	}
}

func formatContainerState(state corev1.ContainerState) string {
	switch {
	case state.Running != nil:
		return "running"
	case state.Waiting != nil:
		return fmt.Sprintf("waiting(%s: %s)", state.Waiting.Reason, state.Waiting.Message)
	case state.Terminated != nil:
		return fmt.Sprintf("terminated(%s exit=%d: %s)", state.Terminated.Reason, state.Terminated.ExitCode, state.Terminated.Message)
	default:
		return "unknown"
	}
}

func waitForRun(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	waitForRunPhase(t, run, timeout, v1alpha1.RunSucceeded)
}

func waitForRunPhase(t *testing.T, run *v1alpha1.Run, timeout time.Duration, expected v1alpha1.RunPhase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastPhase v1alpha1.RunPhase
	var lastAttempt int32
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run %s, last phase=%s, attempt=%d, msg=%s", run.Name, lastPhase, lastAttempt, run.Status.Message)
		default:
		}

		time.Sleep(500 * time.Millisecond)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}

		if run.Status.Phase != lastPhase || run.Status.Attempt != lastAttempt {
			t.Logf("Run %s: phase=%s, attempt=%d (pod=%s)", run.Name, run.Status.Phase, run.Status.Attempt, run.Status.AssignedPod)
			for _, c := range run.Status.Conditions {
				t.Logf("  Condition: type=%s status=%s reason=%s", c.Type, c.Status, c.Reason)
			}
			lastPhase = run.Status.Phase
			lastAttempt = run.Status.Attempt
		}

		switch run.Status.Phase {
		case expected:
			return
		case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
			t.Fatalf("expected phase=%s, got phase=%s, msg=%s (attempt=%d)", expected, run.Status.Phase, run.Status.Message, run.Status.Attempt)
		}
	}
}

func waitForAnyTerminalRunPhase(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for terminal run %s, last phase=%s msg=%s", run.Name, run.Status.Phase, run.Status.Message)
		default:
		}

		time.Sleep(500 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		switch run.Status.Phase {
		case v1alpha1.RunSucceeded, v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
			return
		}
	}
}

func findRunCondition(run *v1alpha1.Run, typ string) *metav1.Condition {
	for i := range run.Status.Conditions {
		if run.Status.Conditions[i].Type == typ {
			return &run.Status.Conditions[i]
		}
	}
	return nil
}

func assertCancelledRun(t *testing.T, run *v1alpha1.Run) {
	t.Helper()
	if run.Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("phase = %s, want Cancelled", run.Status.Phase)
	}
	if run.Status.CompletionTime == nil {
		t.Fatal("expected completion time for cancelled run")
	}
	running := findRunCondition(run, "Running")
	if running == nil {
		t.Fatal("expected Running condition")
	}
	if running.Status != metav1.ConditionFalse || running.Reason != "Cancelled" {
		t.Fatalf("expected Running=False reason=Cancelled, got status=%s reason=%s", running.Status, running.Reason)
	}
	completed := findRunCondition(run, "Completed")
	if completed == nil {
		t.Fatal("expected Completed condition")
	}
	if completed.Status != metav1.ConditionFalse || completed.Reason != "Cancelled" {
		t.Fatalf("expected Completed=False reason=Cancelled, got status=%s reason=%s", completed.Status, completed.Reason)
	}
}

func requestRunCancel(t *testing.T, run *v1alpha1.Run) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run for cancel: %v", err)
		}
		run.Spec.CancelRequested = true
		if err := k8sClient.Update(context.Background(), run); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("failed to request cancellation for run %s", run.Name)
}

func waitForRunDeleted(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		var current v1alpha1.Run
		err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), &current)
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatalf("get run while waiting for delete: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run %s to be deleted, phase=%s completion=%v", run.Name, current.Status.Phase, current.Status.CompletionTime)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForWorkflow(t *testing.T, wf *v1alpha1.Workflow, timeout time.Duration) {
	t.Helper()
	waitForWorkflowPhase(t, wf, timeout, v1alpha1.WorkflowSucceeded)
}

func waitForWorkflowPhase(t *testing.T, wf *v1alpha1.Workflow, timeout time.Duration, expected v1alpha1.WorkflowPhase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for workflow %s phase=%s, last phase=%s msg=%s", wf.Name, expected, wf.Status.Phase, wf.Status.Message)
		default:
		}
		time.Sleep(500 * time.Millisecond)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(wf), wf); err != nil {
			t.Fatalf("get workflow: %v", err)
		}
		t.Logf("Workflow %s: phase=%s", wf.Name, wf.Status.Phase)

		switch wf.Status.Phase {
		case expected:
			return
		case v1alpha1.WorkflowSucceeded, v1alpha1.WorkflowFailed:
			t.Fatalf("Workflow failed: %s", wf.Status.Message)
		}
	}
}

func waitForWorkflowChildRun(t *testing.T, workflowName, jobName, stepName string, timeout time.Duration) *v1alpha1.Run {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for {
		var runs v1alpha1.RunList
		if err := k8sClient.List(context.Background(), &runs,
			client.InNamespace(testNamespace),
			client.MatchingLabels{"workflow": workflowName, "job": jobName, "step": stepName},
		); err != nil {
			t.Fatalf("list workflow child runs: %v", err)
		}
		if len(runs.Items) == 1 {
			return &runs.Items[0]
		}
		if len(runs.Items) > 1 {
			t.Fatalf("expected one child run for %s/%s/%s, got %d", workflowName, jobName, stepName, len(runs.Items))
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for child run workflow=%s job=%s step=%s", workflowName, jobName, stepName)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func TestFullRunLifecycle(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	const stdout = "hello-not-in-run-status"
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{"echo " + stdout},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (runtime=bash)", run.Name)
	waitForRun(t, run, 30*time.Second)
	if run.Status.Message != "execution completed" {
		t.Fatalf("success message = %q, want stable summary", run.Status.Message)
	}
	if strings.Contains(run.Status.Message, stdout) {
		t.Fatalf("success message contains stdout: %q", run.Status.Message)
	}
	t.Logf("Run completed successfully: %s", run.Status.Message)
}

func TestFilesystemArtifacts(t *testing.T) {
	runtimeName := "bash-filesystem-artifacts"
	claimName := "e2e-filesystem-artifacts"
	ensureFilesystemRuntime(t, runtimeName, claimName)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-artifacts-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args: []string{
				`mkdir -p "$KRUNTIME_ARTIFACTS_DIR/bundle"; printf report > "$KRUNTIME_ARTIFACTS_DIR/report.txt"; printf nested > "$KRUNTIME_ARTIFACTS_DIR/bundle/data.txt"`,
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create artifact run: %v", err)
	}
	waitForRun(t, run, 30*time.Second)

	if len(run.Status.ArtifactRefs) != 2 {
		t.Fatalf("artifact refs = %#v, want 2", run.Status.ArtifactRefs)
	}
	var report, bundle *v1alpha1.ArtifactRef
	for i := range run.Status.ArtifactRefs {
		ref := &run.Status.ArtifactRefs[i]
		if ref.Driver != v1alpha1.ArtifactDriverFilesystem ||
			ref.Location.Filesystem == nil ||
			ref.Location.Filesystem.VolumeClaimName != claimName {
			t.Fatalf("invalid filesystem artifact ref: %#v", ref)
		}
		if ref.Name == "report.txt" {
			report = ref
		}
		if ref.Name == "bundle" {
			bundle = ref
		}
	}
	if report == nil || report.SizeBytes != int64(len("report")) || !strings.HasPrefix(report.Digest, "sha256:") {
		t.Fatalf("report ref = %#v", report)
	}
	if bundle == nil || bundle.Type != v1alpha1.ArtifactTypeDirectory ||
		bundle.ContentType != "application/gzip" ||
		!strings.HasPrefix(bundle.Digest, "sha256:") {
		t.Fatalf("bundle ref = %#v", bundle)
	}

	downloadDir := t.TempDir()
	reportPath := filepath.Join(downloadDir, "report.txt")
	if _, err := krt.DownloadArtifact(context.Background(), k8sClient, restConfig, testNamespace, run.Name, report.Name, reportPath, 19093); err != nil {
		t.Fatalf("download report artifact: %v", err)
	}
	reportContent, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(reportContent) != "report" {
		t.Fatalf("downloaded report = %q, want report", reportContent)
	}

	bundlePath := filepath.Join(downloadDir, "bundle.tar.gz")
	if _, err := krt.DownloadArtifact(context.Background(), k8sClient, restConfig, testNamespace, run.Name, bundle.Name, bundlePath, 19094); err != nil {
		t.Fatalf("download directory artifact: %v", err)
	}
	assertTarGzFile(t, bundlePath, "data.txt", "nested")

	artifactPath := "/var/lib/kruntimes/artifacts/" + report.Location.Filesystem.Path
	ttlSeconds := int32(1)
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get artifact run for TTL: %v", err)
		}
		run.Spec.TTLSecondsAfterFinished = &ttlSeconds
		if err := k8sClient.Update(context.Background(), run); err == nil {
			break
		}
		if i == 9 {
			t.Fatal("failed to set artifact Run TTL")
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitForRunDeleted(t, run, 30*time.Second)

	if _, stderr, err := execInPod(context.Background(), run.Status.AssignedPod, "runtimed", []string{"test", "!", "-e", artifactPath}); err != nil {
		t.Fatalf("artifact remained after Run deletion: %v: %s", err, stderr)
	}
}

func assertTarGzFile(t *testing.T, path, name, wantContent string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			t.Fatalf("archive does not contain %s", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != name {
			continue
		}
		content, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != wantContent {
			t.Fatalf("archive %s = %q, want %q", name, content, wantContent)
		}
		return
	}
}

func TestRunTimeout(t *testing.T) {
	runtimeName := "bash-timeout"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-timeout-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 10; echo should_not_print"},
			Timeout: &metav1.Duration{Duration: time.Second},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (timeout)", run.Name)

	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunTimeout)

	if run.Status.Attempt != 1 {
		t.Fatalf("expected one attempt, got %d", run.Status.Attempt)
	}
	if !strings.Contains(run.Status.Message, "timeout") {
		t.Fatalf("expected timeout message, got %q", run.Status.Message)
	}
	if run.Status.CompletionTime == nil {
		t.Fatal("expected completion time for timed out run")
	}

	running := findRunCondition(run, "Running")
	if running == nil {
		t.Fatal("expected Running condition")
	}
	if running.Status != metav1.ConditionFalse || running.Reason != "Timeout" {
		t.Fatalf("expected Running=False reason=Timeout, got status=%s reason=%s", running.Status, running.Reason)
	}

	completed := findRunCondition(run, "Completed")
	if completed == nil {
		t.Fatal("expected Completed condition")
	}
	if completed.Status != metav1.ConditionFalse || completed.Reason != "Timeout" {
		t.Fatalf("expected Completed=False reason=Timeout, got status=%s reason=%s", completed.Status, completed.Reason)
	}

	t.Logf("Run timed out correctly: %s", run.Status.Message)
}

func TestCompletedRunTTLGCDeletesFinishedRun(t *testing.T) {
	runtimeName := "bash-ttl-gc"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)
	ttlSeconds := int32(2)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-ttl-gc-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime:                 runtimeName,
			Args:                    []string{"echo ttl-gc"},
			TTLSecondsAfterFinished: &ttlSeconds,
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (ttl gc)", run.Name)

	waitForRun(t, run, 30*time.Second)
	if run.Status.CompletionTime == nil {
		t.Fatal("expected completion time before TTL GC")
	}
	waitForRunDeleted(t, run, 30*time.Second)
}

func TestRuntimedRecoversRunningRunAfterRestart(t *testing.T) {
	runtimeName := "bash-recovery"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-recovery-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 20; echo recovered"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (runtimed recovery)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning && run.Status.AssignedPod != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run to start, phase=%s pod=%s msg=%s",
				run.Status.Phase, run.Status.AssignedPod, run.Status.Message)
		default:
		}
	}

	beforeRestart := runtimedRestartCount(t, run.Status.AssignedPod)
	killRuntimed(t, run.Status.AssignedPod)
	waitForRuntimedRestart(t, run.Status.AssignedPod, beforeRestart)

	waitForRun(t, run, 60*time.Second)
	if run.Status.Message != "execution completed" {
		t.Fatalf("success message = %q, want stable summary", run.Status.Message)
	}
}

func TestCancelPendingRunWithoutRuntimePod(t *testing.T) {
	runtimeName := fmt.Sprintf("missing-cancel-runtime-%d", time.Now().UnixNano())
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-cancel-pending-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 10"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (pending cancel)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunPending && run.Status.Message != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for pending run, phase=%s msg=%s", run.Status.Phase, run.Status.Message)
		default:
		}
	}

	requestRunCancel(t, run)
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunCancelled)
	assertCancelledRun(t, run)
}

func TestCancelRunningRunDoesNotRetry(t *testing.T) {
	runtimeName := "bash-cancel-running"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-cancel-running-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 30; echo should_not_finish"},
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 3,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (running cancel)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for running run, phase=%s msg=%s", run.Status.Phase, run.Status.Message)
		default:
		}
	}

	requestRunCancel(t, run)
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunCancelled)
	assertCancelledRun(t, run)
	if run.Status.Attempt != 1 {
		t.Fatalf("cancelled run attempt = %d, want 1", run.Status.Attempt)
	}
}

func TestCancelNearCompletionHasStableTerminalPhase(t *testing.T) {
	runtimeName := "bash-cancel-boundary"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-cancel-boundary-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 1; echo boundary_done"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (completion-boundary cancel)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		time.Sleep(100 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for running run, phase=%s msg=%s", run.Status.Phase, run.Status.Message)
		default:
		}
	}

	time.Sleep(900 * time.Millisecond)
	requestRunCancel(t, run)

	waitForAnyTerminalRunPhase(t, run, 30*time.Second)
	terminal := run.Status.Phase
	switch terminal {
	case v1alpha1.RunSucceeded:
	case v1alpha1.RunCancelled:
		assertCancelledRun(t, run)
	default:
		t.Fatalf("unexpected terminal phase after boundary cancel: %s msg=%s", terminal, run.Status.Message)
	}

	time.Sleep(2 * time.Second)
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
		t.Fatalf("get run after terminal: %v", err)
	}
	if run.Status.Phase != terminal {
		t.Fatalf("terminal phase changed from %s to %s", terminal, run.Status.Phase)
	}
}

func TestSchedulerResponsiveness(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-perf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for {
		time.Sleep(200 * time.Millisecond)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}

		if run.Status.Phase != v1alpha1.RunPending {
			elapsed := time.Since(start)
			t.Logf("Run scheduled in %v (phase=%s, pod=%s)", elapsed, run.Status.Phase, run.Status.AssignedPod)
			return
		}

		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for scheduler to pick up run")
		default:
		}
	}
}

func TestSchedulerKeepsRunPendingWithoutRuntimePod(t *testing.T) {
	runtimeName := fmt.Sprintf("missing-runtime-%d", time.Now().UnixNano())
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-no-runtime-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), run) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}

		switch run.Status.Phase {
		case v1alpha1.RunFailed, v1alpha1.RunScheduled, v1alpha1.RunRunning, v1alpha1.RunSucceeded:
			t.Fatalf("expected run to stay Pending without runtime pods, got phase=%s pod=%s msg=%s",
				run.Status.Phase, run.Status.AssignedPod, run.Status.Message)
		}

		if run.Status.Phase == v1alpha1.RunPending &&
			run.Status.AssignedPod == "" &&
			strings.Contains(run.Status.Message, "waiting for available runtime pods") {
			return
		}

		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for pending run observation, phase=%s pod=%s msg=%s",
				run.Status.Phase, run.Status.AssignedPod, run.Status.Message)
		default:
		}
	}
}

func TestSchedulerKeepsRunPendingWhenRuntimeAtCapacity(t *testing.T) {
	runtimeName := "bash-capacity"
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	first := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-capacity-first-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 10; echo first"},
		},
	}
	if err := k8sClient.Create(context.Background(), first); err != nil {
		t.Fatalf("create first run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), first) }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(first), first); err != nil {
			t.Fatalf("get first run: %v", err)
		}
		if first.Status.Phase == v1alpha1.RunRunning {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for first run to start, phase=%s msg=%s", first.Status.Phase, first.Status.Message)
		default:
		}
	}

	second := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-capacity-second-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"echo second"},
		},
	}
	if err := k8sClient.Create(context.Background(), second); err != nil {
		t.Fatalf("create second run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), second) }()

	pendingCtx, pendingCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer pendingCancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(second), second); err != nil {
			t.Fatalf("get second run: %v", err)
		}
		if second.Status.Phase != "" && second.Status.Phase != v1alpha1.RunPending {
			t.Fatalf("expected second run to stay Pending while capacity is full, got phase=%s pod=%s msg=%s",
				second.Status.Phase, second.Status.AssignedPod, second.Status.Message)
		}
		if second.Status.AssignedPod != "" {
			t.Fatalf("expected second run to remain unassigned while capacity is full, got pod=%s", second.Status.AssignedPod)
		}
		select {
		case <-pendingCtx.Done():
			goto capacityObserved
		default:
		}
	}

capacityObserved:
	waitForRun(t, first, 20*time.Second)
	waitForRun(t, second, 30*time.Second)
}

func killRuntimed(t *testing.T, podName string) {
	t.Helper()
	if _, stderr, err := execInPod(context.Background(), podName, "runtimed", []string{"/bin/sh", "-c", "kill 1"}); err != nil {
		t.Logf("kill runtimed returned expected process termination error: %v", err)
		if stderr != "" {
			t.Logf("kill runtimed stderr: %s", stderr)
		}
	}
}

func execInPod(ctx context.Context, podName, containerName string, command []string) (string, string, error) {
	req := coreClientset.CoreV1().RESTClient().Post().
		Namespace(testNamespace).
		Resource("pods").
		Name(podName).
		SubResource("exec")
	req.VersionedParams(&corev1.PodExecOptions{
		Container: containerName,
		Command:   command,
		Stdout:    true,
		Stderr:    true,
	}, clientgoscheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	return stdout.String(), stderr.String(), err
}

func runtimedRestartCount(t *testing.T, podName string) int32 {
	t.Helper()

	var pod corev1.Pod
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Name: podName, Namespace: testNamespace}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == "runtimed" {
			return status.RestartCount
		}
	}
	t.Fatalf("pod %s has no runtimed container status", podName)
	return 0
}

func waitForRuntimedRestart(t *testing.T, podName string, previousRestartCount int32) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	for {
		if runtimedRestartCount(t, podName) > previousRestartCount {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for runtimed container restart in pod %s", podName)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func TestPythonInlineRun(t *testing.T) {
	ensureRuntime(t, "python", pythonRuntimeImage(), 9092)

	inline := `print("hello from python")`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-py-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "python",
			Source:  &v1alpha1.CodeSource{Inline: &inline},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Python Run %s", run.Name)
	waitForRun(t, run, 30*time.Second)
	t.Logf("Python Run completed successfully: %s", run.Status.Message)
}

func TestWorkflowSingleJob(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-wf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "hello",
						Run:  "echo hello_from_workflow",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Logf("Created Workflow %s", wf.Name)
	waitForWorkflow(t, wf, 30*time.Second)
	t.Logf("Workflow succeeded: %s", wf.Status.Message)
}

func TestWorkflowChildRunCancellationFailsWorkflow(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-wf-cancel-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "long-run",
						Run:  "sleep 300",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Logf("Created Workflow %s for child cancel", wf.Name)

	child := waitForWorkflowChildRun(t, wf.Name, "test", "long-run", 30*time.Second)
	waitForRunPhase(t, child, 30*time.Second, v1alpha1.RunRunning)
	requestRunCancel(t, child)
	waitForRunPhase(t, child, 30*time.Second, v1alpha1.RunCancelled)
	waitForWorkflowPhase(t, wf, 30*time.Second, v1alpha1.WorkflowFailed)

	job := wf.Status.Jobs["test"]
	if job.Phase != v1alpha1.JobFailed {
		t.Fatalf("job phase = %s, want Failed", job.Phase)
	}
	step := job.Steps["long-run"]
	if step.Phase != v1alpha1.StepFailed {
		t.Fatalf("step phase = %s, want Failed", step.Phase)
	}
	if step.RunName != child.Name {
		t.Fatalf("step runName = %q, want child run %q", step.RunName, child.Name)
	}
}

func TestWorkflowLongNamesCreateHashedChildRun(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	workflowName := "wf-" + strings.Repeat("a", 60)
	jobName := "job-" + strings.Repeat("b", 59)
	stepName := "step-" + strings.Repeat("c", 58)
	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workflowName,
			Namespace: testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				jobName: {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: stepName,
						Run:  "echo long_names_ok",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow with long names: %v", err)
	}
	t.Logf("Created long-name Workflow %s", wf.Name)
	waitForWorkflow(t, wf, 30*time.Second)

	child := waitForWorkflowChildRun(t, wf.Name, jobName, stepName, 10*time.Second)
	if len(child.Name) > 63 {
		t.Fatalf("child run name length = %d, want <= 63: %s", len(child.Name), child.Name)
	}
	if rawName := fmt.Sprintf("wf-%s-%s-%s", wf.Name, jobName, stepName); child.Name == rawName {
		t.Fatalf("child run name was not hashed: %s", child.Name)
	}
	step := wf.Status.Jobs[jobName].Steps[stepName]
	if step.RunName != child.Name {
		t.Fatalf("step runName = %q, want child run %q", step.RunName, child.Name)
	}
}

func TestWorkflowStepOutputs(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-wf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{
						{
							Name: "gen-version",
							Run:  `echo version=v1.0 >> "$KRUNTIME_OUTPUTS"`,
						},
						{
							Name: "build-image",
							Run:  `echo image=app:${{ steps.gen-version.outputs.version }} >> "$KRUNTIME_OUTPUTS"`,
						},
					},
					Outputs: map[string]string{
						"artifact": "${{ steps.build-image.outputs.image }}",
					},
				},
				"deploy": {
					RunsOn: "bash",
					Needs:  []string{"build"},
					Steps: []v1alpha1.StepSpec{{
						Name: "deploy-step",
						Run:  "echo deploying ${{ jobs.build.outputs.artifact }}",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Logf("Created Workflow %s", wf.Name)
	waitForWorkflow(t, wf, 60*time.Second)
	buildJob := wf.Status.Jobs["build"]
	buildStep := buildJob.Steps["build-image"]
	if buildStep.Outputs == nil || buildStep.Outputs["image"] != "app:v1.0" {
		t.Fatalf("build-image outputs mismatch: got %v", buildStep.Outputs)
	}
	t.Logf("All outputs verified")
}

func TestRunInvalidOutputsDoesNotRetry(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-invalid-outputs-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{`printf 'invalid\n' > "$KRUNTIME_OUTPUTS"`},
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 3,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunFailed)
	assertOutputsFailure(t, run, "OutputsInvalid")
}

func TestRunOversizedOutputsDoesNotRetry(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-oversized-outputs-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args: []string{
				`printf 'value=' > "$KRUNTIME_OUTPUTS"; head -c 8193 /dev/zero | tr '\0' x >> "$KRUNTIME_OUTPUTS"`,
			},
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 3,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunFailed)
	assertOutputsFailure(t, run, "OutputsTooLarge")
}

func assertOutputsFailure(t *testing.T, run *v1alpha1.Run, reason string) {
	t.Helper()
	if run.Status.Attempt != 1 {
		t.Fatalf("attempt = %d, want 1 for non-retryable outputs failure", run.Status.Attempt)
	}
	if run.Status.CompletionTime == nil {
		t.Fatal("expected completion time")
	}
	running := findRunCondition(run, "Running")
	if running == nil || running.Status != metav1.ConditionFalse || running.Reason != reason {
		t.Fatalf("Running condition = %#v, want False/%s", running, reason)
	}
	completed := findRunCondition(run, "Completed")
	if completed == nil || completed.Status != metav1.ConditionFalse || completed.Reason != reason {
		t.Fatalf("Completed condition = %#v, want False/%s", completed, reason)
	}
}

func TestRunRetry(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	// Script that fails the first 2 times, succeeds on the 3rd.
	// Uses a counter file in the workspace (which persists across retries).
	inline := `#!/bin/bash
COUNTER_FILE=retry_count
if [ -f "$COUNTER_FILE" ]; then
  count=$(cat "$COUNTER_FILE")
else
  count=0
fi
count=$((count + 1))
echo "$count" > "$COUNTER_FILE"
if [ "$count" -lt 3 ]; then
  echo "attempt $count, failing intentionally"
  exit 1
fi
echo "succeeded on attempt $count"
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-retry-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime:    "bash",
			Source:     &v1alpha1.CodeSource{Inline: &inline},
			Entrypoint: "script.sh",
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 5,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (retry test)", run.Name)
	waitForRun(t, run, 30*time.Second)
	if run.Status.Attempt < 3 {
		t.Fatalf("expected at least 3 attempts, got %d", run.Status.Attempt)
	}
	t.Logf("Run succeeded after %d attempts: %s", run.Status.Attempt, run.Status.Message)
}

func TestWorkflowTopoOrder(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-wf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				// prep has no needs, runs first.
				"prep": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "generate",
						Run:  `echo version=v2.0 >> "$KRUNTIME_OUTPUTS"`,
					}},
				},
				// lint also has no needs, runs in parallel with prep.
				"lint": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "check",
						Run:  `echo lint=ok >> "$KRUNTIME_OUTPUTS"`,
					}},
				},
				// build needs prep explicitly, and references lint's output implicitly.
				"build": {
					RunsOn: "bash",
					Needs:  []string{"prep"},
					Steps: []v1alpha1.StepSpec{{
						Name: "compile",
						Run:  "echo image=app:${{ jobs.prep.outputs.version }}:${{ jobs.lint.outputs.lint }}",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Logf("Created Workflow %s", wf.Name)
	waitForWorkflow(t, wf, 60*time.Second)
	t.Logf("All jobs completed in correct order")
}

func TestStaleRunNoRetry(t *testing.T) {
	runtimeName := "bash-stale-no-retry"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-stale-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 300"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (stale, no retry)", run.Name)

	// Wait for Run to be Running on a pod.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning {
			t.Logf("Run running on pod %s", run.Status.AssignedPod)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run to start, phase=%s", run.Status.Phase)
		default:
		}
	}

	// Delete the assigned pod.
	podName := run.Status.AssignedPod
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNamespace}}
	if err := k8sClient.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod %s: %v", podName, err)
	}
	t.Logf("Deleted pod %s", podName)

	// Wait for stale reaper to detect and fail the Run.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel2()
	var lastPhase v1alpha1.RunPhase
	for {
		time.Sleep(500 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase != lastPhase {
			t.Logf("Run %s: phase=%s, attempt=%d (pod=%s)", run.Name, run.Status.Phase, run.Status.Attempt, run.Status.AssignedPod)
			lastPhase = run.Status.Phase
		}
		switch run.Status.Phase {
		case v1alpha1.RunFailed:
			t.Logf("Run correctly marked Failed after pod deletion: %s", run.Status.Message)
			return
		case v1alpha1.RunSucceeded:
			t.Error("expected Failed, got Succeeded")
			return
		}
		select {
		case <-ctx2.Done():
			t.Fatalf("timed out waiting for stale detection, phase=%s", run.Status.Phase)
		default:
		}
	}
}

func TestStaleRunWithRetry(t *testing.T) {
	runtimeName := "bash-stale-retry"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-stale-retry-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Args:    []string{"sleep 300"},
			RetryPolicy: &v1alpha1.RetryPolicy{
				MaxAttempts: 3,
				Backoff:     metav1.Duration{Duration: time.Second},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (stale, with retry)", run.Name)

	// Wait for Run to be Running.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning {
			t.Logf("Run running on pod %s", run.Status.AssignedPod)
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run to start, phase=%s", run.Status.Phase)
		default:
		}
	}

	// Delete the assigned pod.
	podName := run.Status.AssignedPod
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: testNamespace}}
	if err := k8sClient.Delete(context.Background(), pod); err != nil {
		t.Fatalf("delete pod %s: %v", podName, err)
	}
	t.Logf("Deleted pod %s", podName)

	// Wait for stale reaper to reset for retry, then scheduler re-assigns.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel2()
	for {
		time.Sleep(500 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunRunning && run.Status.Attempt >= 1 {
			t.Logf("Run retried on pod %s (attempt=%d)", run.Status.AssignedPod, run.Status.Attempt)
			return
		}
		select {
		case <-ctx2.Done():
			t.Fatalf("timed out waiting for retry, phase=%s attempt=%d", run.Status.Phase, run.Status.Attempt)
		default:
		}
	}
}
