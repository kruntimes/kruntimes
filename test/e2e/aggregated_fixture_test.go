package e2e

// Aggregated Run extension API fixtures: tokens, clients, Session operations,
// Console TLS access, port-forwards, and aggregate-specific log helpers.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/kruntimes/kruntimes/api/runextensionapi"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func sessionGatewayToken(t *testing.T, run *v1alpha1.Run) string {
	t.Helper()
	return tokenWithRole(t, run, []rbacv1.PolicyRule{
		{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"runs"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
		{APIGroups: []string{runextensionapi.GroupSessions}, Resources: []string{"runs/session"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
	})
}

func sessionGatewayTokenWithoutSessionAccess(t *testing.T, run *v1alpha1.Run) string {
	t.Helper()
	return tokenWithRole(t, run, []rbacv1.PolicyRule{
		{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"runs"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
	})
}

func functionGatewayToken(t *testing.T, run *v1alpha1.Run) string {
	t.Helper()
	return tokenWithRole(t, run, []rbacv1.PolicyRule{
		{APIGroups: []string{runextensionapi.GroupFunctions}, Resources: []string{"runs/function"}, ResourceNames: []string{run.Name}, Verbs: []string{"create"}},
	})
}

func tokenWithRole(t *testing.T, run *v1alpha1.Run, rules []rbacv1.PolicyRule) string {
	t.Helper()
	serviceAccount, token := newSessionGatewayServiceAccount(t)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccount.Name, Namespace: testNamespace},
		Rules:      rules,
	}
	if err := k8sClient.Create(t.Context(), role); err != nil {
		t.Fatalf("create aggregated API test Role: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), role) })
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: testNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount.Name, Namespace: testNamespace}},
	}
	if err := k8sClient.Create(t.Context(), binding); err != nil {
		t.Fatalf("create aggregated API test RoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), binding) })
	return token
}

func aggregatedLogToken(t *testing.T, run *v1alpha1.Run) string {
	t.Helper()
	return tokenWithRole(t, run, []rbacv1.PolicyRule{
		{APIGroups: []string{runextensionapi.GroupLogs}, Resources: []string{"runs/log"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
	})
}

func sessionGatewayTokenWithoutRunAccess(t *testing.T) string {
	t.Helper()
	_, token := newSessionGatewayServiceAccount(t)
	return token
}

func newSessionGatewayServiceAccount(t *testing.T) (*corev1.ServiceAccount, string) {
	t.Helper()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "session-api-user-" + suffix, Namespace: testNamespace}}
	if err := k8sClient.Create(t.Context(), serviceAccount); err != nil {
		t.Fatalf("create aggregated API test ServiceAccount: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), serviceAccount) })

	response, err := coreClientset.CoreV1().ServiceAccounts(testNamespace).CreateToken(t.Context(), serviceAccount.Name, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create aggregated API test ServiceAccount token: %v", err)
	}
	if response.Status.Token == "" {
		t.Fatal("aggregated API test ServiceAccount token is empty")
	}
	return serviceAccount, response.Status.Token
}

// aggregatedAPIClient returns a Run extension API client authenticated as the
// caller token. An empty token requests no credential at all.
func aggregatedAPIClient(t *testing.T, token string) *runextensionapi.Client {
	t.Helper()
	config := rest.AnonymousClientConfig(restConfig)
	config.BearerToken = token
	client, err := runextensionapi.NewForConfig(config)
	if err != nil {
		t.Fatalf("create aggregated Run extension API client: %v", err)
	}
	return client
}

// invalidCredentialClient sends a bearer token the API server cannot
// authenticate and no other credential.
func invalidCredentialClient(t *testing.T) *runextensionapi.Client {
	t.Helper()
	config := rest.AnonymousClientConfig(restConfig)
	config.BearerToken = "invalid-token"
	client, err := runextensionapi.NewForConfig(config)
	if err != nil {
		t.Fatalf("create invalid-credential Run extension API client: %v", err)
	}
	return client
}

// invokeFunctionEventually retries transient Runtime Service dial failures that
// can happen while a freshly created Runtime Service becomes reachable.
func invokeFunctionEventually(t *testing.T, client *runextensionapi.Client, run *v1alpha1.Run, input []byte) *runextensionapi.FunctionInvocationResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	lastError := ""
	for {
		result, err := client.InvokeFunction(ctx, run.Namespace, run.Name, &runextensionapi.FunctionInvocationOptions{Input: input})
		if err == nil {
			return result
		}
		lastError = err.Error()
		if status := apiErrorStatus(err); status != 0 && status != 503 {
			t.Fatalf("invoke Function: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out invoking Function: %s", lastError)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

type sessionOperationResult struct {
	ExitCode int32
	Stdout   []byte
	Stderr   []byte
	TimedOut bool
}

// runSessionOperation submits one operation through the aggregated Session
// WebSocket and collects its bounded command result.
func runSessionOperation(ctx context.Context, client *runextensionapi.Client, run *v1alpha1.Run, operation runextensionapi.Operation) (sessionOperationResult, error) {
	session, err := client.OpenSession(ctx, run.Namespace, run.Name, sessionOptions(run))
	if err != nil {
		return sessionOperationResult{}, err
	}
	defer session.Close()
	operationID, err := session.Send(ctx, operation)
	if err != nil {
		return sessionOperationResult{}, err
	}
	var result sessionOperationResult
	for {
		event, err := session.Receive(ctx)
		if err != nil {
			return sessionOperationResult{}, err
		}
		if event.Output != nil {
			switch event.Output.Stream {
			case "stdout":
				result.Stdout = append(result.Stdout, event.Output.Data...)
			case "stderr":
				result.Stderr = append(result.Stderr, event.Output.Data...)
			}
		}
		if event.Failed != nil {
			return sessionOperationResult{}, fmt.Errorf("Session operation %s failed: %s", operationID, event.Failed.Message)
		}
		if event.Completed != nil {
			if command := event.Completed.Command; command != nil {
				result.ExitCode, result.TimedOut = command.ExitCode, command.TimedOut
				result.Stdout = append(result.Stdout, command.Stdout...)
				result.Stderr = append(result.Stderr, command.Stderr...)
			}
			return result, nil
		}
	}
}

func sessionOptions(run *v1alpha1.Run) *runextensionapi.SessionOptions {
	if run == nil || run.Spec.Mode.Session == nil || run.Spec.Mode.Session.LeaseTimeoutSeconds == nil || *run.Spec.Mode.Session.LeaseTimeoutSeconds <= 0 {
		return nil
	}
	return &runextensionapi.SessionOptions{HeartbeatInterval: time.Duration(*run.Spec.Mode.Session.LeaseTimeoutSeconds) * time.Second / 3}
}

// waitForSessionFile retries a Session file read while a running command writes
// it.
func waitForSessionFile(t *testing.T, client *runextensionapi.Client, run *v1alpha1.Run, path string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	lastError := ""
	for {
		contents, _, err := client.ReadSessionFile(ctx, run.Namespace, run.Name, path, 0)
		if err == nil {
			return contents
		}
		lastError = err.Error()
		select {
		case <-ctx.Done():
			t.Fatalf("timed out reading Session file %q: %s", path, lastError)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func apiErrorStatus(err error) int {
	apiError, ok := err.(*runextensionapi.APIError)
	if !ok {
		return 0
	}
	return apiError.StatusCode
}

func waitForConsolePod(t *testing.T) *corev1.Pod {
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
			t.Fatal("timed out waiting for a ready Console Pod")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	return podIsReady(pod)
}

// consoleHTTPSURL port-forwards one Console Pod HTTPS port and returns its
// local base URL.
func consoleHTTPSURL(t *testing.T, pod *corev1.Pod) string {
	t.Helper()
	localPort := availableLocalPort(t)
	closer, err := forwardPodPort(t.Context(), pod.Namespace, pod.Name, localPort, 8443)
	if err != nil {
		t.Fatalf("port-forward Console HTTPS: %v", err)
	}
	t.Cleanup(func() { _ = closer.Close() })
	return fmt.Sprintf("https://127.0.0.1:%d", localPort)
}

func consoleTLSClient(t *testing.T, caBundle []byte) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caBundle) {
		t.Fatal("Console TLS Secret has no parseable CA bundle")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs:    pool,
		ServerName: fmt.Sprintf("kruntimes-console.%s.svc", testNamespace),
	}}}
}

func consoleSecretCA(t *testing.T, secretName string) []byte {
	t.Helper()
	secret, err := coreClientset.CoreV1().Secrets(testNamespace).Get(t.Context(), secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("read Console TLS Secret %q: %v", secretName, err)
	}
	ca := secret.Data["ca.crt"]
	if len(ca) == 0 {
		t.Fatalf("Console TLS Secret %q has no ca.crt", secretName)
	}
	return ca
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

func sessionFilePaths(entries []runextensionapi.SessionFileInfo) []string {
	paths := make([]string, len(entries))
	for i := range entries {
		paths[i] = entries[i].Path
	}
	return paths
}

func containsSessionFile(entries []runextensionapi.SessionFileInfo, path string) bool {
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

func assertAggregatedRunLogFollow(t *testing.T, logs *runextensionapi.Client, run *v1alpha1.Run, marker string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	stream, err := logs.GetRunLogs(run.Namespace, run.Name, &runextensionapi.RunLogOptions{TailLines: 100, Follow: true}).Stream(ctx)
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
