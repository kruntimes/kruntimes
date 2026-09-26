package bash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

func startTestServer(t *testing.T) (pb.RuntimeClient, func()) {
	return startTestServerWithOutputLimit(t, defaultOutputLimitBytes)
}

func startTestServerWithOutputLimit(t *testing.T, outputLimit int) (pb.RuntimeClient, func()) {
	t.Helper()

	srv := grpc.NewServer()
	pb.RegisterRuntimeServer(srv, NewServerWithOutputLimit(t.TempDir(), outputLimit))

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	return pb.NewRuntimeClient(conn), func() {
		conn.Close()
		srv.Stop()
	}
}

func startFunctionTestServer(t *testing.T, outputLimit int) (pb.FunctionRuntimeClient, string, func()) {
	t.Helper()

	workDir := t.TempDir()
	srv := grpc.NewServer()
	runtimeServer := NewServerWithOutputLimit(workDir, outputLimit)
	pb.RegisterRuntimeServer(srv, runtimeServer)
	pb.RegisterFunctionRuntimeServer(srv, runtimeServer)

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return pb.NewFunctionRuntimeClient(conn), workDir, func() {
		conn.Close()
		srv.Stop()
	}
}

func startSessionTestServer(t *testing.T) (pb.SessionRuntimeClient, string, func()) {
	t.Helper()

	workDir := t.TempDir()
	srv := grpc.NewServer()
	runtimeServer := NewServer(workDir)
	pb.RegisterSessionRuntimeServer(srv, runtimeServer)

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return pb.NewSessionRuntimeClient(conn), workDir, func() {
		conn.Close()
		srv.Stop()
	}
}

