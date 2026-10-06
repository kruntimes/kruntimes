package e2e

// Console gateway and log API fixtures: tokens, port-forwards, HTTP requests, and service account setup.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/logapi"
	"github.com/kruntimes/kruntimes/sdk/go/sandbox"
)

func sessionGatewayToken(t *testing.T, run *v1alpha1.Run) string {
	t.Helper()
	ctx := context.Background()
	serviceAccount, token := newSessionGatewayServiceAccount(t)

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccount.Name, Namespace: testNamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"runs"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
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

func aggregatedLogToken(t *testing.T, run *v1alpha1.Run) string {
	t.Helper()
	ctx := context.Background()
	serviceAccount, token := newSessionGatewayServiceAccount(t)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccount.Name, Namespace: testNamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"logs.kruntimes.io"}, Resources: []string{"runs/log"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
		},
	}
	if err := k8sClient.Create(ctx, role); err != nil {
		t.Fatalf("create aggregated log API test Role: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, role) })
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: testNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount.Name, Namespace: testNamespace}},
	}
	if err := k8sClient.Create(ctx, binding); err != nil {
		t.Fatalf("create aggregated log API test RoleBinding: %v", err)
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
		pods, err := coreClientset.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=console"})
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
	closer, err := forwardPodPort(t.Context(), pod.Namespace, pod.Name, localPort, 8443)
	if err != nil {
		t.Fatalf("port-forward Runtime gateway: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	return fmt.Sprintf("https://127.0.0.1:%d%s", localPort, parsed.EscapedPath())
}

func gatewayTLSEndpointURL(t *testing.T, pod *corev1.Pod, endpoint string) string {
	t.Helper()
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Path == "" {
		t.Fatalf("parse HTTPS gateway endpoint %q: %v", endpoint, err)
	}
	localPort := availableLocalPort(t)
	closer, err := forwardPodPort(t.Context(), pod.Namespace, pod.Name, localPort, 8443)
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
		ServerName: fmt.Sprintf("kruntimes-console.%s.svc", namespace),
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
	return waitForGatewayResponseWithClient(t, gatewayInsecureHTTPClient, method, requestURL, token, body, expectedStatus)
}

func waitForGatewayResponseWithClient(t *testing.T, httpClient *http.Client, method, requestURL, token string, body []byte, expectedStatus int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	lastResult := ""
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
	response, err := gatewayInsecureHTTPClient.Do(request)
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

func sessionFilePaths(entries []sandbox.FileInfo) []string {
	paths := make([]string, len(entries))
	for i := range entries {
		paths[i] = entries[i].Path
	}
	return paths
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

func assertAggregatedRunLogFollow(t *testing.T, logs logapi.Client, run *v1alpha1.Run, marker string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	stream, err := logs.GetLogs(run.Namespace, run.Name, &logapi.RunLogOptions{TailLines: 100, Follow: true}).Stream(ctx)
	if err != nil {
		t.Fatalf("open aggregated Run log follow stream: %v", err)
	}
	defer stream.Close()
	decoder := json.NewDecoder(stream)
	for {
		var entry struct {
			Stream  string `json:"stream"`
			Message string `json:"message"`
		}
		if err := decoder.Decode(&entry); err != nil {
			t.Fatalf("decode aggregated Run log record: %v", err)
		}
		if entry.Stream == "stdout" && entry.Message == marker {
			return
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
