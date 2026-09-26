package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestGatewayGetsSessionThroughRuntimeService(t *testing.T) {
	run := readySessionRun()
	client := &fakeSessionRuntimeClient{status: func(_ context.Context, request *pb.GetSessionStatusRequest, _ ...grpc.CallOption) (*pb.SessionStatus, error) {
		if request.GetIdentity().GetRunUid() != string(run.UID) || request.GetIdentity().GetAssignedPodUid() != "pod-uid" {
			t.Fatalf("identity = %#v", request.GetIdentity())
		}
		return &pb.SessionStatus{State: pb.SessionState_SESSION_STATE_READY}, nil
	}}
	dialer := &fakeDialer{client: client}
	server := testServer(t, run, allowAuthorizer{}, dialer)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/namespaces/default/runtimes/bash/sessions/session-uid", nil)
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if dialer.address != "runtime-bash.default:9093" {
		t.Fatalf("Runtime Service address = %q", dialer.address)
	}
	if !strings.Contains(response.Body.String(), `"state":"SESSION_STATE_READY"`) {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestGatewayRejectsUnauthorizedRequestBeforeDialingRuntime(t *testing.T) {
	dialer := &fakeDialer{client: &fakeSessionRuntimeClient{}}
	server := testServer(t, readySessionRun(), denyAuthorizer{}, dialer)

	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/namespaces/default/runtimes/bash/sessions/session-uid", nil))

	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if dialer.address != "" {
		t.Fatalf("dialed Runtime Service %q for denied request", dialer.address)
	}
}

func TestGatewayExecutesExactlyOneOperation(t *testing.T) {
	run := readySessionRun()
	client := &fakeSessionRuntimeClient{execute: func(_ context.Context, request *pb.ExecuteSessionOperationRequest, _ ...grpc.CallOption) (*pb.ExecuteSessionOperationResponse, error) {
		if got := request.GetCommand().GetArgv(); len(got) != 2 || got[0] != "echo" || got[1] != "hello" {
			t.Fatalf("command = %#v", request.GetCommand())
		}
		return &pb.ExecuteSessionOperationResponse{Command: &pb.SessionCommandResult{ExitCode: 0, Stdout: []byte("hello\n")}}, nil
	}}
	server := testServer(t, run, allowAuthorizer{}, &fakeDialer{client: client})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/namespaces/default/runtimes/bash/sessions/session-uid/operations:execute", strings.NewReader(`{"command":{"argv":["echo","hello"]}}`))
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"stdout":"aGVsbG8K"`) {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestGatewayStreamsSessionOperationAsNDJSON(t *testing.T) {
	run := readySessionRun()
	client := &fakeSessionRuntimeClient{stream: func(_ context.Context, request *pb.ExecuteSessionOperationRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[pb.SessionOperationEvent], error) {
		if request.GetIdentity().GetRunUid() != string(run.UID) {
			t.Fatalf("identity = %#v", request.GetIdentity())
		}
		return &fakeSessionOperationStream{events: []*pb.SessionOperationEvent{
			{Sequence: 1, Event: &pb.SessionOperationEvent_Accepted{Accepted: &pb.SessionOperationAccepted{}}},
			{Sequence: 2, Event: &pb.SessionOperationEvent_Progress{Progress: &pb.SessionOperationProgress{Kind: pb.SessionOperationProgressKind_SESSION_OPERATION_PROGRESS_KIND_TEXT_DELTA, Message: "working"}}},
			{Sequence: 3, Event: &pb.SessionOperationEvent_Completed{Completed: &pb.ExecuteSessionOperationResponse{Command: &pb.SessionCommandResult{ExitCode: 0, Stdout: []byte("done\n")}}}},
		}}, nil
	}}
	server := testServer(t, run, allowAuthorizer{}, &fakeDialer{client: client})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/namespaces/default/runtimes/bash/sessions/session-uid/operations:stream", strings.NewReader(`{"command":{"argv":["echo","done"]}}`))
	server.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Type"); got != "application/x-ndjson; charset=utf-8" {
		t.Fatalf("content type = %q", got)
	}
	lines := strings.Split(strings.TrimSpace(response.Body.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %#v", lines)
	}
	if !strings.Contains(lines[0], `"sequence":1`) || !strings.Contains(lines[0], `"type":"accepted"`) {
		t.Fatalf("accepted event = %s", lines[0])
	}
	if !strings.Contains(lines[1], `"type":"progress"`) || !strings.Contains(lines[1], `"message":"working"`) {
		t.Fatalf("progress event = %s", lines[1])
	}
	if !strings.Contains(lines[2], `"type":"completed"`) || !strings.Contains(lines[2], `"stdout":"ZG9uZQo="`) {
		t.Fatalf("completed event = %s", lines[2])
	}
}

func TestGatewayInvokesFunctionThroughRuntimedProxy(t *testing.T) {
	run := readyFunctionRun()
	functionClient := &fakeFunctionRuntimeClient{invoke: func(_ context.Context, request *pb.InvokeFunctionRequest, _ ...grpc.CallOption) (*pb.InvokeFunctionResponse, error) {
		if request.GetRegistration().GetRunUid() != string(run.UID) || request.GetRegistration().GetRegistrationId() != "" {
			t.Fatalf("registration = %#v", request.GetRegistration())
		}
		if string(request.GetInput()) != `{"value":"hello"}` || request.GetInvocationId() != "caller-id" {
			t.Fatalf("request = %#v", request)
		}
		return &pb.InvokeFunctionResponse{InvocationId: "caller-id", Output: []byte(`{"ok":true}`), ContentType: "application/json"}, nil
	}}
	dialer := &fakeDialer{client: &fakeSessionRuntimeClient{}, functionClient: functionClient}
	server := testServer(t, run, allowAuthorizer{}, dialer)
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/namespaces/default/runtimes/bash/functions/function-uid:invoke", strings.NewReader(`{"value":"hello"}`)).WithContext(t.Context())
	request.Header.Set("X-Kruntime-Invocation-ID", "caller-id")
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"invocationId":"caller-id"`) {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestGatewayRejectsOversizedFunctionInputBeforeDialingRuntime(t *testing.T) {
	dialer := &fakeDialer{client: &fakeSessionRuntimeClient{}, functionClient: &fakeFunctionRuntimeClient{}}
	server := testServer(t, readyFunctionRun(), allowAuthorizer{}, dialer)
	server.MaxRequestBodyBytes = 8

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/namespaces/default/runtimes/bash/functions/function-uid:invoke", strings.NewReader(`{"value":"too large"}`))
	server.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if dialer.address != "" {
		t.Fatalf("dialed Runtime Service %q for rejected request", dialer.address)
	}
}

func TestGatewayRejectsRequestBodyOverConfiguredLimitBeforeDialingRuntime(t *testing.T) {
	dialer := &fakeDialer{client: &fakeSessionRuntimeClient{}}
	server := testServer(t, readySessionRun(), allowAuthorizer{}, dialer)
	server.MaxRequestBodyBytes = 64
	payload := `{"command":{"argv":["echo","` + strings.Repeat("x", 80) + `"]}}`

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/namespaces/default/runtimes/bash/sessions/session-uid/operations:execute", strings.NewReader(payload))
	server.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if dialer.address != "" {
		t.Fatalf("dialed Runtime Service %q for oversized request", dialer.address)
	}
}

func TestGatewayRejectsResponseOverConfiguredLimitWithoutPartialJSON(t *testing.T) {
	client := &fakeSessionRuntimeClient{execute: func(_ context.Context, _ *pb.ExecuteSessionOperationRequest, _ ...grpc.CallOption) (*pb.ExecuteSessionOperationResponse, error) {
		return &pb.ExecuteSessionOperationResponse{Command: &pb.SessionCommandResult{ExitCode: 0, Stdout: []byte(strings.Repeat("x", 128))}}, nil
	}}
	server := testServer(t, readySessionRun(), allowAuthorizer{}, &fakeDialer{client: client})
	server.MaxResponseBodyBytes = 64

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/namespaces/default/runtimes/bash/sessions/session-uid/operations:execute", strings.NewReader(`{"command":{"argv":["true"]}}`))
	server.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got, want := response.Body.String(), "{\"error\":\"gateway response exceeds configured limit\"}\n"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}
	if strings.Contains(response.Body.String(), "stdout") {
		t.Fatalf("response contains partial successful JSON: %s", response.Body.String())
	}
}

func TestGatewayListsSessionFilesInPages(t *testing.T) {
	run := readySessionRun()
	client := &fakeSessionRuntimeClient{list: func(_ context.Context, request *pb.ListSessionFilesRequest, _ ...grpc.CallOption) (*pb.ListSessionFilesResponse, error) {
		if request.GetIdentity().GetRunUid() != string(run.UID) || request.GetPath() != "notes" || request.GetLimit() != 2 || request.GetPageToken() != "after-notes" {
			t.Fatalf("list request = %#v", request)
		}
		return &pb.ListSessionFilesResponse{
			Entries:       []*pb.SessionFileInfo{{Path: "build.log", SizeBytes: 12}},
			NextPageToken: "next-notes",
		}, nil
	}}
	server := testServer(t, run, allowAuthorizer{}, &fakeDialer{client: client})

	response := httptest.NewRecorder()
	server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/namespaces/default/runtimes/bash/sessions/session-uid/files?path=notes&limit=2&pageToken=after-notes", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if got, want := response.Body.String(), `{"entries":[{"path":"build.log","directory":false,"sizeBytes":12}],"nextPageToken":"next-notes"}`+"\n"; got != want {
		t.Fatalf("response = %q, want %q", got, want)
	}

	invalid := httptest.NewRecorder()
	server.ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/v1/namespaces/default/runtimes/bash/sessions/session-uid/files?limit=0", nil))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid limit status = %d, body = %s", invalid.Code, invalid.Body.String())
	}
}

