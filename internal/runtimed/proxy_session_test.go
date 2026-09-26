package runtimed

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestSessionRuntimeProxyCallsLocalRuntimeServerForOwner(t *testing.T) {
	run := proxySessionRun("session-run", "bash", "pod-a", "pod-a-uid")
	reader := newSessionProxyReader(t, run)
	local := &sessionRuntimeClient{execute: func(_ context.Context, request *pb.ExecuteSessionOperationRequest) (*pb.ExecuteSessionOperationResponse, error) {
		if request.GetIdentity().GetRunUid() != string(run.UID) {
			t.Fatalf("Run UID = %q, want %q", request.GetIdentity().GetRunUid(), run.UID)
		}
		return &pb.ExecuteSessionOperationResponse{Command: &pb.SessionCommandResult{ExitCode: 0, Stdout: []byte("done")}}, nil
	}}
	proxy := newSessionRuntimeProxy(reader, reader, local, run.Namespace, "bash", "pod-a", "9093")

	response, err := proxy.ExecuteSessionOperation(t.Context(), &pb.ExecuteSessionOperationRequest{
		Identity:  &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: "pod-a-uid"},
		Operation: &pb.ExecuteSessionOperationRequest_Command{Command: &pb.SessionCommand{Argv: []string{"echo", "done"}}},
	})
	if err != nil {
		t.Fatalf("ExecuteSessionOperation() error = %v", err)
	}
	if string(response.GetCommand().GetStdout()) != "done" {
		t.Fatalf("stdout = %q, want done", response.GetCommand().GetStdout())
	}
}

func TestSessionRuntimeProxyStreamsOrderedEventsForOwner(t *testing.T) {
	run := proxySessionRun("session-run", "bash", "pod-a", "pod-a-uid")
	reader := newSessionProxyReader(t, run)
	local := &sessionRuntimeClient{stream: func(_ context.Context, request *pb.ExecuteSessionOperationRequest) (grpc.ServerStreamingClient[pb.SessionOperationEvent], error) {
		if request.GetIdentity().GetRunUid() != string(run.UID) {
			t.Fatalf("Run UID = %q, want %q", request.GetIdentity().GetRunUid(), run.UID)
		}
		return &fakeSessionOperationClientStream{events: []*pb.SessionOperationEvent{
			{Sequence: 99, Event: &pb.SessionOperationEvent_Progress{Progress: &pb.SessionOperationProgress{Kind: pb.SessionOperationProgressKind_SESSION_OPERATION_PROGRESS_KIND_STATUS, Message: "running"}}},
			{Sequence: 100, Event: &pb.SessionOperationEvent_Completed{Completed: &pb.ExecuteSessionOperationResponse{Command: &pb.SessionCommandResult{ExitCode: 0, Stdout: []byte("done\n")}}}},
		}}, nil
	}}
	proxy := newSessionRuntimeProxy(reader, reader, local, run.Namespace, "bash", "pod-a", "9093")
	var output bytes.Buffer
	proxy.logWriter = &output
	server := &fakeSessionOperationServer{ctx: t.Context()}

	err := proxy.StreamSessionOperation(&pb.ExecuteSessionOperationRequest{
		Identity:  &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: "pod-a-uid"},
		Operation: &pb.ExecuteSessionOperationRequest_Command{Command: &pb.SessionCommand{Argv: []string{"echo", "done"}}},
	}, server)
	if err != nil {
		t.Fatalf("StreamSessionOperation() error = %v", err)
	}
	if len(server.events) != 3 {
		t.Fatalf("events = %#v", server.events)
	}
	for index, event := range server.events {
		if want := int64(index + 1); event.GetSequence() != want {
			t.Fatalf("event %d sequence = %d, want %d", index, event.GetSequence(), want)
		}
	}
	if server.events[0].GetAccepted() == nil || server.events[1].GetProgress().GetMessage() != "running" || string(server.events[2].GetCompleted().GetCommand().GetStdout()) != "done\n" {
		t.Fatalf("events = %#v", server.events)
	}
	lines := decodeSessionLogLines(t, output.String())
	if len(lines) != 2 || lines[0].Stream != "stdout" || lines[0].Message != "done" || lines[1].Stream != "audit" {
		t.Fatalf("streaming operation logs = %#v", lines)
	}
}

