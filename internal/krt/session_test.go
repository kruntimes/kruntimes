package krt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/sdk/go/sandbox"
)

type sessionTestHTTPDoer struct{}

func (sessionTestHTTPDoer) Do(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("unexpected HTTP request")
}

func testKRTSession(t *testing.T, handler func(*websocket.Conn)) *sandbox.Session {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/namespaces/default/runtimes/bash/sessions/run-uid/operations:ws" {
			t.Errorf("WebSocket path = %q", r.URL.Path)
		}
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade WebSocket: %v", err)
			return
		}
		defer func() { _ = connection.Close() }()
		handler(connection)
	}))
	t.Cleanup(server.Close)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: "default", UID: types.UID("run-uid")},
		Spec:       v1alpha1.RunSpec{Runtime: "bash", Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status: v1alpha1.RunStatus{Phase: v1alpha1.RunReady, Endpoint: &v1alpha1.RunEndpoint{
			Protocol: v1alpha1.RunEndpointProtocolHTTP,
			URL:      server.URL + "/v1/namespaces/default/runtimes/bash/sessions/run-uid",
		}},
	}
	client, err := sandbox.New(sandbox.Config{
		Runs: fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build(), HTTPClient: sessionTestHTTPDoer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := client.Open(t.Context(), "default", "ready")
	if err != nil {
		t.Fatal(err)
	}
	session, err := opened.OpenSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestSessionOperationRendersOutputAndCommandExit(t *testing.T) {
	session := testKRTSession(t, func(connection *websocket.Conn) {
		var send struct {
			Type      string `json:"type"`
			Operation struct {
				Command sandbox.Command `json:"command"`
			} `json:"operation"`
		}
		if err := connection.ReadJSON(&send); err != nil {
			t.Errorf("read operation: %v", err)
			return
		}
		if send.Type != "send" || len(send.Operation.Command.Argv) != 2 || send.Operation.Command.Argv[0] != "sh" {
			t.Errorf("send = %#v", send)
		}
		for _, frame := range []string{
			`{"sequence":1,"type":"accepted","accepted":{"operationID":"op-1"}}`,
			`{"sequence":2,"type":"output","output":{"stream":"stdout","data":"aGVsbG8="}}`,
			`{"sequence":3,"type":"output","output":{"stream":"stderr","data":"d2FybmluZwo="}}`,
			`{"sequence":4,"type":"progress","progress":{"kind":"status","message":"working"}}`,
			`{"sequence":5,"type":"completed","completed":{"command":{"exitCode":7}}}`,
		} {
			if err := connection.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
				t.Errorf("write event: %v", err)
				return
			}
		}
	})
	var stdout, stderr bytes.Buffer
	err := runSessionOperation(t.Context(), session, sandbox.Operation{Command: &sandbox.Command{Argv: []string{"sh", "-c"}}}, sessionOutputText, &stdout, &stderr)
	if !isSessionOperationFailure(err) || !strings.Contains(err.Error(), "status 7") {
		t.Fatalf("operation error = %v", err)
	}
	if stdout.String() != "hello" {
		t.Errorf("stdout = %q", stdout.String())
	}
	for _, want := range []string{"accepted: op-1", "warning\n", "working", "completed"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q lacks %q", stderr.String(), want)
		}
	}
}

func TestSessionCommandTimeoutIsFailure(t *testing.T) {
	session := testKRTSession(t, func(connection *websocket.Conn) {
		var send any
		if err := connection.ReadJSON(&send); err != nil {
			t.Errorf("read operation: %v", err)
			return
		}
		_ = connection.WriteJSON(map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		_ = connection.WriteJSON(map[string]any{"sequence": 2, "type": "completed", "completed": map[string]any{"command": map[string]any{"exitCode": 0, "timedOut": true}}})
	})
	err := runSessionOperation(t.Context(), session, sandbox.Operation{Command: &sandbox.Command{Shell: "sleep 10"}}, sessionOutputText, io.Discard, io.Discard)
	if !isSessionOperationFailure(err) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("operation error = %v, want timeout", err)
	}
}

func TestSessionAttachAcceptsNDJSONOperationsAndEmitsEvents(t *testing.T) {
	session := testKRTSession(t, func(connection *websocket.Conn) {
		for index := 1; index <= 2; index++ {
			var send struct {
				Type      string            `json:"type"`
				Operation sandbox.Operation `json:"operation"`
			}
			if err := connection.ReadJSON(&send); err != nil {
				t.Errorf("read operation %d: %v", index, err)
				return
			}
			if send.Type != "send" || send.Operation.Command == nil || send.Operation.Command.Shell != fmt.Sprintf("echo %d", index) {
				t.Errorf("operation %d = %#v", index, send)
			}
			for _, frame := range []string{
				fmt.Sprintf(`{"sequence":1,"type":"accepted","accepted":{"operationID":"op-%d"}}`, index),
				`{"sequence":2,"type":"completed","completed":{"command":{"exitCode":0}}}`,
			} {
				if err := connection.WriteMessage(websocket.TextMessage, []byte(frame)); err != nil {
					t.Errorf("write event: %v", err)
					return
				}
			}
		}
	})
	var stdout, stderr bytes.Buffer
	input := strings.NewReader("{\"command\":{\"shell\":\"echo 1\"}}\n{\"command\":{\"shell\":\"echo 2\"}}\n")
	if err := attachSession(t.Context(), session, input, &stdout, &stderr, sessionOutputNDJSON, sessionOutputNDJSON); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("NDJSON lines = %d, want 4: %q", len(lines), stdout.String())
	}
	for _, line := range lines {
		var event sandbox.OperationEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid NDJSON event %q: %v", line, err)
		}
	}
	if strings.Contains(stderr.String(), "session>") {
		t.Errorf("non-interactive attach printed prompt: %q", stderr.String())
	}
}

