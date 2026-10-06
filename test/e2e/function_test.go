package e2e

// Function mode scenarios: gateway invocation, idle/total timeout expiry, capacity release, and pod loss.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	"github.com/kruntimes/kruntimes/internal/runstatus"
)

func TestFunctionGatewayInvokesAuthorizedFunction(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