func TestSessionRuntimeProxyPreservesFilePageFields(t *testing.T) {
	run := proxySessionRun("session-run", "bash", "pod-a", "pod-a-uid")
	reader := newSessionProxyReader(t, run)
	local := &sessionRuntimeClient{list: func(_ context.Context, request *pb.ListSessionFilesRequest) (*pb.ListSessionFilesResponse, error) {
		if request.GetPath() != "notes" || request.GetLimit() != 2 || request.GetPageToken() != "after-notes" {
			t.Fatalf("list request = %#v", request)
		}
		return &pb.ListSessionFilesResponse{NextPageToken: "next-notes"}, nil
	}}
	proxy := newSessionRuntimeProxy(reader, reader, local, run.Namespace, "bash", "pod-a", "9093")

	response, err := proxy.ListSessionFiles(t.Context(), &pb.ListSessionFilesRequest{
		Identity:  &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: "pod-a-uid"},
		Path:      "notes",
		Limit:     2,
		PageToken: "after-notes",
	})
	if err != nil {
		t.Fatalf("ListSessionFiles() error = %v", err)
	}
	if response.GetNextPageToken() != "next-notes" {
		t.Fatalf("next page token = %q, want next-notes", response.GetNextPageToken())
	}
}

func TestSessionRuntimeProxyEmitsStructuredCommandAndAuditLogs(t *testing.T) {
	run := proxySessionRun("session-run", "bash", "pod-a", "pod-a-uid")
	reader := newSessionProxyReader(t, run)
	local := &sessionRuntimeClient{execute: func(_ context.Context, _ *pb.ExecuteSessionOperationRequest) (*pb.ExecuteSessionOperationResponse, error) {
		return &pb.ExecuteSessionOperationResponse{Command: &pb.SessionCommandResult{
			ExitCode: 7,
			Stdout:   []byte("first\nsecond\n"),
			Stderr:   []byte("problem\n"),
		}}, nil
	}}
	proxy := newSessionRuntimeProxy(reader, reader, local, run.Namespace, "bash", "pod-a", "9093")
	var output bytes.Buffer
	proxy.logWriter = &output

	_, err := proxy.ExecuteSessionOperation(t.Context(), &pb.ExecuteSessionOperationRequest{
		Identity:  &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: "pod-a-uid"},
		Operation: &pb.ExecuteSessionOperationRequest_Command{Command: &pb.SessionCommand{Argv: []string{"sh", "-c", "secret command"}}},
	})
	if err != nil {
		t.Fatalf("ExecuteSessionOperation() error = %v", err)
	}

	lines := decodeSessionLogLines(t, output.String())
	if len(lines) != 4 {
		t.Fatalf("log lines = %#v, want stdout x2, stderr, audit", lines)
	}
	for i, line := range lines {
		if line.RunUID != string(run.UID) || line.AssignedPodUID != run.Status.AssignedPodUID || line.RunName != run.Name || line.Namespace != run.Namespace || line.Runtime != run.Spec.Runtime || line.Pod != "pod-a" {
			t.Fatalf("line %d metadata = %#v", i, line)
		}
		if line.Operation != "command" || line.Outcome != "failed" || line.ExitCode == nil || *line.ExitCode != 7 {
			t.Fatalf("line %d operation metadata = %#v", i, line)
		}
	}
	if lines[0].Stream != "stdout" || lines[0].Message != "first" || lines[1].Stream != "stdout" || lines[1].Message != "second" {
		t.Fatalf("stdout lines = %#v", lines[:2])
	}
	if lines[2].Stream != "stderr" || lines[2].Message != "problem" {
		t.Fatalf("stderr line = %#v", lines[2])
	}
	if lines[3].Stream != "audit" || lines[3].Message != "session operation completed" {
		t.Fatalf("audit line = %#v", lines[3])
	}
	if strings.Contains(output.String(), "secret command") {
		t.Fatalf("logs must not contain command text: %s", output.String())
	}
}

