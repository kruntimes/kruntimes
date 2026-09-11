package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/krt"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	"github.com/kruntimes/kruntimes/internal/runstatus"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
	"github.com/kruntimes/kruntimes/sdk/go/sandbox"
)

const testNamespace = "default"

const (
	certManagerE2EEnabledEnv      = "KRUNTIMES_E2E_CERT_MANAGER"
	certManagerGatewayCertificate = "kruntimes-gateway"
	certManagerGatewayTLSSecret   = "kruntimes-gateway-cert-manager-tls"
	gatewayBoundsE2EEnabledEnv    = "KRUNTIMES_E2E_GATEWAY_BOUNDS"
)

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

func diagnosisRuntimeImage() string {
	if image := os.Getenv("KRUNTIMES_DIAGNOSIS_RUNTIME_IMAGE"); image != "" {
		return image
	}
	return "kruntimes-diagnosis-runtime:latest"
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
	ensureRuntimeWithReplicasAndRunsCapacity(t, name, image, port, 1, runsCapacity)
}

func ensureRuntimeWithReplicasAndRunsCapacity(t *testing.T, name, image string, port, replicas, runsCapacity int32) {
	t.Helper()

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate(image, port),
			Port:     port,
			Replicas: replicas,
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
		existing.Spec.Template = rt.Spec.Template
		existing.Spec.Port = port
		existing.Spec.Replicas = replicas
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

func TestRuntimeReadyReplicasTracksRuntimedAvailability(t *testing.T) {
	runtimeName := "bash-readiness"
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)
	// A fresh E2E installation may wait for controller leader election before
	// creating the Deployment, so include that startup time in the initial wait.
	waitForRuntimeReadyReplicas(t, runtimeName, 1, 90*time.Second)

	podName := runtimePodName(t, runtimeName)
	previousRestartCount := runtimedRestartCount(t, podName)
	killRuntimed(t, podName)
	waitForRuntimeReadyReplicas(t, runtimeName, 0, 45*time.Second)

	waitForRuntimedRestart(t, podName, previousRestartCount)
	waitForRuntimeReadyReplicas(t, runtimeName, 1, 60*time.Second)
}

func runtimePodName(t *testing.T, runtimeName string) string {
	t.Helper()
	var pods corev1.PodList
	if err := k8sClient.List(context.Background(), &pods,
		client.InNamespace(testNamespace),
		client.MatchingLabels{"runtime": runtimeName, "app": "kruntimes-" + runtimeName},
	); err != nil {
		t.Fatalf("list Runtime Pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("Runtime %s Pods = %d, want 1", runtimeName, len(pods.Items))
	}
	return pods.Items[0].Name
}

func waitForRuntimeReadyReplicas(t *testing.T, runtimeName string, want int32, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	key := client.ObjectKey{Namespace: testNamespace, Name: runtimeName}
	for {
		runtimeResource := &v1alpha1.Runtime{}
		if err := k8sClient.Get(ctx, key, runtimeResource); err == nil && runtimeResource.Status.ReadyReplicas == want {
			return
		} else if ctx.Err() != nil {
			t.Fatalf("wait for Runtime %s readyReplicas=%d: %v", runtimeName, want, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
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
			Template: runtimePodTemplate(bashRuntimeImage(), 9091),
			Port:     9091,
			Replicas: 1,
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

func runtimePodTemplate(image string, port int32) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:      "runtime",
				Image:     image,
				Args:      []string{fmt.Sprintf("--port=%d", port), "--work-dir=/workspace"},
				Resources: e2eRuntimeResources(),
			}},
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
				if isRuntimePodReady(&pod, runtimeImage, daemonImage, runsCapacity) && runtimeServiceHasReadyEndpoint(ctx, name) {
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

// runtimeServiceHasReadyEndpoint verifies that the Runtime Service can route
// gateway requests to a ready runtimed Pod. Pod readiness alone does not prove
// that the EndpointSlice controller has published the Service endpoint yet.
func runtimeServiceHasReadyEndpoint(ctx context.Context, runtimeName string) bool {
	var slices discoveryv1.EndpointSliceList
	if err := k8sClient.List(ctx, &slices,
		client.InNamespace(testNamespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: "runtime-" + runtimeName},
	); err != nil {
		return false
	}
	for _, slice := range slices.Items {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Conditions.Ready == nil || *endpoint.Conditions.Ready {
				return true
			}
		}
	}
	return false
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
		runtimeImage := "<missing>"
		if len(rt.Spec.Template.Spec.Containers) > 0 {
			runtimeImage = rt.Spec.Template.Spec.Containers[0].Image
		}
		t.Logf("Runtime %s: generation=%d replicas=%d readyReplicas=%d image=%s daemonImage=%s port=%d",
			name, rt.Generation, rt.Spec.Replicas, rt.Status.ReadyReplicas, runtimeImage, rt.Spec.DaemonImage, rt.Spec.Port)
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

func waitForWorkflowRunPhase(t *testing.T, workflowRun *v1alpha1.WorkflowRun, timeout time.Duration, expected v1alpha1.WorkflowPhase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var lastPhase v1alpha1.WorkflowPhase
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for workflowrun %s, last phase=%s, msg=%s", workflowRun.Name, lastPhase, workflowRun.Status.Message)
		default:
		}

		time.Sleep(500 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			t.Fatalf("get workflowrun: %v", err)
		}
		if workflowRun.Status.Phase != lastPhase {
			t.Logf("WorkflowRun %s: phase=%s", workflowRun.Name, workflowRun.Status.Phase)
			lastPhase = workflowRun.Status.Phase
		}
		switch workflowRun.Status.Phase {
		case expected:
			return
		case v1alpha1.WorkflowSucceeded, v1alpha1.WorkflowFailed, v1alpha1.WorkflowCancelled:
			t.Fatalf("expected phase=%s, got phase=%s, msg=%s", expected, workflowRun.Status.Phase, workflowRun.Status.Message)
		}
	}
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
	if ready := findRunCondition(run, runstatus.ConditionReady); ready != nil && ready.Status != metav1.ConditionFalse {
		t.Fatalf("expected terminal Ready condition to be false, got status=%s reason=%s", ready.Status, ready.Reason)
	}
}

func requestRunCancel(t *testing.T, run *v1alpha1.Run) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run for cancel: %v", err)
		}
		run.Spec.Termination = &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationImmediate}
		if err := k8sClient.Update(context.Background(), run); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("failed to request cancellation for run %s", run.Name)
}

func requestRunDrain(t *testing.T, run *v1alpha1.Run) {
	t.Helper()
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run for drain: %v", err)
		}
		run.Spec.Termination = &v1alpha1.RunTermination{Mode: v1alpha1.RunTerminationDrain}
		if err := k8sClient.Update(context.Background(), run); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("failed to request drain for run %s", run.Name)
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

// deleteRunAndWait removes a Run while its Runtime is still available.  This
// matters for Session and Function Runs: their registration-cleanup finalizer
// must successfully release the remote Runtime Server state before deletion.
func deleteRunAndWait(t *testing.T, run *v1alpha1.Run, timeout time.Duration) {
	t.Helper()
	if err := k8sClient.Delete(context.Background(), run); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete Run %s: %v", run.Name, err)
	}
	waitForRunDeleted(t, run, timeout)
}

func taskMode(args ...string) v1alpha1.RunMode {
	return v1alpha1.RunMode{
		Task: &v1alpha1.RunTaskMode{Args: args},
	}
}

