package logapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kruntimes/kruntimes/internal/gateway"
)

func TestLogRoute(t *testing.T) {
	namespace, name, ok := logRoute("/apis/logs.kruntimes.io/v1alpha1/namespaces/team-a/runs/build/log")
	if !ok || namespace != "team-a" || name != "build" {
		t.Fatalf("logRoute() = %q, %q, %t", namespace, name, ok)
	}
	if _, _, ok := logRoute("/v1/namespaces/team-a/runs/build/log"); ok {
		t.Fatal("logRoute() accepted direct Gateway path")
	}
}

func TestCursorRoundTripAndEntryIdentity(t *testing.T) {
	first := gateway.RunLogEntry{Timestamp: "2026-09-11T00:00:00Z", Stream: "stdout", Message: "first"}
	second := gateway.RunLogEntry{Timestamp: "2026-09-11T00:00:00Z", Stream: "stdout", Message: "second"}
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
		server.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, response.Code)
		}
	}
}