func decodeSessionLogLines(t *testing.T, output string) []executionLogLine {
	t.Helper()
	var lines []executionLogLine
	for _, raw := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		var line executionLogLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("decode log line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

func TestSessionRuntimeProxyForwardsToAssignedRuntimePod(t *testing.T) {
	run := proxySessionRun("session-run", "bash", "pod-b", "pod-b-uid")
	owner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-b",
			Namespace: run.Namespace,
			UID:       types.UID("pod-b-uid"),
			Labels:    map[string]string{"runtime": "bash"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.2"},
	}
	reader := newSessionProxyReader(t, run, owner)
	peer := &sessionRuntimeClient{execute: func(ctx context.Context, _ *pb.ExecuteSessionOperationRequest) (*pb.ExecuteSessionOperationResponse, error) {
		values, ok := metadata.FromOutgoingContext(ctx)
		if !ok || len(values.Get(sessionForwardedMetadataKey)) == 0 {
			t.Fatal("forwarded request did not include the forwarding marker")
		}
		return &pb.ExecuteSessionOperationResponse{}, nil
	}}
	proxy := newSessionRuntimeProxy(reader, reader, &sessionRuntimeClient{}, run.Namespace, "bash", "pod-a", "9093")
	proxy.dialPeer = func(_ context.Context, address string) (pb.SessionRuntimeClient, io.Closer, error) {
		if address != net.JoinHostPort(owner.Status.PodIP, "9093") {
			t.Fatalf("peer address = %q, want %q", address, net.JoinHostPort(owner.Status.PodIP, "9093"))
		}
		return peer, nopCloser{}, nil
	}

	_, err := proxy.ExecuteSessionOperation(t.Context(), &pb.ExecuteSessionOperationRequest{
		Identity:  &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: "pod-b-uid"},
		Operation: &pb.ExecuteSessionOperationRequest_Command{Command: &pb.SessionCommand{Argv: []string{"true"}}},
	})
	if err != nil {
		t.Fatalf("ExecuteSessionOperation() error = %v", err)
	}
}

func TestSessionRuntimeProxyRejectsOwnerFromAnotherRuntime(t *testing.T) {
	run := proxySessionRun("session-run", "bash", "pod-b", "pod-b-uid")
	owner := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-b",
			Namespace: run.Namespace,
			UID:       types.UID("pod-b-uid"),
			Labels:    map[string]string{"runtime": "python"},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.2"},
	}
	reader := newSessionProxyReader(t, run, owner)
	proxy := newSessionRuntimeProxy(reader, reader, &sessionRuntimeClient{}, run.Namespace, "bash", "pod-a", "9093")
	proxy.dialPeer = func(context.Context, string) (pb.SessionRuntimeClient, io.Closer, error) {
		t.Fatal("dialPeer must not be called for a different Runtime")
		return nil, nil, nil
	}

	_, err := proxy.GetSessionStatus(t.Context(), &pb.GetSessionStatusRequest{
		Identity: &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: "pod-b-uid"},
	})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("GetSessionStatus() code = %s, want %s", status.Code(err), codes.Unavailable)
	}
}

