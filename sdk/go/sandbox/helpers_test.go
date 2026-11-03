package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	k8sclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

var helperUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// helperSandbox returns a ready Sandbox whose aggregated Session endpoint is
// served by handler, plus the fake Run client so tests can prove the helpers
// never release the backing Run.
func helperSandbox(t *testing.T, handler http.HandlerFunc) (*Sandbox, k8sclient.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	run := readySessionRun()
	runs := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := New(Config{RESTConfig: &rest.Config{Host: server.URL, BearerToken: "token"}, Runs: runs})
	if err != nil {
		t.Fatal(err)
	}
	return &Sandbox{client: client, run: run.DeepCopy()}, runs
}

func dialSession(t *testing.T, writer http.ResponseWriter, request *http.Request) *websocket.Conn {
	t.Helper()
	connection, err := helperUpgrader.Upgrade(writer, request, nil)
	if err != nil {
		t.Errorf("upgrade Session connection: %v", err)
		return nil
	}
	return connection
}

func readSendFrame(t *testing.T, connection *websocket.Conn) struct {
	Type      string    `json:"type"`
	Operation Operation `json:"operation"`
} {
	t.Helper()
	var frame struct {
		Type      string    `json:"type"`
		Operation Operation `json:"operation"`
	}
	if err := connection.ReadJSON(&frame); err != nil {
		t.Errorf("read send frame: %v", err)
	}
	if frame.Type != "send" {
		t.Errorf("first Session frame type = %q, want send", frame.Type)
	}
	return frame
}

func writeEvent(t *testing.T, connection *websocket.Conn, value any) {
	t.Helper()
	if err := connection.WriteJSON(value); err != nil {
		t.Errorf("write Session event: %v", err)
	}
}

func writeAcceptedAndCompleted(t *testing.T, connection *websocket.Conn) {
	t.Helper()
	writeEvent(t, connection, map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
	writeEvent(t, connection, map[string]any{"sequence": 2, "type": "completed", "completed": map[string]any{}})
}

func runExists(t *testing.T, runs k8sclient.Client) *v1alpha1.Run {
	t.Helper()
	run := &v1alpha1.Run{}
	if err := runs.Get(t.Context(), k8sclient.ObjectKey{Namespace: "default", Name: "session"}, run); err != nil {
		t.Fatalf("Run was released by a helper: %v", err)
	}
	return run
}

func TestSandboxRunCommandStreamsOutputAndReturnsResult(t *testing.T) {
	server := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := dialSession(t, writer, request)
		if connection == nil {
			return
		}
		defer connection.Close()
		frame := readSendFrame(t, connection)
		if frame.Operation.Command == nil || len(frame.Operation.Command.Argv) != 2 {
			t.Errorf("operation = %#v, want one command", frame.Operation)
			return
		}
		writeEvent(t, connection, map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeEvent(t, connection, map[string]any{"sequence": 2, "type": "output", "output": map[string]any{"stream": "stdout", "data": []byte("hi")}})
		writeEvent(t, connection, map[string]any{"sequence": 3, "type": "output", "output": map[string]any{"stream": "stderr", "data": []byte("oo")}})
		writeEvent(t, connection, map[string]any{"sequence": 4, "type": "progress", "progress": map[string]any{"kind": "status", "message": "working"}})
		writeEvent(t, connection, map[string]any{"sequence": 5, "type": "completed", "completed": map[string]any{"command": map[string]any{"exitCode": 0}}})
		// Keep the connection open until the helper closes it.
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	})
	sandbox, runs := helperSandbox(t, server)

	var stdout, stderr []byte
	var progress []string
	result, err := sandbox.RunCommand(t.Context(), Command{Argv: []string{"echo", "ok"}}, &CommandOptions{
		OnStdout:   func(data []byte) error { stdout = append(stdout, data...); return nil },
		OnStderr:   func(data []byte) error { stderr = append(stderr, data...); return nil },
		OnProgress: func(event OperationProgress) error { progress = append(progress, event.Kind); return nil },
	})
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if string(stdout) != "hi" || string(stderr) != "oo" || len(progress) != 1 || progress[0] != "status" {
		t.Fatalf("callbacks: stdout=%q stderr=%q progress=%v", stdout, stderr, progress)
	}
	if result.ExitCode != 0 || string(result.Stdout) != "hi" || string(result.Stderr) != "oo" || result.TimedOut {
		t.Fatalf("result = %#v", result)
	}
	if run := runExists(t, runs); run.Spec.Termination != nil {
		t.Fatalf("helper requested termination: %#v", run.Spec.Termination)
	}
}

