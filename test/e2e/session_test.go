package e2e

// Session mode scenarios: aggregated API authorization, streaming, TLS, and run exhaustion.

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

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

	"github.com/kruntimes/kruntimes/api/runextensionapi"
	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/krt"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	"github.com/kruntimes/kruntimes/internal/runstatus"
	sandboxsdk "github.com/kruntimes/kruntimes/sdk/go/sandbox"
)

func TestAggregatedSessionAPIAuthorizesOperations(t *testing.T) {
	t.Parallel()
	runtimeName := fmt.Sprintf("session-api-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-api-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
			Env:     []corev1.EnvVar{{Name: "KRUNTIMES_SESSION_DEFAULT", Value: "registration"}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)
	token := sessionGatewayToken(t, run)

	// kube-apiserver authenticates and authorizes every Session operation.
	if _, err := invalidCredentialClient(t).Sessions().NewExecutor(t.Context(), run.Namespace, run.Name, nil); apiErrorStatus(err) != http.StatusUnauthorized {
		t.Fatalf("unauthenticated Session open status = %d, want 401 (%v)", apiErrorStatus(err), err)
	}
	if _, err := aggregatedAPIClient(t, sessionGatewayTokenWithoutRunAccess(t)).Sessions().NewExecutor(t.Context(), run.Namespace, run.Name, nil); apiErrorStatus(err) != http.StatusForbidden {
		t.Fatalf("unauthorized Session open status = %d, want 403 (%v)", apiErrorStatus(err), err)
	}
	if _, err := aggregatedAPIClient(t, sessionGatewayTokenWithoutSessionAccess(t, run)).Sessions().NewExecutor(t.Context(), run.Namespace, run.Name, nil); apiErrorStatus(err) != http.StatusForbidden {
		t.Fatalf("Session open without runs/session status = %d, want 403 (%v)", apiErrorStatus(err), err)
	}

	client := aggregatedAPIClient(t, token)
	operation, err := runSessionOperation(t.Context(), client, run, runextensionapi.Operation{Command: &runextensionapi.Command{Shell: "printf api-ok"}})
	if err != nil || operation.ExitCode != 0 || string(operation.Stdout) != "api-ok" {
		t.Fatalf("Session command result = %#v, %v; want successful api-ok output", operation, err)
	}
	waitForSessionCommandLogs(t, run, "api-ok")

	operation, err = runSessionOperation(t.Context(), client, run, runextensionapi.Operation{Command: &runextensionapi.Command{
		Shell: `printf '%s:%s' "$KRUNTIMES_SESSION_DEFAULT" "$KRUNTIMES_SESSION_COMMAND"`,
		Env:   map[string]string{"KRUNTIMES_SESSION_COMMAND": "command"},
	}})
	if err != nil || operation.ExitCode != 0 || string(operation.Stdout) != "registration:command" {
		t.Fatalf("Session command environment result = %#v, %v; want registration:command", operation, err)
	}

	if _, err := runSessionOperation(t.Context(), client, run, runextensionapi.Operation{WriteFile: &runextensionapi.FileWrite{Path: "notes/result.txt", Contents: []byte("api-file"), CreateParents: true}}); err != nil {
		t.Fatalf("write Session file: %v", err)
	}
	if _, err := runSessionOperation(t.Context(), client, run, runextensionapi.Operation{WriteFile: &runextensionapi.FileWrite{Path: "../outside.txt", Contents: []byte("escape"), CreateParents: true}}); err == nil {
		t.Fatal("escaping Session write succeeded")
	}

	page, err := client.Sessions().ListFiles(t.Context(), run.Namespace, run.Name, &runextensionapi.ListSessionFilesOptions{Directory: "notes"})
	if err != nil {
		t.Fatalf("list Session files: %v", err)
	}
	if !containsSessionFile(page.Entries, "result.txt") {
		t.Fatalf("Session files = %#v, want result.txt", page.Entries)
	}
	contents, _, err := client.Sessions().ReadFile(t.Context(), run.Namespace, run.Name, "notes/result.txt", 0)
	if err != nil || string(contents) != "api-file" {
		t.Fatalf("Session file contents = %q, %v; want api-file", contents, err)
	}

	// A Run that has no capacity yet is authorized but cannot serve a Session.
	pending := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-api-pending-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), pending); err != nil {
		t.Fatalf("create pending Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pending) })
	if _, err := aggregatedAPIClient(t, sessionGatewayToken(t, pending)).Sessions().NewExecutor(t.Context(), pending.Namespace, pending.Name, nil); apiErrorStatus(err) != http.StatusConflict {
		t.Fatalf("pending Session open status = %d, want 409 (%v)", apiErrorStatus(err), err)
	}

	requestRunCancel(t, run)
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunCancelled)
	assertCancelledRun(t, run)
	if _, err := client.Sessions().NewExecutor(t.Context(), run.Namespace, run.Name, nil); apiErrorStatus(err) != http.StatusConflict {
		t.Fatalf("cancelled Session open status = %d, want 409 (%v)", apiErrorStatus(err), err)
	}
}

