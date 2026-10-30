package console

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/rest"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRunExtensionAPIProxyForwardsCallerCredential(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got, want := request.URL.Path, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session"; got != want {
			t.Errorf("upstream path = %q, want %q", got, want)
		}
		if got, want := request.Header.Get("Authorization"), "Bearer caller-token"; got != want {
			t.Errorf("Authorization = %q, want %q", got, want)
		}
		if cookie := request.Header.Get("Cookie"); cookie != "" {
			t.Errorf("upstream Cookie = %q, want stripped", cookie)
		}
		if origin := request.Header.Get("Origin"); origin != "" {
			t.Errorf("upstream Origin = %q, want stripped", origin)
		}
		writer.WriteHeader(http.StatusUpgradeRequired)
		_, _ = writer.Write([]byte(`{"kind":"Status","status":"Failure","code":426}`))
	}))
	defer upstream.Close()

	proxy, err := NewRunExtensionAPIProxy(&rest.Config{Host: upstream.URL})
	if err != nil {
		t.Fatalf("NewRunExtensionAPIProxy: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session", nil)
	request.Header.Set("Authorization", "Bearer caller-token")
	response := httptest.NewRecorder()
	proxy.Proxy(response, request, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session")

	if response.Code != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); body != `{"kind":"Status","status":"Failure","code":426}` {
		t.Fatalf("body = %q", body)
	}
}

func TestServerProxiesSessionOperationWithConsoleSessionCredential(t *testing.T) {
	proxy, err := NewRunExtensionAPIProxy(&rest.Config{Host: "https://api.example"})
	if err != nil {
		t.Fatalf("NewRunExtensionAPIProxy: %v", err)
	}
	server := dashboardTestServer(t)
	server.RunExtensionAPI = proxy

	request := httptest.NewRequest(http.MethodGet, "/api/namespaces/team-a/runs/shell/session", nil)
	request.Host = "console.example"
	request.Header.Set("Origin", "https://console.example")
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "caller-token"})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)

	// The configured transport cannot reach api.example, so the proxy reports an
	// unavailable aggregated API. Reaching that answer proves the Console route
	// accepted the cookie credential and relayed the request.
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
}

func TestSessionOperationProxyRequiresCallerCredential(t *testing.T) {
	proxy, err := NewRunExtensionAPIProxy(&rest.Config{Host: "https://api.example"})
	if err != nil {
		t.Fatalf("NewRunExtensionAPIProxy: %v", err)
	}
	server := dashboardTestServer(t)
	server.RunExtensionAPI = proxy

	response := requestDashboard(t, server, http.MethodGet, "/api/namespaces/team-a/runs/shell/session", nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
}

func TestSessionOperationProxyRejectsCrossOriginRequest(t *testing.T) {
	proxy, err := NewRunExtensionAPIProxy(&rest.Config{Host: "https://api.example"})
	if err != nil {
		t.Fatalf("NewRunExtensionAPIProxy: %v", err)
	}
	server := dashboardTestServer(t)
	server.RunExtensionAPI = proxy

	request := httptest.NewRequest(http.MethodGet, "/api/namespaces/team-a/runs/shell/session", nil)
	request.Host = "console.example"
	request.Header.Set("Origin", "https://attacker.example")
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "caller-token"})
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", response.Code)
	}
}

func TestRunExtensionAPIProxyUnavailableUpstream(t *testing.T) {
	proxy, err := NewRunExtensionAPIProxy(&rest.Config{Host: "https://api.example"})
	if err != nil {
		t.Fatalf("NewRunExtensionAPIProxy: %v", err)
	}
	proxy.transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("aggregated API unavailable")
	})
	request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session", nil)
	request.Header.Set("Authorization", "Bearer caller-token")
	response := httptest.NewRecorder()
	proxy.Proxy(response, request, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session")
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", response.Code)
	}
}

func TestRunExtensionAPIRouteBuildsTargetPath(t *testing.T) {
	if len(runExtensionAPIRoutes) == 0 {
		t.Fatal("runExtensionAPIRoutes is empty")
	}
	route := runExtensionAPIRoutes[0]
	if route.consolePattern != "GET /api/namespaces/{namespace}/runs/{name}/session" {
		t.Fatalf("console pattern = %q", route.consolePattern)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/namespaces/team-a/runs/shell/session", nil)
	request.SetPathValue("namespace", "team-a")
	request.SetPathValue("name", "shell")
	got := route.targetPath(request)
	want := "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session"
	if got != want {
		t.Fatalf("target path = %q, want %q", got, want)
	}
}