func TestSandboxRunCommandNonZeroExitIsTerminalResult(t *testing.T) {
	server := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := dialSession(t, writer, request)
		if connection == nil {
			return
		}
		defer connection.Close()
		_ = readSendFrame(t, connection)
		writeEvent(t, connection, map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeEvent(t, connection, map[string]any{"sequence": 2, "type": "completed", "completed": map[string]any{"command": map[string]any{"exitCode": 2, "timedOut": true}}})
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	})
	sandbox, runs := helperSandbox(t, server)

	result, err := sandbox.RunCommand(t.Context(), Command{Shell: "exit 2"}, nil)
	if err != nil {
		t.Fatalf("RunCommand returned an error for a non-zero exit: %v", err)
	}
	if result.ExitCode != 2 || !result.TimedOut {
		t.Fatalf("result = %#v, want exit 2 and timed out", result)
	}
	runExists(t, runs)
}

func TestSandboxRunCommandFailedEventIsOperationFailedError(t *testing.T) {
	server := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := dialSession(t, writer, request)
		if connection == nil {
			return
		}
		defer connection.Close()
		_ = readSendFrame(t, connection)
		writeEvent(t, connection, map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeEvent(t, connection, map[string]any{"sequence": 2, "type": "failed", "failed": map[string]any{"code": 9, "message": "denied"}})
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	})
	sandbox, runs := helperSandbox(t, server)

	_, err := sandbox.RunCommand(t.Context(), Command{Argv: []string{"true"}}, nil)
	var failed *OperationFailedError
	if !errors.As(err, &failed) || failed.Code != 9 || failed.Message != "denied" {
		t.Fatalf("RunCommand error = %T %v, want OperationFailedError", err, err)
	}
	runExists(t, runs)
}

func TestSandboxRunCommandCancellationSendsCancelFrame(t *testing.T) {
	cancelled := make(chan string, 1)
	server := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := dialSession(t, writer, request)
		if connection == nil {
			return
		}
		defer connection.Close()
		_ = readSendFrame(t, connection)
		writeEvent(t, connection, map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeEvent(t, connection, map[string]any{"sequence": 2, "type": "output", "output": map[string]any{"stream": "stdout", "data": []byte("tick")}})
		for {
			var frame struct {
				Type        string `json:"type"`
				OperationID string `json:"operationID"`
			}
			if err := connection.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type == "cancel" {
				cancelled <- frame.OperationID
				return
			}
		}
	})
	sandbox, runs := helperSandbox(t, server)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := sandbox.RunCommand(ctx, Command{Shell: "sleep 60"}, &CommandOptions{
		OnStdout: func([]byte) error {
			cancel()
			return nil
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RunCommand error = %T %v, want context.Canceled", err, err)
	}
	select {
	case operationID := <-cancelled:
		if operationID != "op-1" {
			t.Fatalf("cancel frame operation = %q, want op-1", operationID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunCommand did not send a cancel frame")
	}
	runExists(t, runs)
}

func TestSandboxRunCommandTransportFailureIsTransportErrorWithoutReplay(t *testing.T) {
	var connections atomic.Int32
	server := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connections.Add(1)
		connection := dialSession(t, writer, request)
		if connection == nil {
			return
		}
		_ = readSendFrame(t, connection)
		writeEvent(t, connection, map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		// Drop the connection without a terminal event. The helper must not
		// reconnect or replay the command.
		_ = connection.Close()
	})
	sandbox, runs := helperSandbox(t, server)

	_, err := sandbox.RunCommand(t.Context(), Command{Argv: []string{"true"}}, nil)
	var transport *TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("RunCommand error = %T %v, want TransportError", err, err)
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("Session connections = %d, want 1 (no replay)", got)
	}
	runExists(t, runs)
}

func TestSandboxRunCommandClosesItsConnection(t *testing.T) {
	closed := make(chan struct{})
	server := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := dialSession(t, writer, request)
		if connection == nil {
			return
		}
		defer connection.Close()
		_ = readSendFrame(t, connection)
		writeEvent(t, connection, map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeEvent(t, connection, map[string]any{"sequence": 2, "type": "completed", "completed": map[string]any{"command": map[string]any{"exitCode": 0}}})
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				close(closed)
				return
			}
		}
	})
	sandbox, runs := helperSandbox(t, server)

	if _, err := sandbox.RunCommand(t.Context(), Command{Argv: []string{"true"}}, nil); err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("RunCommand returned without closing its Session connection")
	}
	runExists(t, runs)
}