func TestKRTSessionOperations(t *testing.T) {
	t.Parallel()
	runtimeName := fmt.Sprintf("krt-session-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-krt-session-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)
	if run.Status.Mode != "session" {
		t.Fatalf("Run status mode = %q, want session", run.Status.Mode)
	}
	token := krtSessionToken(t, run)

	execute := func(input string, args ...string) (string, string, error) {
		cmd := krt.NewRootCmd()
		cmd.SilenceUsage = true
		var stdout, stderr bytes.Buffer
		cmd.SetIn(strings.NewReader(input))
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs(args)
		err := cmd.ExecuteContext(t.Context())
		return stdout.String(), stderr.String(), err
	}
	stdout, stderr, err := execute("printf cert-auth-ok\n", "session", "attach", run.Name, "--namespace", testNamespace)
	if err != nil || stdout != "cert-auth-ok" || stderr != "" {
		t.Fatalf("krt session attach through aggregated API with kubeconfig client certificate = stdout %q, stderr %q, error %v", stdout, stderr, err)
	}
	stdout, stderr, err = execute("", "session", "exec", run.Name, "--namespace", testNamespace, "--token", token, "--", "sh", "-c", "printf cli-ok")
	if err != nil || stdout != "cli-ok" || stderr != "" {
		t.Fatalf("krt session exec = stdout %q, stderr %q, error %v", stdout, stderr, err)
	}
	stdout, stderr, err = execute("printf attach-ok\n", "session", "attach", run.Name, "--namespace", testNamespace, "--token", token)
	if err != nil || stdout != "attach-ok" || stderr != "" {
		t.Fatalf("krt session attach = stdout %q, stderr %q, error %v", stdout, stderr, err)
	}
	_, _, err = execute("", "session", "exec", run.Name, "--namespace", testNamespace,
		"--token", sessionGatewayTokenWithoutSessionAccess(t, run), "--", "sh", "-c", "printf forbidden")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("krt session exec without runs/session permission = %v, want HTTP 403", err)
	}
	_, _, err = execute("", "session", "exec", run.Name, "--namespace", testNamespace, "--token", token, "--", "sh", "-c", "exit 7")
	if err == nil || !strings.Contains(err.Error(), "status 7") {
		t.Fatalf("krt session command exit = %v, want status 7", err)
	}
	stdout, _, err = execute("", "list", "--namespace", testNamespace, "--runtime", runtimeName)
	if err != nil || !strings.Contains(stdout, "MODE") || !strings.Contains(stdout, "session") {
		t.Fatalf("krt list = %q, %v", stdout, err)
	}
	kubectl := exec.CommandContext(t.Context(), "kubectl", "get", "run", run.Name, "-n", testNamespace)
	output, err := kubectl.CombinedOutput()
	if err != nil || !strings.Contains(string(output), "MODE") || !strings.Contains(string(output), "session") {
		t.Fatalf("kubectl get run = %q, %v", output, err)
	}
}