func TestSessionGatewayExecutesAuthorizedOperation(t *testing.T) {
	runtimeName := fmt.Sprintf("session-gateway-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-gateway-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
			Env: []corev1.EnvVar{
				{Name: "KRUNTIMES_SESSION_DEFAULT", Value: "registration"},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)
	if run.Status.Endpoint == nil || run.Status.Endpoint.Protocol != v1alpha1.RunEndpointProtocolHTTPS || len(run.Status.Endpoint.CABundle) == 0 {
		t.Fatalf("Session Run endpoint = %#v, want HTTPS gateway endpoint with a CA bundle", run.Status.Endpoint)
	}

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)

	statusResponse := waitForGatewayResponse(t, http.MethodGet, baseURL, token, nil, http.StatusOK)
	var sessionStatus struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(statusResponse, &sessionStatus); err != nil {
		t.Fatalf("decode Session status response: %v", err)
	}
	if sessionStatus.State != "SESSION_STATE_READY" {
		t.Fatalf("Session state = %q, want SESSION_STATE_READY", sessionStatus.State)
	}

	unauthenticated := waitForGatewayResponse(t, http.MethodGet, baseURL, "", nil, http.StatusUnauthorized)
	if !strings.Contains(string(unauthenticated), "bearer token is required") {
		t.Fatalf("unauthenticated response = %s", unauthenticated)
	}
	unauthorizedToken := sessionGatewayTokenWithoutRunAccess(t)
	_ = waitForGatewayResponse(t, http.MethodGet, baseURL, unauthorizedToken, nil, http.StatusForbidden)

	payload := []byte(`{"command":{"argv":["sh","-c","printf gateway-ok"]}}`)
	operationResponse := waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token, payload, http.StatusOK)
	var operation struct {
		Command struct {
			ExitCode int32  `json:"exitCode"`
			Stdout   []byte `json:"stdout"`
		} `json:"command"`
	}
	if err := json.Unmarshal(operationResponse, &operation); err != nil {
		t.Fatalf("decode Session operation response: %v", err)
	}
	if operation.Command.ExitCode != 0 || string(operation.Command.Stdout) != "gateway-ok" {
		t.Fatalf("Session command result = %#v, want successful gateway-ok output", operation.Command)
	}
	waitForSessionCommandLogs(t, run, "gateway-ok")

	envPayload := []byte(`{"command":{"argv":["sh","-c","printf '%s:%s' \"$KRUNTIMES_SESSION_DEFAULT\" \"$KRUNTIMES_SESSION_COMMAND\""],"env":{"KRUNTIMES_SESSION_COMMAND":"command"}}}`)
	envResponse := waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token, envPayload, http.StatusOK)
	if err := json.Unmarshal(envResponse, &operation); err != nil {
		t.Fatalf("decode Session environment response: %v", err)
	}
	if operation.Command.ExitCode != 0 || string(operation.Command.Stdout) != "registration:command" {
		t.Fatalf("Session command environment result = %#v, want registration:command", operation.Command)
	}

	writePayload := []byte(`{"writeFile":{"path":"notes/result.txt","contents":"Z2F0ZXdheS1maWxl","createParents":true}}`)
	_ = waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token, writePayload, http.StatusOK)
	escapingWrite := []byte(`{"writeFile":{"path":"../outside.txt","contents":"ZXNjYXBl","createParents":true}}`)
	_ = waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token, escapingWrite, http.StatusBadRequest)

	filesResponse := waitForGatewayResponse(t, http.MethodGet, baseURL+"/files?path=notes", token, nil, http.StatusOK)
	var files struct {
		Entries []struct {
			Path string `json:"path"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(filesResponse, &files); err != nil {
		t.Fatalf("decode Session files response: %v", err)
	}
	if !containsSessionFile(files.Entries, "result.txt") {
		t.Fatalf("Session files = %#v, want result.txt", files.Entries)
	}

	fileResponse := waitForGatewayResponse(t, http.MethodGet, baseURL+"/files/notes/result.txt", token, nil, http.StatusOK)
	var file struct {
		Contents []byte `json:"contents"`
	}
	if err := json.Unmarshal(fileResponse, &file); err != nil {
		t.Fatalf("decode Session file response: %v", err)
	}
	if string(file.Contents) != "gateway-file" {
		t.Fatalf("Session file contents = %q, want gateway-file", file.Contents)
	}

	requestRunCancel(t, run)
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunCancelled)
	assertCancelledRun(t, run)
	_ = waitForGatewayResponse(t, http.MethodGet, baseURL, token, nil, http.StatusConflict)
}

func TestGatewayServesAuthorizedRunLogs(t *testing.T) {
	runtimeName := fmt.Sprintf("run-logs-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-run-logs-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	marker := "gateway-run-log-e2e"
	_ = waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token,
		[]byte(`{"command":{"argv":["sh","-c","printf gateway-run-log-e2e"]}}`), http.StatusOK)
	waitForSessionCommandLogs(t, run, marker)
	requestRunCancel(t, run)
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunCancelled)

	// A Runtime Pod's logs are shared. Emit more than the former global source
	// tail through a later Run on the same Pod; the first Run's retained logs
	// must remain discoverable after it has released the Runtime slot.
	noisyRun := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-run-log-noise-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), noisyRun); err != nil {
		t.Fatalf("create noisy Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), noisyRun) })
	waitForRunPhase(t, noisyRun, 30*time.Second, v1alpha1.RunReady)
	noisyBaseURL := gatewayEndpointURL(t, waitForGatewayPod(t), noisyRun.Status.Endpoint.URL)
	noisyToken := sessionGatewayToken(t, noisyRun)
	for range 55 {
		_ = waitForGatewayResponse(t, http.MethodPost, noisyBaseURL+"/operations:execute", noisyToken,
			[]byte(`{"command":{"argv":["sh","-c","printf gateway-run-log-noise"]}}`), http.StatusOK)
	}
	waitForSessionCommandLogs(t, noisyRun, "gateway-run-log-noise")

	logURL := gatewayRunLogsURL(t, baseURL, run)
	_ = waitForGatewayResponse(t, http.MethodGet, logURL, sessionGatewayTokenWithoutRunAccess(t), nil, http.StatusForbidden)

	entries := waitForGatewayRunLogEntries(t, logURL, token, marker)
	if !hasGatewayRunLogEntry(entries, "stdout", marker) || !hasGatewayRunLogEntry(entries, "audit", "session operation completed") {
		t.Fatalf("snapshot Run logs = %#v, want stdout and audit records", entries)
	}
	// The CLI uses the same external Gateway contract. Its token has get only on
	// this Run: it deliberately has neither Pod discovery, pods/log, nor
	// pods/portforward permission.
	cliGatewayURL, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse Gateway URL for krt logs: %v", err)
	}
	cliGatewayURL.Path = ""
	cliGatewayURL.RawPath = ""
	cliGatewayURL.RawQuery = ""
	cmd := krt.NewRootCmd()
	var cliStdout, cliStderr bytes.Buffer
	cmd.SetOut(&cliStdout)
	cmd.SetErr(&cliStderr)
	cmd.SetArgs([]string{"logs", run.Name, "--namespace", testNamespace, "--token", token, "--gateway-url", cliGatewayURL.String(), "--tail", "100"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("krt logs through Gateway: %v\\nstderr: %s", err, cliStderr.String())
	}
	if !strings.Contains(cliStdout.String(), marker) {
		t.Fatalf("krt logs stdout = %q, want %q", cliStdout.String(), marker)
	}

	// With no Gateway URL, krt uses the aggregated API and the ordinary
	// kubeconfig bearer-token transport. This proves neither direct Gateway
	// reachability nor pods/log permission is needed for the default UX.
	cmd = krt.NewRootCmd()
	cliStdout.Reset()
	cliStderr.Reset()
	cmd.SetOut(&cliStdout)
	cmd.SetErr(&cliStderr)
	cmd.SetArgs([]string{"logs", run.Name, "--namespace", testNamespace, "--token", token, "--tail", "100"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("krt logs through aggregated API: %v\nstderr: %s", err, cliStderr.String())
	}
	if !strings.Contains(cliStdout.String(), marker) {
		t.Fatalf("krt logs through aggregated API stdout = %q, want %q", cliStdout.String(), marker)
	}

	followEntries := followGatewayRunLogs(t, logURL+"?tailLines=100&follow=true", token, marker)
	if !hasGatewayRunLogEntry(followEntries, "stdout", marker) {
		t.Fatalf("follow Run logs = %#v, want stdout record", followEntries)
	}
}

func TestGatewayFollowsOneShotRunLogsBeforeCompletion(t *testing.T) {
	runtimeName := fmt.Sprintf("live-task-logs-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	marker := "one-shot-live-log-e2e"
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-live-task-logs-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("printf '" + marker + "\\n'; sleep 10; printf 'completed\\n'"),
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create one-shot Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunRunning)

	// Task Runs do not expose a Session endpoint, but the log route itself is
	// independent of it. Use the shared Gateway port-forward at its root.
	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), "http://runtime-gateway/")
	token := sessionGatewayToken(t, run)
	entries := followGatewayRunLogs(t, gatewayRunLogsURL(t, baseURL, run)+"?tailLines=100&follow=true", token, marker)
	if !hasGatewayRunLogEntry(entries, "stdout", marker) {
		t.Fatalf("follow Run logs = %#v, want stdout marker", entries)
	}

	var current v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatalf("get one-shot Run after live log: %v", err)
	}
	if current.Status.Phase != v1alpha1.RunRunning {
		t.Fatalf("Run phase after receiving live log = %s, want Running", current.Status.Phase)
	}
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunSucceeded)
}

type gatewayRunLogEntry struct {
	Stream    string `json:"stream"`
	Message   string `json:"message"`
	Operation string `json:"operation"`
	Outcome   string `json:"outcome"`
}

func gatewayRunLogsURL(t *testing.T, gatewayURL string, run *v1alpha1.Run) string {
	t.Helper()
	parsed, err := url.Parse(gatewayURL)
	if err != nil {
		t.Fatalf("parse gateway URL %q: %v", gatewayURL, err)
	}
	return fmt.Sprintf("%s://%s/v1/namespaces/%s/runtimes/%s/runs/%s/logs", parsed.Scheme, parsed.Host,
		url.PathEscape(run.Namespace), url.PathEscape(run.Spec.Runtime), url.PathEscape(string(run.UID)))
}

func waitForGatewayRunLogEntries(t *testing.T, logURL, token, marker string) []gatewayRunLogEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for {
		contents, err := gatewayRequest(ctx, http.MethodGet, logURL, token, nil, http.StatusOK)
		if err == nil {
			var response struct {
				Items []gatewayRunLogEntry `json:"items"`
			}
			if decodeErr := json.Unmarshal(contents, &response); decodeErr != nil {
				t.Fatalf("decode Gateway Run logs: %v", decodeErr)
			}
			if hasGatewayRunLogEntry(response.Items, "stdout", marker) {
				return response.Items
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for Gateway Run log %q: %v", marker, err)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func followGatewayRunLogs(t *testing.T, logURL, token, marker string) []gatewayRunLogEntry {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, logURL, nil)
	if err != nil {
		t.Fatalf("create Gateway Run log follow request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("send Gateway Run log follow request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		contents, _ := io.ReadAll(response.Body)
		t.Fatalf("Gateway Run log follow status = %d, want %d: %s", response.StatusCode, http.StatusOK, contents)
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "application/x-ndjson" {
		t.Fatalf("Gateway Run log follow Content-Type = %q, want application/x-ndjson", contentType)
	}

	decoder := json.NewDecoder(response.Body)
	var entries []gatewayRunLogEntry
	for {
		var entry gatewayRunLogEntry
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("decode Gateway Run log follow record: %v", err)
		}
		entries = append(entries, entry)
		if entry.Stream == "stdout" && entry.Message == marker {
			return entries
		}
	}
}

func hasGatewayRunLogEntry(entries []gatewayRunLogEntry, stream, message string) bool {
	for _, entry := range entries {
		if entry.Stream == stream && entry.Message == message {
			return true
		}
	}
	return false
}

func TestFunctionGatewayInvokesAuthorizedFunction(t *testing.T) {
	runtimeName := fmt.Sprintf("function-gateway-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, pythonRuntimeImage(), 9092, 1)

	inline := `import os

def handler(event):
    with open(os.environ["KRUNTIME_OUTPUTS"], "w") as outputs:
        outputs.write("from-file=" + event["value"] + "\n")
    return {"status": "ok", "value": event["value"], "outputs": {"source": "python", "value": event["value"]}}
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-gateway-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create Function Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)
	if run.Status.Endpoint == nil || run.Status.Endpoint.Protocol != v1alpha1.RunEndpointProtocolHTTPS || len(run.Status.Endpoint.CABundle) == 0 {
		t.Fatalf("Function Run endpoint = %#v, want HTTPS gateway endpoint with a CA bundle", run.Status.Endpoint)
	}

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	response := waitForGatewayResponse(t, http.MethodPost, baseURL, token, []byte(`{"value":"gateway"}`), http.StatusOK)
	var invocation struct {
		InvocationID string            `json:"invocationId"`
		Output       []byte            `json:"output"`
		Outputs      map[string]string `json:"outputs"`
		ContentType  string            `json:"contentType"`
	}
	if err := json.Unmarshal(response, &invocation); err != nil {
		t.Fatalf("decode Function invocation response: %v", err)
	}
	if invocation.InvocationID == "" || invocation.ContentType != "application/json" || string(invocation.Output) != "{\"status\": \"ok\", \"value\": \"gateway\", \"outputs\": {\"source\": \"python\", \"value\": \"gateway\"}}\n" || !reflect.DeepEqual(invocation.Outputs, map[string]string{"from-file": "gateway", "source": "python", "value": "gateway"}) {
		t.Fatalf("Function invocation = %#v, want successful JSON response", invocation)
	}
	waitForFunctionInvocationLogs(t, run, invocation.InvocationID)
	secondResponse := waitForGatewayResponse(t, http.MethodPost, baseURL, token, []byte(`{"value":"again"}`), http.StatusOK)
	if err := json.Unmarshal(secondResponse, &invocation); err != nil {
		t.Fatalf("decode repeated Function invocation response: %v", err)
	}
	if invocation.InvocationID == "" || string(invocation.Output) != "{\"status\": \"ok\", \"value\": \"again\", \"outputs\": {\"source\": \"python\", \"value\": \"again\"}}\n" || !reflect.DeepEqual(invocation.Outputs, map[string]string{"from-file": "again", "source": "python", "value": "again"}) {
		t.Fatalf("repeated Function invocation = %#v, want successful JSON response", invocation)
	}

	_ = waitForGatewayResponse(t, http.MethodPost, baseURL, "", []byte(`{"value":"unauthenticated"}`), http.StatusUnauthorized)
	_ = waitForGatewayResponse(t, http.MethodPost, baseURL, sessionGatewayTokenWithoutRunAccess(t), []byte(`{"value":"unauthorized"}`), http.StatusForbidden)
}

func TestFunctionRunExpiresWhenIdle(t *testing.T) {
	runtimeName := fmt.Sprintf("function-idle-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, pythonRuntimeImage(), 9092, 1)
	idleTimeout := int32(1)
	inline := `def handler(event):
    return event
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-idle-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler", IdleTimeoutSeconds: &idleTimeout}},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create idle Function Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)
	waitForRunPhase(t, run, 15*time.Second, v1alpha1.RunTimeout)
	condition := findRunCondition(run, runstatus.ConditionCompleted)
	if condition == nil || condition.Reason != runretry.ReasonTimeout {
		t.Fatalf("Completed condition = %#v, want Timeout", condition)
	}
}

func TestFunctionRunCancellationReleasesRuntimeCapacity(t *testing.T) {
	runtimeName := fmt.Sprintf("function-cancel-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, pythonRuntimeImage(), 9092, 1)
	inline := `def handler(event):
    return event
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-cancel-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create Function Run: %v", err)
	}
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	requestRunCancel(t, run)
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunCancelled)
	assertCancelledRun(t, run)

	successor := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-cancel-successor-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(t.Context(), successor); err != nil {
		t.Fatalf("create successor Function Run: %v", err)
	}
	waitForRunPhase(t, successor, 30*time.Second, v1alpha1.RunReady)
	deleteRunAndWait(t, successor, 30*time.Second)
	deleteRunAndWait(t, run, 30*time.Second)
}

func TestDeletingFunctionRunReleasesRuntimeCapacity(t *testing.T) {
	runtimeName := fmt.Sprintf("function-delete-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, pythonRuntimeImage(), 9092, 1)
	inline := `def handler(event):
    return event
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-delete-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create Function Run: %v", err)
	}
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	deleteRunAndWait(t, run, 30*time.Second)

	successor := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-delete-successor-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(t.Context(), successor); err != nil {
		t.Fatalf("create successor Function Run: %v", err)
	}
	waitForRunPhase(t, successor, 30*time.Second, v1alpha1.RunReady)
	deleteRunAndWait(t, successor, 30*time.Second)
}

func TestFunctionRunExpiresWhenTotalTimeoutReached(t *testing.T) {
	runtimeName := fmt.Sprintf("function-total-timeout-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, pythonRuntimeImage(), 9092, 1)
	timeout := metav1.Duration{Duration: 5 * time.Second}
	inline := `def handler(event):
    return event
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-total-timeout-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Timeout: &timeout,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create Function Run: %v", err)
	}
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)
	waitForRunPhase(t, run, 15*time.Second, v1alpha1.RunTimeout)
	condition := findRunCondition(run, runstatus.ConditionCompleted)
	if condition == nil || condition.Reason != runretry.ReasonTimeout {
		t.Fatalf("Completed condition = %#v, want Timeout", condition)
	}

	successor := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-timeout-successor-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(t.Context(), successor); err != nil {
		t.Fatalf("create successor Function Run: %v", err)
	}
	waitForRunPhase(t, successor, 30*time.Second, v1alpha1.RunReady)
	deleteRunAndWait(t, successor, 30*time.Second)
	deleteRunAndWait(t, run, 30*time.Second)
}

