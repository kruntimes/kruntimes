package logapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

func TestRESTClientPreservesKubernetesErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(writer).Encode(metav1.Status{
			TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
			Status:   metav1.StatusFailure,
			Reason:   metav1.StatusReasonConflict,
			Code:     http.StatusConflict,
			Message:  "assigned Runtime Pod is no longer available; Run logs cannot be read",
		})
	}))
	defer server.Close()

	client, err := NewClient(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.GetLogs("team-a", "run-a", nil).Do(t.Context()).Raw()
	if err == nil {
		t.Fatal("GetLogs() succeeded, want error")
	}
	var apiStatus apierrors.APIStatus
	if !errors.As(err, &apiStatus) {
		t.Fatalf("error %T does not expose Kubernetes status: %v", err, err)
	}
	status := apiStatus.Status()
	if status.Code != http.StatusConflict || status.Reason != metav1.StatusReasonConflict || status.Message != "assigned Runtime Pod is no longer available; Run logs cannot be read" {
		t.Fatalf("status = %#v", status)
	}
}