func TestSessionAttachKeepsFailureExitAfterLaterSuccess(t *testing.T) {
	session := testKRTSession(t, func(connection *websocket.Conn) {
		for index := 1; index <= 2; index++ {
			var send any
			if err := connection.ReadJSON(&send); err != nil {
				t.Errorf("read operation %d: %v", index, err)
				return
			}
			_ = connection.WriteJSON(map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": fmt.Sprintf("op-%d", index)}})
			exitCode := 0
			if index == 1 {
				exitCode = 7
			}
			_ = connection.WriteJSON(map[string]any{"sequence": 2, "type": "completed", "completed": map[string]any{"command": map[string]int{"exitCode": exitCode}}})
		}
	})
	var stdout, stderr bytes.Buffer
	err := attachSession(t.Context(), session, strings.NewReader("exit 7\necho recovered\n"), &stdout, &stderr, "command", sessionOutputText)
	if !isSessionOperationFailure(err) || !strings.Contains(err.Error(), "status 7") {
		t.Fatalf("attach result = %v, want first operation failure", err)
	}
	if got := strings.Count(stderr.String(), "Session operation completed"); got != 2 {
		t.Fatalf("completed operations = %d, want 2: %q", got, stderr.String())
	}
}

type sessionCancelWriter struct{ cancel context.CancelFunc }

func (w sessionCancelWriter) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}

func TestSessionInterruptSendsOperationIDBeforeClosing(t *testing.T) {
	cancelled := make(chan string, 1)
	session := testKRTSession(t, func(connection *websocket.Conn) {
		var send any
		if err := connection.ReadJSON(&send); err != nil {
			t.Errorf("read send: %v", err)
			return
		}
		if err := connection.WriteJSON(map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}}); err != nil {
			t.Errorf("write accepted: %v", err)
			return
		}
		var cancel struct {
			Type        string `json:"type"`
			OperationID string `json:"operationID"`
		}
		if err := connection.ReadJSON(&cancel); err != nil {
			t.Errorf("read cancel: %v", err)
			return
		}
		if cancel.Type != "cancel" {
			t.Errorf("cancel type = %q", cancel.Type)
		}
		cancelled <- cancel.OperationID
		_ = connection.WriteJSON(map[string]any{"sequence": 2, "type": "failed", "failed": map[string]any{"code": 1, "message": "cancelled"}})
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := runSessionOperation(ctx, session, sandbox.Operation{Command: &sandbox.Command{Shell: "sleep 10"}}, sessionOutputText, io.Discard, sessionCancelWriter{cancel: cancel})
	if err == nil {
		t.Fatal("interrupted operation returned success")
	}
	select {
	case got := <-cancelled:
		if got != "op-1" {
			t.Errorf("cancelled operation = %q", got)
		}
	default:
		t.Fatal("no cancel frame received")
	}
}

func TestRunModeName(t *testing.T) {
	for _, item := range []struct {
		mode v1alpha1.RunMode
		want string
	}{
		{v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}}, "task"},
		{v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}, "session"},
		{v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{}}, "function"},
	} {
		if got := item.mode.Name(); got != item.want {
			t.Errorf("mode name = %q, want %q", got, item.want)
		}
	}
}

func TestRunListShowsEachMode(t *testing.T) {
	runs := &v1alpha1.RunList{Items: []v1alpha1.Run{
		{ObjectMeta: metav1.ObjectMeta{Name: "one"}, Spec: v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "two"}, Spec: v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "three"}, Spec: v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{}}}},
	}}
	var output bytes.Buffer
	if err := writeRunListTable(&output, runs); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"MODE", "one", "task", "two", "session", "three", "function"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("Run list %q lacks %q", output.String(), want)
		}
	}
}

func TestConsoleServiceFromEndpoint(t *testing.T) {
	namespace, service, port, ok := consoleServiceFromEndpoint("https://kruntimes-console.platform.svc:8443/v1/namespaces/work/runtimes/bash/sessions/uid")
	if !ok || namespace != "platform" || service != "kruntimes-console" || port != 8443 {
		t.Fatalf("Console service = %q, %q, %d, %t", namespace, service, port, ok)
	}
	if _, _, _, ok := consoleServiceFromEndpoint("https://gateway.example.com/v1/sessions/uid"); ok {
		t.Fatal("external endpoint treated as in-cluster Console service")
	}
}
