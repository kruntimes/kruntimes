package e2e

// Runtime pool fixtures: warm Runtime CRs, pod readiness, diagnostics, and restart helpers.
//
// Scenarios that need a Runtime call the helpers here instead of hand-rolling
// pod polling, so runtime setup is created once and shared across tests.

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

// sharedRuntimeFixtures caches the process-wide Runtime pools that most
// scenarios borrow ("bash" and "python"). The first scenario to need a pool
// creates it and waits for the pod; every later scenario reuses it instead of
// re-issuing the same spec update and pod wait. The mutex also serializes
// concurrent first use when scenarios run in parallel.
var sharedRuntimeFixtures sync.Map // runtime name -> *sharedRuntimeFixture

type sharedRuntimeFixture struct {
	mu   sync.Mutex
	done bool
}

// e2eSharedRunsCapacity is the worker count the shared warm pools are created
// with. The product default is 2 concurrent Runs per pod; scenarios that run in
// parallel would otherwise queue behind each other on the shared pool and turn
// a scheduling queue into a test timeout. Capacity only affects how many Runs
// execute concurrently, not what those Runs do.
const e2eSharedRunsCapacity int32 = 8

// runtimePodReadyTimeout bounds how long a scenario waits for a Runtime pod to
// become Ready and publish a Service endpoint.
const runtimePodReadyTimeout = 120 * time.Second

func isSharedRuntime(name string) bool {
	return name == "bash" || name == "python"
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

	if isSharedRuntime(name) {
		if runsCapacity <= 0 {
			runsCapacity = e2eSharedRunsCapacity
		}
		fixture, _ := sharedRuntimeFixtures.LoadOrStore(name, &sharedRuntimeFixture{})
		shared := fixture.(*sharedRuntimeFixture)
		shared.mu.Lock()
		defer shared.mu.Unlock()
		if shared.done {
			// The pool already exists; still confirm the pod is serving so a
			// scenario does not race a restart triggered by another scenario.
			waitForRuntimePod(t, name, image, runtimedImage(), runsCapacity, "shared runtime pod")
			return
		}
		if err := applyRuntime(name, image, port, replicas, runsCapacity); err != nil {
			t.Fatalf("ensure shared runtime %s: %v", name, err)
		}
		waitForRuntimePod(t, name, image, runtimedImage(), runsCapacity, "runtime pods")
		shared.done = true
		return
	}

	if err := applyRuntime(name, image, port, replicas, runsCapacity); err != nil {
		t.Fatalf("ensure runtime %s: %v", name, err)
	}
	cleanupRuntime(t, name)

	waitForRuntimePod(t, name, image, runtimedImage(), runsCapacity, "runtime pods")
}

// applyRuntime creates the Runtime or, when it already exists, updates it to
// the requested spec. It returns an error so callers that cache the result do
// not poison later scenarios with a cached failure.
func applyRuntime(name, image string, port, replicas, runsCapacity int32) error {
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
	if err := k8sClient.Create(context.Background(), rt); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create runtime: %w", err)
		}
		existing := &v1alpha1.Runtime{}
		if getErr := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(rt), existing); getErr != nil {
			return fmt.Errorf("get runtime: %w", getErr)
		}
		existing.Spec.Template = rt.Spec.Template
		existing.Spec.Port = port
		existing.Spec.Replicas = replicas
		if runsCapacity > 0 {
			existing.Spec.Capacity = rt.Spec.Capacity
		}
		if updateErr := k8sClient.Update(context.Background(), existing); updateErr != nil {
			return fmt.Errorf("update runtime: %w", updateErr)
		}
	}
	return nil
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
	key := client.ObjectKey{Namespace: testNamespace, Name: runtimeName}
	var lastReady int32
	var lastErr error
	waitFor(t, timeout, func() string {
		return fmt.Sprintf("Runtime %s readyReplicas=%d (last readyReplicas=%d, err=%v)", runtimeName, want, lastReady, lastErr)
	}, func(ctx context.Context) (bool, error) {
		runtimeResource := &v1alpha1.Runtime{}
		if err := k8sClient.Get(ctx, key, runtimeResource); err != nil {
			lastErr = err
			return false, err
		}
		lastErr = nil
		lastReady = runtimeResource.Status.ReadyReplicas
		return lastReady == want, nil
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

	var lastErr error
	waitFor(t, runtimePodReadyTimeout, func() string {
		return fmt.Sprintf("%s (Runtime %s: image=%s daemonImage=%s runsCapacity=%d, last error=%v)", description, name, runtimeImage, daemonImage, runsCapacity, lastErr)
	}, func(ctx context.Context) (bool, error) {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods,
			client.InNamespace(testNamespace),
			client.MatchingLabels{"runtime": name},
		); err != nil {
			lastErr = err
			return false, err
		}
		lastErr = nil
		for _, pod := range pods.Items {
			if isRuntimePodReady(&pod, runtimeImage, daemonImage, runsCapacity) && runtimeServiceHasReadyEndpoint(ctx, name) {
				return true, nil
			}
		}
		return false, nil
	}, func() {
		dumpRuntimeDiagnostics(t, name, runtimeImage, daemonImage, runsCapacity, lastErr)
	})
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