func writeFunctionHandler(t *testing.T, workDir, name, script string) string {
	t.Helper()
	functionDir := filepath.Join(workDir, name)
	if err := os.MkdirAll(functionDir, 0o755); err != nil {
		t.Fatalf("mkdir function directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(functionDir, "handler.sh"), []byte(script), 0o644); err != nil {
		t.Fatalf("write handler: %v", err)
	}
	return functionDir
}

func registerFunction(t *testing.T, client pb.FunctionRuntimeClient, workDir, runUID string, attempt int32, digest string) *pb.FunctionRegistration {
	t.Helper()
	resp, err := client.RegisterFunction(context.Background(), &pb.RegisterFunctionRequest{
		RunUid:              runUID,
		RegistrationAttempt: attempt,
		WorkingDir:          workDir,
		Handler:             "handler.handle",
		RegistrationDigest:  digest,
	})
	if err != nil {
		t.Fatalf("RegisterFunction: %v", err)
	}
	if resp.State != pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY {
		t.Fatalf("registration state = %v, want ready", resp.State)
	}
	return resp.Registration
}

func TestFunctionRuntimeRegisterInvokeAndUnregister(t *testing.T) {
	client, workDir, cleanup := startFunctionTestServer(t, defaultOutputLimitBytes)
	defer cleanup()
	functionDir := writeFunctionHandler(t, workDir, "one", "handle() { printf '%s' \"$1\"; }\n")
	registration := registerFunction(t, client, functionDir, "run-uid-1", 1, "sha256:first")
	initialStatus, err := client.FunctionStatus(context.Background(), &pb.FunctionStatusRequest{Registration: registration})
	if err != nil {
		t.Fatalf("FunctionStatus after registration: %v", err)
	}
	if initialStatus.LastActivityUnixNano <= 0 {
		t.Fatalf("initial last activity = %d, want registration timestamp", initialStatus.LastActivityUnixNano)
	}

	resp, err := client.InvokeFunction(context.Background(), &pb.InvokeFunctionRequest{
		Registration: registration,
		InvocationId: "invoke-1",
		Input:        []byte(`{"message":"hello"}`),
		ContentType:  "application/json",
	})
	if err != nil {
		t.Fatalf("InvokeFunction: %v", err)
	}
	if got, want := string(resp.Output), `{"message":"hello"}`; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if resp.InvocationId != "invoke-1" {
		t.Fatalf("invocation ID = %q, want caller-provided ID", resp.InvocationId)
	}

	generated, err := client.InvokeFunction(context.Background(), &pb.InvokeFunctionRequest{
		Registration: registration,
		Input:        []byte(`{"message":"generated"}`),
		ContentType:  "application/json",
	})
	if err != nil {
		t.Fatalf("InvokeFunction without ID: %v", err)
	}
	if !strings.HasPrefix(generated.InvocationId, "inv_") {
		t.Fatalf("generated invocation ID = %q, want inv_ prefix", generated.InvocationId)
	}

	statusResp, err := client.FunctionStatus(context.Background(), &pb.FunctionStatusRequest{Registration: registration})
	if err != nil {
		t.Fatalf("FunctionStatus: %v", err)
	}
	if statusResp.State != pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY || statusResp.InFlight != 0 || statusResp.LastActivityUnixNano == 0 {
		t.Fatalf("status = %#v", statusResp)
	}

	if _, err := client.UnregisterFunction(context.Background(), &pb.UnregisterFunctionRequest{Registration: registration}); err != nil {
		t.Fatalf("UnregisterFunction: %v", err)
	}
	if _, err := client.FunctionStatus(context.Background(), &pb.FunctionStatusRequest{Registration: registration}); status.Code(err) != codes.NotFound {
		t.Fatalf("FunctionStatus after unregister = %v, want NotFound", err)
	}
}

func TestSessionRuntimeRegisterStatusCloseAndAssignmentFencing(t *testing.T) {
	client, workDir, cleanup := startSessionTestServer(t)
	defer cleanup()

	identity := &pb.SessionIdentity{RunUid: "session-run", AssignedPodUid: "pod-a"}
	request := &pb.RegisterSessionRequest{Identity: identity, WorkingDir: workDir}
	registered, err := client.RegisterSession(context.Background(), request)
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if registered.State != pb.SessionState_SESSION_STATE_READY || registered.LastActivityUnixNano == 0 {
		t.Fatalf("registration status = %#v, want ready with activity", registered)
	}

	// Re-registering the same Run assignment is idempotent.
	if _, err := client.RegisterSession(context.Background(), request); err != nil {
		t.Fatalf("idempotent RegisterSession: %v", err)
	}
	if _, err := client.RegisterSession(context.Background(), &pb.RegisterSessionRequest{
		Identity:   &pb.SessionIdentity{RunUid: identity.RunUid, AssignedPodUid: "pod-b"},
		WorkingDir: workDir,
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("RegisterSession stale assignment = %v, want FailedPrecondition", err)
	}

	current, err := client.GetSessionStatus(context.Background(), &pb.GetSessionStatusRequest{Identity: identity})
	if err != nil {
		t.Fatalf("GetSessionStatus: %v", err)
	}
	if current.State != pb.SessionState_SESSION_STATE_READY {
		t.Fatalf("session state = %v, want ready", current.State)
	}
	if _, err := client.CloseSession(context.Background(), &pb.CloseSessionRequest{Identity: identity}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if _, err := client.CloseSession(context.Background(), &pb.CloseSessionRequest{Identity: identity}); err != nil {
		t.Fatalf("idempotent CloseSession: %v", err)
	}
	if _, err := client.GetSessionStatus(context.Background(), &pb.GetSessionStatusRequest{Identity: identity}); status.Code(err) != codes.NotFound {
		t.Fatalf("GetSessionStatus after close = %v, want NotFound", err)
	}
}

func TestNewServerWithSessionTerminationGrace(t *testing.T) {
	configured := 17 * time.Millisecond
	if got := NewServerWithSessionTerminationGrace(t.TempDir(), configured).sessionTerminationGrace; got != configured {
		t.Fatalf("Session termination grace = %s, want %s", got, configured)
	}
	if got := NewServerWithSessionTerminationGrace(t.TempDir(), 0).sessionTerminationGrace; got != processTerminationGrace {
		t.Fatalf("default Session termination grace = %s, want %s", got, processTerminationGrace)
	}
}

func TestSessionRuntimeRejectsEscapingWorkspace(t *testing.T) {
	client, _, cleanup := startSessionTestServer(t)
	defer cleanup()

	_, err := client.RegisterSession(context.Background(), &pb.RegisterSessionRequest{
		Identity:   &pb.SessionIdentity{RunUid: "session-run", AssignedPodUid: "pod-a"},
		WorkingDir: t.TempDir(),
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("RegisterSession escaping workspace = %v, want InvalidArgument", err)
	}
}

func TestSessionRuntimeExecutesCommandsAndConfinesFiles(t *testing.T) {
	client, workDir, cleanup := startSessionTestServer(t)
	defer cleanup()

	identity := &pb.SessionIdentity{RunUid: "session-run", AssignedPodUid: "pod-a"}
	if _, err := client.RegisterSession(context.Background(), &pb.RegisterSessionRequest{
		Identity:   identity,
		WorkingDir: workDir,
		Env: map[string]string{
			"KRUNTIMES_SESSION_DEFAULT": "session",
		},
	}); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if _, err := client.ExecuteSessionOperation(context.Background(), &pb.ExecuteSessionOperationRequest{Identity: identity}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ExecuteSessionOperation without operation = %v, want InvalidArgument", err)
	}
	if _, err := client.ExecuteSessionOperation(context.Background(), &pb.ExecuteSessionOperationRequest{
		Identity: identity,
		Operation: &pb.ExecuteSessionOperationRequest_CreateDirectory{CreateDirectory: &pb.SessionDirectoryCreate{
			Path: "scratch/nested",
		}},
	}); err != nil {
		t.Fatalf("ExecuteSessionOperation create directory: %v", err)
	}
	if _, err := client.ExecuteSessionOperation(context.Background(), &pb.ExecuteSessionOperationRequest{
		Identity: identity,
		Operation: &pb.ExecuteSessionOperationRequest_WriteFile{WriteFile: &pb.SessionFileWrite{
			Path: "notes/hello.txt", Contents: []byte("hello\n"), CreateParents: true,
		}},
	}); err != nil {
		t.Fatalf("ExecuteSessionOperation write: %v", err)
	}
	read, err := client.ReadSessionFile(context.Background(), &pb.ReadSessionFileRequest{Identity: identity, Path: "notes/hello.txt", MaxBytes: 1024})
	if err != nil {
		t.Fatalf("ReadSessionFile: %v", err)
	}
	if got := string(read.Contents); got != "hello\n" || read.Truncated {
		t.Fatalf("read = %#v, want hello without truncation", read)
	}
	listed, err := client.ListSessionFiles(context.Background(), &pb.ListSessionFilesRequest{Identity: identity})
	if err != nil {
		t.Fatalf("ListSessionFiles: %v", err)
	}
	var notes *pb.SessionFileInfo
	for _, entry := range listed.Entries {
		if entry.Path == "notes" {
			notes = entry
			break
		}
	}
	if notes == nil || !notes.Directory {
		t.Fatalf("directory entries = %#v, want notes directory", listed.Entries)
	}
	operation, err := client.ExecuteSessionOperation(context.Background(), &pb.ExecuteSessionOperationRequest{
		Identity: identity,
		Operation: &pb.ExecuteSessionOperationRequest_Command{Command: &pb.SessionCommand{
			Argv: []string{"bash", "-c", "printf '%s:%s:' \"$KRUNTIMES_SESSION_DEFAULT\" \"$KRUNTIMES_SESSION_TEST\"; cat notes/hello.txt"},
			Env:  map[string]string{"KRUNTIMES_SESSION_TEST": "environment"},
		}},
	})
	if err != nil {
		t.Fatalf("ExecuteSessionOperation command: %v", err)
	}
	if operation.Command == nil || operation.Command.ExitCode != 0 || string(operation.Command.Stdout) != "session:environment:hello\n" || operation.Command.TimedOut {
		t.Fatalf("command result = %#v", operation)
	}
	if _, err := client.ExecuteSessionOperation(context.Background(), &pb.ExecuteSessionOperationRequest{
		Identity: identity,
		Operation: &pb.ExecuteSessionOperationRequest_RenameFile{RenameFile: &pb.SessionFileRename{
			SourcePath: "notes/hello.txt", DestinationPath: "notes/greeting.txt",
		}},
	}); err != nil {
		t.Fatalf("ExecuteSessionOperation rename: %v", err)
	}
	if _, err := client.ExecuteSessionOperation(context.Background(), &pb.ExecuteSessionOperationRequest{
		Identity: identity,
		Operation: &pb.ExecuteSessionOperationRequest_DeleteFile{DeleteFile: &pb.SessionFileDelete{
			Path: "notes", Recursive: true,
		}},
	}); err != nil {
		t.Fatalf("ExecuteSessionOperation delete: %v", err)
	}
	if _, err := client.ReadSessionFile(context.Background(), &pb.ReadSessionFileRequest{Identity: identity, Path: "../outside", MaxBytes: 1}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("ReadSessionFile escaping path = %v, want InvalidArgument", err)
	}
}

func TestSessionRuntimeStreamsCommandOutputBeforeCompletion(t *testing.T) {
	client, workDir, cleanup := startSessionTestServer(t)
	defer cleanup()
	identity := &pb.SessionIdentity{RunUid: "stream-session", AssignedPodUid: "pod-a"}
	if _, err := client.RegisterSession(context.Background(), &pb.RegisterSessionRequest{Identity: identity, WorkingDir: workDir}); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}

	started := time.Now()
	stream, err := client.StreamSessionOperation(context.Background(), &pb.ExecuteSessionOperationRequest{
		Identity: identity,
		Operation: &pb.ExecuteSessionOperationRequest_Command{Command: &pb.SessionCommand{
			Argv: []string{"sh", "-c", "printf first; sleep 1; printf second"},
		}},
	})
	if err != nil {
		t.Fatalf("StreamSessionOperation: %v", err)
	}
	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive first stream event: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= 750*time.Millisecond {
		t.Fatalf("first event arrived after %s, want output before command completion", elapsed)
	}
	if output := first.GetOutput(); output == nil || output.GetStream() != pb.SessionOperationOutputStream_SESSION_OPERATION_OUTPUT_STREAM_STDOUT || string(output.GetData()) != "first" {
		t.Fatalf("first event = %#v", first)
	}

	var completed *pb.ExecuteSessionOperationResponse
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("receive stream event: %v", err)
		}
		if event.GetCompleted() != nil {
			completed = event.GetCompleted()
		}
	}
	if completed == nil || completed.GetCommand().GetExitCode() != 0 || string(completed.GetCommand().GetStdout()) != "firstsecond" {
		t.Fatalf("completed event = %#v", completed)
	}
}

func TestSessionRuntimeListsFilesInBoundedPages(t *testing.T) {
	client, workDir, cleanup := startSessionTestServer(t)
	defer cleanup()

	identity := &pb.SessionIdentity{RunUid: "session-pages", AssignedPodUid: "pod-a"}
	if _, err := client.RegisterSession(context.Background(), &pb.RegisterSessionRequest{Identity: identity, WorkingDir: workDir}); err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	for _, name := range []string{"z", "b", "a", "\u00e9"} {
		if err := os.WriteFile(filepath.Join(workDir, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write session file %q: %v", name, err)
		}
	}

	first, err := client.ListSessionFiles(context.Background(), &pb.ListSessionFilesRequest{Identity: identity, Limit: 2})
	if err != nil {
		t.Fatalf("ListSessionFiles first page: %v", err)
	}
	if got, want := sessionFileNames(first.Entries), []string{"a", "b"}; !slices.Equal(got, want) || first.NextPageToken == "" {
		t.Fatalf("first page = %#v, want entries %v and a next page token", first, want)
	}

	second, err := client.ListSessionFiles(context.Background(), &pb.ListSessionFilesRequest{Identity: identity, Limit: 2, PageToken: first.NextPageToken})
	if err != nil {
		t.Fatalf("ListSessionFiles second page: %v", err)
	}
	if got, want := sessionFileNames(second.Entries), []string{"z", "\u00e9"}; !slices.Equal(got, want) || second.NextPageToken != "" {
		t.Fatalf("second page = %#v, want final entries %v", second, want)
	}

	// This token is encoded as the documented cross-runtime JSON cursor. A
	// Python Runtime Server must accept the same unpadded base64url value.
	pythonToken := "eyJ2IjoxLCJwYXRoIjoiIiwiYWZ0ZXIiOiJiIn0"
	fromPython, err := client.ListSessionFiles(context.Background(), &pb.ListSessionFilesRequest{Identity: identity, Limit: 2, PageToken: pythonToken})
	if err != nil {
		t.Fatalf("ListSessionFiles Python token: %v", err)
	}
	if got, want := sessionFileNames(fromPython.Entries), []string{"z", "\u00e9"}; !slices.Equal(got, want) {
		t.Fatalf("Python token page entries = %v, want %v", got, want)
	}

	for _, request := range []*pb.ListSessionFilesRequest{
		{Identity: identity, Limit: maxSessionFilePageSize + 1},
		{Identity: identity, PageToken: "not-a-token"},
		{Identity: identity, Path: "other", PageToken: first.NextPageToken},
	} {
		if _, err := client.ListSessionFiles(context.Background(), request); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("ListSessionFiles request %#v error = %v, want InvalidArgument", request, err)
		}
	}
}

func sessionFileNames(entries []*pb.SessionFileInfo) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Path)
	}
	return names
}

