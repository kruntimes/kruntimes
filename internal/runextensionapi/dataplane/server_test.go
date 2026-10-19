package dataplane

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
)

func TestSessionFileListingUsesOwnerRuntime(t *testing.T) {
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
	server := &Server{Dialer: dialer}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files?path=notes&limit=2&pageToken=after-notes", nil)
	server.ServeSessionFiles(response, request, run)

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

func TestSessionFileReadIsBoundedAndReturned(t *testing.T) {
	run := readySessionRun()
	dialer := &fakeRuntimeDialer{client: &fakeSessionRuntimeClient{read: func(_ context.Context, request *pb.ReadSessionFileRequest) (*pb.ReadSessionFileResponse, error) {
		if request.GetPath() != "artifacts/test/output.log" || request.GetMaxBytes() != 64 {
			t.Fatalf("read request = %#v", request)
		}
		return &pb.ReadSessionFileResponse{Contents: []byte("done\n"), Truncated: true}, nil
	}}}
	server := &Server{Dialer: dialer}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files/artifacts/test/output.log?maxBytes=64", nil)
	server.ServeSessionFile(response, request, run, "artifacts/test/output.log")

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

func TestSessionFileErrorsUseDataPlaneStatus(t *testing.T) {
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
			server := &Server{Dialer: dialer}
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files/notes/a.txt", nil)
			server.ServeSessionFile(response, request, run, "notes/a.txt")
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d", response.Code, test.want)
			}
			var apiError struct {
				Error string `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&apiError); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if apiError.Error != status.Convert(test.err).Message() {
				t.Fatalf("error = %q", apiError.Error)
			}
		})
	}
}

func TestSessionFileRejectsInvalidMaxBytes(t *testing.T) {
	server := &Server{}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files/notes/a.txt?maxBytes=0", nil)
	server.ServeSessionFile(response, request, readySessionRun(), "notes/a.txt")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestSessionFileRoutesRejectNonGet(t *testing.T) {
	run := readySessionRun()
	for _, path := range []string{"notes/a.txt", ""} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files/notes/a.txt", nil)
		if path == "" {
			request = httptest.NewRequest(http.MethodPost, "/apis/sessions.kruntimes.io/v1alpha1/namespaces/team-a/runs/shell/session/files", nil)
		}
		if path == "" {
			(&Server{}).ServeSessionFiles(response, request, run)
		} else {
			(&Server{}).ServeSessionFile(response, request, run, path)
		}
		if response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", response.Code)
		}
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
