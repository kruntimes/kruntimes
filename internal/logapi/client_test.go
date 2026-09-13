package logapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/client-go/rest"
)

func TestRESTClientBuildsRunLogRequest(t *testing.T) {
	requestSeen := make(chan *http.Request, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestSeen <- request.Clone(context.Background())
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"items":[]}`)
	}))
	defer server.Close()

	client, err := NewClient(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	response, err := client.WithBearerToken("caller-token").GetLogs("team-a", "run-a", &RunLogOptions{
		TailLines:      25,
		Follow:         true,
		Cursor:         "cursor-1",
		TimeoutSeconds: 30,
	}).Do(t.Context()).Raw()
	if err != nil {
		t.Fatalf("GetLogs().Do().Raw() error = %v", err)
	}
	if string(response) != `{"items":[]}` {
		t.Fatalf("response = %q", response)
	}

	request := <-requestSeen
	if request.Method != http.MethodGet {
		t.Fatalf("method = %q, want GET", request.Method)
	}
	if request.URL.Path != "/apis/logs.kruntimes.io/v1alpha1/namespaces/team-a/runs/run-a/log" {
		t.Fatalf("path = %q", request.URL.Path)
	}
	if request.Header.Get("Authorization") != "Bearer caller-token" {
		t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
	}
	query := request.URL.Query()
	if query.Get("tailLines") != "25" || query.Get("follow") != "true" || query.Get("cursor") != "cursor-1" || query.Get("timeoutSeconds") != "30" {
		t.Fatalf("query = %q", request.URL.RawQuery)
	}
}

func TestNewClientRequiresKubernetesConfiguration(t *testing.T) {
	for _, config := range []*rest.Config{nil, {}} {
		if _, err := NewClient(config); err == nil {
			t.Fatalf("NewClient(%#v) succeeded without Kubernetes host", config)
		}
	}
}
