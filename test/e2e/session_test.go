package e2e

// Session mode scenarios: Console gateway authorization, streaming, TLS, bounds, and run exhaustion.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/krt"
	"github.com/kruntimes/kruntimes/internal/logapi"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	"github.com/kruntimes/kruntimes/internal/runstatus"
	"github.com/kruntimes/kruntimes/sdk/go/sandbox"
)

func TestSessionGatewayExecutesAuthorizedOperation(t *testing.T) {
	t.Parallel()
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

func TestSessionGatewayStreamsCommandOutput(t *testing.T) {
	t.Parallel()
	runtimeName := fmt.Sprintf("session-stream-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-stream-", Namespace: testNamespace},
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
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/operations:stream", strings.NewReader(`{"command":{"argv":["sh","-c","printf stream-first; sleep 2; printf stream-last"]}}`))
	if err != nil {
		t.Fatalf("create streaming gateway request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := gatewayInsecureHTTPClient.Do(request)
	if err != nil {
		t.Fatalf("call streaming gateway: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		contents, _ := io.ReadAll(response.Body)
		t.Fatalf("streaming gateway status = %d: %s", response.StatusCode, contents)
	}
	if got := response.Header.Get("Content-Type"); got != "application/x-ndjson; charset=utf-8" {
		t.Fatalf("streaming content type = %q", got)
	}

	// Authorization and the initial HTTP response are outside the command
	// streaming contract. Measure whether output arrives while the command is
	// executing only after the NDJSON stream is established.
	streamStarted := time.Now()
	reader := bufio.NewReader(response.Body)
	seenFirstOutput := false
	var stdout strings.Builder
	seenCompleted := false
	lastSequence := int64(0)
	for {
		line, err := reader.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read streaming gateway event: %v", err)
		}
		var event struct {
			Sequence int64  `json:"sequence"`
			Type     string `json:"type"`
			Output   *struct {
				Stream string `json:"stream"`
				Data   []byte `json:"data"`
			} `json:"output"`
			Completed *struct {
				Command *struct {
					ExitCode int32  `json:"exitCode"`
					Stdout   []byte `json:"stdout"`
				} `json:"command"`
			} `json:"completed"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode streaming gateway event %q: %v", line, err)
		}
		if event.Sequence != lastSequence+1 {
			t.Fatalf("event sequence = %d after %d", event.Sequence, lastSequence)
		}
		lastSequence = event.Sequence
		if event.Type == "output" && event.Output != nil && event.Output.Stream == "stdout" && string(event.Output.Data) == "stream-first" {
			if elapsed := time.Since(streamStarted); elapsed >= time.Second {
				t.Fatalf("first command output arrived after %s, want it before command completion", elapsed)
			}
			seenFirstOutput = true
		}
		if event.Type == "output" && event.Output != nil && event.Output.Stream == "stdout" {
			stdout.Write(event.Output.Data)
		}
		if event.Type == "completed" && event.Completed != nil && event.Completed.Command != nil {
			if event.Completed.Command.ExitCode != 0 || len(event.Completed.Command.Stdout) != 0 {
				t.Fatalf("completed command = %#v", event.Completed.Command)
			}
			seenCompleted = true
		}
	}
	if !seenFirstOutput || !seenCompleted || stdout.String() != "stream-firststream-last" {
		t.Fatalf("stream did not include early output and completion: first=%t completed=%t stdout=%q", seenFirstOutput, seenCompleted, stdout.String())
	}

	websocketURL := "wss" + strings.TrimPrefix(baseURL, "https") + "/operations:ws"
	connection, websocketResponse, err := (&websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}).Dial(websocketURL, http.Header{"Authorization": []string{"Bearer " + token}}) //nolint:gosec // E2E local port-forward only.
	if err != nil {
		if websocketResponse != nil {
			t.Fatalf("dial operation WebSocket: %v (status %d)", err, websocketResponse.StatusCode)
		}
		t.Fatalf("dial operation WebSocket: %v", err)
	}
	defer connection.Close()
	if err := connection.WriteJSON(map[string]any{"type": "send", "operation": map[string]any{"command": map[string]any{"argv": []string{"sh", "-c", "printf websocket"}}}}); err != nil {
		t.Fatalf("write WebSocket send frame: %v", err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	websocketOutput := false
	websocketCompleted := false
	lastSequence = 0
	for !websocketCompleted {
		var event struct {
			Sequence int64  `json:"sequence"`
			Type     string `json:"type"`
			Output   *struct {
				Stream string `json:"stream"`
				Data   []byte `json:"data"`
			} `json:"output"`
			Completed *struct {
				Command *struct {
					ExitCode int32 `json:"exitCode"`
				} `json:"command"`
			} `json:"completed"`
		}
		if err := connection.ReadJSON(&event); err != nil {
			t.Fatalf("read WebSocket operation event: %v", err)
		}
		if event.Sequence != lastSequence+1 {
			t.Fatalf("WebSocket event sequence = %d after %d", event.Sequence, lastSequence)
		}
		lastSequence = event.Sequence
		if event.Type == "output" && event.Output != nil && event.Output.Stream == "stdout" && string(event.Output.Data) == "websocket" {
			websocketOutput = true
		}
		if event.Type == "completed" && event.Completed != nil && event.Completed.Command != nil {
			if event.Completed.Command.ExitCode != 0 {
				t.Fatalf("WebSocket command exit code = %d", event.Completed.Command.ExitCode)
			}
			websocketCompleted = true
		}
	}
	if !websocketOutput {
		t.Fatal("WebSocket stream did not include command output")
	}
}

func TestAggregatedRunLogAPIServesAuthorizedRunLogs(t *testing.T) {
	t.Parallel()
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

	// The CLI uses the Kubernetes aggregated API. Its token has only get on the
	// exact logs.kruntimes.io/runs/log resource: it has neither Run, Pod, nor
	// pods/log permission.
	cmd := krt.NewRootCmd()
	var cliStdout, cliStderr bytes.Buffer
	cmd.SetOut(&cliStdout)
	cmd.SetErr(&cliStderr)
	cmd.SetArgs([]string{"logs", run.Name, "--namespace", testNamespace, "--token", aggregatedLogToken(t, run), "--tail", "100"})
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("krt logs through aggregated API: %v\\nstderr: %s", err, cliStderr.String())
	}
	if !strings.Contains(cliStdout.String(), marker) {
		t.Fatalf("krt logs through aggregated API stdout = %q, want %q", cliStdout.String(), marker)
	}

	cmd = krt.NewRootCmd()
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"logs", run.Name, "--namespace", testNamespace, "--token", sessionGatewayTokenWithoutRunAccess(t), "--tail", "100"})
	if err := cmd.ExecuteContext(t.Context()); err == nil {
		t.Fatal("krt logs without logs.kruntimes.io/runs/log permission succeeded")
	}
}

func TestAggregatedRunLogAPIFollowsOneShotRunBeforeCompletion(t *testing.T) {
	t.Parallel()
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

	logConfig := rest.CopyConfig(restConfig)
	logConfig.BearerToken = aggregatedLogToken(t, run)
	logConfig.BearerTokenFile = ""
	logs, err := logapi.NewClient(logConfig)
	if err != nil {
		t.Fatalf("create aggregated Run log API client: %v", err)
	}
	assertAggregatedRunLogFollow(t, logs, run, marker)

	var current v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatalf("get one-shot Run after live log: %v", err)
	}
	if current.Status.Phase != v1alpha1.RunRunning {
		t.Fatalf("Run phase after receiving live log = %s, want Running", current.Status.Phase)
	}
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunSucceeded)
}

func TestSessionRuntimeProxyForwardsToNonOwnerPod(t *testing.T) {
	t.Parallel()
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

func TestSessionGatewayServesTLS(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	portForwardClient := &http.Client{Transport: &http.Transport{Proxy: nil, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // E2E local port-forward only.
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
	if !slices.Contains(leaf.DNSNames, "kruntimes-console.default.svc") {
		t.Fatalf("cert-manager Console TLS certificate DNS names = %v, want kruntimes-console.default.svc", leaf.DNSNames)
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

func TestSessionGatewaySerializesMutations(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	runtimeName := fmt.Sprintf("sdk-session-gateway-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	serviceAccount, token := newSessionGatewayServiceAccount(t)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccount.Name, Namespace: testNamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"runs"}, Verbs: []string{"create", "get", "update", "delete"}},
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
	forward, err := sandbox.StartConsolePortForward(t.Context(), sdkConfig, testNamespace, "kruntimes-console", 443)
	if err != nil {
		t.Fatalf("start SDK Runtime gateway port-forward: %v", err)
	}
	t.Cleanup(forward.Close)
	sdk, err := sandbox.NewFromRESTConfig(sdkConfig, sandbox.Config{HTTPClient: forward})
	if err != nil {
		t.Fatalf("create Sandbox SDK client: %v", err)
	}
	leaseTimeout := int32(3)
	acquired, err := sdk.Runtime(testNamespace, runtimeName).AcquireSandbox(t.Context(), sandbox.AcquireOptions{
		GenerateName: "e2e-sdk-session-",
		Session:      &v1alpha1.RunSessionMode{LeaseTimeoutSeconds: &leaseTimeout},
	})
	if err != nil {
		t.Fatalf("create SDK Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), acquired.Run()) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	connection, err := acquired.OpenSession(ctx)
	if err != nil {
		t.Fatalf("open SDK Session connection: %v", err)
	}
	operationID, err := connection.Send(ctx, sandbox.Command{Argv: []string{"sh", "-c", "printf sdk-port-forward"}})
	if err != nil {
		t.Fatalf("send SDK Session command: %v", err)
	}
	var output bytes.Buffer
	for {
		event, err := connection.Receive(ctx)
		if err != nil {
			t.Fatalf("receive SDK Session event: %v", err)
		}
		if event.Output != nil && event.Output.Stream == "stdout" {
			output.Write(event.Output.Data)
		}
		if event.Completed != nil {
			if event.Completed.Command == nil || event.Completed.Command.ExitCode != 0 {
				t.Fatalf("SDK Session completion = %#v, want successful command", event.Completed)
			}
			break
		}
		if event.Failed != nil {
			t.Fatalf("SDK Session operation %s failed: %#v", operationID, event.Failed)
		}
	}
	if output.String() != "sdk-port-forward" {
		t.Fatalf("SDK Session output = %q, want sdk-port-forward", output.String())
	}
	// The SDK heartbeat must retain the Session Run beyond its lease interval
	// without issuing an artificial operation.
	time.Sleep(4 * time.Second)
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(acquired.Run()), acquired.Run()); err != nil {
		t.Fatalf("get heartbeating SDK Session Run: %v", err)
	}
	if acquired.Run().Status.Phase != v1alpha1.RunReady {
		t.Fatalf("heartbeating SDK Session phase = %s, want Ready", acquired.Run().Status.Phase)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close SDK Session connection: %v", err)
	}
	for _, name := range []string{"alpha.txt", "beta.txt", "gamma.txt"} {
		if err := acquired.WriteFile(ctx, "pages/"+name, []byte(name), true); err != nil {
			t.Fatalf("write SDK Session page file %q: %v", name, err)
		}
	}
	page, err := acquired.ListFiles(ctx, sandbox.ListFilesOptions{Directory: "pages", Limit: 2})
	if err != nil {
		t.Fatalf("list first SDK Session file page: %v", err)
	}
	if got, want := sessionFilePaths(page.Entries), []string{"alpha.txt", "beta.txt"}; !slices.Equal(got, want) || page.NextPageToken == "" {
		t.Fatalf("first SDK Session file page = %#v, want %#v and next token", page, want)
	}
	page, err = acquired.ListFiles(ctx, sandbox.ListFilesOptions{Directory: "pages", Limit: 2, PageToken: page.NextPageToken})
	if err != nil {
		t.Fatalf("list second SDK Session file page: %v", err)
	}
	if got, want := sessionFilePaths(page.Entries), []string{"gamma.txt"}; !slices.Equal(got, want) || page.NextPageToken != "" {
		t.Fatalf("second SDK Session file page = %#v, want %#v and no next token", page, want)
	}
	if err := acquired.Release(ctx); err != nil {
		t.Fatalf("release SDK Session Run: %v", err)
	}

	cancelled, err := sdk.Runtime(testNamespace, runtimeName).AcquireSandbox(ctx, sandbox.AcquireOptions{GenerateName: "e2e-sdk-cancel-"})
	if err != nil {
		t.Fatalf("create SDK cancellation Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cancelled.Run()) })
	if err := cancelled.Cancel(ctx); err != nil {
		t.Fatalf("cancel SDK Session Run: %v", err)
	}
	if cancelled.Run().Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("SDK Cancel phase = %s, want Cancelled", cancelled.Run().Status.Phase)
	}
}

func TestSessionRunExpiresWhenIdle(t *testing.T) {
	t.Parallel()
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

func TestSessionRunLeaseExpiresAfterConnectionHeartbeatsStop(t *testing.T) {
	t.Parallel()
	runtimeName := fmt.Sprintf("session-lease-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)
	leaseTimeout := int32(3)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-lease-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{LeaseTimeoutSeconds: &leaseTimeout}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create lease Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	websocketURL := "ws" + strings.TrimPrefix(baseURL, "http") + "/operations:ws"
	connection, response, err := websocket.DefaultDialer.Dial(websocketURL, http.Header{"Authorization": []string{"Bearer " + sessionGatewayToken(t, run)}})
	if err != nil {
		if response != nil {
			t.Fatalf("dial lease WebSocket: %v (status %d)", err, response.StatusCode)
		}
		t.Fatalf("dial lease WebSocket: %v", err)
	}
	// The authenticated upgrade is an initial server-observed heartbeat. It must
	// keep the Run ready during one lease interval before the connection closes.
	time.Sleep(time.Second)
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), run); err != nil {
		t.Fatalf("get heartbeat Session Run: %v", err)
	}
	if run.Status.Phase != v1alpha1.RunReady {
		t.Fatalf("Session Run phase with active connection = %s, want Ready", run.Status.Phase)
	}
	_ = connection.Close()
	waitForRunPhase(t, run, 15*time.Second, v1alpha1.RunTimeout)
	if run.Status.Message != "session lease expired" {
		t.Fatalf("lease expiry message = %q, want session lease expired", run.Status.Message)
	}
}

func TestSessionRunExpiresWhenTotalTimeoutReached(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

	// The aggregated log API can no longer recover logs once the assigned
	// Runtime Pod is gone. The CLI must retain that actionable API Status
	// message instead of replacing it with client-go's generic 409 error.
	cmd := krt.NewRootCmd()
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{
		"logs", run.Name,
		"--namespace", testNamespace,
		"--token", aggregatedLogToken(t, run),
		"--tail", "100",
	})
	err := cmd.ExecuteContext(t.Context())
	if err == nil {
		t.Fatal("krt logs for a Run whose assigned Runtime Pod was deleted succeeded")
	}
	if !apierrors.IsConflict(err) {
		t.Fatalf("krt logs error = %v, want Conflict", err)
	}
	const podGoneMessage = "assigned Runtime Pod is no longer available; Run logs cannot be read"
	if !strings.Contains(err.Error(), podGoneMessage) {
		t.Fatalf("krt logs error = %q, want actionable message %q", err, podGoneMessage)
	}
}