func TestSessionRuntimeProxyRejectsStaleAssignment(t *testing.T) {
	run := proxySessionRun("session-run", "bash", "pod-a", "pod-a-uid")
	reader := newSessionProxyReader(t, run)
	proxy := newSessionRuntimeProxy(reader, reader, &sessionRuntimeClient{}, run.Namespace, "bash", "pod-a", "9093")

	_, err := proxy.GetSessionStatus(t.Context(), &pb.GetSessionStatusRequest{
		Identity: &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: "old-pod-uid"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("GetSessionStatus() code = %s, want %s", status.Code(err), codes.FailedPrecondition)
	}
}

func proxySessionRun(name, runtimeName, podName, podUID string) *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunReady, AssignedPod: podName, AssignedPodUID: podUID},
	}
}

func newSessionProxyReader(t *testing.T, objects ...runtime.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&v1alpha1.Run{}, runtimeRunIndexField, func(object client.Object) []string {
			run, ok := object.(*v1alpha1.Run)
			if !ok || run.Spec.Runtime == "" {
				return nil
			}
			return []string{run.Spec.Runtime}
		}).
		WithRuntimeObjects(objects...).
		Build()
}

type sessionRuntimeClient struct {
	execute func(context.Context, *pb.ExecuteSessionOperationRequest) (*pb.ExecuteSessionOperationResponse, error)
	stream  func(context.Context, *pb.ExecuteSessionOperationRequest) (grpc.ServerStreamingClient[pb.SessionOperationEvent], error)
	list    func(context.Context, *pb.ListSessionFilesRequest) (*pb.ListSessionFilesResponse, error)
}

func (c *sessionRuntimeClient) RegisterSession(context.Context, *pb.RegisterSessionRequest, ...grpc.CallOption) (*pb.SessionStatus, error) {
	return nil, status.Error(codes.Unimplemented, "RegisterSession")
}

func (c *sessionRuntimeClient) GetSessionStatus(context.Context, *pb.GetSessionStatusRequest, ...grpc.CallOption) (*pb.SessionStatus, error) {
	return nil, status.Error(codes.Unimplemented, "GetSessionStatus")
}

func (c *sessionRuntimeClient) ExecuteSessionOperation(ctx context.Context, request *pb.ExecuteSessionOperationRequest, _ ...grpc.CallOption) (*pb.ExecuteSessionOperationResponse, error) {
	if c.execute == nil {
		return nil, status.Error(codes.Unimplemented, "ExecuteSessionOperation")
	}
	return c.execute(ctx, request)
}

func (c *sessionRuntimeClient) StreamSessionOperation(ctx context.Context, request *pb.ExecuteSessionOperationRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[pb.SessionOperationEvent], error) {
	if c.stream != nil {
		return c.stream(ctx, request)
	}
	return nil, status.Error(codes.Unimplemented, "StreamSessionOperation")
}

func (c *sessionRuntimeClient) ReadSessionFile(context.Context, *pb.ReadSessionFileRequest, ...grpc.CallOption) (*pb.ReadSessionFileResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ReadSessionFile")
}

func (c *sessionRuntimeClient) ListSessionFiles(ctx context.Context, request *pb.ListSessionFilesRequest, _ ...grpc.CallOption) (*pb.ListSessionFilesResponse, error) {
	if c.list != nil {
		return c.list(ctx, request)
	}
	return nil, status.Error(codes.Unimplemented, "ListSessionFiles")
}

func (c *sessionRuntimeClient) CloseSession(context.Context, *pb.CloseSessionRequest, ...grpc.CallOption) (*pb.CloseSessionResponse, error) {
	return nil, status.Error(codes.Unimplemented, "CloseSession")
}

type fakeSessionOperationClientStream struct {
	events []*pb.SessionOperationEvent
	index  int
}

func (s *fakeSessionOperationClientStream) Recv() (*pb.SessionOperationEvent, error) {
	if s.index >= len(s.events) {
		return nil, io.EOF
	}
	event := s.events[s.index]
	s.index++
	return event, nil
}

