package sandbox

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func sandboxForServer(t *testing.T, serverURL string) *Sandbox {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{RESTConfig: &rest.Config{Host: serverURL, BearerToken: "token"}, Runs: fake.NewClientBuilder().WithScheme(scheme).Build()})
	if err != nil {
		t.Fatal(err)
	}
	return &Sandbox{client: client, run: &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "session", Namespace: "default", UID: "run-uid"},
		Spec:       v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunReady},
	}}
}

func TestOpenSessionUsesKubeconfigCredentials(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session" {
			http.Error(w, "wrong aggregated Session path", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer caller-token" {
			http.Error(w, "missing kubeconfig credentials", http.StatusUnauthorized)
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			_ = connection.Close()
		}
	}))
	defer server.Close()
	config := &rest.Config{Host: server.URL, BearerToken: "caller-token",
		TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})}}
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{RESTConfig: config, Runs: fake.NewClientBuilder().WithScheme(scheme).Build()})
	if err != nil {
		t.Fatal(err)
	}
	sandbox := &Sandbox{client: client, run: &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "shell", Namespace: "team-a"},
		Spec:       v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunReady},
	}}
	session, err := sandbox.OpenSession(t.Context())
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	_ = session.Close()
}

func TestSandboxSessionSendsReceivesAndCloses(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/sessions.kruntimes.io/v1alpha1/namespaces/default/runs/session/session" {
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
			Type           string `json:"type"`
			IdempotencyKey string `json:"idempotencyKey"`
			Operation      struct {
				Command Command `json:"command"`
			} `json:"operation"`
		}
		if err := connection.ReadJSON(&send); err != nil || send.Type != "send" || send.IdempotencyKey != "operation-1" || len(send.Operation.Command.Argv) != 2 || send.Operation.Command.Argv[0] != "echo" {
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

	sandbox := sandboxForServer(t, server.URL)
	session, err := sandbox.OpenSession(t.Context())
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	defer session.Close()
	operationID, err := session.Send(t.Context(), Operation{Command: &Command{Argv: []string{"echo", "ok"}}}, "operation-1")
	if err != nil || operationID != "operation-1" {
		t.Fatalf("Send = %q, %v", operationID, err)
	}
	select {
	case event := <-session.Receive():
		if event.Type != "completed" || event.Completed == nil {
			t.Fatalf("Receive = %#v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a Session event")
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

	sandbox := sandboxForServer(t, server.URL)
	session, err := sandbox.OpenSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	operationID, err := session.Send(t.Context(), Operation{Command: &Command{Argv: []string{"sleep", "1"}}})
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
	sandbox := sandboxForServer(t, server.URL)
	sandbox.run.Spec.Mode.Session = &v1alpha1.RunSessionMode{LeaseTimeoutSeconds: &leaseTimeout}
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