func krtSessionToken(t *testing.T, run *v1alpha1.Run) string {
	t.Helper()
	serviceAccount, token := newSessionGatewayServiceAccount(t)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccount.Name, Namespace: testNamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"runs"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
			{APIGroups: []string{runextensionapi.GroupSessions}, Resources: []string{"runs/session"}, ResourceNames: []string{run.Name}, Verbs: []string{"get"}},
		},
	}
	if err := k8sClient.Create(t.Context(), role); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), role) })
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: testNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount.Name, Namespace: testNamespace}},
	}
	if err := k8sClient.Create(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), binding) })
	return token
}

func TestAggregatedSessionAPIStreamsCommandOutput(t *testing.T) {
	t.Parallel()
	runtimeName := fmt.Sprintf("session-stream-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-stream-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	client := aggregatedAPIClient(t, sessionGatewayToken(t, run))
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	session, err := client.Sessions().NewExecutor(ctx, run.Namespace, run.Name, sessionOptions(run))
	if err != nil {
		t.Fatalf("open Session connection: %v", err)
	}
	defer session.Close()
	if _, err := session.Send(ctx, runextensionapi.Operation{Command: &runextensionapi.Command{Shell: "printf stream-first; sleep 3; printf stream-last"}}); err != nil {
		t.Fatalf("send streaming command: %v", err)
	}
	var stdout strings.Builder
	var firstOutputAt, completedAt time.Time
	lastSequence := int64(1) // Send consumes the accepted event with sequence 1.
	for event := range session.Receive() {
		if event.Sequence != lastSequence+1 {
			t.Fatalf("event sequence = %d after %d", event.Sequence, lastSequence)
		}
		lastSequence = event.Sequence
		if event.Output != nil && event.Output.Stream == "stdout" {
			if firstOutputAt.IsZero() {
				firstOutputAt = time.Now()
			}
			stdout.Write(event.Output.Data)
		}
		if event.Failed != nil {
			t.Fatalf("streaming command failed: %s", event.Failed.Message)
		}
		if event.Completed != nil {
			completedAt = time.Now()
			if event.Completed.Command == nil || event.Completed.Command.ExitCode != 0 {
				t.Fatalf("streaming completion = %#v", event.Completed)
			}
			if firstOutputAt.IsZero() || completedAt.Sub(firstOutputAt) < time.Second {
				t.Fatal("command output was not streamed before completion")
			}
			if got := stdout.String(); got != "stream-firststream-last" {
				t.Fatalf("streaming output = %q", got)
			}
			return
		}
	}
	if err := session.Err(); err != nil {
		t.Fatalf("receive streaming event: %v", err)
	}
	t.Fatal("Session stream closed before completion")
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

	client := aggregatedAPIClient(t, sessionGatewayToken(t, run))
	marker := "run-log-e2e"
	if _, err := runSessionOperation(t.Context(), client, run, runextensionapi.Operation{Command: &runextensionapi.Command{Shell: "printf run-log-e2e"}}); err != nil {
		t.Fatalf("write Session log marker: %v", err)
	}
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
	noisyClient := aggregatedAPIClient(t, sessionGatewayToken(t, noisyRun))
	for range 55 {
		if _, err := runSessionOperation(t.Context(), noisyClient, noisyRun, runextensionapi.Operation{Command: &runextensionapi.Command{Shell: "printf run-log-noise"}}); err != nil {
			t.Fatalf("write noisy Session log: %v", err)
		}
	}
	waitForSessionCommandLogs(t, noisyRun, "run-log-noise")

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
	logs, err := runextensionapi.NewForConfig(logConfig)
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

func TestConsoleServesTLS(t *testing.T) {
	t.Parallel()
	pod := waitForConsolePod(t)
	httpClient := consoleTLSClient(t, consoleSecretCA(t, "kruntimes-console-tls"))
	response, err := httpClient.Get(consoleHTTPSURL(t, pod) + "/healthz")
	if err != nil {
		t.Fatalf("GET Console healthz over TLS: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Console healthz status = %d, want 200", response.StatusCode)
	}
}

func TestConsoleServesCertManagerTLS(t *testing.T) {
	if os.Getenv(certManagerE2EEnabledEnv) != "true" {
		t.Skipf("set %s=true to run the cert-manager E2E", certManagerE2EEnabledEnv)
	}

	certificate := &unstructured.Unstructured{}
	certificate.SetGroupVersionKind(schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"})
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: certManagerGatewayCertificate}, certificate); err != nil {
		t.Fatalf("get cert-manager Console Certificate: %v", err)
	}
	if !certificateReady(certificate) {
		t.Fatalf("cert-manager Console Certificate status = %#v, want Ready=True", certificate.Object["status"])
	}

	secret := &corev1.Secret{}
	if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: testNamespace, Name: certManagerGatewayTLSSecret}, secret); err != nil {
		t.Fatalf("get cert-manager Console TLS Secret: %v", err)
	}
	if len(secret.Data["ca.crt"]) == 0 || len(secret.Data["tls.crt"]) == 0 || len(secret.Data["tls.key"]) == 0 {
		t.Fatalf("cert-manager Console TLS Secret keys = %v, want ca.crt, tls.crt, and tls.key", secret.Data)
	}
	certificatePEM, _ := pem.Decode(secret.Data["tls.crt"])
	if certificatePEM == nil {
		t.Fatal("decode cert-manager Console TLS certificate PEM")
	}
	leaf, err := x509.ParseCertificate(certificatePEM.Bytes)
	if err != nil {
		t.Fatalf("parse cert-manager Console TLS certificate: %v", err)
	}
	if !slices.Contains(leaf.DNSNames, "kruntimes-console.default.svc") {
		t.Fatalf("cert-manager Console TLS certificate DNS names = %v, want kruntimes-console.default.svc", leaf.DNSNames)
	}

	pod := waitForConsolePod(t)
	httpClient := consoleTLSClient(t, secret.Data["ca.crt"])
	response, err := httpClient.Get(consoleHTTPSURL(t, pod) + "/healthz")
	if err != nil {
		t.Fatalf("GET Console healthz over cert-manager TLS: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Console cert-manager healthz status = %d, want 200", response.StatusCode)
	}
}