func TestFunctionRunRecoversInvocationAfterRuntimedRestart(t *testing.T) {
	runtimeName := fmt.Sprintf("function-recovery-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, pythonRuntimeImage(), 9092, 1)
	inline := `def handler(event):
    return {"value": event["value"]}
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-recovery-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create recovering Function Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	_ = waitForGatewayResponse(t, http.MethodPost, baseURL, token, []byte(`{"value":"before"}`), http.StatusOK)
	previousRestartCount := runtimedRestartCount(t, run.Status.AssignedPod)
	killRuntimed(t, run.Status.AssignedPod)
	waitForRuntimedRestart(t, run.Status.AssignedPod, previousRestartCount)

	response := waitForGatewayResponse(t, http.MethodPost, baseURL, token, []byte(`{"value":"after"}`), http.StatusOK)
	var invocation struct {
		Output []byte `json:"output"`
	}
	if err := json.Unmarshal(response, &invocation); err != nil {
		t.Fatalf("decode recovered Function invocation response: %v", err)
	}
	if string(invocation.Output) != "{\"value\": \"after\"}\n" {
		t.Fatalf("recovered Function invocation = %#v, want post-restart output", invocation)
	}
}

func TestFunctionRuntimeProxyForwardsToNonOwnerPod(t *testing.T) {
	runtimeName := fmt.Sprintf("function-proxy-%d", time.Now().UnixNano())
	ensureRuntimeWithReplicasAndRunsCapacity(t, runtimeName, pythonRuntimeImage(), 9092, 2, 1)
	waitForRuntimeReadyReplicas(t, runtimeName, 2, 60*time.Second)
	inline := `def handler(event):
    return {"value": event["value"]}
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-proxy-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create proxied Function Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	var pods corev1.PodList
	if err := k8sClient.List(context.Background(), &pods, client.InNamespace(run.Namespace), client.MatchingLabels{"runtime": runtimeName}); err != nil {
		t.Fatalf("list Runtime Pods: %v", err)
	}
	var proxyPod *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name != run.Status.AssignedPod && podReady(pod) {
			proxyPod = pod
			break
		}
	}
	if proxyPod == nil {
		t.Fatalf("Runtime Pods = %#v, want a ready non-owner for Function proxy test", pods.Items)
	}
	ownerPod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: run.Namespace, Name: run.Status.AssignedPod}, ownerPod); err != nil {
		t.Fatalf("get owner Runtime Pod: %v", err)
	}
	if ownerPod.Status.PodIP == "" {
		t.Fatalf("owner Runtime Pod %s has no Pod IP", ownerPod.Name)
	}
	if _, stderr, err := execInPod(context.Background(), proxyPod.Name, "runtimed", []string{"/bin/bash", "-c", fmt.Sprintf("timeout 3 /bin/bash -c '>/dev/tcp/%s/9093'", ownerPod.Status.PodIP)}); err != nil {
		t.Fatalf("non-owner runtimed cannot reach owner %s: %v: %s", ownerPod.Status.PodIP, err, stderr)
	}

	ownerPort := availableLocalPort(t)
	ownerForward, err := forwardPodPort(t.Context(), ownerPod.Namespace, ownerPod.Name, ownerPort, 9093)
	if err != nil {
		t.Fatalf("port-forward owner runtimed: %v", err)
	}
	ownerConnection, err := grpc.NewClient(fmt.Sprintf("passthrough:///127.0.0.1:%d", ownerPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		_ = ownerForward.Close()
		t.Fatalf("dial owner runtimed: %v", err)
	}
	directCtx, directCancel := context.WithTimeout(t.Context(), 10*time.Second)
	directResponse, directErr := pb.NewFunctionRuntimeClient(ownerConnection).InvokeFunction(directCtx, &pb.InvokeFunctionRequest{
		Registration: &pb.FunctionRegistration{RunUid: string(run.UID)}, InvocationId: "owner-invoke", ContentType: "application/json", Input: []byte(`{"value":"owner"}`),
	})
	directCancel()
	_ = ownerConnection.Close()
	_ = ownerForward.Close()
	if directErr != nil {
		t.Fatalf("invoke Function through owner runtimed: %v", directErr)
	}
	if string(directResponse.GetOutput()) != "{\"value\": \"owner\"}\n" {
		t.Fatalf("owner Function invocation = %#v, want direct response", directResponse)
	}

	localPort := availableLocalPort(t)
	forward, err := forwardPodPort(t.Context(), proxyPod.Namespace, proxyPod.Name, localPort, 9093)
	if err != nil {
		t.Fatalf("port-forward non-owner runtimed: %v", err)
	}
	t.Cleanup(func() { _ = forward.Close() })
	connection, err := grpc.NewClient(fmt.Sprintf("passthrough:///127.0.0.1:%d", localPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial non-owner runtimed: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	invokeCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	response, err := pb.NewFunctionRuntimeClient(connection).InvokeFunction(invokeCtx, &pb.InvokeFunctionRequest{
		Registration: &pb.FunctionRegistration{RunUid: string(run.UID)},
		InvocationId: "proxied-invoke",
		ContentType:  "application/json",
		Input:        []byte(`{"value":"proxied"}`),
	})
	if err != nil {
		t.Logf("non-owner runtimed logs:\n%s", runtimedPodLogs(t, proxyPod.Namespace, proxyPod.Name))
		t.Logf("owner runtimed logs:\n%s", runtimedPodLogs(t, ownerPod.Namespace, ownerPod.Name))
		t.Fatalf("invoke Function through non-owner runtimed: %v", err)
	}
	if response.GetInvocationId() != "proxied-invoke" || string(response.GetOutput()) != "{\"value\": \"proxied\"}\n" {
		t.Fatalf("proxied Function invocation = %#v, want non-owner forwarding response", response)
	}
}

func TestFunctionRunFailsWhenAssignedRuntimePodIsLost(t *testing.T) {
	runtimeName := fmt.Sprintf("function-pod-loss-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, pythonRuntimeImage(), 9092, 1)
	inline := `def handler(event):
    return event
`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-function-pod-loss-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Source:  &v1alpha1.CodeSource{Inline: &inline, InlinePath: "app.py"},
			Mode:    v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "app.handler"}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create Function Run: %v", err)
	}
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	podName := run.Status.AssignedPod
	if podName == "" {
		t.Fatal("Function Run reached Ready without an assigned Runtime Pod")
	}
	if err := k8sClient.Delete(t.Context(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: run.Namespace},
	}); err != nil {
		t.Fatalf("delete assigned Runtime Pod %s: %v", podName, err)
	}

	// A Function registration belongs to this exact Pod. Once that Pod is
	// gone, the stale assignment must become terminal instead of being served
	// by a replacement Pod.
	waitForRunPhase(t, run, 60*time.Second, v1alpha1.RunFailed)
	condition := findRunCondition(run, runstatus.ConditionCompleted)
	if condition == nil || (condition.Reason != runretry.ReasonPodGone && condition.Reason != runretry.ReasonPodTerminating) {
		t.Fatalf("Completed condition = %#v, want PodGone or PodTerminating", condition)
	}
	deleteRunAndWait(t, run, 60*time.Second)
}

func TestSessionRuntimeProxyForwardsToNonOwnerPod(t *testing.T) {
	runtimeName := fmt.Sprintf("session-proxy-%d", time.Now().UnixNano())
	ensureRuntimeWithReplicasAndRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 2, 1)
	waitForRuntimeReadyReplicas(t, runtimeName, 2, 60*time.Second)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-proxy-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create proxied Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	var pods corev1.PodList
	if err := k8sClient.List(context.Background(), &pods, client.InNamespace(run.Namespace), client.MatchingLabels{"runtime": runtimeName}); err != nil {
		t.Fatalf("list Runtime Pods: %v", err)
	}
	var proxyPod *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name != run.Status.AssignedPod && podReady(pod) {
			proxyPod = pod
			break
		}
	}
	if proxyPod == nil {
		t.Fatalf("Runtime Pods = %#v, want a ready non-owner for Session proxy test", pods.Items)
	}
	ownerPod := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: run.Namespace, Name: run.Status.AssignedPod}, ownerPod); err != nil {
		t.Fatalf("get owner Runtime Pod: %v", err)
	}
	if ownerPod.Status.PodIP == "" {
		t.Fatalf("owner Runtime Pod %s has no Pod IP", ownerPod.Name)
	}
	if _, stderr, err := execInPod(context.Background(), proxyPod.Name, "runtimed", []string{"/bin/bash", "-c", fmt.Sprintf("timeout 3 /bin/bash -c '>/dev/tcp/%s/9093'", ownerPod.Status.PodIP)}); err != nil {
		t.Fatalf("non-owner runtimed cannot reach owner %s: %v: %s", ownerPod.Status.PodIP, err, stderr)
	}

	localPort := availableLocalPort(t)
	forward, err := forwardPodPort(t.Context(), proxyPod.Namespace, proxyPod.Name, localPort, 9093)
	if err != nil {
		t.Fatalf("port-forward non-owner runtimed: %v", err)
	}
	t.Cleanup(func() { _ = forward.Close() })
	connection, err := grpc.NewClient(fmt.Sprintf("passthrough:///127.0.0.1:%d", localPort), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial non-owner runtimed: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	statusCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	response, err := pb.NewSessionRuntimeClient(connection).GetSessionStatus(statusCtx, &pb.GetSessionStatusRequest{
		Identity: &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: string(run.Status.AssignedPodUID)},
	})
	if err != nil {
		t.Logf("non-owner runtimed logs:\n%s", runtimedPodLogs(t, proxyPod.Namespace, proxyPod.Name))
		t.Logf("owner runtimed logs:\n%s", runtimedPodLogs(t, ownerPod.Namespace, ownerPod.Name))
		t.Fatalf("get Session status through non-owner runtimed: %v", err)
	}
	if response.GetState() != pb.SessionState_SESSION_STATE_READY {
		t.Fatalf("proxied Session state = %s, want %s", response.GetState(), pb.SessionState_SESSION_STATE_READY)
	}
}

func runtimedPodLogs(t *testing.T, namespace, podName string) string {
	t.Helper()
	stream, err := coreClientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{Container: "runtimed"}).Stream(context.Background())
	if err != nil {
		return fmt.Sprintf("get logs: %v", err)
	}
	defer stream.Close()
	contents, err := io.ReadAll(stream)
	if err != nil {
		return fmt.Sprintf("read logs: %v", err)
	}
	return string(contents)
}

func TestSessionGatewayServesTLS(t *testing.T) {
	runtimeName := fmt.Sprintf("session-gateway-tls-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-gateway-tls-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create TLS Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)
	if run.Status.Endpoint == nil || run.Status.Endpoint.Protocol != v1alpha1.RunEndpointProtocolHTTPS || len(run.Status.Endpoint.CABundle) == 0 {
		t.Fatalf("Session Run endpoint = %#v, want HTTPS gateway endpoint with a CA bundle", run.Status.Endpoint)
	}

	gatewayPod := waitForGatewayPod(t)
	baseURL := gatewayTLSEndpointURL(t, gatewayPod, run.Status.Endpoint.URL)
	response := waitForGatewayResponseWithClient(t, gatewayTLSHTTPClient(t, gatewayPod.Namespace, run.Status.Endpoint.CABundle), http.MethodGet, baseURL, sessionGatewayToken(t, run), nil, http.StatusOK)
	var sessionStatus struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(response, &sessionStatus); err != nil {
		t.Fatalf("decode TLS Session status: %v", err)
	}
	if sessionStatus.State != "SESSION_STATE_READY" {
		t.Fatalf("TLS Session state = %q, want SESSION_STATE_READY", sessionStatus.State)
	}
}

func TestSessionGatewayEnforcesTransferBounds(t *testing.T) {
	if os.Getenv(gatewayBoundsE2EEnabledEnv) != "true" {
		t.Skipf("set %s=true to run the gateway transfer-bounds E2E", gatewayBoundsE2EEnabledEnv)
	}
	runtimeName := fmt.Sprintf("session-gateway-bounds-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-gateway-bounds-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create transfer-bounds Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	portForwardClient := &http.Client{Transport: &http.Transport{Proxy: nil}}
	// Establish the port-forward with a response that remains below the focused
	// response limit before exercising rejection paths.
	_ = waitForGatewayResponseWithClient(t, portForwardClient, http.MethodGet, baseURL, token, nil, http.StatusOK)
	oversizedRequest := []byte(`{"command":{"argv":["true"],"stdin":"` + strings.Repeat("eA==", 160) + `"}}`)
	requestResponse := waitForGatewayResponseWithClient(t, portForwardClient, http.MethodPost, baseURL+"/operations:execute", token, oversizedRequest, http.StatusRequestEntityTooLarge)
	if !strings.Contains(string(requestResponse), "gateway request body exceeds configured limit") {
		t.Fatalf("oversized request response = %s", requestResponse)
	}

	responseResponse := waitForGatewayResponseWithClient(t, portForwardClient, http.MethodPost, baseURL+"/operations:execute", token,
		[]byte(`{"command":{"argv":["sh","-c","head -c 1024 /dev/zero"]}}`), http.StatusRequestEntityTooLarge)
	if got, want := string(responseResponse), "{\"error\":\"gateway response exceeds configured limit\"}\n"; got != want {
		t.Fatalf("oversized response body = %q, want %q", got, want)
	}

	headerRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, baseURL, nil)
	if err != nil {
		t.Fatalf("create oversized-header request: %v", err)
	}
	headerRequest.Header.Set("X-Kruntimes-E2E-Bounds", strings.Repeat("x", 512<<10))
	headerResponse, err := portForwardClient.Do(headerRequest)
	if err != nil {
		t.Fatalf("send oversized-header request: %v", err)
	}
	defer headerResponse.Body.Close()
	if headerResponse.StatusCode != http.StatusRequestHeaderFieldsTooLarge {
		contents, _ := io.ReadAll(headerResponse.Body)
		t.Fatalf("oversized header status = %d, want %d: %s", headerResponse.StatusCode, http.StatusRequestHeaderFieldsTooLarge, contents)
	}
}

func TestSessionGatewayServesCertManagerTLS(t *testing.T) {
	if os.Getenv(certManagerE2EEnabledEnv) != "true" {
		t.Skipf("set %s=true to run the cert-manager E2E", certManagerE2EEnabledEnv)
	}

	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: certManagerGatewayCertificate}, certificate); err != nil {
		t.Fatalf("get cert-manager gateway Certificate: %v", err)
	}
	if !certificateReady(certificate) {
		t.Fatalf("cert-manager gateway Certificate status = %#v, want Ready=True", certificate.Object["status"])
	}

	secret := &corev1.Secret{}
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: certManagerGatewayTLSSecret}, secret); err != nil {
		t.Fatalf("get cert-manager gateway TLS Secret: %v", err)
	}
	if len(secret.Data["ca.crt"]) == 0 || len(secret.Data["tls.crt"]) == 0 || len(secret.Data["tls.key"]) == 0 {
		t.Fatalf("cert-manager gateway TLS Secret keys = %v, want ca.crt, tls.crt, and tls.key", secret.Data)
	}
	certificatePEM, _ := pem.Decode(secret.Data["tls.crt"])
	if certificatePEM == nil {
		t.Fatal("decode cert-manager gateway TLS certificate PEM")
	}
	leaf, err := x509.ParseCertificate(certificatePEM.Bytes)
	if err != nil {
		t.Fatalf("parse cert-manager gateway TLS certificate: %v", err)
	}
	if !slices.Contains(leaf.DNSNames, "kruntimes-gateway.default.svc") {
		t.Fatalf("cert-manager gateway TLS certificate DNS names = %v, want kruntimes-gateway.default.svc", leaf.DNSNames)
	}

	runtimeName := fmt.Sprintf("session-gateway-cert-manager-tls-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-gateway-cert-manager-tls-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create cert-manager TLS Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)
	if run.Status.Endpoint == nil || run.Status.Endpoint.Protocol != v1alpha1.RunEndpointProtocolHTTPS {
		t.Fatalf("Session Run endpoint = %#v, want HTTPS gateway endpoint", run.Status.Endpoint)
	}
	if !bytes.Equal(run.Status.Endpoint.CABundle, secret.Data["ca.crt"]) {
		t.Fatal("Session Run endpoint CA bundle does not match the cert-manager gateway TLS Secret")
	}

	gatewayPod := waitForGatewayPod(t)
	baseURL := gatewayTLSEndpointURL(t, gatewayPod, run.Status.Endpoint.URL)
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse cert-manager gateway endpoint %q: %v", baseURL, err)
	}
	healthURL := fmt.Sprintf("%s://%s/healthz", parsedURL.Scheme, parsedURL.Host)
	_ = waitForGatewayResponseWithClient(t, gatewayTLSHTTPClient(t, gatewayPod.Namespace, run.Status.Endpoint.CABundle), http.MethodGet, healthURL, "", nil, http.StatusOK)
}

