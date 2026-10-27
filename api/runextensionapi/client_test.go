package runextensionapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/rest"
)

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewForConfig(&rest.Config{Host: server.URL, BearerToken: "caller-token"})
	if err != nil {
		t.Fatalf("NewForConfig() error = %v", err)
	}
	return client, server
}

func TestGetRunLogsBuildsAggregatedRequest(t *testing.T) {
	var seen *http.Request
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = request.Clone(context.Background())
		_, _ = io.WriteString(writer, `{"items":[]}`)
	}))
	response, err := client.GetRunLogs("team-a", "run-a", &RunLogOptions{TailLines: 25, Follow: true, Cursor: "cursor-1", TimeoutSeconds: 30}).Do(t.Context()).Raw()
	if err != nil {
		t.Fatalf("Raw() error = %v", err)
	}
	if string(response) != `{"items":[]}` {
		t.Fatalf("response = %q", response)
	}
	if seen == nil {
		t.Fatal("log request was not observed")
	}
	if got, want := seen.URL.Path, "/apis/logs.kruntimes.io/v1alpha1/namespaces/team-a/runs/run-a/log"; got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
	query := seen.URL.Query()
	if query.Get("tailLines") != "25" || query.Get("follow") != "true" || query.Get("cursor") != "cursor-1" || query.Get("timeoutSeconds") != "30" {
		t.Fatalf("query = %q", seen.URL.RawQuery)
	}
	if got := seen.Header.Get("Authorization"); got != "Bearer caller-token" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestWithBearerTokenForwardsRelayCredential(t *testing.T) {
	var seen string
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = request.Header.Get("Authorization")
		_, _ = io.WriteString(writer, `{"items":[]}`)
	}))
	if _, err := client.WithBearerToken("relay-token").GetRunLogs("team-a", "run-a", nil).Do(t.Context()).Raw(); err != nil {
		t.Fatalf("Raw() error = %v", err)
	}
	if seen != "Bearer relay-token" {
		t.Fatalf("Authorization = %q", seen)
	}
}

func TestOpenSessionUsesAggregatedPath(t *testing.T) {
	upgrader := websocket.Upgrader{}
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.URL.Path, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer caller-token" {
			t.Errorf("Authorization = %q", got)
		}
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		var send struct {
			Type      string    `json:"type"`
			Operation Operation `json:"operation"`
		}
		if err := connection.ReadJSON(&send); err != nil {
			t.Errorf("read send: %v", err)
			return
		}
		if send.Type != "send" || send.Operation.Command == nil || send.Operation.Command.Shell != "echo ok" {
			t.Errorf("send frame = %#v", send)
		}
		_ = connection.WriteJSON(map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "op-1"}})
		_ = connection.WriteJSON(map[string]any{"sequence": 2, "type": "completed", "completed": map[string]any{"command": map[string]any{"exitCode": 0}}})
	}))
	session, err := client.OpenSession(t.Context(), "team-a", "shell", nil)
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
	defer session.Close()
	operationID, err := session.Send(t.Context(), Operation{Command: &Command{Shell: "echo ok"}})
	if err != nil || operationID != "op-1" {
		t.Fatalf("Send() = %q, %v", operationID, err)
	}
	event, err := session.Receive(t.Context())
	if err != nil || event.Completed == nil {
		t.Fatalf("Receive() = %#v, %v", event, err)
	}
}

func TestSessionHeartbeatUsesConfiguredInterval(t *testing.T) {
	heartbeats := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{}
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			var frame struct {
				Type string `json:"type"`
			}
			if err := connection.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Type == "heartbeat" {
				heartbeats <- struct{}{}
				return
			}
		}
	}))
	session, err := client.OpenSession(t.Context(), "team-a", "shell", &SessionOptions{HeartbeatInterval: 100 * time.Millisecond})
	if err != nil {
		t.Fatalf("OpenSession() error = %v", err)
	}
	defer session.Close()
	select {
	case <-heartbeats:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not send a heartbeat frame")
	}
}

