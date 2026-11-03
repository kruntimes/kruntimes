package runextensionapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newExecTestExecutor(t *testing.T, handler func(*websocket.Conn)) *SessionExecutor {
	t.Helper()
	upgrader := websocket.Upgrader{}
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade Session connection: %v", err)
			return
		}
		defer func() { _ = connection.Close() }()
		handler(connection)
	}))
	executor, err := client.Sessions().NewExecutor(t.Context(), "team-a", "shell", nil)
	if err != nil {
		t.Fatalf("NewExecutor() error = %v", err)
	}
	t.Cleanup(func() { _ = executor.Close() })
	return executor
}

// waitForSessionClose blocks until the client closes the Session connection, so
// the test server never closes it before the client consumed every frame.
func waitForSessionClose(connection *websocket.Conn) {
	_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		if _, _, err := connection.ReadMessage(); err != nil {
			return
		}
	}
}

func readExecSend(t *testing.T, connection *websocket.Conn) sessionSendFrame {
	t.Helper()
	var send sessionSendFrame
	if err := connection.ReadJSON(&send); err != nil {
		t.Errorf("read Session send frame: %v", err)
	}
	return send
}

func writeExecFrame(t *testing.T, connection *websocket.Conn, frame map[string]any) {
	t.Helper()
	if err := connection.WriteJSON(frame); err != nil {
		t.Errorf("write Session event: %v", err)
	}
}