func certificateReady(certificate *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(certificate.Object, "status", "conditions")
	if err != nil || !found {
		return false
	}
	for _, condition := range conditions {
		condition, ok := condition.(map[string]any)
		if ok && condition["type"] == "Ready" && condition["status"] == "True" {
			return true
		}
	}
	return false
}

func TestSessionGatewaySerializesMutations(t *testing.T) {
	runtimeName := fmt.Sprintf("session-fifo-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-fifo-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create FIFO Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	gatewayPod := waitForGatewayPod(t)
	baseURL := gatewayEndpointURL(t, gatewayPod, run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	_ = waitForGatewayResponse(t, http.MethodGet, baseURL, token, nil, http.StatusOK)
	firstResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := gatewayRequest(ctx, http.MethodPost, baseURL+"/operations:execute", token,
			[]byte(`{"command":{"argv":["sh","-c","printf started > started; sleep 1; printf first > result.txt"]}}`), http.StatusOK)
		firstResult <- err
	}()

	// Reading is not a mutation, so this confirms that the first command is
	// active before the second mutation is submitted to the owner queue.
	_ = waitForGatewayResponse(t, http.MethodGet, baseURL+"/files/started", token, nil, http.StatusOK)
	_ = waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token,
		[]byte(`{"writeFile":{"path":"result.txt","contents":"c2Vjb25k"}}`), http.StatusOK)
	if err := <-firstResult; err != nil {
		t.Fatalf("execute first FIFO mutation: %v", err)
	}

	response := waitForGatewayResponse(t, http.MethodGet, baseURL+"/files/result.txt", token, nil, http.StatusOK)
	var file struct {
		Contents []byte `json:"contents"`
	}
	if err := json.Unmarshal(response, &file); err != nil {
		t.Fatalf("decode FIFO result file: %v", err)
	}
	if string(file.Contents) != "second" {
		t.Fatalf("serialized mutation result = %q, want second", file.Contents)
	}
}

func TestSessionRunCancellationTerminatesActiveGatewayCommand(t *testing.T) {
	runtimeName := fmt.Sprintf("session-cancel-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-cancel-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create cancellation Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	gatewayPod := waitForGatewayPod(t)
	baseURL := gatewayEndpointURL(t, gatewayPod, run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	_ = waitForGatewayResponse(t, http.MethodGet, baseURL, token, nil, http.StatusOK)
	commandResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := gatewayRequest(ctx, http.MethodPost, baseURL+"/operations:execute", token,
			[]byte(`{"command":{"argv":["sh","-c","printf started > started; sleep 20; printf completed > completed"]}}`), http.StatusOK)
		commandResult <- err
	}()

	_ = waitForGatewayResponse(t, http.MethodGet, baseURL+"/files/started", token, nil, http.StatusOK)
	requestRunCancel(t, run)
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunCancelled)
	assertCancelledRun(t, run)
	if err := <-commandResult; err == nil {
		t.Fatal("active Session command succeeded after its Run was cancelled")
	}
	_ = waitForGatewayResponse(t, http.MethodGet, baseURL, token, nil, http.StatusConflict)
}

func TestSessionRunDrainCompletesAcceptedGatewayCommand(t *testing.T) {
	runtimeName := fmt.Sprintf("session-drain-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-drain-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create draining Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	gatewayPod := waitForGatewayPod(t)
	baseURL := gatewayEndpointURL(t, gatewayPod, run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	_ = waitForGatewayResponse(t, http.MethodGet, baseURL, token, nil, http.StatusOK)
	commandResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := gatewayRequest(ctx, http.MethodPost, baseURL+"/operations:execute", token,
			[]byte(`{"command":{"argv":["sh","-c","printf started > started; sleep 15; printf completed > completed"]}}`), http.StatusOK)
		commandResult <- err
	}()

	// A successful read proves runtimed accepted the command before Drain
	// fenced later operations.
	_ = waitForGatewayResponse(t, http.MethodGet, baseURL+"/files/started", token, nil, http.StatusOK)
	requestRunDrain(t, run)
	waitForRunPhase(t, run, 10*time.Second, v1alpha1.RunFinalizing)
	_ = waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token,
		[]byte(`{"writeFile":{"path":"rejected.txt","contents":"cmVqZWN0ZWQ="}}`), http.StatusConflict)
	if err := <-commandResult; err != nil {
		t.Fatalf("accepted Session command did not complete during Drain: %v", err)
	}
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunSucceeded)
}

func TestSandboxSDKUsesGatewayServicePortForward(t *testing.T) {
	runtimeName := fmt.Sprintf("sdk-session-gateway-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	serviceAccount, token := newSessionGatewayServiceAccount(t)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccount.Name, Namespace: testNamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"runs"}, Verbs: []string{"create", "get", "update"}},
			{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
		},
	}
	if err := k8sClient.Create(t.Context(), role); err != nil {
		t.Fatalf("create SDK test Role: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), role) })
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: testNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount.Name, Namespace: testNamespace}},
	}
	if err := k8sClient.Create(t.Context(), binding); err != nil {
		t.Fatalf("create SDK test RoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), binding) })

	sdkConfig := rest.CopyConfig(restConfig)
	sdkConfig.BearerToken = token
	sdkConfig.BearerTokenFile = ""
	forward, err := sandbox.StartGatewayPortForward(t.Context(), sdkConfig, testNamespace, "kruntimes-gateway", 80)
	if err != nil {
		t.Fatalf("start SDK Runtime gateway port-forward: %v", err)
	}
	t.Cleanup(forward.Close)
	sdk, err := sandbox.NewFromRESTConfig(sdkConfig, sandbox.Config{HTTPClient: forward})
	if err != nil {
		t.Fatalf("create Sandbox SDK client: %v", err)
	}
	session, err := sdk.Create(t.Context(), sandbox.CreateOptions{GenerateName: "e2e-sdk-session-", Namespace: testNamespace, Runtime: runtimeName})
	if err != nil {
		t.Fatalf("create SDK Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), session.Run()) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := session.Wait(ctx); err != nil {
		t.Fatalf("wait for SDK Session Run: %v", err)
	}
	result, err := session.Execute(ctx, sandbox.Command{Argv: []string{"sh", "-c", "printf sdk-port-forward"}})
	if err != nil {
		t.Fatalf("execute SDK Session command: %v", err)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "sdk-port-forward" {
		t.Fatalf("SDK Session command result = %#v, want successful sdk-port-forward output", result)
	}
	for _, name := range []string{"alpha.txt", "beta.txt", "gamma.txt"} {
		if err := session.WriteFile(ctx, "pages/"+name, []byte(name), true); err != nil {
			t.Fatalf("write SDK Session page file %q: %v", name, err)
		}
	}
	page, err := session.ListFiles(ctx, sandbox.ListFilesOptions{Directory: "pages", Limit: 2})
	if err != nil {
		t.Fatalf("list first SDK Session file page: %v", err)
	}
	if got, want := sessionFilePaths(page.Entries), []string{"alpha.txt", "beta.txt"}; !slices.Equal(got, want) || page.NextPageToken == "" {
		t.Fatalf("first SDK Session file page = %#v, want %#v and next token", page, want)
	}
	page, err = session.ListFiles(ctx, sandbox.ListFilesOptions{Directory: "pages", Limit: 2, PageToken: page.NextPageToken})
	if err != nil {
		t.Fatalf("list second SDK Session file page: %v", err)
	}
	if got, want := sessionFilePaths(page.Entries), []string{"gamma.txt"}; !slices.Equal(got, want) || page.NextPageToken != "" {
		t.Fatalf("second SDK Session file page = %#v, want %#v and no next token", page, want)
	}
	if err := session.Close(ctx); err != nil {
		t.Fatalf("close SDK Session Run: %v", err)
	}
	if session.Run().Status.Phase != v1alpha1.RunSucceeded {
		t.Fatalf("SDK Close phase = %s, want Succeeded", session.Run().Status.Phase)
	}

	cancelled, err := sdk.Create(t.Context(), sandbox.CreateOptions{GenerateName: "e2e-sdk-cancel-", Namespace: testNamespace, Runtime: runtimeName})
	if err != nil {
		t.Fatalf("create SDK cancellation Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cancelled.Run()) })
	if err := cancelled.Wait(ctx); err != nil {
		t.Fatalf("wait for SDK cancellation Session Run: %v", err)
	}
	if err := cancelled.Cancel(ctx); err != nil {
		t.Fatalf("cancel SDK Session Run: %v", err)
	}
	if cancelled.Run().Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("SDK Cancel phase = %s, want Cancelled", cancelled.Run().Status.Phase)
	}
}

func sessionFilePaths(entries []sandbox.FileInfo) []string {
	paths := make([]string, len(entries))
	for i := range entries {
		paths[i] = entries[i].Path
	}
	return paths
}

func TestKubernetesDiagnosisRuntimeCanReadNamespace(t *testing.T) {
	name := fmt.Sprintf("diagnosis-runtime-%d", time.Now().UnixNano())
	serviceAccountName := "diagnosis-reader-" + fmt.Sprintf("%d", time.Now().UnixNano())
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName, Namespace: testNamespace}}
	if err := k8sClient.Create(t.Context(), serviceAccount); err != nil {
		t.Fatalf("create diagnosis Runtime ServiceAccount: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), serviceAccount) })
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName, Namespace: testNamespace},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}},
	}
	if err := k8sClient.Create(t.Context(), role); err != nil {
		t.Fatalf("create diagnosis Runtime Role: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), role) })
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: testNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccountName, Namespace: testNamespace}},
	}
	if err := k8sClient.Create(t.Context(), binding); err != nil {
		t.Fatalf("create diagnosis Runtime RoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), binding) })

	runtimeObject := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate(diagnosisRuntimeImage(), 9092),
			Port:     9092,
			Replicas: 1,
			Capacity: &v1alpha1.RuntimeCapacity{Resources: corev1.ResourceList{
				corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(1, resource.DecimalSI),
			}},
		},
	}
	runtimeObject.Spec.Template.Spec.ServiceAccountName = serviceAccountName
	if err := k8sClient.Create(t.Context(), runtimeObject); err != nil {
		t.Fatalf("create diagnosis Runtime: %v", err)
	}
	cleanupRuntime(t, name)
	waitForRuntimePod(t, name, diagnosisRuntimeImage(), runtimedImage(), 1, "diagnosis Runtime Pod")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-diagnosis-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: name, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create diagnosis Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 45*time.Second, v1alpha1.RunReady)

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	response := waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token,
		[]byte(`{"command":{"argv":["kubectl","get","pods","--namespace","default","--output","json"]}}`), http.StatusOK)
	var operation struct {
		Command struct {
			ExitCode int32  `json:"exitCode"`
			Stdout   []byte `json:"stdout"`
		} `json:"command"`
	}
	if err := json.Unmarshal(response, &operation); err != nil {
		t.Fatalf("decode diagnosis command response: %v", err)
	}
	if operation.Command.ExitCode != 0 {
		t.Fatalf("diagnosis kubectl command failed: %s", operation.Command.Stdout)
	}
	var pods corev1.PodList
	if err := json.Unmarshal(operation.Command.Stdout, &pods); err != nil || len(pods.Items) == 0 {
		t.Fatalf("diagnosis kubectl output = %s, unmarshal error = %v", operation.Command.Stdout, err)
	}
}

