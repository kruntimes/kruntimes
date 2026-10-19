package runextensionapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
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
	for _, path := range []string{"/apis/logs.kruntimes.io", "/apis/logs.kruntimes.io/v1alpha1", "/apis/sessions.kruntimes.io", "/apis/sessions.kruntimes.io/v1alpha1"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, response.Code)
		}
	}
}

func TestAggregatedSessionRejectsUnverifiedIdentity(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session", nil)
	request.Header.Set("X-Remote-User", "admin")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("forged aggregation identity status = %d, want 401", response.Code)
	}
}

func TestAggregatedSessionFilesRejectUnverifiedIdentity(t *testing.T) {
	server := &Server{}
	for _, path := range []string{
		"/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files",
		"/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files/notes/result.txt",
	} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("X-Remote-User", "admin")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("forged aggregation identity for %q status = %d, want 401", path, response.Code)
		}
	}
}

func TestAggregatedSessionFilesRejectNonGet(t *testing.T) {
	server := &Server{}
	for _, path := range []string{
		"/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files",
		"/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files/notes/result.txt",
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, nil))
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s = %d, want 405", path, response.Code)
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

func TestAggregatedSessionFileListingUsesOwnerRuntime(t *testing.T) {
	run := readySessionRun()
	dialer := &fakeRuntimeDialer{client: &fakeSessionRuntimeClient{list: func(_ context.Context, request *pb.ListSessionFilesRequest) (*pb.ListSessionFilesResponse, error) {
		if request.GetIdentity().GetRunUid() != string(run.UID) || request.GetIdentity().GetAssignedPodUid() != run.Status.AssignedPodUID {
			t.Fatalf("identity = %#v", request.GetIdentity())
		}
		if request.GetPath() != "notes" || request.GetLimit() != 2 || request.GetPageToken() != "after-notes" {
			t.Fatalf("list request = %#v", request)
		}
		return &pb.ListSessionFilesResponse{
			Entries: []*pb.SessionFileInfo{
				{Path: "notes/a.txt", SizeBytes: 3},
				{Path: "notes/archive", Directory: true},
			},
			NextPageToken: "next-page",
		}, nil
	}}}
	server := &Server{SessionRuntimes: dialer}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files?path=notes&limit=2&pageToken=after-notes", nil)
	server.listSessionFiles(response, request, run)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if dialer.address != "runtime-bash.team-a.svc:9093" {
		t.Fatalf("Runtime Service address = %q", dialer.address)
	}
	var listing struct {
		Entries []struct {
			Path      string `json:"path"`
			Directory bool   `json:"directory"`
			SizeBytes int64  `json:"sizeBytes"`
		} `json:"entries"`
		NextPageToken string `json:"nextPageToken"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if len(listing.Entries) != 2 || listing.Entries[0].Path != "notes/a.txt" || listing.Entries[0].SizeBytes != 3 || !listing.Entries[1].Directory || listing.NextPageToken != "next-page" {
		t.Fatalf("listing = %#v", listing)
	}
}

func TestAggregatedSessionFileReadIsBoundedAndReturned(t *testing.T) {
	run := readySessionRun()
	dialer := &fakeRuntimeDialer{client: &fakeSessionRuntimeClient{read: func(_ context.Context, request *pb.ReadSessionFileRequest) (*pb.ReadSessionFileResponse, error) {
		if request.GetPath() != "artifacts/test/output.log" || request.GetMaxBytes() != 64 {
			t.Fatalf("read request = %#v", request)
		}
		return &pb.ReadSessionFileResponse{Contents: []byte("done\n"), Truncated: true}, nil
	}}}
	server := &Server{SessionRuntimes: dialer}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files/artifacts/test/output.log?maxBytes=64", nil)
	server.readSessionFile(response, request, run, "artifacts/test/output.log")

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var read struct {
		Contents  []byte `json:"contents"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.NewDecoder(response.Body).Decode(&read); err != nil {
		t.Fatalf("decode read: %v", err)
	}
	if string(read.Contents) != "done\n" || !read.Truncated {
		t.Fatalf("read = %#v", read)
	}
}

func TestAggregatedSessionFileErrorsUseKubernetesStatus(t *testing.T) {
	run := readySessionRun()
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "not found", err: status.Error(codes.NotFound, "file not found"), want: http.StatusNotFound},
		{name: "failed precondition", err: status.Error(codes.FailedPrecondition, "Session is not active"), want: http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			dialer := &fakeRuntimeDialer{client: &fakeSessionRuntimeClient{read: func(context.Context, *pb.ReadSessionFileRequest) (*pb.ReadSessionFileResponse, error) {
				return nil, test.err
			}}}
			server := &Server{SessionRuntimes: dialer}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files/notes/a.txt", nil)
			server.readSessionFile(response, request, run, "notes/a.txt")
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			var apiStatus metav1.Status
			if err := json.NewDecoder(response.Body).Decode(&apiStatus); err != nil {
				t.Fatalf("decode status: %v", err)
			}
			if apiStatus.Status != metav1.StatusFailure || apiStatus.Code != int32(test.want) {
				t.Fatalf("status = %#v", apiStatus)
			}
		})
	}
}

func TestAggregatedSessionFileRejectsInvalidMaxBytes(t *testing.T) {
	server := &Server{}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files/notes/a.txt?maxBytes=0", nil)
	server.readSessionFile(response, request, readySessionRun(), "notes/a.txt")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func readySessionRun() *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "shell", Namespace: "team-a", UID: "run-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash", Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunReady, AssignedPodUID: "pod-uid"},
	}
}

type fakeRuntimeDialer struct {
	address string
	client  pb.SessionRuntimeClient
}

func (d *fakeRuntimeDialer) Dial(_ context.Context, address string) (pb.SessionRuntimeClient, io.Closer, error) {
	d.address = address
	return d.client, io.NopCloser(strings.NewReader("")), nil
}

type fakeSessionRuntimeClient struct {
	pb.SessionRuntimeClient
	list func(context.Context, *pb.ListSessionFilesRequest) (*pb.ListSessionFilesResponse, error)
	read func(context.Context, *pb.ReadSessionFileRequest) (*pb.ReadSessionFileResponse, error)
}

func (c *fakeSessionRuntimeClient) ListSessionFiles(ctx context.Context, request *pb.ListSessionFilesRequest, _ ...grpc.CallOption) (*pb.ListSessionFilesResponse, error) {
	if c.list == nil {
		return nil, status.Error(codes.Unimplemented, "ListSessionFiles")
	}
	return c.list(ctx, request)
}

func (c *fakeSessionRuntimeClient) ReadSessionFile(ctx context.Context, request *pb.ReadSessionFileRequest, _ ...grpc.CallOption) (*pb.ReadSessionFileResponse, error) {
	if c.read == nil {
		return nil, status.Error(codes.Unimplemented, "ReadSessionFile")
	}
	return c.read(ctx, request)
}