func TestSandboxFileHelpersSendOneShotOperations(t *testing.T) {
	operations := make(chan Operation, 4)
	server := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := dialSession(t, writer, request)
		if connection == nil {
			return
		}
		defer connection.Close()
		frame := readSendFrame(t, connection)
		operations <- frame.Operation
		writeAcceptedAndCompleted(t, connection)
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	})
	sandbox, runs := helperSandbox(t, server)

	if err := sandbox.WriteFile(t.Context(), "notes/a.txt", []byte("hi"), &WriteFileOptions{CreateParents: true}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := sandbox.CreateDirectory(t.Context(), "notes"); err != nil {
		t.Fatalf("CreateDirectory: %v", err)
	}
	if err := sandbox.DeleteFile(t.Context(), "old", &DeleteFileOptions{Recursive: true}); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if err := sandbox.RenameFile(t.Context(), "a.txt", "b.txt", &RenameFileOptions{Overwrite: true}); err != nil {
		t.Fatalf("RenameFile: %v", err)
	}

	write := <-operations
	if write.WriteFile == nil || write.WriteFile.Path != "notes/a.txt" || string(write.WriteFile.Contents) != "hi" || !write.WriteFile.CreateParents {
		t.Fatalf("writeFile operation = %#v", write)
	}
	mkdir := <-operations
	if mkdir.CreateDirectory == nil || mkdir.CreateDirectory.Path != "notes" {
		t.Fatalf("createDirectory operation = %#v", mkdir)
	}
	remove := <-operations
	if remove.DeleteFile == nil || remove.DeleteFile.Path != "old" || !remove.DeleteFile.Recursive {
		t.Fatalf("deleteFile operation = %#v", remove)
	}
	rename := <-operations
	if rename.RenameFile == nil || rename.RenameFile.SourcePath != "a.txt" || rename.RenameFile.DestinationPath != "b.txt" || !rename.RenameFile.Overwrite {
		t.Fatalf("renameFile operation = %#v", rename)
	}
	runExists(t, runs)
}

func TestSandboxFileHelperFailureIsOperationFailedError(t *testing.T) {
	server := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection := dialSession(t, writer, request)
		if connection == nil {
			return
		}
		defer connection.Close()
		_ = readSendFrame(t, connection)
		writeEvent(t, connection, map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeEvent(t, connection, map[string]any{"sequence": 2, "type": "failed", "failed": map[string]any{"code": 7, "message": "permission denied"}})
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
		}
	})
	sandbox, runs := helperSandbox(t, server)

	err := sandbox.WriteFile(t.Context(), "notes/a.txt", []byte("hi"), nil)
	var failed *OperationFailedError
	if !errors.As(err, &failed) || failed.Code != 7 {
		t.Fatalf("WriteFile error = %T %v, want OperationFailedError", err, err)
	}
	runExists(t, runs)
}