func TestListAndReadSessionFiles(t *testing.T) {
	var requests []string
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.URL.String())
		if strings.HasSuffix(request.URL.Path, "/files/notes/a.txt") {
			_, _ = io.WriteString(writer, `{"contents":"aGVsbG8=","truncated":true}`)
			return
		}
		_, _ = io.WriteString(writer, `{"entries":[{"path":"a.txt","sizeBytes":3}],"nextPageToken":"next"}`)
	}))
	page, err := client.ListSessionFiles(t.Context(), "team-a", "shell", &ListSessionFilesOptions{Directory: "notes", Limit: 2, PageToken: "after"})
	if err != nil {
		t.Fatalf("ListSessionFiles() error = %v", err)
	}
	if len(page.Entries) != 1 || page.Entries[0].Path != "a.txt" || page.NextPageToken != "next" {
		t.Fatalf("page = %#v", page)
	}
	contents, truncated, err := client.ReadSessionFile(t.Context(), "team-a", "shell", "notes/a.txt", 64)
	if err != nil {
		t.Fatalf("ReadSessionFile() error = %v", err)
	}
	if string(contents) != "hello" || !truncated {
		t.Fatalf("contents = %q truncated = %t", contents, truncated)
	}
	if len(requests) != 2 || !strings.Contains(requests[0], "path=notes") || !strings.Contains(requests[0], "limit=2") || !strings.Contains(requests[1], "maxBytes=64") {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestAPIErrorDecodesStatusAndBackendError(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(writer, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"forbidden by RBAC","code":403}`)
	}))
	_, _, err := client.ReadSessionFile(t.Context(), "team-a", "shell", "notes/a.txt", 0)
	var apiError *APIError
	if err == nil || !errors.As(err, &apiError) || apiError.StatusCode != http.StatusForbidden || apiError.Message != "forbidden by RBAC" {
		t.Fatalf("error = %v", err)
	}

	client, _ = newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(writer, `{"error":"Session is not active"}`)
	}))
	_, _, err = client.ReadSessionFile(t.Context(), "team-a", "shell", "notes/a.txt", 0)
	if err == nil || !errors.As(err, &apiError) || apiError.Message != "Session is not active" {
		t.Fatalf("error = %v", err)
	}
}

func TestInvokeFunctionSendsInputAndInvocationID(t *testing.T) {
	var body []byte
	client, _ := newTestClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.URL.Path, "/apis/functions.kruntimes.io/v1alpha1/namespaces/team-a/runs/handler/function"; got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
		if got := request.Header.Get("X-Kruntime-Invocation-ID"); got != "inv-1" {
			t.Errorf("invocation ID = %q", got)
		}
		body, _ = io.ReadAll(request.Body)
		_, _ = io.WriteString(writer, `{"invocationId":"inv-1","output":"ZG9uZQ==","contentType":"text/plain"}`)
	}))
	result, err := client.InvokeFunction(t.Context(), "team-a", "handler", &FunctionInvocationOptions{Input: []byte(`{"payload":true}`), InvocationID: "inv-1"})
	if err != nil {
		t.Fatalf("InvokeFunction() error = %v", err)
	}
	if string(body) != `{"payload":true}` {
		t.Fatalf("input body = %q", body)
	}
	if result.InvocationID != "inv-1" || string(result.Output) != "done" || result.ContentType != "text/plain" {
		t.Fatalf("result = %#v", result)
	}
	if _, err := client.InvokeFunction(t.Context(), "team-a", "handler", &FunctionInvocationOptions{Input: []byte("not-json")}); err == nil {
		t.Fatal("InvokeFunction accepted invalid JSON input")
	}
}

func TestGroupRESTInterfacesBuildAggregatedRequests(t *testing.T) {
	client, server := newTestClient(t, http.NewServeMux())
	if got, want := client.Sessions().Get().Namespace("team-a").Resource("runs").Name("shell").SubResource("session").URL().String(),
		server.URL+"/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session"; got != want {
		t.Fatalf("Session request URL = %q, want %q", got, want)
	}
	if got, want := client.Sessions().Get().Namespace("team-a").Resource("runs").Name("shell").SubResource("session").Suffix("files").URL().String(),
		server.URL+"/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files"; got != want {
		t.Fatalf("Session files request URL = %q, want %q", got, want)
	}
	if got, want := client.Functions().Post().Namespace("team-a").Resource("runs").Name("handler").SubResource("function").URL().String(),
		server.URL+"/apis/functions.kruntimes.io/v1alpha1/namespaces/team-a/runs/handler/function"; got != want {
		t.Fatalf("Function request URL = %q, want %q", got, want)
	}
	if got, want := client.Logs().Get().Namespace("team-a").Resource("runs").Name("run-a").SubResource("log").URL().String(),
		server.URL+"/apis/logs.kruntimes.io/v1alpha1/namespaces/team-a/runs/run-a/log"; got != want {
		t.Fatalf("Run log request URL = %q, want %q", got, want)
	}
}