func TestSessionRunExpiresWhenIdle(t *testing.T) {
	runtimeName := fmt.Sprintf("session-idle-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)
	idleTimeout := int32(1)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-idle-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{IdleTimeoutSeconds: &idleTimeout}},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create idle Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 10*time.Second, v1alpha1.RunTimeout)
}

func TestSessionRunExpiresWhenTotalTimeoutReached(t *testing.T) {
	runtimeName := fmt.Sprintf("session-total-timeout-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)
	timeout := metav1.Duration{Duration: 5 * time.Second}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-total-timeout-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Timeout: &timeout,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)
	waitForRunPhase(t, run, 15*time.Second, v1alpha1.RunTimeout)
	condition := findRunCondition(run, runstatus.ConditionCompleted)
	if condition == nil || condition.Reason != runretry.ReasonTimeout {
		t.Fatalf("Completed condition = %#v, want Timeout", condition)
	}
}

func TestSessionRunFailsWhenAssignedRuntimePodIsLost(t *testing.T) {
	runtimeName := fmt.Sprintf("session-pod-loss-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-pod-loss-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	podName := run.Status.AssignedPod
	if podName == "" {
		t.Fatal("Session Run reached Ready without an assigned Runtime Pod")
	}
	if err := k8sClient.Delete(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: run.Namespace},
	}); err != nil {
		t.Fatalf("delete assigned Runtime Pod %s: %v", podName, err)
	}

	// A Ready session owns an ephemeral workspace on this exact Pod. It must
	// become terminal rather than be re-registered on a replacement Pod.
	waitForRunPhase(t, run, 60*time.Second, v1alpha1.RunFailed)
	condition := findRunCondition(run, runstatus.ConditionCompleted)
	if condition == nil || (condition.Reason != runretry.ReasonPodGone && condition.Reason != runretry.ReasonPodTerminating) {
		t.Fatalf("Completed condition = %#v, want PodGone or PodTerminating", condition)
	}
}

func containsSessionFile(entries []struct {
	Path string `json:"path"`
}, path string) bool {
	for _, entry := range entries {
		if entry.Path == path {
			return true
		}
	}
	return false
}

func waitForSessionCommandLogs(t *testing.T, run *v1alpha1.Run, message string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		stream, err := coreClientset.CoreV1().Pods(run.Namespace).GetLogs(run.Status.AssignedPod, &corev1.PodLogOptions{Container: "runtimed"}).Stream(ctx)
		if err == nil {
			contents, readErr := io.ReadAll(stream)
			_ = stream.Close()
			if readErr == nil && containsSessionCommandLogs(string(contents), string(run.UID), message) {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for structured Session logs for Run %s", run.Name)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func containsSessionCommandLogs(contents, runUID, message string) bool {
	stdoutFound := false
	auditFound := false
	for _, raw := range strings.Split(strings.TrimSuffix(contents, "\n"), "\n") {
		var line struct {
			RunUID    string `json:"run_uid"`
			Stream    string `json:"stream"`
			Message   string `json:"message"`
			Operation string `json:"operation"`
			Outcome   string `json:"outcome"`
		}
		if json.Unmarshal([]byte(raw), &line) != nil || line.RunUID != runUID || line.Operation != "command" || line.Outcome != "succeeded" {
			continue
		}
		if line.Stream == "stdout" && line.Message == message {
			stdoutFound = true
		}
		if line.Stream == "audit" && line.Message == "session operation completed" {
			auditFound = true
		}
	}
	return stdoutFound && auditFound
}

func waitForFunctionInvocationLogs(t *testing.T, run *v1alpha1.Run, invocationID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		stream, err := coreClientset.CoreV1().Pods(run.Namespace).GetLogs(run.Status.AssignedPod, &corev1.PodLogOptions{Container: "runtimed"}).Stream(ctx)
		if err == nil {
			contents, readErr := io.ReadAll(stream)
			_ = stream.Close()
			if readErr == nil && containsFunctionInvocationLogs(string(contents), string(run.UID), invocationID) {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for structured Function invocation logs for Run %s", run.Name)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func containsFunctionInvocationLogs(contents, runUID, invocationID string) bool {
	for _, raw := range strings.Split(strings.TrimSuffix(contents, "\n"), "\n") {
		var line struct {
			RunUID       string `json:"run_uid"`
			InvocationID string `json:"invocation_id"`
			Stream       string `json:"stream"`
			Message      string `json:"message"`
			Operation    string `json:"operation"`
			Outcome      string `json:"outcome"`
		}
		if json.Unmarshal([]byte(raw), &line) == nil && line.RunUID == runUID && line.InvocationID == invocationID && line.Stream == "audit" && line.Message == "function invocation completed" && line.Operation == "function_invoke" && line.Outcome == "succeeded" {
			return true
		}
	}
	return false
}

func sessionGatewayToken(t *testing.T, run *v1alpha1.Run) string {
	t.Helper()
	ctx := context.Background()
	serviceAccount, token := newSessionGatewayServiceAccount(t)

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccount.Name, Namespace: testNamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"runs"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
			{APIGroups: []string{"logs.kruntimes.io"}, Resources: []string{"runs/log"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
		},
	}
	if err := k8sClient.Create(ctx, role); err != nil {
		t.Fatalf("create gateway test Role: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, role) })
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: testNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount.Name, Namespace: testNamespace}},
	}
	if err := k8sClient.Create(ctx, binding); err != nil {
		t.Fatalf("create gateway test RoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, binding) })

	return token
}

func sessionGatewayTokenWithoutRunAccess(t *testing.T) string {
	t.Helper()
	_, token := newSessionGatewayServiceAccount(t)
	return token
}

func newSessionGatewayServiceAccount(t *testing.T) (*corev1.ServiceAccount, string) {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "session-gateway-user-" + suffix, Namespace: testNamespace}}
	if err := k8sClient.Create(ctx, serviceAccount); err != nil {
		t.Fatalf("create gateway test ServiceAccount: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, serviceAccount) })

	response, err := coreClientset.CoreV1().ServiceAccounts(testNamespace).CreateToken(ctx, serviceAccount.Name, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create gateway test ServiceAccount token: %v", err)
	}
	if response.Status.Token == "" {
		t.Fatal("gateway test ServiceAccount token is empty")
	}
	return serviceAccount, response.Status.Token
}

func waitForGatewayPod(t *testing.T) *corev1.Pod {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		pods, err := coreClientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=runtime-gateway"})
		if err == nil {
			for i := range pods.Items {
				if podReady(&pods.Items[i]) {
					return &pods.Items[i]
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for Runtime gateway Pod: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	if pod == nil || pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func gatewayEndpointURL(t *testing.T, pod *corev1.Pod, endpoint string) string {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Path == "" {
		t.Fatalf("parse gateway endpoint %q: %v", endpoint, err)
	}
	localPort := availableLocalPort(t)
	closer, err := forwardPodPort(t.Context(), pod.Namespace, pod.Name, localPort, 8084)
	if err != nil {
		t.Fatalf("port-forward Runtime gateway: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	return fmt.Sprintf("http://127.0.0.1:%d%s", localPort, parsed.EscapedPath())
}

func gatewayTLSEndpointURL(t *testing.T, pod *corev1.Pod, endpoint string) string {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Path == "" {
		t.Fatalf("parse HTTPS gateway endpoint %q: %v", endpoint, err)
	}
	localPort := availableLocalPort(t)
	closer, err := forwardPodPort(t.Context(), pod.Namespace, pod.Name, localPort, 8444)
	if err != nil {
		t.Fatalf("port-forward HTTPS Runtime gateway: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	return fmt.Sprintf("https://127.0.0.1:%d%s", localPort, parsed.EscapedPath())
}

func gatewayTLSHTTPClient(t *testing.T, namespace string, caBundle []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundle) {
		t.Fatal("Runtime gateway endpoint has no parseable CA bundle")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pool,
		ServerName: fmt.Sprintf("kruntimes-gateway.%s.svc", namespace),
	}}}
}

func availableLocalPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve local port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func waitForGatewayResponse(t *testing.T, method, requestURL, token string, body []byte, expectedStatus int) []byte {
	t.Helper()
	return waitForGatewayResponseWithClient(t, http.DefaultClient, method, requestURL, token, body, expectedStatus)
}

func waitForGatewayResponseWithClient(t *testing.T, httpClient *http.Client, method, requestURL, token string, body []byte, expectedStatus int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lastResult := "no response"
	for {
		// Gateway authorization performs both a TokenReview and a
		// SubjectAccessReview, each of which may consume its own API-server
		// round trip. Keep the individual request deadline above that work while
		// retaining the bounded overall retry budget.
		requestCtx, requestCancel := context.WithTimeout(ctx, 5*time.Second)
		request, err := http.NewRequestWithContext(requestCtx, method, requestURL, bytes.NewReader(body))
		if err != nil {
			requestCancel()
			t.Fatalf("create gateway request: %v", err)
		}
		if len(body) > 0 {
			request.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := httpClient.Do(request)
		if err == nil {
			contents, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			requestCancel()
			if readErr != nil {
				t.Fatalf("read gateway response: %v", readErr)
			}
			if response.StatusCode == expectedStatus {
				return contents
			}
			lastResult = fmt.Sprintf("status %d: %s", response.StatusCode, contents)
			if response.StatusCode != http.StatusNotFound && response.StatusCode != http.StatusConflict && response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("gateway response status = %d, want %d: %s", response.StatusCode, expectedStatus, contents)
			}
		} else {
			requestCancel()
			lastResult = err.Error()
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for gateway response status %d: %s", expectedStatus, lastResult)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func gatewayRequest(ctx context.Context, method, requestURL, token string, body []byte, expectedStatus int) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create gateway request: %w", err)
	}
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("send gateway request: %w", err)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read gateway response: %w", err)
	}
	if response.StatusCode != expectedStatus {
		return nil, fmt.Errorf("gateway response status = %d, want %d: %s", response.StatusCode, expectedStatus, contents)
	}
	return contents, nil
}

func forwardPodPort(ctx context.Context, namespace, podName string, localPort, remotePort int) (io.Closer, error) {
	transport, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build port-forward transport: %w", err)
	}
	requestURL := coreClientset.CoreV1().RESTClient().Post().Namespace(namespace).Resource("pods").Name(podName).SubResource("portforward").URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, requestURL)
	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("%d:%d", localPort, remotePort)}, stopCh, readyCh, io.Discard, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("create port-forward: %w", err)
	}
	running := &e2ePortForward{stopCh: stopCh, done: make(chan error, 1)}
	go func() { running.done <- forwarder.ForwardPorts() }()
	select {
	case <-readyCh:
		return running, nil
	case err := <-running.done:
		return nil, fmt.Errorf("port-forward to Pod %s exited: %w", podName, err)
	case <-ctx.Done():
		_ = running.Close()
		return nil, ctx.Err()
	}
}

type e2ePortForward struct {
	stopCh chan struct{}
	done   chan error
	once   sync.Once
}

func (f *e2ePortForward) Close() error {
	f.once.Do(func() { close(f.stopCh) })
	select {
	case err := <-f.done:
		return err
	case <-time.After(2 * time.Second):
		return fmt.Errorf("timed out stopping port-forward")
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
			Mode:    taskMode("echo " + stdout),
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

func TestWorkflowTriggerMaterializesAndExecutesTemplate(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-template-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Inputs: map[string]v1alpha1.WorkflowInputSpec{
				"message": {Required: true},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "render",
						Run:  "test \"$MESSAGE\" = \"${{ inputs.message }}\"",
						Env:  map[string]string{"MESSAGE": "${{ inputs.message }}"},
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRunName := "e2e-trigger-" + nameSuffix
	cmd := krt.NewRootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"wf", "trigger", workflow.Name, "--name", workflowRunName, "--set", "message=rendered-by-e2e", "--namespace", testNamespace})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("trigger workflow: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), workflowRunName) {
		t.Fatalf("trigger output = %q, want workflowrun name %q", stdout.String(), workflowRunName)
	}

	workflowRun := &v1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: workflowRunName, Namespace: testNamespace}}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
		t.Fatalf("get materialized workflowrun: %v", err)
	}
	step := workflowRun.Spec.Jobs["build"].Steps[0]
	if step.Run != "test \"$MESSAGE\" = \"rendered-by-e2e\"" || step.Env["MESSAGE"] != "rendered-by-e2e" {
		t.Fatalf("materialized step = %#v, want rendered inputs", step)
	}

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowSucceeded)
	if workflowRun.Status.Jobs["build"].Phase != v1alpha1.JobSucceeded {
		t.Fatalf("build job status = %#v, want Succeeded", workflowRun.Status.Jobs["build"])
	}
}

func TestWorkflowRunExecutesActionAndProjectsOutputs(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.ActionSpec{
			Inputs: map[string]v1alpha1.ActionInputSpec{
				"message": {Required: true},
			},
			Outputs: map[string]v1alpha1.ActionOutputSpec{
				"endpoint": {Value: "${{ steps.emit.outputs.endpoint }}"},
			},
			Steps: []v1alpha1.StepSpec{
				{
					Name: "validate",
					Run:  `test "$MESSAGE" = "action-input"`,
					Env:  map[string]string{"MESSAGE": "${{ inputs.message }}"},
				},
				{
					Name: "emit",
					Run:  `printf 'endpoint=https://action.e2e.example.com\n' > "$KRUNTIME_OUTPUTS"`,
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), action); err != nil {
		t.Fatalf("create Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), action) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: "bash",
				Steps: []v1alpha1.StepSpec{
					{Name: "setup", Uses: action.Name, With: map[string]string{"message": "action-input"}},
					{
						Name: "consume",
						Run:  `test "$ENDPOINT" = "https://action.e2e.example.com"`,
						Env:  map[string]string{"ENDPOINT": "${{ steps.setup.outputs.endpoint }}"},
					},
				},
			},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with Action call: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	setup := workflowRun.Status.Jobs["build"].Steps[0]
	if setup.Phase != v1alpha1.StepSucceeded || setup.Outputs["endpoint"] != "https://action.e2e.example.com" {
		t.Fatalf("Action caller step = %#v, want projected endpoint", setup)
	}
	if len(setup.ActionSteps) != 2 || setup.ActionSteps[0].Phase != v1alpha1.StepSucceeded || setup.ActionSteps[1].Phase != v1alpha1.StepSucceeded {
		t.Fatalf("Action internal steps = %#v, want both succeeded", setup.ActionSteps)
	}
	if consume := workflowRun.Status.Jobs["build"].Steps[1]; consume.Phase != v1alpha1.StepSucceeded {
		t.Fatalf("consumer step = %#v, want succeeded after Action output projection", consume)
	}
}

func TestWorkflowRunFailsWhenActionChildRunFails(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-fail-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{{Name: "fail", Run: "exit 17"}},
		},
	}
	if err := k8sClient.Create(context.Background(), action); err != nil {
		t.Fatalf("create failing Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), action) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-fail-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: action.Name}}},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with failing Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowFailed)
	step := workflowRun.Status.Jobs["build"].Steps[0]
	if step.Phase != v1alpha1.StepFailed || len(step.ActionSteps) != 1 || step.ActionSteps[0].Phase != v1alpha1.StepFailed {
		t.Fatalf("failing Action status = %#v, want failed caller and child step", step)
	}
}

func TestWorkflowRunCancellationPropagatesToActionChildRun(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-cancel-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{{Name: "wait", Run: "sleep 30"}},
		},
	}
	if err := k8sClient.Create(context.Background(), action); err != nil {
		t.Fatalf("create cancellable Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), action) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-cancel-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: action.Name}}},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with cancellable Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	actionRunName := waitForActionChildRun(t, workflowRun, "build", "wait", 20*time.Second)
	workflowRun.Spec.CancelRequested = true
	if err := k8sClient.Update(context.Background(), workflowRun); err != nil {
		t.Fatalf("request WorkflowRun cancellation: %v", err)
	}

	actionRun := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: actionRunName, Namespace: testNamespace}}
	waitForRunPhase(t, actionRun, 30*time.Second, v1alpha1.RunCancelled)
	if !actionRun.Spec.HasImmediateTermination() {
		t.Fatalf("Action child Run %s does not request immediate termination", actionRun.Name)
	}
	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowCancelled)
}

func TestWorkflowRunRejectsMissingActionWithoutChildRun(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-missing-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: "does-not-exist-" + nameSuffix}}},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with missing Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowFailed)
	if !strings.Contains(workflowRun.Status.Message, "does not exist") {
		t.Fatalf("missing Action message = %q, want missing definition", workflowRun.Status.Message)
	}
	var runs v1alpha1.RunList
	if err := k8sClient.List(context.Background(), &runs,
		client.InNamespace(testNamespace),
		client.MatchingLabels{v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID)}); err != nil {
		t.Fatalf("list child Runs for rejected WorkflowRun: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Fatalf("child Runs = %#v, want none for missing Action", runs.Items)
	}
}