func TestGatewayRejectsInvalidOperationShape(t *testing.T) {
	server := testServer(t, readySessionRun(), allowAuthorizer{}, &fakeDialer{client: &fakeSessionRuntimeClient{}})

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/namespaces/default/runtimes/bash/sessions/session-uid/operations:execute", strings.NewReader(`{"command":{"shell":"true"},"deleteFile":{"path":"x"}}`))
	server.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestGatewayLimitsConcurrentRequests(t *testing.T) {
	run := readySessionRun()
	started := make(chan struct{})
	release := make(chan struct{})
	client := &fakeSessionRuntimeClient{status: func(_ context.Context, _ *pb.GetSessionStatusRequest, _ ...grpc.CallOption) (*pb.SessionStatus, error) {
		close(started)
		<-release
		return &pb.SessionStatus{State: pb.SessionState_SESSION_STATE_READY}, nil
	}}
	server := testServer(t, run, allowAuthorizer{}, &fakeDialer{client: client})
	server.MaxConcurrentRequests = 1

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/namespaces/default/runtimes/bash/sessions/session-uid", nil))
		firstDone <- response
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach the Runtime Service")
	}

	health := httptest.NewRecorder()
	server.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, want %d", health.Code, http.StatusOK)
	}

	second := httptest.NewRecorder()
	server.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/v1/namespaces/default/runtimes/bash/sessions/session-uid", nil))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, body = %s", second.Code, second.Body.String())
	}

	close(release)
	select {
	case first := <-firstDone:
		if first.Code != http.StatusOK {
			t.Fatalf("first request status = %d, body = %s", first.Code, first.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not complete")
	}
}

func TestGatewayUsesConfiguredHeaderLimit(t *testing.T) {
	server := &Server{MaxHeaderBytes: 4096}
	if got := server.httpServer().MaxHeaderBytes; got != 4096 {
		t.Fatalf("MaxHeaderBytes = %d, want 4096", got)
	}
	if got := (&Server{}).httpServer().MaxHeaderBytes; got != DefaultMaxHeaderBytes {
		t.Fatalf("default MaxHeaderBytes = %d, want %d", got, DefaultMaxHeaderBytes)
	}
}

func TestGatewayTLSConfigRejectsIncompleteFiles(t *testing.T) {
	for _, server := range []*Server{
		{TLSCertificateFile: "certificate.pem"},
		{TLSPrivateKeyFile: "key.pem"},
	} {
		if _, err := server.tlsConfig(); err == nil || !strings.Contains(err.Error(), "both Runtime gateway TLS certificate and private key files are required") {
			t.Fatalf("tlsConfig() error = %v, want incomplete TLS file error", err)
		}
	}
}

func TestGatewayTLSConfigRejectsInvalidCertificate(t *testing.T) {
	directory := t.TempDir()
	certificate := directory + "/tls.crt"
	privateKey := directory + "/tls.key"
	if err := os.WriteFile(certificate, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privateKey, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{TLSCertificateFile: certificate, TLSPrivateKeyFile: privateKey}
	if _, err := server.tlsConfig(); err == nil || !strings.Contains(err.Error(), "load Runtime gateway TLS certificate") {
		t.Fatalf("tlsConfig() error = %v, want certificate load error", err)
	}
}

func TestGatewayTLSConfigEnablesOptionalClientCertificateVerification(t *testing.T) {
	certificateFile, privateKeyFile := testTLSFiles(t)
	config, err := (&Server{
		TLSCertificateFile: certificateFile,
		TLSPrivateKeyFile:  privateKeyFile,
		TLSClientCAFile:    certificateFile,
	}).tlsConfig()
	if err != nil {
		t.Fatalf("tlsConfig() error = %v", err)
	}
	if config.ClientAuth != tls.VerifyClientCertIfGiven || config.ClientCAs == nil {
		t.Fatalf("client TLS configuration = %#v, want optional verified client certificates", config)
	}
}

func TestGatewayTLSConfigRejectsClientCAWithoutHTTPS(t *testing.T) {
	if _, err := (&Server{TLSClientCAFile: "client-ca.pem"}).tlsConfig(); err == nil || !strings.Contains(err.Error(), "TLS client CA requires") {
		t.Fatalf("tlsConfig() error = %v, want client CA HTTPS error", err)
	}
}

func TestGatewayHTTPSNegotiatesHTTP2(t *testing.T) {
	certificateFile, privateKeyFile := testTLSFiles(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	server := &Server{HTTPSAddress: address, TLSCertificateFile: certificateFile, TLSPrivateKeyFile: privateKeyFile}
	result := make(chan error, 1)
	go func() { result <- server.Start(ctx) }()

	client := &http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // Test-only self-signed certificate.
	var response *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for {
		response, err = client.Get("https://" + address + "/healthz")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("get Gateway health endpoint: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if response.ProtoMajor != 2 {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("HTTP protocol = %s, want HTTP/2", response.Proto)
	}
	if err := response.Body.Close(); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Gateway server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Gateway server did not stop")
	}
}

func testTLSFiles(t *testing.T) (string, string) {
	t.Helper()
	seed := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := seed.TLS.Certificates[0]
	seed.Close()
	privateKey, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certificateFile := directory + "/tls.crt"
	privateKeyFile := directory + "/tls.key"
	if err := os.WriteFile(certificateFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privateKeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certificateFile, privateKeyFile
}

func testServer(t *testing.T, run *v1alpha1.Run, authorizer Authorizer, dialer SessionRuntimeDialer) *Server {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithIndex(&v1alpha1.Run{}, runtimeIndexField, func(object client.Object) []string {
		return []string{object.(*v1alpha1.Run).Spec.Runtime}
	}).WithObjects(run).Build()
	return &Server{Runs: reader, Authorizer: authorizer, Dialer: dialer, FunctionDialer: dialer.(FunctionRuntimeDialer)}
}

func readyFunctionRun() *v1alpha1.Run {
	return &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: "function", Namespace: "default", UID: types.UID("function-uid")}, Spec: v1alpha1.RunSpec{Runtime: "bash", Mode: v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "handler.invoke"}}}, Status: v1alpha1.RunStatus{Phase: v1alpha1.RunReady, AssignedPod: "runtime-pod", AssignedPodUID: "pod-uid"}}
}