func TestFunctionRuntimeRegistrationFencing(t *testing.T) {
	client, workDir, cleanup := startFunctionTestServer(t, defaultOutputLimitBytes)
	defer cleanup()
	functionDir := writeFunctionHandler(t, workDir, "fence", "handle() { printf '{}'; }\n")
	first := registerFunction(t, client, functionDir, "run-uid-2", 1, "sha256:first")

	idempotent := registerFunction(t, client, functionDir, "run-uid-2", 1, "sha256:first")
	if idempotent.RegistrationId != first.RegistrationId {
		t.Fatalf("idempotent registration ID = %q, want %q", idempotent.RegistrationId, first.RegistrationId)
	}
	if _, err := client.RegisterFunction(context.Background(), &pb.RegisterFunctionRequest{
		RunUid: "run-uid-2", RegistrationAttempt: 1, WorkingDir: functionDir, Handler: "handler.handle", RegistrationDigest: "sha256:changed",
	}); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("same attempt with changed digest = %v, want AlreadyExists", err)
	}

	second := registerFunction(t, client, functionDir, "run-uid-2", 2, "sha256:second")
	if second.RegistrationId == first.RegistrationId {
		t.Fatal("new registration attempt reused its registration ID")
	}
	if _, err := client.InvokeFunction(context.Background(), &pb.InvokeFunctionRequest{
		Registration: first, InvocationId: "stale", Input: []byte(`{}`), ContentType: "application/json",
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("InvokeFunction with stale registration = %v, want FailedPrecondition", err)
	}
}