func (*fakeSessionOperationClientStream) Header() (metadata.MD, error) { return nil, nil }
func (*fakeSessionOperationClientStream) Trailer() metadata.MD         { return nil }
func (*fakeSessionOperationClientStream) CloseSend() error             { return nil }
func (*fakeSessionOperationClientStream) Context() context.Context     { return context.Background() }
func (*fakeSessionOperationClientStream) SendMsg(any) error            { return nil }
func (*fakeSessionOperationClientStream) RecvMsg(any) error            { return io.EOF }

type fakeSessionOperationServer struct {
	ctx    context.Context
	events []*pb.SessionOperationEvent
}

func (s *fakeSessionOperationServer) Send(event *pb.SessionOperationEvent) error {
	s.events = append(s.events, event)
	return nil
}

func (s *fakeSessionOperationServer) SetHeader(metadata.MD) error  { return nil }
func (s *fakeSessionOperationServer) SendHeader(metadata.MD) error { return nil }
func (s *fakeSessionOperationServer) SetTrailer(metadata.MD)       {}
func (s *fakeSessionOperationServer) Context() context.Context     { return s.ctx }
func (*fakeSessionOperationServer) SendMsg(any) error              { return nil }
func (*fakeSessionOperationServer) RecvMsg(any) error              { return io.EOF }

func TestForwardedRecognizesIncomingMarker(t *testing.T) {
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs(sessionForwardedMetadataKey, "true"))
	if !forwarded(ctx) {
		t.Fatal("forwarded() = false, want true")
	}
}

func TestRuntimePeerTargetUsesPassthroughResolver(t *testing.T) {
	address := net.JoinHostPort("10.0.0.2", "9093")
	if got, want := runtimePeerTarget(address), "passthrough:///"+address; got != want {
		t.Fatalf("runtimePeerTarget(%q) = %q, want %q", address, got, want)
	}
}

func TestFunctionRuntimeProxyResolvesOwnerRegistration(t *testing.T) {
	setTestWorkspace(t)
	run := proxyFunctionRun("function-run", "bash", "pod-a", "pod-a-uid")
	var logs bytes.Buffer
	controller := &Controller{PodName: "pod-a", ExecutionLogWriter: &logs}
	registration := &pb.FunctionRegistration{RunUid: string(run.UID), RegistrationId: "private-registration"}
	ar := newActiveRun(run, time.Now())
	ar.finishFunctionRegistration(registration, nil)
	controller.activeRuns.Store(string(run.UID), ar)
	local := &functionRuntimeClient{invoke: func(_ context.Context, request *pb.InvokeFunctionRequest) (*pb.InvokeFunctionResponse, error) {
		if request.GetRegistration().GetRegistrationId() != "private-registration" || request.GetRegistration().GetRunUid() != string(run.UID) {
			t.Fatalf("local request registration = %#v", request.GetRegistration())
		}
		if err := os.MkdirAll(filepath.Dir(ar.outputPath), 0o755); err != nil {
			t.Fatalf("create outputs directory: %v", err)
		}
		if err := os.WriteFile(ar.outputPath, []byte("from-file=value\n"), 0o600); err != nil {
			t.Fatalf("write invocation outputs: %v", err)
		}
		return &pb.InvokeFunctionResponse{InvocationId: request.GetInvocationId(), Output: request.GetInput(), Outputs: map[string]string{"from-adapter": "value"}}, nil
	}}
	reader := newSessionProxyReader(t, run)
	proxy := newFunctionRuntimeProxy(reader, reader, local, controller, run.Namespace, "bash", "pod-a", "9093")
	response, err := proxy.InvokeFunction(t.Context(), &pb.InvokeFunctionRequest{Registration: &pb.FunctionRegistration{RunUid: string(run.UID)}, InvocationId: "invoke-1", Input: []byte(`{"ok":true}`), ContentType: "application/json"})
	if err != nil {
		t.Fatalf("InvokeFunction: %v", err)
	}
	if response.GetInvocationId() != "invoke-1" || string(response.GetOutput()) != `{"ok":true}` {
		t.Fatalf("response = %#v", response)
	}
	if got := response.GetOutputs(); len(got) != 2 || got["from-file"] != "value" || got["from-adapter"] != "value" {
		t.Fatalf("outputs = %#v, want merged adapter and file values", got)
	}
	lines := decodeSessionLogLines(t, logs.String())
	if len(lines) != 1 || lines[0].InvocationID != "invoke-1" || lines[0].Operation != "function_invoke" || lines[0].Outcome != "succeeded" || lines[0].Stream != "audit" {
		t.Fatalf("function invocation audit logs = %#v", lines)
	}
}