func TestExecStreamsEventsAndReturnsCommandResult(t *testing.T) {
	executor := newExecTestExecutor(t, func(connection *websocket.Conn) {
		send := readExecSend(t, connection)
		if send.Type != "send" || send.Operation.Command == nil || len(send.Operation.Command.Argv) != 2 {
			t.Errorf("send frame = %#v", send)
		}
		writeExecFrame(t, connection, map[string]any{"type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeExecFrame(t, connection, map[string]any{"type": "output", "output": map[string]any{"stream": "stdout", "data": []byte("hello")}})
		writeExecFrame(t, connection, map[string]any{"type": "output", "output": map[string]any{"stream": "stderr", "data": []byte("warn\n")}})
		writeExecFrame(t, connection, map[string]any{"type": "progress", "progress": map[string]any{"kind": "status", "message": "working"}})
		writeExecFrame(t, connection, map[string]any{"type": "completed", "completed": map[string]any{"command": map[string]any{"exitCode": 0}}})
		waitForSessionClose(connection)
	})
	var events []OperationEvent
	result, err := executor.Exec(t.Context(), Operation{Command: &Command{Argv: []string{"sh", "-c"}}}, &ExecOptions{
		OnEvent: func(event OperationEvent) error {
			events = append(events, event)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Exec() error = %v", err)
	}
	if result.OperationID != "op-1" {
		t.Fatalf("operation ID = %q", result.OperationID)
	}
	if result.Command == nil || result.Command.ExitCode != 0 {
		t.Fatalf("command result = %#v", result.Command)
	}
	if got, want := string(result.Command.Stdout), "hello"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got, want := string(result.Command.Stderr), "warn\n"; got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
	wantTypes := []string{"accepted", "output", "output", "progress", "completed"}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %d, want %d", len(events), len(wantTypes))
	}
	for index, want := range wantTypes {
		if events[index].Type != want {
			t.Errorf("event %d type = %q, want %q", index, events[index].Type, want)
		}
	}
	if events[0].Accepted == nil || events[0].Accepted.OperationID != "op-1" || events[0].Sequence != 1 {
		t.Errorf("accepted event = %#v", events[0])
	}
}

func TestExecReturnsOperationFailedError(t *testing.T) {
	executor := newExecTestExecutor(t, func(connection *websocket.Conn) {
		readExecSend(t, connection)
		writeExecFrame(t, connection, map[string]any{"type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeExecFrame(t, connection, map[string]any{"type": "failed", "failed": map[string]any{"code": 13, "message": "boom"}})
		waitForSessionClose(connection)
	})
	result, err := executor.Exec(t.Context(), Operation{Command: &Command{Shell: "false"}}, nil)
	if err == nil {
		t.Fatal("Exec() returned success for a failed operation")
	}
	var failed *OperationFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("error = %v, want OperationFailedError", err)
	}
	if failed.Code != 13 || failed.Message != "boom" {
		t.Errorf("failure = %#v", failed)
	}
	if got, want := failed.Error(), "Session operation failed (13): boom"; got != want {
		t.Errorf("failure message = %q, want %q", got, want)
	}
	if result == nil || result.OperationID != "op-1" || result.Command != nil {
		t.Errorf("result = %#v", result)
	}
}

func TestExecReturnsCommandOutcomes(t *testing.T) {
	for _, test := range []struct {
		name         string
		command      map[string]any
		wantExitCode int32
		wantTimedOut bool
		wantCommand  bool
	}{
		{name: "zero exit", command: map[string]any{"exitCode": 0}, wantCommand: true},
		{name: "non-zero exit", command: map[string]any{"exitCode": 7}, wantExitCode: 7, wantCommand: true},
		{name: "timed out", command: map[string]any{"exitCode": 124, "timedOut": true}, wantExitCode: 124, wantTimedOut: true, wantCommand: true},
		{name: "file operation", wantCommand: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			executor := newExecTestExecutor(t, func(connection *websocket.Conn) {
				readExecSend(t, connection)
				writeExecFrame(t, connection, map[string]any{"type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
				completed := map[string]any{}
				if test.command != nil {
					completed["command"] = test.command
				}
				writeExecFrame(t, connection, map[string]any{"type": "completed", "completed": completed})
				waitForSessionClose(connection)
			})
			result, err := executor.Exec(t.Context(), Operation{WriteFile: &FileWrite{Path: "notes/a.txt", Contents: []byte("a")}}, nil)
			if err != nil {
				t.Fatalf("Exec() error = %v", err)
			}
			if !test.wantCommand {
				if result.Command != nil {
					t.Fatalf("command result = %#v, want nil", result.Command)
				}
				return
			}
			if result.Command == nil {
				t.Fatal("command result is nil")
			}
			if result.Command.ExitCode != test.wantExitCode || result.Command.TimedOut != test.wantTimedOut {
				t.Errorf("command result = %#v", result.Command)
			}
		})
	}
}

func TestExecCancellationSendsCancelFrame(t *testing.T) {
	cancelFrame := make(chan string, 1)
	executor := newExecTestExecutor(t, func(connection *websocket.Conn) {
		readExecSend(t, connection)
		writeExecFrame(t, connection, map[string]any{"type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		var frame struct {
			Type        string `json:"type"`
			OperationID string `json:"operationID"`
		}
		if err := connection.ReadJSON(&frame); err != nil {
			t.Errorf("read cancel frame: %v", err)
			return
		}
		if frame.Type != "cancel" {
			t.Errorf("cancel frame type = %q", frame.Type)
		}
		cancelFrame <- frame.OperationID
		writeExecFrame(t, connection, map[string]any{"type": "failed", "failed": map[string]any{"code": 1, "message": "cancelled"}})
		waitForSessionClose(connection)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result, err := executor.Exec(ctx, Operation{Command: &Command{Shell: "sleep 10"}}, &ExecOptions{
		OnEvent: func(event OperationEvent) error {
			if event.Type == "accepted" {
				cancel()
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("Exec() returned success for a canceled operation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if result == nil || result.OperationID != "op-1" {
		t.Errorf("result = %#v", result)
	}
	select {
	case operationID := <-cancelFrame:
		if operationID != "op-1" {
			t.Errorf("cancelled operation ID = %q", operationID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client did not send a cancel frame")
	}
}

func TestExecReturnsTransportError(t *testing.T) {
	executor := newExecTestExecutor(t, func(connection *websocket.Conn) {
		readExecSend(t, connection)
		writeExecFrame(t, connection, map[string]any{"type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		// Returning closes the connection before the terminal event.
	})
	_, err := executor.Exec(t.Context(), Operation{Command: &Command{Shell: "echo ok"}}, nil)
	if err == nil {
		t.Fatal("Exec() returned success after the transport failed")
	}
	var transport *TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("error = %v, want TransportError", err)
	}
}

func TestExecReusesConnectionAfterTerminalEvent(t *testing.T) {
	executor := newExecTestExecutor(t, func(connection *websocket.Conn) {
		for index := 1; index <= 2; index++ {
			send := readExecSend(t, connection)
			if send.Operation.Command == nil {
				t.Errorf("send %d operation = %#v", index, send.Operation)
			}
			writeExecFrame(t, connection, map[string]any{"type": "accepted", "accepted": map[string]string{"operationID": "op-" + string(rune('0'+index))}})
			writeExecFrame(t, connection, map[string]any{"type": "completed", "completed": map[string]any{"command": map[string]any{"exitCode": 0}}})
		}
		waitForSessionClose(connection)
	})
	for index := 1; index <= 2; index++ {
		result, err := executor.Exec(t.Context(), Operation{Command: &Command{Shell: "echo ok"}}, nil)
		if err != nil {
			t.Fatalf("Exec(%d) error = %v", index, err)
		}
		if want := fmt.Sprintf("op-%d", index); result.OperationID != want {
			t.Errorf("operation ID = %q, want %q", result.OperationID, want)
		}
	}
}

func TestExecStopsOnEventCallbackError(t *testing.T) {
	callbackErr := errors.New("render failed")
	executor := newExecTestExecutor(t, func(connection *websocket.Conn) {
		readExecSend(t, connection)
		writeExecFrame(t, connection, map[string]any{"type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeExecFrame(t, connection, map[string]any{"type": "output", "output": map[string]any{"stream": "stdout", "data": []byte("hello")}})
		waitForSessionClose(connection)
	})
	_, err := executor.Exec(t.Context(), Operation{Command: &Command{Shell: "echo hello"}}, &ExecOptions{
		OnEvent: func(event OperationEvent) error {
			if event.Type == "output" {
				return callbackErr
			}
			return nil
		},
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("error = %v, want callback error", err)
	}
}

func TestSessionsExecOpensAndClosesConnection(t *testing.T) {
	closed := make(chan struct{})
	upgrader := websocket.Upgrader{}
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade Session connection: %v", err)
			return
		}
		defer func() { _ = connection.Close() }()
		readExecSend(t, connection)
		writeExecFrame(t, connection, map[string]any{"type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		writeExecFrame(t, connection, map[string]any{"type": "completed", "completed": map[string]any{"command": map[string]any{"exitCode": 0}}})
		_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				close(closed)
				return
			}
		}
	}))
	result, err := client.Sessions().Exec(t.Context(), "team-a", "shell", Operation{Command: &Command{Shell: "echo ok"}}, &ExecOptions{HeartbeatInterval: time.Second})
	if err != nil {
		t.Fatalf("Sessions().Exec() error = %v", err)
	}
	if result.Command == nil || result.Command.ExitCode != 0 {
		t.Fatalf("command result = %#v", result.Command)
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Sessions().Exec() did not close the Session connection")
	}
}

func TestExecAfterCloseReturnsTransportError(t *testing.T) {
	executor := newExecTestExecutor(t, func(connection *websocket.Conn) {
		waitForSessionClose(connection)
	})
	if err := executor.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	_, err := executor.Exec(t.Context(), Operation{Command: &Command{Shell: "echo ok"}}, nil)
	if err == nil {
		t.Fatal("Exec() returned success on a closed Session connection")
	}
	var transport *TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("error = %v, want TransportError", err)
	}
}
