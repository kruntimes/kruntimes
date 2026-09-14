package logapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/internal/runlogs"
)

func TestRunLogRoute(t *testing.T) {
	server := &Server{}
	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/apis/logs.kruntimes.io/v1alpha1/namespaces/team-a/runs/build/log", want: http.StatusUnauthorized},
		{method: http.MethodPost, path: "/apis/logs.kruntimes.io/v1alpha1/namespaces/team-a/runs/build/log", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/v1/namespaces/team-a/runs/build/log", want: http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(test.method, test.path, nil))
		if response.Code != test.want {
			t.Errorf("%s %s = %d, want %d", test.method, test.path, response.Code, test.want)
		}
	}
}

func TestCursorRoundTripAndEntryIdentity(t *testing.T) {
	first := runlogs.Entry{Timestamp: "2026-09-11T00:00:00Z", Stream: "stdout", Message: "first"}
	second := runlogs.Entry{Timestamp: "2026-09-11T00:00:00Z", Stream: "stdout", Message: "second"}
	value := newCursor("run-uid", 1, first)
	parsed, err := decodeCursor(value)
	if err != nil || parsed.RunUID != "run-uid" || parsed.Index != 1 {
		t.Fatalf("decodeCursor() = %#v, %v", parsed, err)
	}
	if value == newCursor("run-uid", 1, second) {
		t.Fatal("cursor must bind the individual log entry")
	}
	if _, err := decodeCursor("not-a-cursor"); err == nil {
		t.Fatal("decodeCursor() accepted malformed cursor")
	}
}

func TestDiscoveryEndpoints(t *testing.T) {
	server := &Server{}
	for _, path := range []string{"/apis/logs.kruntimes.io", "/apis/logs.kruntimes.io/v1alpha1"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, response.Code)
		}
	}
}

func TestWriteErrorUsesKubernetesStatus(t *testing.T) {
	response := httptest.NewRecorder()
	(&Server{}).writeError(response, http.StatusConflict, "assigned Runtime Pod is no longer available; Run logs cannot be read")

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
	var status metav1.Status
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.TypeMeta.Kind != "Status" || status.TypeMeta.APIVersion != "v1" || status.Status != metav1.StatusFailure || status.Reason != metav1.StatusReasonConflict || status.Code != http.StatusConflict {
		t.Fatalf("status = %#v", status)
	}
	if status.Message != "assigned Runtime Pod is no longer available; Run logs cannot be read" {
		t.Fatalf("message = %q", status.Message)
	}
}
