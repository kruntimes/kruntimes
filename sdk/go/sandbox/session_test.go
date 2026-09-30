package sandbox

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestSandboxSessionSendsReceivesAndCloses(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/namespaces/default/runtimes/bash/sessions/run-uid/operations:ws" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer connection.Close()
		var send struct {
			Type string `json:"type"`
		}
		if err := connection.ReadJSON(&send); err != nil || send.Type != "send" {
			t.Fatalf("send frame = %#v, err = %v", send, err)
		}
		if err := connection.WriteJSON(map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "operation-1"}}); err != nil {
			t.Fatalf("write accepted: %v", err)
		}
		if err := connection.WriteJSON(map[string]any{"sequence": 2, "type": "completed", "completed": map[string]any{"command": map[string]any{"exitCode": 0}}}); err != nil {
			t.Fatalf("write completed: %v", err)
		}
	}))
	defer server.Close()

	sandbox := readySandbox(t, httpDoer(func(*http.Request) (*http.Response, error) { return nil, nil }))
	sandbox.client.bearerToken = "token"
	sandbox.run.Status.Endpoint.URL = server.URL + "/v1/namespaces/default/runtimes/bash/sessions/run-uid"
	session, err := sandbox.OpenSession(t.Context())
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer session.Close()
	operationID, err := session.Send(t.Context(), Command{Argv: []string{"echo", "ok"}})
	if err != nil || operationID != "operation-1" {
		t.Fatalf("Send = %q, %v", operationID, err)
	}
	event, err := session.Receive(t.Context())
	if err != nil || event.Type != "completed" || event.Completed == nil {
		t.Fatalf("Receive = %#v, %v", event, err)
	}
}

func TestSandboxSessionCancelUsesActiveOperationID(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	cancelled := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer connection.Close()
		var send map[string]any
		if err := connection.ReadJSON(&send); err != nil {
			t.Fatalf("read send: %v", err)
		}
		if err := connection.WriteJSON(map[string]any{"sequence": 1, "type": "accepted", "accepted": map[string]string{"operationID": "operation-1"}}); err != nil {
			t.Fatalf("write accepted: %v", err)
		}
		var cancel struct {
			Type        string `json:"type"`
			OperationID string `json:"operationID"`
		}
		if err := connection.ReadJSON(&cancel); err != nil {
			t.Fatalf("read cancel: %v", err)
		}
		cancelled <- cancel.OperationID
	}))
	defer server.Close()

	sandbox := readySandbox(t, httpDoer(func(*http.Request) (*http.Response, error) { return nil, nil }))
	sandbox.run.Status.Endpoint.URL = server.URL + "/v1/namespaces/default/runtimes/bash/sessions/run-uid"
	session, err := sandbox.OpenSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	operationID, err := session.Send(t.Context(), Command{Argv: []string{"sleep", "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Cancel(t.Context(), operationID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if got := <-cancelled; got != operationID {
		t.Fatalf("cancelled operation = %q", got)
	}
}

func TestSandboxSessionMaintainsConfiguredLeaseHeartbeat(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	heartbeats := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
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
	defer server.Close()

	leaseTimeout := int32(1)
	sandbox := readySandbox(t, httpDoer(func(*http.Request) (*http.Response, error) { return nil, nil }))
	sandbox.run.Spec.Mode.Session = &v1alpha1.RunSessionMode{LeaseTimeoutSeconds: &leaseTimeout}
	sandbox.run.Status.Endpoint.URL = server.URL + "/v1/namespaces/default/runtimes/bash/sessions/run-uid"
	session, err := sandbox.OpenSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	select {
	case <-heartbeats:
	case <-time.After(time.Second):
		t.Fatal("SDK did not send a lease heartbeat")
	}
}