func TestFunctionRuntimeDoesNotInterpretInputAsShell(t *testing.T) {
	client, workDir, cleanup := startFunctionTestServer(t, defaultOutputLimitBytes)
	defer cleanup()
	functionDir := writeFunctionHandler(t, workDir, "safe", "handle() { printf '%s' \"$1\"; }\n")
	registration := registerFunction(t, client, functionDir, "run-uid-3", 1, "sha256:safe")
	marker := filepath.Join(workDir, "must-not-exist")
	input := fmt.Sprintf(`{"value":"$(touch %s)"}`, marker)

	resp, err := client.InvokeFunction(context.Background(), &pb.InvokeFunctionRequest{
		Registration: registration, InvocationId: "safe-input", Input: []byte(input), ContentType: "application/json",
	})
	if err != nil {
		t.Fatalf("InvokeFunction: %v", err)
	}
	if got := string(resp.Output); got != input {
		t.Fatalf("output = %q, want original input", got)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("input was interpreted as shell source; marker stat error = %v", err)
	}
}

func TestFunctionRuntimeRejectsInvalidInputAndBoundsOutput(t *testing.T) {
	client, workDir, cleanup := startFunctionTestServer(t, 16)
	defer cleanup()
	functionDir := writeFunctionHandler(t, workDir, "limits", "handle() { printf '%s' '0123456789abcdefx'; }\n")
	registration := registerFunction(t, client, functionDir, "run-uid-4", 1, "sha256:limits")

	if _, err := client.InvokeFunction(context.Background(), &pb.InvokeFunctionRequest{
		Registration: registration, InvocationId: "bad-type", Input: []byte(`{}`), ContentType: "text/plain",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("InvokeFunction unsupported content type = %v, want InvalidArgument", err)
	}
	if _, err := client.InvokeFunction(context.Background(), &pb.InvokeFunctionRequest{
		Registration: registration, InvocationId: "too-large", Input: []byte(`{}`), ContentType: "application/json",
	}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("InvokeFunction oversized response = %v, want ResourceExhausted", err)
	}
}

func TestFunctionRuntimeRejectsMissingHandlerAndEscapingWorkspace(t *testing.T) {
	client, workDir, cleanup := startFunctionTestServer(t, defaultOutputLimitBytes)
	defer cleanup()
	functionDir := writeFunctionHandler(t, workDir, "validation", "other() { printf '{}'; }\n")
	if _, err := client.RegisterFunction(context.Background(), &pb.RegisterFunctionRequest{
		RunUid: "run-uid-missing-handler", RegistrationAttempt: 1, WorkingDir: functionDir, Handler: "handler.handle", RegistrationDigest: "sha256:missing",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("RegisterFunction missing handler = %v, want InvalidArgument", err)
	}

	externalDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(externalDir, "handler.sh"), []byte("handle() { printf '{}'; }\n"), 0o644); err != nil {
		t.Fatalf("write external handler: %v", err)
	}
	escapingLink := filepath.Join(workDir, "escaping")
	if err := os.Symlink(externalDir, escapingLink); err != nil {
		t.Fatalf("create workspace symlink: %v", err)
	}
	if _, err := client.RegisterFunction(context.Background(), &pb.RegisterFunctionRequest{
		RunUid: "run-uid-escaping-workspace", RegistrationAttempt: 1, WorkingDir: escapingLink, Handler: "handler.handle", RegistrationDigest: "sha256:escaping",
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("RegisterFunction escaping workspace = %v, want InvalidArgument", err)
	}
}

func TestFunctionRuntimeUnregisterCancelsInFlightInvocation(t *testing.T) {
	client, workDir, cleanup := startFunctionTestServer(t, defaultOutputLimitBytes)
	defer cleanup()
	functionDir := writeFunctionHandler(t, workDir, "cancel", "handle() { sleep 30; printf '{}'; }\n")
	registration := registerFunction(t, client, functionDir, "run-uid-cancel", 1, "sha256:cancel")

	invokeResult := make(chan error, 1)
	go func() {
		_, err := client.InvokeFunction(context.Background(), &pb.InvokeFunctionRequest{
			Registration: registration, InvocationId: "invoke-cancel", Input: []byte(`{}`), ContentType: "application/json",
		})
		invokeResult <- err
	}()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		functionStatus, err := client.FunctionStatus(context.Background(), &pb.FunctionStatusRequest{Registration: registration})
		if err != nil {
			t.Fatalf("FunctionStatus: %v", err)
		}
		if functionStatus.InFlight == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := client.UnregisterFunction(context.Background(), &pb.UnregisterFunctionRequest{
		Registration: registration, CancelInFlight: true, DrainTimeoutMillis: 5_000,
	}); err != nil {
		t.Fatalf("UnregisterFunction: %v", err)
	}
	if err := <-invokeResult; status.Code(err) != codes.Canceled {
		t.Fatalf("InvokeFunction after cancellation = %v, want Canceled", err)
	}
}

func TestCreateAndGetTask_Success(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "test-1",
		Args: []string{"echo hello"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var resp *pb.StatusResponse
	for i := 0; i < 50; i++ {
		resp, err = client.Status(ctx, &pb.StatusRequest{Id: "test-1"})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED || resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Errorf("expected SUCCEEDED, got %v (stderr=%s err=%s)", resp.State, resp.Stderr, resp.ErrorMessage)
	}
}

func TestCreateAndGetTask_Failure(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "test-2",
		Args: []string{"exit 42"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var resp *pb.StatusResponse
	for i := 0; i < 50; i++ {
		resp, err = client.Status(ctx, &pb.StatusRequest{Id: "test-2"})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED || resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.ExecutionState_EXECUTION_STATE_FAILED {
		t.Errorf("expected FAILED, got %v", resp.State)
	}
	if resp.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", resp.ExitCode)
	}
}

func TestListAndDeleteTask(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "test-3",
		Args: []string{"sleep 10"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	listResp, err := client.List(ctx, &pb.ListRequest{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(listResp.Entries) != 1 {
		t.Errorf("expected 1 request, got %d", len(listResp.Entries))
	}

	_, err = client.Cancel(ctx, &pb.CancelRequest{Id: "test-3"})
	if err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	listResp, err = client.List(ctx, &pb.ListRequest{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(listResp.Entries) != 0 {
		t.Errorf("expected 0 tasks after delete, got %d", len(listResp.Entries))
	}
}

func TestGetTask_NotFound(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.Status(ctx, &pb.StatusRequest{Id: "nonexistent"})
	if err == nil {
		t.Error("expected error for nonexistent request")
	}
}

func TestForgetTerminalExecution(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "forget-terminal",
		Args: []string{"echo done"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	waitForTerminalStatus(t, client, "forget-terminal")

	if _, err := client.Forget(ctx, &pb.ForgetRequest{Id: "forget-terminal"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := client.Status(ctx, &pb.StatusRequest{Id: "forget-terminal"}); status.Code(err) != codes.NotFound {
		t.Fatalf("Status error = %v, want NotFound", err)
	}
}

func TestForgetRejectsRunningExecution(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "forget-running",
		Args: []string{"sleep 30"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := client.Forget(ctx, &pb.ForgetRequest{Id: "forget-running"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Forget error = %v, want FailedPrecondition", err)
	}
	if _, err := client.Cancel(ctx, &pb.CancelRequest{Id: "forget-running"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
}

func TestCreateTask_Duplicate(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "dup-1",
		Args: []string{"echo first"},
	})
	if err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	// Duplicate Execute should succeed (cancels the old execution for retry).
	_, err = client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "dup-1",
		Args: []string{"echo second"},
	})
	if err != nil {
		t.Fatalf("second CreateTask (retry): %v", err)
	}
}

func TestCreateTask_MultipleCommands(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "multi-1",
		Args: []string{"export FOO=bar", "echo $FOO"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var resp *pb.StatusResponse
	for i := 0; i < 50; i++ {
		resp, err = client.Status(ctx, &pb.StatusRequest{Id: "multi-1"})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED || resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Errorf("expected SUCCEEDED, got %v (stderr=%s)", resp.State, resp.Stderr)
	}
	fmt.Printf("stdout: %s\n", resp.Stdout)
}

func TestExecuteShellCArgs(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "shell-c",
		Args: []string{"sh", "-c", `echo "$MESSAGE"`, "ignored-shell-name"},
		Env:  map[string]string{"MESSAGE": "hello from sh -c"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	resp := waitForTerminalStatus(t, client, "shell-c")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Fatalf("state = %v, want succeeded: stderr=%s error=%s", resp.State, resp.Stderr, resp.ErrorMessage)
	}
	if resp.Stdout != "hello from sh -c\n" {
		t.Fatalf("stdout = %q", resp.Stdout)
	}
	if strings.Contains(resp.Stderr, "-c: command not found") {
		t.Fatalf("stderr contains old line-script failure: %q", resp.Stderr)
	}
}

func TestExecuteEnvironmentOverridesRuntimeEnvironment(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	_, err := client.Execute(context.Background(), &pb.ExecuteRequest{
		Id:   "environment-override",
		Args: []string{"sh", "-c", `printf '%s' "$PATH"`},
		Env:  map[string]string{"PATH": "/tool-cache/bin"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	response := waitForTerminalStatus(t, client, "environment-override")
	if response.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Fatalf("state = %v, want succeeded: stderr=%s error=%s", response.State, response.Stderr, response.ErrorMessage)
	}
	if response.Stdout != "/tool-cache/bin" {
		t.Fatalf("PATH = %q, want %q", response.Stdout, "/tool-cache/bin")
	}
}

func TestExecute_InlineSource(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	// Simulate what runtimed does: write inline code to script in a temp dir,
	// then pass working_dir to the ExecuteRequest.
	workDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "script"), []byte("echo hello_from_inline"), 0o644)

	ctx := context.Background()
	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:         "inline-1",
		WorkingDir: workDir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var resp *pb.StatusResponse
	for i := 0; i < 50; i++ {
		resp, err = client.Status(ctx, &pb.StatusRequest{Id: "inline-1"})
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED || resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Errorf("expected SUCCEEDED, got %v (stderr=%s)", resp.State, resp.Stderr)
	}
	if resp.Stdout != "hello_from_inline\n" {
		t.Errorf("expected 'hello_from_inline\n', got %q", resp.Stdout)
	}
}

func TestExecute_EntrypointArgs(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	workDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "script"), []byte("printf 'first=%s second=%s\\n' \"$1\" \"$2\""), 0o644)

	ctx := context.Background()
	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:         "entrypoint-args",
		WorkingDir: workDir,
		Args:       []string{"hello", "world"},
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	resp := waitForTerminalStatus(t, client, "entrypoint-args")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Fatalf("state = %v, want succeeded: stderr=%s error=%s", resp.State, resp.Stderr, resp.ErrorMessage)
	}
	if resp.Stdout != "first=hello second=world\n" {
		t.Fatalf("stdout = %q", resp.Stdout)
	}
}

func TestHealth(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	resp, err := client.Health(context.Background(), &pb.HealthRequest{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !resp.Healthy {
		t.Error("expected healthy=true")
	}
}

func TestOutputIsBounded(t *testing.T) {
	const outputLimit = 128
	client, cleanup := startTestServerWithOutputLimit(t, outputLimit)
	defer cleanup()

	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id: "bounded-output",
		Args: []string{
			"head -c 4096 /dev/zero | tr '\\0' x",
			"head -c 4096 /dev/zero | tr '\\0' y >&2",
		},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	resp := waitForTerminalStatus(t, client, "bounded-output")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Fatalf("state = %v, want succeeded: %s", resp.State, resp.ErrorMessage)
	}
	if !strings.HasSuffix(resp.Stdout, outputTruncatedMarker) {
		t.Fatalf("stdout does not contain truncation marker: %q", resp.Stdout)
	}
	if got, want := len(strings.TrimSuffix(resp.Stdout, outputTruncatedMarker)), outputLimit; got != want {
		t.Fatalf("retained stdout bytes = %d, want %d", got, want)
	}
	if !strings.HasSuffix(resp.Stderr, outputTruncatedMarker) {
		t.Fatalf("stderr does not contain truncation marker: %q", resp.Stderr)
	}
	if got, want := len(strings.TrimSuffix(resp.Stderr, outputTruncatedMarker)), outputLimit; got != want {
		t.Fatalf("retained stderr bytes = %d, want %d", got, want)
	}
}

func TestStatusSnapshotIsImmutable(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	entry := newExecutionEntry(cancel, 64)
	stdout := executionOutput{entry: entry}

	if _, err := stdout.Write([]byte("before")); err != nil {
		t.Fatalf("write initial output: %v", err)
	}
	snapshot := entry.snapshot("immutable")
	if _, err := stdout.Write([]byte("-after")); err != nil {
		t.Fatalf("write later output: %v", err)
	}
	entry.complete(pb.ExecutionState_EXECUTION_STATE_SUCCEEDED, 0, "")

	if snapshot.State != pb.ExecutionState_EXECUTION_STATE_RUNNING {
		t.Fatalf("snapshot state = %v, want running", snapshot.State)
	}
	if snapshot.Stdout != "before" {
		t.Fatalf("snapshot stdout = %q, want immutable initial output", snapshot.Stdout)
	}
	current := entry.snapshot("immutable")
	if current.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Fatalf("current state = %v, want succeeded", current.State)
	}
	if current.Stdout != "before-after" {
		t.Fatalf("current stdout = %q, want all output", current.Stdout)
	}
}

func TestConcurrentStatusListAndReplacement(t *testing.T) {
	client, cleanup := startTestServerWithOutputLimit(t, 256)
	defer cleanup()

	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "concurrent",
		Args: []string{"while :; do echo stdout; echo stderr >&2; done"},
	}); err != nil {
		t.Fatalf("initial Execute: %v", err)
	}

	start := make(chan struct{})
	errCh := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(list bool) {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				var err error
				if list {
					_, err = client.List(ctx, &pb.ListRequest{})
				} else {
					_, err = client.Status(ctx, &pb.StatusRequest{Id: "concurrent"})
				}
				if err != nil {
					errCh <- err
					return
				}
			}
		}(i%2 == 0)
	}
	close(start)

	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "concurrent",
		Args: []string{"printf replacement"},
	}); err != nil {
		t.Fatalf("replacement Execute: %v", err)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent status operation: %v", err)
	}

	resp := waitForTerminalStatus(t, client, "concurrent")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Fatalf("replacement state = %v, want succeeded: %s", resp.State, resp.ErrorMessage)
	}
	if resp.Stdout != "replacement" {
		t.Fatalf("replacement stdout = %q", resp.Stdout)
	}
}

func TestCancelTerminatesProcessGroup(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	workDir := t.TempDir()
	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:         "cancel-process-group",
		WorkingDir: workDir,
		Args:       []string{"sleep 30 & echo $! > child.pid; wait"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	pidPath := filepath.Join(workDir, "child.pid")
	var childPID int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		content, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(content)))
			if err != nil {
				t.Fatalf("parse child pid: %v", err)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child process pid was not written")
	}

	if _, err := client.Cancel(ctx, &pb.CancelRequest{Id: "cancel-process-group"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d still exists after cancellation", childPID)
}

func TestCancelKillsProcessIgnoringSIGTERM(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	workDir := t.TempDir()
	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:         "cancel-stubborn-process",
		WorkingDir: workDir,
		Args:       []string{"(trap '' TERM; while :; do sleep 30; done) & echo $! > child.pid; wait"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	childPID := waitForPIDFile(t, filepath.Join(workDir, "child.pid"))

	started := time.Now()
	if _, err := client.Cancel(ctx, &pb.CancelRequest{Id: "cancel-stubborn-process"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if elapsed := time.Since(started); elapsed < processTerminationGrace {
		t.Fatalf("Cancel returned after %v, before termination grace %v", elapsed, processTerminationGrace)
	}
	waitForProcessExit(t, childPID)
}

func TestTimeoutKillsProcessGroupAndWaits(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	workDir := t.TempDir()
	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:             "timeout-process-group",
		WorkingDir:     workDir,
		TimeoutSeconds: 1,
		Args:           []string{"(trap '' TERM; while :; do sleep 30; done) & echo $! > child.pid; wait"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	childPID := waitForPIDFile(t, filepath.Join(workDir, "child.pid"))

	resp := waitForTerminalStatus(t, client, "timeout-process-group")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_FAILED {
		t.Fatalf("state = %v, want failed", resp.State)
	}
	if resp.ErrorMessage != "timeout" {
		t.Fatalf("error message = %q, want timeout", resp.ErrorMessage)
	}
	if resp.ExitCode != -1 {
		t.Fatalf("exit code = %d, want -1", resp.ExitCode)
	}
	waitForProcessExit(t, childPID)
}

func TestRejectsEscapingEntrypoint(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "script"), []byte("echo ok\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	if _, err := client.Execute(context.Background(), &pb.ExecuteRequest{
		Id:         "bad-entrypoint",
		WorkingDir: workDir,
		Entrypoint: "../escape.sh",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	resp := waitForTerminalStatus(t, client, "bad-entrypoint")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_FAILED {
		t.Fatalf("state = %v, want failed", resp.State)
	}
	if !strings.Contains(resp.ErrorMessage, "entrypoint") {
		t.Fatalf("error message = %q, want entrypoint validation", resp.ErrorMessage)
	}
}

func waitForTerminalStatus(t *testing.T, client pb.RuntimeClient, id string) *pb.StatusResponse {
	t.Helper()
	ctx := context.Background()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Status(ctx, &pb.StatusRequest{Id: id})
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		switch resp.State {
		case pb.ExecutionState_EXECUTION_STATE_SUCCEEDED, pb.ExecutionState_EXECUTION_STATE_FAILED:
			return resp
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for execution %s", id)
	return nil
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		content, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
			if err != nil {
				t.Fatalf("parse pid from %s: %v", path, err)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still exists", pid)
}