func TestFunctionRuntimeProxyForwardsToOwner(t *testing.T) {
	run := proxyFunctionRun("function-run", "bash", "pod-b", "pod-b-uid")
	owner := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-b", Namespace: run.Namespace, UID: types.UID("pod-b-uid"), Labels: map[string]string{"runtime": "bash"}}, Status: corev1.PodStatus{PodIP: "10.0.0.2"}}
	reader := newSessionProxyReader(t, run, owner)
	proxy := newFunctionRuntimeProxy(reader, reader, &functionRuntimeClient{}, &Controller{}, run.Namespace, "bash", "pod-a", "9093")
	proxy.dialPeer = func(_ context.Context, address string) (pb.FunctionRuntimeClient, io.Closer, error) {
		if address != net.JoinHostPort(owner.Status.PodIP, "9093") {
			t.Fatalf("peer address = %q", address)
		}
		return &functionRuntimeClient{invoke: func(ctx context.Context, request *pb.InvokeFunctionRequest) (*pb.InvokeFunctionResponse, error) {
			values, _ := metadata.FromOutgoingContext(ctx)
			if len(values.Get(sessionForwardedMetadataKey)) == 0 {
				t.Fatal("missing forwarding marker")
			}
			return &pb.InvokeFunctionResponse{}, nil
		}}, nopCloser{}, nil
	}
	if _, err := proxy.InvokeFunction(t.Context(), &pb.InvokeFunctionRequest{Registration: &pb.FunctionRegistration{RunUid: string(run.UID)}}); err != nil {
		t.Fatalf("InvokeFunction: %v", err)
	}
}

func proxyFunctionRun(name, runtimeName, podName, podUID string) *v1alpha1.Run {
	return &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")}, Spec: v1alpha1.RunSpec{Runtime: runtimeName, Mode: v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{Handler: "handler.invoke"}}}, Status: v1alpha1.RunStatus{Phase: v1alpha1.RunReady, AssignedPod: podName, AssignedPodUID: podUID}}
}

type functionRuntimeClient struct {
	invoke func(context.Context, *pb.InvokeFunctionRequest) (*pb.InvokeFunctionResponse, error)
}

func (c *functionRuntimeClient) RegisterFunction(context.Context, *pb.RegisterFunctionRequest, ...grpc.CallOption) (*pb.RegisterFunctionResponse, error) {
	return nil, status.Error(codes.Unimplemented, "RegisterFunction")
}
func (c *functionRuntimeClient) FunctionStatus(context.Context, *pb.FunctionStatusRequest, ...grpc.CallOption) (*pb.FunctionStatusResponse, error) {
	return nil, status.Error(codes.Unimplemented, "FunctionStatus")
}
func (c *functionRuntimeClient) InvokeFunction(ctx context.Context, request *pb.InvokeFunctionRequest, _ ...grpc.CallOption) (*pb.InvokeFunctionResponse, error) {
	if c.invoke == nil {
		return nil, status.Error(codes.Unimplemented, "InvokeFunction")
	}
	return c.invoke(ctx, request)
}
func (c *functionRuntimeClient) UnregisterFunction(context.Context, *pb.UnregisterFunctionRequest, ...grpc.CallOption) (*pb.UnregisterFunctionResponse, error) {
	return nil, status.Error(codes.Unimplemented, "UnregisterFunction")
}