func TestWorkflowRunRecoversActionAfterControllerRestart(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-recovery-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{
				{Name: "first", Run: "sleep 5"},
				{Name: "second", Run: "true"},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), action); err != nil {
		t.Fatalf("create recovery Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), action) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-recovery-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: action.Name}}},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with recovery Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	firstRunName := waitForActionChildRun(t, workflowRun, "build", "first", 20*time.Second)
	restartController(t)
	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	setup := workflowRun.Status.Jobs["build"].Steps[0]
	if len(setup.ActionSteps) != 2 || setup.ActionSteps[0].RunName != firstRunName || setup.ActionSteps[0].Phase != v1alpha1.StepSucceeded || setup.ActionSteps[1].Phase != v1alpha1.StepSucceeded {
		t.Fatalf("recovered Action status = %#v, want both persisted Action steps succeeded", setup)
	}
}

func waitForActionChildRun(t *testing.T, workflowRun *v1alpha1.WorkflowRun, jobName, actionStepName string, timeout time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			t.Fatalf("get WorkflowRun while waiting for Action child: %v", err)
		}
		for _, step := range workflowRun.Status.Jobs[jobName].Steps {
			for _, actionStep := range step.ActionSteps {
				if actionStep.Name == actionStepName && actionStep.RunName != "" {
					return actionStep.RunName
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for Action child Run: %#v", workflowRun.Status)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func restartController(t *testing.T) {
	t.Helper()
	const selector = "app.kubernetes.io/component=controller,app.kubernetes.io/instance=kruntimes"
	pods, err := coreClientset.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("list controller Pods: err=%v pods=%#v", err, pods.Items)
	}
	previousName := pods.Items[0].Name
	if err := coreClientset.CoreV1().Pods(testNamespace).Delete(context.Background(), previousName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete controller Pod %s: %v", previousName, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for {
		pods, err := coreClientset.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{LabelSelector: selector})
		if err == nil {
			for index := range pods.Items {
				pod := &pods.Items[index]
				if pod.Name != previousName && podIsReady(pod) {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for controller replacement after deleting %s", previousName)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func podIsReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning || pod.DeletionTimestamp != nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func TestWorkflowRunExecutesReusableWorkflowAndProjectsOutputs(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-deploy-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"endpoint": {Value: "${{ jobs.apply.outputs.endpoint }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"apply": {
					RunsOn: "bash",
					Outputs: map[string]string{
						"endpoint": "${{ steps.deploy.outputs.endpoint }}",
					},
					Steps: []v1alpha1.StepSpec{{
						Name: "deploy",
						Run:  `printf 'endpoint=https://e2e.example.com\n' > "$KRUNTIME_OUTPUTS"`,
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-reuse-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"deploy": {Uses: workflow.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	deploy := workflowRun.Status.Jobs["deploy"]
	if deploy.WorkflowRunName == "" || deploy.Phase != v1alpha1.JobSucceeded || deploy.Outputs["endpoint"] != "https://e2e.example.com" {
		t.Fatalf("deploy job status = %#v, want succeeded reusable call with projected endpoint", deploy)
	}
}

func TestWorkflowRunExecutesNestedReusableWorkflows(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	leaf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-leaf-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"result": {Value: "${{ jobs.test.outputs.result }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Outputs: map[string]string{
						"result": "${{ steps.emit.outputs.result }}",
					},
					Steps: []v1alpha1.StepSpec{{
						Name: "emit",
						Run:  `printf 'result=nested-ok\n' > "$KRUNTIME_OUTPUTS"`,
					}},
				},
			},
		},
	}
	middle := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-middle-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"result": {Value: "${{ jobs.call-leaf.outputs.result }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"call-leaf": {Uses: leaf.Name},
			},
		},
	}
	for _, workflow := range []*v1alpha1.Workflow{leaf, middle} {
		if err := k8sClient.Create(context.Background(), workflow); err != nil {
			t.Fatalf("create reusable workflow %s: %v", workflow.Name, err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })
	}

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-nested-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call-middle": {Uses: middle.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	call := workflowRun.Status.Jobs["call-middle"]
	if call.WorkflowRunName == "" || call.Phase != v1alpha1.JobSucceeded || call.Outputs["result"] != "nested-ok" {
		t.Fatalf("call-middle job status = %#v, want succeeded nested reusable call with projected result", call)
	}
}

func TestWorkflowRunCancellationPropagatesToReusableWorkflow(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cancel-reuse-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"apply": {
				RunsOn: "bash",
				Steps:  []v1alpha1.StepSpec{{Name: "wait", Run: "sleep 300"}},
			},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cancel-reuse-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"deploy": {Uses: workflow.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			t.Fatalf("get workflowrun: %v", err)
		}
		if workflowRun.Status.Jobs["deploy"].WorkflowRunName != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for reusable call to start: %#v", workflowRun.Status)
		default:
		}
	}
	workflowRun.Spec.CancelRequested = true
	if err := k8sClient.Update(context.Background(), workflowRun); err != nil {
		t.Fatalf("request workflowrun cancellation: %v", err)
	}

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowCancelled)
}

func TestWorkflowRunRejectsReusableWorkflowCycle(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowA := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cycle-a-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call-b": {Uses: "e2e-cycle-b-" + nameSuffix},
		}},
	}
	workflowB := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cycle-b-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call-a": {Uses: workflowA.Name},
		}},
	}
	for _, workflow := range []*v1alpha1.Workflow{workflowA, workflowB} {
		if err := k8sClient.Create(context.Background(), workflow); err != nil {
			t.Fatalf("create reusable workflow %s: %v", workflow.Name, err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })
	}

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cycle-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call-a": {Uses: workflowA.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 20*time.Second, v1alpha1.WorkflowFailed)
	status := workflowRun.Status.Jobs["call-a"]
	if status.Phase != v1alpha1.JobFailed || status.WorkflowRunName != "" {
		t.Fatalf("call-a status = %#v, want failed call without child workflowrun", status)
	}
	if !strings.Contains(workflowRun.Status.Message, "workflow call cycle:") {
		t.Fatalf("message = %q, want reusable workflow cycle", workflowRun.Status.Message)
	}
}

func TestWorkflowRunFreezesReusableTemplateAfterChildCreation(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-freeze-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"result": {Value: "${{ jobs.apply.outputs.result }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"apply": {
					RunsOn: "bash",
					Outputs: map[string]string{
						"result": "${{ steps.emit.outputs.result }}",
					},
					Steps: []v1alpha1.StepSpec{{
						Name: "emit",
						Run:  `sleep 2; printf 'result=original\n' > "$KRUNTIME_OUTPUTS"`,
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-freeze-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call": {Uses: workflow.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			t.Fatalf("get workflowrun: %v", err)
		}
		if workflowRun.Status.Jobs["call"].WorkflowRunName != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for reusable child creation: %#v", workflowRun.Status)
		default:
		}
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflow), workflow); err != nil {
		t.Fatalf("get reusable workflow: %v", err)
	}
	workflow.Spec.Outputs["result"] = v1alpha1.WorkflowOutputSpec{Value: "changed"}
	if err := k8sClient.Update(context.Background(), workflow); err != nil {
		t.Fatalf("update reusable workflow after child creation: %v", err)
	}

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowSucceeded)
	if got := workflowRun.Status.Jobs["call"].Outputs["result"]; got != "original" {
		t.Fatalf("call output = %q, want original frozen output contract", got)
	}
}

func TestWorkflowRunBindsReusableTemplateWhenCallBecomesReady(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-late-bind-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"result": {Value: "${{ jobs.apply.outputs.result }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"apply": reusableOutputJob(`printf 'result=original\n' > "$KRUNTIME_OUTPUTS"`),
			},
		},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-late-bind-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"gate": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "wait", Run: "sleep 2"}}},
			"call": {Needs: []string{"gate"}, Uses: workflow.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current v1alpha1.Workflow
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflow), &current); err != nil {
			return err
		}
		current.Spec.Jobs["apply"] = reusableOutputJob(`printf 'result=updated\n' > "$KRUNTIME_OUTPUTS"`)
		return k8sClient.Update(context.Background(), &current)
	}); err != nil {
		t.Fatalf("update reusable workflow before call becomes ready: %v", err)
	}

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowSucceeded)
	if got := workflowRun.Status.Jobs["call"].Outputs["result"]; got != "updated" {
		t.Fatalf("call output = %q, want updated late-bound template output", got)
	}
}

func reusableOutputJob(run string) v1alpha1.JobSpec {
	return v1alpha1.JobSpec{
		RunsOn: "bash",
		Outputs: map[string]string{
			"result": "${{ steps.emit.outputs.result }}",
		},
		Steps: []v1alpha1.StepSpec{{Name: "emit", Run: run}},
	}
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
			Mode: taskMode(
				`mkdir -p "$KRUNTIME_ARTIFACTS_DIR/bundle"; printf report > "$KRUNTIME_ARTIFACTS_DIR/report.txt"; printf nested > "$KRUNTIME_ARTIFACTS_DIR/bundle/data.txt"`,
			),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create artifact run: %v", err)
	}
	waitForRun(t, run, 30*time.Second)

	if len(run.Status.ArtifactRefs) != 2 {
		t.Fatalf("artifact refs = %#v, want 2", run.Status.ArtifactRefs)
	}
	if run.Status.ArtifactStore == nil || run.Status.ArtifactStore.Filesystem == nil ||
		run.Status.ArtifactStore.Filesystem.VolumeClaimName != claimName {
		t.Fatalf("artifact store cleanup snapshot = %#v", run.Status.ArtifactStore)
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

	deleteRuntimeAndWait(t, runtimeName, 30*time.Second)

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
	assertFilesystemArtifactMissing(t, claimName, report.Location.Filesystem.Path)
}