func readySessionRun() *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: types.UID("session-uid")},
		Spec:       v1alpha1.RunSpec{Runtime: "bash", Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunReady, AssignedPod: "runtime-pod", AssignedPodUID: "pod-uid"},
	}
}

func completedTaskRun() *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: types.UID("task-uid")},
		Spec:       v1alpha1.RunSpec{Runtime: "bash", Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}}},
		Status: v1alpha1.RunStatus{
			Phase:          v1alpha1.RunSucceeded,
			AssignedPod:    "runtime-pod",
			AssignedPodUID: "pod-uid",
			StartTime:      &metav1.Time{Time: time.Date(2026, time.September, 3, 12, 0, 0, 0, time.UTC)},
		},
	}
}

type allowAuthorizer struct{}

func (allowAuthorizer) Authorize(context.Context, *http.Request, *v1alpha1.Run) error { return nil }

type denyAuthorizer struct{}

func (denyAuthorizer) Authorize(context.Context, *http.Request, *v1alpha1.Run) error {
	return status.Error(codes.PermissionDenied, "denied")
}

type fakeDialer struct {
	client         pb.SessionRuntimeClient
	functionClient pb.FunctionRuntimeClient
	address        string
}

func (d *fakeDialer) DialFunction(_ context.Context, address string) (pb.FunctionRuntimeClient, io.Closer, error) {
	d.address = address
	return d.functionClient, nopCloser{}, nil
}