func TestAggregatedSessionRunCancellationTerminatesActiveCommand(t *testing.T) {
	runtimeName := fmt.Sprintf("session-cancel-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-cancel-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create cancellation Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	client := aggregatedAPIClient(t, sessionGatewayToken(t, run))
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	session, err := client.Sessions().NewExecutor(ctx, run.Namespace, run.Name, sessionOptions(run))
	if err != nil {
		t.Fatalf("open Session connection: %v", err)
	}
	defer session.Close()
	commandErr := make(chan error, 1)
	go func() {
		if _, err := session.Send(ctx, runextensionapi.Operation{Command: &runextensionapi.Command{Shell: "printf started > started; sleep 20; printf completed > completed"}}); err != nil {
			commandErr <- err
			return
		}
		for event := range session.Receive() {
			if event.Failed != nil {
				commandErr <- fmt.Errorf("Session operation failed: %s", event.Failed.Message)
				return
			}
			if event.Completed != nil {
				commandErr <- nil
				return
			}
		}
		commandErr <- session.Err()
	}()

	waitForSessionCommandFile(t, client, run, "started", commandErr)
	requestRunCancel(t, run)
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunCancelled)
	assertCancelledRun(t, run)
	if err := <-commandErr; err == nil {
		t.Fatal("active Session command succeeded after its Run was cancelled")
	}
	if _, err := client.Sessions().NewExecutor(t.Context(), run.Namespace, run.Name, nil); apiErrorStatus(err) != http.StatusConflict {
		t.Fatalf("cancelled Session open status = %d, want 409 (%v)", apiErrorStatus(err), err)
	}
}