func TestSessionRunExportsArtifactsOnDrain(t *testing.T) {
	runtimeName := "bash-session-artifacts"
	claimName := "e2e-session-artifacts"
	ensureFilesystemRuntime(t, runtimeName, claimName)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-artifacts-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create Session artifact Run: %v", err)
	}
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	response := waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token,
		[]byte(`{"command":{"argv":["sh","-c","printf session-report > \"$KRUNTIME_ARTIFACTS_DIR/report.txt\"; printf done"]}}`), http.StatusOK)
	var operation struct {
		Command struct {
			ExitCode int32  `json:"exitCode"`
			Stdout   []byte `json:"stdout"`
		} `json:"command"`
	}
	if err := json.Unmarshal(response, &operation); err != nil {
		t.Fatalf("decode Session artifact command response: %v", err)
	}
	if operation.Command.ExitCode != 0 || string(operation.Command.Stdout) != "done" {
		t.Fatalf("Session artifact command = %#v, want successful done output", operation.Command)
	}

	requestRunDrain(t, run)
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunSucceeded)
	if run.Status.ArtifactStore == nil || run.Status.ArtifactStore.Filesystem == nil ||
		run.Status.ArtifactStore.Filesystem.VolumeClaimName != claimName {
		t.Fatalf("artifact store cleanup snapshot = %#v", run.Status.ArtifactStore)
	}
	if len(run.Status.ArtifactRefs) != 1 || run.Status.ArtifactRefs[0].Name != "report.txt" {
		t.Fatalf("artifact refs = %#v, want report.txt", run.Status.ArtifactRefs)
	}

	destination := filepath.Join(t.TempDir(), "report.txt")
	if _, err := krt.DownloadArtifact(t.Context(), k8sClient, restConfig, testNamespace, run.Name, "report.txt", destination, 19095); err != nil {
		t.Fatalf("download Session artifact: %v", err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "session-report" {
		t.Fatalf("downloaded Session artifact = %q, want session-report", contents)
	}
}

func TestRunStagesArtifactInputs(t *testing.T) {
	runtimeName := "bash-artifact-inputs"
	claimName := "e2e-artifact-inputs"
	ensureFilesystemRuntime(t, runtimeName, claimName)

	producer := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-artifact-producer-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode(`mkdir -p "$KRUNTIME_ARTIFACTS_DIR/bundle"; printf report > "$KRUNTIME_ARTIFACTS_DIR/report.txt"; printf nested > "$KRUNTIME_ARTIFACTS_DIR/bundle/data.txt"`),
		},
	}
	if err := k8sClient.Create(context.Background(), producer); err != nil {
		t.Fatalf("create artifact producer: %v", err)
	}
	waitForRun(t, producer, 30*time.Second)

	refs := make(map[string]v1alpha1.ArtifactRef, len(producer.Status.ArtifactRefs))
	for _, ref := range producer.Status.ArtifactRefs {
		refs[ref.Name] = ref
	}
	report, reportFound := refs["report.txt"]
	bundle, bundleFound := refs["bundle"]
	if !reportFound || !bundleFound {
		t.Fatalf("producer artifact refs = %#v, want report.txt and bundle", producer.Status.ArtifactRefs)
	}

	consumer := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-artifact-consumer-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			ArtifactInputs: []v1alpha1.ArtifactInput{
				{Ref: report, Path: "inputs/report.txt"},
				{Ref: bundle, Path: "inputs/bundle"},
			},
			Mode: taskMode(`test "$(cat inputs/report.txt)" = report && test "$(cat inputs/bundle/data.txt)" = nested`),
		},
	}
	if err := k8sClient.Create(context.Background(), consumer); err != nil {
		t.Fatalf("create artifact consumer: %v", err)
	}
	waitForRun(t, consumer, 30*time.Second)

	sessionConsumer := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-artifact-consumer-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			ArtifactInputs: []v1alpha1.ArtifactInput{
				{Ref: report, Path: "inputs/report.txt"},
				{Ref: bundle, Path: "inputs/bundle"},
			},
			Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(context.Background(), sessionConsumer); err != nil {
		t.Fatalf("create Session artifact consumer: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sessionConsumer) })
	waitForRunPhase(t, sessionConsumer, 30*time.Second, v1alpha1.RunReady)
	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), sessionConsumer.Status.Endpoint.URL)
	token := sessionGatewayToken(t, sessionConsumer)
	response := waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token,
		[]byte(`{"command":{"argv":["sh","-c","printf '%s:%s' \"$(cat inputs/report.txt)\" \"$(cat inputs/bundle/data.txt)\""]}}`), http.StatusOK)
	var operation struct {
		Command struct {
			ExitCode int32  `json:"exitCode"`
			Stdout   []byte `json:"stdout"`
		} `json:"command"`
	}
	if err := json.Unmarshal(response, &operation); err != nil {
		t.Fatalf("decode Session artifact input response: %v", err)
	}
	if operation.Command.ExitCode != 0 || string(operation.Command.Stdout) != "report:nested" {
		t.Fatalf("Session artifact input result = %#v, want report:nested", operation.Command)
	}
	requestRunCancel(t, sessionConsumer)
	waitForRunPhase(t, sessionConsumer, 20*time.Second, v1alpha1.RunCancelled)

	missing := report.DeepCopy()
	missing.Name = "missing.txt"
	missing.Location.Filesystem.Path = filepath.ToSlash(filepath.Join("namespaces", testNamespace, "runs", "missing", missing.Name))
	missingConsumer := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-missing-artifact-consumer-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:        runtimeName,
			ArtifactInputs: []v1alpha1.ArtifactInput{{Ref: *missing, Path: "inputs/missing.txt"}},
			Mode:           taskMode(`exit 1`),
		},
	}
	if err := k8sClient.Create(context.Background(), missingConsumer); err != nil {
		t.Fatalf("create missing-artifact consumer: %v", err)
	}
	waitForRunPhase(t, missingConsumer, 30*time.Second, v1alpha1.RunFailed)
	if !strings.Contains(missingConsumer.Status.Message, "open artifact input") {
		t.Fatalf("missing artifact consumer message = %q, want artifact input error", missingConsumer.Status.Message)
	}
}

func deleteRuntimeAndWait(t *testing.T, name string, timeout time.Duration) {
	t.Helper()
	runtimeResource := &v1alpha1.Runtime{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	if err := k8sClient.Delete(context.Background(), runtimeResource); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("delete Runtime %s: %v", name, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods, client.InNamespace(testNamespace), client.MatchingLabels{"runtime": name}); err != nil {
			t.Fatalf("list Runtime pods: %v", err)
		}
		if len(pods.Items) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for Runtime %s pods to be deleted", name)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func assertFilesystemArtifactMissing(t *testing.T, claimName, relativePath string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "artifact-inspector-", Namespace: testNamespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "inspector", Image: bashRuntimeImage(),
				Command:      []string{"test"},
				Args:         []string{"!", "-e", "/artifacts/" + relativePath},
				VolumeMounts: []corev1.VolumeMount{{Name: "artifacts", MountPath: "/artifacts"}},
			}},
			Volumes: []corev1.Volume{{
				Name: "artifacts",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claimName,
				}},
			}},
		},
	}
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create artifact inspector Pod: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			t.Fatalf("get artifact inspector Pod: %v", err)
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			return
		case corev1.PodFailed:
			t.Fatalf("artifact inspector found path %s", relativePath)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for artifact inspector Pod, phase=%s", pod.Status.Phase)
		case <-time.After(500 * time.Millisecond):
		}
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
			Mode:    taskMode("sleep 10; echo should_not_print"),
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
			Mode:                    taskMode("echo ttl-gc"),
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
			Mode:    taskMode("sleep 20; echo recovered"),
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
			Mode:    taskMode("sleep 10"),
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
			Mode:    taskMode("sleep 30; echo should_not_finish"),
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
			Mode:    taskMode("sleep 1; echo boundary_done"),
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
			Mode:    taskMode("echo hello"),
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
			Mode:    taskMode("echo hello"),
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

func TestPersistentWorkspaceBindingFencesRuntimePodReplacement(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	runtimeName := "workspace-binding-" + nameSuffix
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-" + nameSuffix, Namespace: testNamespace},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: runtimeName},
	}
	if err := k8sClient.Create(context.Background(), workspace); err != nil {
		t.Fatalf("create PersistentWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workspace) })

	waitForPersistentWorkspacePhase(t, workspace, 45*time.Second, v1alpha1.PersistentWorkspaceBound)
	if workspace.Status.BoundPod == "" || workspace.Status.BoundPodUID == "" || workspace.Status.Path == "" {
		t.Fatalf("bound workspace status = %#v, want fenced Pod and path", workspace.Status)
	}
	boundPod := workspace.Status.BoundPod
	writerRun := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-write-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode("echo workspace-data > shared.txt"),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), writerRun); err != nil {
		t.Fatalf("create workspace writer Run: %v", err)
	}
	waitForRun(t, writerRun, 30*time.Second)
	if writerRun.Status.AssignedPod != boundPod {
		t.Fatalf("workspace writer assignedPod = %q, want bound Pod %q", writerRun.Status.AssignedPod, boundPod)
	}
	readerRun := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-read-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode(`test "$(cat shared.txt)" = workspace-data`),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), readerRun); err != nil {
		t.Fatalf("create workspace reader Run: %v", err)
	}
	waitForRun(t, readerRun, 30*time.Second)
	if readerRun.Status.AssignedPod != boundPod {
		t.Fatalf("workspace reader assignedPod = %q, want bound Pod %q", readerRun.Status.AssignedPod, boundPod)
	}
	if err := coreClientset.CoreV1().Pods(testNamespace).Delete(context.Background(), boundPod, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete bound Runtime Pod %s: %v", boundPod, err)
	}

	waitForPersistentWorkspacePhase(t, workspace, 60*time.Second, v1alpha1.PersistentWorkspaceLost)
	if workspace.Status.BoundPod != boundPod {
		t.Fatalf("lost workspace boundPod = %q, want original %q", workspace.Status.BoundPod, boundPod)
	}
	lostRun := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-lost-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode("echo workspace-lost"),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), lostRun); err != nil {
		t.Fatalf("create lost workspace run: %v", err)
	}
	waitForPendingRunMessage(t, lostRun, 30*time.Second, "was lost")
}

func TestPersistentWorkspaceAdmissionAuthorization(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-admission-" + suffix, Namespace: testNamespace},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: "bash"},
	}
	if err := k8sClient.Create(ctx, workspace); err != nil {
		t.Fatalf("create authorization workspace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, workspace) })

	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "workspace-user-" + suffix, Namespace: testNamespace}}
	if err := k8sClient.Create(ctx, serviceAccount); err != nil {
		t.Fatalf("create workspace test ServiceAccount: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, serviceAccount) })

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-user-" + suffix, Namespace: testNamespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{v1alpha1.GroupVersion.Group},
			Resources: []string{"runs"},
			Verbs:     []string{"create"},
		}},
	}
	if err := k8sClient.Create(ctx, role); err != nil {
		t.Fatalf("create workspace test Role: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, role) })
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: testNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount.Name, Namespace: testNamespace}},
	}
	if err := k8sClient.Create(ctx, binding); err != nil {
		t.Fatalf("create workspace test RoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, binding) })

	workspaceUser := impersonatedClient(t, "system:serviceaccount:"+testNamespace+":"+serviceAccount.Name)
	deniedRun := workspaceAuthorizationRun("workspace-denied-"+suffix, workspace.Name)
	if err := workspaceUser.Create(ctx, deniedRun); !apierrors.IsForbidden(err) {
		t.Fatalf("create Run without workspace use permission = %v, want forbidden", err)
	}

	role.Rules = append(role.Rules, rbacv1.PolicyRule{
		APIGroups:     []string{v1alpha1.GroupVersion.Group},
		Resources:     []string{"persistentworkspaces/use"},
		ResourceNames: []string{workspace.Name},
		Verbs:         []string{"use"},
	})
	if err := k8sClient.Update(ctx, role); err != nil {
		t.Fatalf("grant named workspace use permission: %v", err)
	}
	allowedRun := workspaceAuthorizationRun("workspace-allowed-"+suffix, workspace.Name)
	if err := workspaceUser.Create(ctx, allowedRun); err != nil {
		t.Fatalf("create Run with named workspace use permission: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, allowedRun) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-workflow-" + suffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "run", Run: "echo authorized"}}},
		}},
	}
	if err := k8sClient.Create(ctx, workflowRun); err != nil {
		t.Fatalf("create WorkflowRun for controller authorization: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, workflowRun) })
	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)

	workflowWorkspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "workspace-controller-" + suffix,
			Namespace:       testNamespace,
			Labels:          map[string]string{v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID), v1alpha1.WorkflowJobLabel: "build"},
			OwnerReferences: []metav1.OwnerReference{workflowRunOwnerReference(workflowRun)},
		},
		Spec: v1alpha1.PersistentWorkspaceSpec{Runtime: "bash"},
	}
	if err := k8sClient.Create(ctx, workflowWorkspace); err != nil {
		t.Fatalf("create WorkflowRun-owned workspace: %v", err)
	}
	controllerClient := impersonatedClient(t, "system:serviceaccount:"+testNamespace+":kruntimes-controller")
	crossJobRun := workspaceAuthorizationRun("workspace-controller-cross-job-"+suffix, workflowWorkspace.Name)
	crossJobRun.Labels = map[string]string{
		v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID),
		v1alpha1.WorkflowJobLabel:    "other",
	}
	crossJobRun.OwnerReferences = []metav1.OwnerReference{workflowRunOwnerReference(workflowRun)}
	if err := controllerClient.Create(ctx, crossJobRun); !apierrors.IsForbidden(err) {
		t.Fatalf("controller create cross-job workspace Run = %v, want forbidden", err)
	}
}