func (d *fakeDialer) Dial(_ context.Context, address string) (pb.SessionRuntimeClient, io.Closer, error) {
	d.address = address
	return d.client, nopCloser{}, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

type fakeSessionRuntimeClient struct {
	pb.SessionRuntimeClient
	status  func(context.Context, *pb.GetSessionStatusRequest, ...grpc.CallOption) (*pb.SessionStatus, error)
	execute func(context.Context, *pb.ExecuteSessionOperationRequest, ...grpc.CallOption) (*pb.ExecuteSessionOperationResponse, error)
	stream  func(context.Context, *pb.ExecuteSessionOperationRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[pb.SessionOperationEvent], error)
	list    func(context.Context, *pb.ListSessionFilesRequest, ...grpc.CallOption) (*pb.ListSessionFilesResponse, error)
}

type fakeFunctionRuntimeClient struct {
	pb.FunctionRuntimeClient
	invoke func(context.Context, *pb.InvokeFunctionRequest, ...grpc.CallOption) (*pb.InvokeFunctionResponse, error)
}

func (c *fakeFunctionRuntimeClient) InvokeFunction(ctx context.Context, request *pb.InvokeFunctionRequest, options ...grpc.CallOption) (*pb.InvokeFunctionResponse, error) {
	if c.invoke == nil {
		return nil, status.Error(codes.Unimplemented, "InvokeFunction")
	}
	return c.invoke(ctx, request, options...)
}

func (c *fakeSessionRuntimeClient) GetSessionStatus(ctx context.Context, request *pb.GetSessionStatusRequest, options ...grpc.CallOption) (*pb.SessionStatus, error) {
	if c.status == nil {
		return nil, status.Error(codes.Unimplemented, "GetSessionStatus")
	}
	return c.status(ctx, request, options...)
}
func (c *fakeSessionRuntimeClient) ExecuteSessionOperation(ctx context.Context, request *pb.ExecuteSessionOperationRequest, options ...grpc.CallOption) (*pb.ExecuteSessionOperationResponse, error) {
	if c.execute == nil {
		return nil, status.Error(codes.Unimplemented, "ExecuteSessionOperation")
	}
	return c.execute(ctx, request, options...)
}

func (c *fakeSessionRuntimeClient) StreamSessionOperation(ctx context.Context, request *pb.ExecuteSessionOperationRequest, options ...grpc.CallOption) (grpc.ServerStreamingClient[pb.SessionOperationEvent], error) {
	if c.stream == nil {
		return nil, status.Error(codes.Unimplemented, "StreamSessionOperation")
	}
	return c.stream(ctx, request, options...)
}

func (c *fakeSessionRuntimeClient) ListSessionFiles(ctx context.Context, request *pb.ListSessionFilesRequest, options ...grpc.CallOption) (*pb.ListSessionFilesResponse, error) {
	if c.list == nil {
		return nil, status.Error(codes.Unimplemented, "ListSessionFiles")
	}
	return c.list(ctx, request, options...)
}

type fakeSessionOperationStream struct {
	events []*pb.SessionOperationEvent
	index  int
}

func (s *fakeSessionOperationStream) Recv() (*pb.SessionOperationEvent, error) {
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*fakeSessionOperationStream) Header() (metadata.MD, error) { return nil, nil }
func (*fakeSessionOperationStream) Trailer() metadata.MD         { return nil }
func (*fakeSessionOperationStream) CloseSend() error             { return nil }
func (*fakeSessionOperationStream) Context() context.Context     { return context.Background() }
func (*fakeSessionOperationStream) SendMsg(any) error            { return nil }
func (*fakeSessionOperationStream) RecvMsg(any) error            { return io.EOF }