func TestAggregatedSessionRunDrainCompletesAcceptedCommand(t *testing.T) {
	runtimeName := fmt.Sprintf("session-drain-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-drain-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: runtimeName, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create draining Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	client := aggregatedAPIClient(t, sessionGatewayToken(t, run))
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	session, err := client.Sessions().NewExecutor(ctx, run.Namespace, run.Name, sessionOptions(run))
	if err != nil {
		t.Fatalf("open Session connection: %v", err)
	}
	defer session.Close()
	commandErr := make(chan error, 1)
	go func() {
		if _, err := session.Send(ctx, runextensionapi.Operation{Command: &runextensionapi.Command{Shell: "printf started > started; sleep 10; printf completed > completed"}}); err != nil {
			commandErr <- err
			return
		}
		for event := range session.Receive() {
			if event.Failed != nil {
				commandErr <- fmt.Errorf("Session operation failed: %s", event.Failed.Message)
				return
			}
			if event.Completed != nil {
				commandErr <- nil
				return
			}
		}
		commandErr <- session.Err()
	}()

	// A successful read proves runtimed accepted the command before Drain
	// fenced later operations.
	waitForSessionFile(t, client, run, "started")
	requestRunDrain(t, run)
	waitForRunPhase(t, run, 10*time.Second, v1alpha1.RunFinalizing)
	if _, err := client.Sessions().NewExecutor(t.Context(), run.Namespace, run.Name, nil); err == nil {
		t.Fatal("Session connection opened after Drain")
	}
	if err := <-commandErr; err != nil {
		t.Fatalf("accepted Session command did not complete during Drain: %v", err)
	}
	waitForRunPhase(t, run, 20*time.Second, v1alpha1.RunSucceeded)
}

func TestSandboxSDKUsesAggregatedSessionAPI(t *testing.T) {
	t.Parallel()
	runtimeName := fmt.Sprintf("sdk-session-api-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)

	serviceAccount, token := newSessionGatewayServiceAccount(t)
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccount.Name, Namespace: testNamespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{v1alpha1.GroupVersion.Group}, Resources: []string{"runs"}, Verbs: []string{"create", "get", "update", "delete"}},
			{APIGroups: []string{runextensionapi.GroupSessions}, Resources: []string{"runs/session"}, Verbs: []string{"get"}},
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
	sdk, err := sandboxsdk.NewFromRESTConfig(sdkConfig, sandboxsdk.Config{})
	if err != nil {
		t.Fatalf("create Sandbox SDK client: %v", err)
	}
	// Keep the lease longer than a loaded kind cluster's Runtime Service dial
	// window; the dedicated lease test owns expiry coverage.
	leaseTimeout := int32(60)
	sandbox, err := sdk.Runtime(testNamespace, runtimeName).AcquireSandbox(t.Context(), sandboxsdk.AcquireOptions{
		GenerateName: "e2e-sdk-session-",
		Session:      &v1alpha1.RunSessionMode{LeaseTimeoutSeconds: &leaseTimeout},
	})
	if err != nil {
		t.Fatalf("create SDK Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sandbox.Run()) })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	connection, err := sandbox.OpenSession(ctx)
	if err != nil {
		t.Fatalf("open SDK Session connection: %v", err)
	}
	operationID, err := connection.Send(ctx, sandboxsdk.Operation{Command: &sandboxsdk.Command{Argv: []string{"sh", "-c", "printf sdk-aggregated"}}})
	if err != nil {
		t.Fatalf("send SDK Session command: %v", err)
	}
	var output bytes.Buffer
	for event := range connection.Receive() {
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
	if err := connection.Err(); err != nil {
		t.Fatalf("receive SDK Session event: %v", err)
	}
	if output.String() != "sdk-aggregated" {
		t.Fatalf("SDK Session output = %q, want sdk-aggregated", output.String())
	}
	// The SDK heartbeat must retain the Session Run beyond its lease interval
	// without issuing an artificial operation.
	time.Sleep(16 * time.Second)
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(sandbox.Run()), sandbox.Run()); err != nil {
		t.Fatalf("get heartbeating SDK Session Run: %v", err)
	}
	if sandbox.Run().Status.Phase != v1alpha1.RunReady {
		t.Fatalf("heartbeating SDK Session phase = %s, want Ready", sandbox.Run().Status.Phase)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("close SDK Session connection: %v", err)
	}
	postReconnectCtx, postReconnectCancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer postReconnectCancel()
	connection, err = sandbox.OpenSession(postReconnectCtx)
	if err != nil {
		t.Fatalf("reopen SDK Session connection: %v", err)
	}
	for _, name := range []string{"alpha.txt", "beta.txt", "gamma.txt"} {
		operationCtx, operationCancel := context.WithTimeout(t.Context(), 30*time.Second)
		started := time.Now()
		if _, err := connection.Send(operationCtx, sandboxsdk.Operation{WriteFile: &sandboxsdk.FileWrite{Path: "pages/" + name, Contents: []byte(name), CreateParents: true}}); err != nil {
			operationCancel()
			t.Fatalf("send SDK Session file operation %q: %v", name, err)
		}
		for event := range connection.Receive() {
			if event.Completed != nil {
				break
			}
		}
		if err := connection.Err(); err != nil {
			operationCancel()
			t.Fatalf("receive SDK Session file operation %q: %v", name, err)
		}
		operationCancel()
		t.Logf("SDK Session file operation %q completed in %s", name, time.Since(started))
	}
	defer connection.Close()
	page, err := sandbox.ListFiles(postReconnectCtx, sandboxsdk.ListFilesOptions{Directory: "pages", Limit: 2})
	if err != nil {
		t.Fatalf("list first SDK Session file page: %v", err)
	}
	if got, want := sessionFilePaths(page.Entries), []string{"alpha.txt", "beta.txt"}; !slices.Equal(got, want) || page.NextPageToken == "" {
		t.Fatalf("first SDK Session file page = %#v, want %#v and next token", page, want)
	}
	page, err = sandbox.ListFiles(postReconnectCtx, sandboxsdk.ListFilesOptions{Directory: "pages", Limit: 2, PageToken: page.NextPageToken})
	if err != nil {
		t.Fatalf("list second SDK Session file page: %v", err)
	}
	if got, want := sessionFilePaths(page.Entries), []string{"gamma.txt"}; !slices.Equal(got, want) || page.NextPageToken != "" {
		t.Fatalf("second SDK Session file page = %#v, want %#v and no next token", page, want)
	}
	if err := sandbox.Release(postReconnectCtx); err != nil {
		t.Fatalf("release SDK Session Run: %v", err)
	}

	cancelCtx, cancelCancellation := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelCancellation()
	cancelled, err := sdk.Runtime(testNamespace, runtimeName).AcquireSandbox(cancelCtx, sandboxsdk.AcquireOptions{GenerateName: "e2e-sdk-cancel-"})
	if err != nil {
		t.Fatalf("create SDK cancellation Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), cancelled.Run()) })
	if err := cancelled.Cancel(cancelCtx); err != nil {
		t.Fatalf("cancel SDK Session Run: %v", err)
	}
	if cancelled.Run().Status.Phase != v1alpha1.RunCancelled {
		t.Fatalf("SDK Cancel phase = %s, want Cancelled", cancelled.Run().Status.Phase)
	}
}

func TestSessionRunLeaseExpiresAfterConnectionHeartbeatsStop(t *testing.T) {
	t.Parallel()
	runtimeName := fmt.Sprintf("session-lease-%d", time.Now().UnixNano())
	ensureRuntimeWithRunsCapacity(t, runtimeName, bashRuntimeImage(), 9091, 1)
	leaseTimeout := int32(15)
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

	apiClient := aggregatedAPIClient(t, sessionGatewayToken(t, run))
	// The client maintains the lease with private heartbeat frames while the
	// connection is open.
	session, err := apiClient.Sessions().NewExecutor(t.Context(), run.Namespace, run.Name, &runextensionapi.SessionOptions{HeartbeatInterval: 4 * time.Second})
	if err != nil {
		t.Fatalf("open lease Session connection: %v", err)
	}
	// Observe Ready beyond the original lease window. A single early check
	// would pass even if every heartbeat were ignored by the owner runtimed.
	for range 5 {
		time.Sleep(4 * time.Second)
		if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get heartbeat Session Run: %v", err)
		}
		if run.Status.Phase != v1alpha1.RunReady {
			t.Fatalf("Session Run phase with active connection = %s, want Ready", run.Status.Phase)
		}
	}
	_ = session.Close()
	waitForRunPhase(t, run, 25*time.Second, v1alpha1.RunTimeout)
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