func workspaceAuthorizationRun(name, workspaceName string) *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   "bash",
			Mode:      v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{Args: []string{"echo authorization"}}},
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspaceName},
		},
	}
}

func impersonatedClient(t *testing.T, username string) client.Client {
	t.Helper()
	config := rest.CopyConfig(restConfig)
	config.Impersonate = rest.ImpersonationConfig{UserName: username}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	impersonated, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create impersonated Kubernetes client: %v", err)
	}
	return impersonated
}

func workflowRunOwnerReference(workflowRun *v1alpha1.WorkflowRun) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "WorkflowRun",
		Name:       workflowRun.Name,
		UID:        workflowRun.UID,
		Controller: &controller,
	}
}

func TestPersistentWorkspaceExplicitDeletionCleansRetainedData(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	runtimeName := "workspace-cleanup-" + nameSuffix
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-cleanup-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime:       runtimeName,
			CleanupPolicy: v1alpha1.PersistentWorkspaceRetain,
		},
	}
	if err := k8sClient.Create(context.Background(), workspace); err != nil {
		t.Fatalf("create PersistentWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workspace) })

	waitForPersistentWorkspacePhase(t, workspace, 45*time.Second, v1alpha1.PersistentWorkspaceBound)
	marker := "cleanup-marker"
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-cleanup-write-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode("echo retained-data > " + marker),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create workspace writer Run: %v", err)
	}
	waitForRun(t, run, 30*time.Second)

	markerPath := filepath.Join(workspace.Status.Path, marker)
	if _, stderr, err := execInPod(context.Background(), workspace.Status.BoundPod, "runtimed", []string{"/bin/sh", "-c", "test -f " + markerPath}); err != nil {
		t.Fatalf("verify workspace marker before deletion: %v: %s", err, stderr)
	}
	if err := k8sClient.Delete(context.Background(), workspace); err != nil {
		t.Fatalf("delete PersistentWorkspace: %v", err)
	}
	waitForPersistentWorkspaceDeleted(t, workspace, 45*time.Second)
	if _, stderr, err := execInPod(context.Background(), workspace.Status.BoundPod, "runtimed", []string{"/bin/sh", "-c", "test ! -e " + markerPath}); err != nil {
		t.Fatalf("verify workspace marker after deletion: %v: %s", err, stderr)
	}
}

func TestPersistentWorkspaceTTLDeletionCleansData(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	runtimeName := "workspace-ttl-cleanup-" + nameSuffix
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	// A newly Bound workspace begins its unused interval immediately. Leave
	// enough time to create and complete the writer Run before exercising the
	// post-Run TTL cleanup path.
	ttlSeconds := int32(10)
	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-ttl-cleanup-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime:               runtimeName,
			CleanupPolicy:         v1alpha1.PersistentWorkspaceDeleteAfterTTL,
			TTLSecondsAfterUnused: &ttlSeconds,
		},
	}
	if err := k8sClient.Create(context.Background(), workspace); err != nil {
		t.Fatalf("create PersistentWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workspace) })

	waitForPersistentWorkspacePhase(t, workspace, 45*time.Second, v1alpha1.PersistentWorkspaceBound)
	marker := "ttl-cleanup-marker"
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-ttl-cleanup-write-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode("echo ttl-data > " + marker),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create workspace writer Run: %v", err)
	}
	waitForRun(t, run, 30*time.Second)

	markerPath := filepath.Join(workspace.Status.Path, marker)
	if _, stderr, err := execInPod(context.Background(), workspace.Status.BoundPod, "runtimed", []string{"/bin/sh", "-c", "test -f " + markerPath}); err != nil {
		t.Fatalf("verify workspace marker before TTL cleanup: %v: %s", err, stderr)
	}
	waitForPersistentWorkspaceDeleted(t, workspace, 45*time.Second)
	if _, stderr, err := execInPod(context.Background(), workspace.Status.BoundPod, "runtimed", []string{"/bin/sh", "-c", "test ! -e " + markerPath}); err != nil {
		t.Fatalf("verify workspace marker after TTL cleanup: %v: %s", err, stderr)
	}
}

func TestWorkflowRunSharesJobLocalWorkspace(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("e2e-workspace-%d", time.Now().UnixNano()),
			Namespace: testNamespace,
		},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: "bash",
				Steps: []v1alpha1.StepSpec{
					{Name: "write", Run: "echo workflow-data > shared.txt"},
					{Name: "read", Run: `test "$(cat shared.txt)" = workflow-data`},
				},
			},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowSucceeded)
	var workspaces v1alpha1.PersistentWorkspaceList
	if err := k8sClient.List(context.Background(), &workspaces, client.InNamespace(testNamespace), client.MatchingLabels{
		v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID),
		v1alpha1.WorkflowJobLabel:    "build",
	}); err != nil {
		t.Fatalf("list job workspaces: %v", err)
	}
	if len(workspaces.Items) != 1 {
		t.Fatalf("job workspaces = %#v, want one", workspaces.Items)
	}
	workspace := &workspaces.Items[0]
	if workspace.Spec.Runtime != "bash" || !metav1.IsControlledBy(workspace, workflowRun) {
		t.Fatalf("workspace = %#v, want WorkflowRun-owned bash workspace", workspace)
	}

	var runs v1alpha1.RunList
	if err := k8sClient.List(context.Background(), &runs, client.InNamespace(testNamespace), client.MatchingLabels{
		v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID),
		v1alpha1.WorkflowJobLabel:    "build",
	}); err != nil {
		t.Fatalf("list child Runs: %v", err)
	}
	if len(runs.Items) != 2 {
		t.Fatalf("child Runs = %#v, want write and read", runs.Items)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if run.Spec.Workspace == nil || run.Spec.Workspace.Name != workspace.Name {
			t.Fatalf("Run %s workspace = %#v, want %q", run.Name, run.Spec.Workspace, workspace.Name)
		}
	}
}

func TestWorkflowRunTransfersArtifactsBetweenJobs(t *testing.T) {
	runtimeName := fmt.Sprintf("workflow-artifacts-%d", time.Now().UnixNano())
	claimName := runtimeName + "-artifacts"
	ensureFilesystemRuntime(t, runtimeName, claimName)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-workflow-artifacts-" + fmt.Sprint(time.Now().UnixNano()), Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: runtimeName,
				Steps: []v1alpha1.StepSpec{{
					Name: "package",
					Run:  `mkdir -p "$KRUNTIME_ARTIFACTS_DIR"; printf workflow-artifact > "$KRUNTIME_ARTIFACTS_DIR/dist.txt"`,
				}},
			},
			"verify": {
				RunsOn: runtimeName,
				Needs:  []string{"build"},
				Steps: []v1alpha1.StepSpec{{
					Name: "verify",
					Artifacts: []v1alpha1.WorkflowArtifactInput{{
						From: "jobs.build.artifacts.dist.txt",
						Path: "dist.txt",
					}},
					Run: `test "$(cat dist.txt)" = workflow-artifact`,
				}},
			},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create artifact WorkflowRun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	artifact, found := workflowRun.Status.Jobs["build"].Artifacts["dist.txt"]
	if !found || artifact.Location.Filesystem == nil || artifact.Location.Filesystem.Path == "" {
		t.Fatalf("build artifacts = %#v, want dist.txt filesystem reference", workflowRun.Status.Jobs["build"].Artifacts)
	}

	missingArtifact := workflowRun.DeepCopy()
	missingArtifact.ResourceVersion = ""
	missingArtifact.UID = ""
	missingArtifact.Name = workflowRun.Name + "-missing"
	missingArtifact.CreationTimestamp = metav1.Time{}
	missingArtifact.Status = v1alpha1.WorkflowRunStatus{}
	missingArtifact.Spec.Jobs["verify"] = v1alpha1.JobSpec{
		RunsOn: runtimeName,
		Needs:  []string{"build"},
		Steps: []v1alpha1.StepSpec{{
			Name: "verify",
			Artifacts: []v1alpha1.WorkflowArtifactInput{{
				From: "jobs.build.artifacts.missing.txt",
				Path: "missing.txt",
			}},
			Run: "exit 0",
		}},
	}
	if err := k8sClient.Create(context.Background(), missingArtifact); err != nil {
		t.Fatalf("create missing-artifact WorkflowRun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), missingArtifact) })
	waitForWorkflowRunPhase(t, missingArtifact, 45*time.Second, v1alpha1.WorkflowFailed)
	if status := missingArtifact.Status.Jobs["verify"]; status.Phase != v1alpha1.JobFailed {
		t.Fatalf("missing artifact verify job = %#v, want Failed", status)
	}
}

func waitForPersistentWorkspacePhase(t *testing.T, workspace *v1alpha1.PersistentWorkspace, timeout time.Duration, phase v1alpha1.PersistentWorkspacePhase) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workspace), workspace); err != nil {
			t.Fatalf("get PersistentWorkspace while waiting for %s: %v", phase, err)
		}
		if workspace.Status.Phase == phase {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("PersistentWorkspace = %#v, want phase %s", workspace.Status, phase)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForPersistentWorkspaceDeleted(t *testing.T, workspace *v1alpha1.PersistentWorkspace, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		var current v1alpha1.PersistentWorkspace
		err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workspace), &current)
		if apierrors.IsNotFound(err) {
			return
		}
		if err != nil {
			t.Fatalf("get PersistentWorkspace while waiting for deletion: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("PersistentWorkspace %s/%s was not deleted", workspace.Namespace, workspace.Name)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func waitForPendingRunMessage(t *testing.T, run *v1alpha1.Run, timeout time.Duration, message string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run while waiting for Pending: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunPending && run.Status.AssignedPod == "" && strings.Contains(run.Status.Message, message) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("Run status = %#v, want unassigned Pending Run with message containing %q", run.Status, message)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func TestSchedulerReactivatesPendingRunWhenRuntimePodBecomesReady(t *testing.T) {
	runtimeName := fmt.Sprintf("scheduler-wakeup-%d", time.Now().UnixNano())
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-runtime-wakeup-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode("echo runtime-ready"),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), run) }()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get pending run: %v", err)
		}
		if run.Status.Phase == v1alpha1.RunPending &&
			strings.Contains(run.Status.Message, "waiting for available runtime pods") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for pending run observation, phase=%s msg=%s", run.Status.Phase, run.Status.Message)
		default:
		}
	}

	// The Runtime Pod create/ready events must reactivate this Run before the
	// scheduler's 30-second no-capacity polling fallback.
	started := time.Now()
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)
	waitForRun(t, run, 20*time.Second)
	if elapsed := time.Since(started); elapsed >= 30*time.Second {
		t.Fatalf("pending Run completed after %s, want Runtime Pod event wakeup before polling fallback", elapsed)
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
			Mode:    taskMode("sleep 10; echo first"),
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
			Mode:    taskMode("echo second"),
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

func TestSchedulerInterRunAffinityBootstrap(t *testing.T) {
	runtimeName := "bash-affinity"
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 2)
	affinity := &v1alpha1.RunAffinity{RunAffinity: &v1alpha1.RunAffinityRules{
		RequiredDuringSchedulingIgnoredDuringExecution: []v1alpha1.RunAffinityTerm{{
			TopologyKey:   v1alpha1.RunAffinityTopologyRuntimePod,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"cohort": "build"}},
		}},
	}}
	first := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-affinity-first-", Namespace: testNamespace, Labels: map[string]string{"cohort": "build"}},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Affinity: affinity, Mode: taskMode("sleep 5; echo first")},
	}
	if err := k8sClient.Create(context.Background(), first); err != nil {
		t.Fatalf("create first affinity run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), first) }()
	waitForRunPhase(t, first, 20*time.Second, v1alpha1.RunRunning)

	second := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-affinity-second-", Namespace: testNamespace, Labels: map[string]string{"cohort": "build"}},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Affinity: affinity, Mode: taskMode("sleep 1; echo second")},
	}
	if err := k8sClient.Create(context.Background(), second); err != nil {
		t.Fatalf("create second affinity run: %v", err)
	}
	defer func() { _ = k8sClient.Delete(context.Background(), second) }()
	waitForRun(t, second, 20*time.Second)
	if second.Status.AssignedPod != first.Status.AssignedPod {
		t.Fatalf("second Run assigned to %q, want affinity target %q", second.Status.AssignedPod, first.Status.AssignedPod)
	}
	waitForRun(t, first, 20*time.Second)
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
			Mode:    taskMode(),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Python Run %s", run.Name)
	waitForRun(t, run, 30*time.Second)
	t.Logf("Python Run completed successfully: %s", run.Status.Message)
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
			Mode:    taskMode(`printf 'invalid\n' > "$KRUNTIME_OUTPUTS"`),
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
			Mode: taskMode(
				`printf 'value=' > "$KRUNTIME_OUTPUTS"; head -c 8193 /dev/zero | tr '\0' x >> "$KRUNTIME_OUTPUTS"`,
			),
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
			Runtime: "bash",
			Source:  &v1alpha1.CodeSource{Inline: &inline},
			Mode:    taskMode(),
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
			Mode:    taskMode("sleep 300"),
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
			Mode:    taskMode("sleep 300"),
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
