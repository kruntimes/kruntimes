package runtimed

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	artifactv1 "github.com/kruntimes/kruntimes/api/artifact/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

func TestArtifactServerDownload(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), artifactDownloadChunkBytes+17)
	ref := testArtifactRef("report.txt", int64(len(payload)))
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{ArtifactRefs: []v1alpha1.ArtifactRef{ref}},
	}
	client := newArtifactTestClient(t, run, &artifactReadStore{content: payload}, "bash")

	stream, err := client.Download(t.Context(), &artifactv1.DownloadRequest{
		Namespace: "team-a", RunName: "run-1", ArtifactName: "report.txt",
	})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv(metadata) error = %v", err)
	}
	if got := first.GetMetadata(); got == nil || got.GetName() != ref.Name ||
		got.GetType() != string(ref.Type) || got.GetContentType() != ref.ContentType ||
		got.GetSizeBytes() != ref.SizeBytes || got.GetDigest() != ref.Digest {
		t.Fatalf("metadata = %#v", got)
	}
	if len(first.GetData()) != 0 {
		t.Fatalf("metadata frame has %d data bytes", len(first.GetData()))
	}

	var downloaded []byte
	var chunks int
	for {
		response, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv(data) error = %v", recvErr)
		}
		if response.GetMetadata() != nil {
			t.Fatal("data frame unexpectedly contains metadata")
		}
		if len(response.GetData()) > artifactDownloadChunkBytes {
			t.Fatalf("chunk size = %d", len(response.GetData()))
		}
		downloaded = append(downloaded, response.GetData()...)
		chunks++
	}
	if chunks != 2 {
		t.Fatalf("chunks = %d, want 2", chunks)
	}
	if !bytes.Equal(downloaded, payload) {
		t.Fatal("downloaded content differs")
	}
}

func TestArtifactServerRejectsRuntimeMismatch(t *testing.T) {
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec:       v1alpha1.RunSpec{Runtime: "python"},
		Status: v1alpha1.RunStatus{
			ArtifactRefs: []v1alpha1.ArtifactRef{testArtifactRef("result", 1)},
		},
	}
	client := newArtifactTestClient(t, run, &artifactReadStore{content: []byte("x")}, "bash")

	stream, err := client.Download(t.Context(), &artifactv1.DownloadRequest{
		Namespace: "team-a", RunName: "run-1", ArtifactName: "result",
	})
	if err != nil {
		t.Fatalf("Download() setup error = %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Recv() code = %v, want %v; error = %v", status.Code(err), codes.PermissionDenied, err)
	}
}

func TestArtifactServerDoesNotOpenUnknownArtifact(t *testing.T) {
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
	}
	store := &artifactReadStore{content: []byte("secret")}
	client := newArtifactTestClient(t, run, store, "bash")

	stream, err := client.Download(t.Context(), &artifactv1.DownloadRequest{
		Namespace: "team-a", RunName: "run-1", ArtifactName: "missing",
	})
	if err != nil {
		t.Fatalf("Download() setup error = %v", err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.NotFound {
		t.Fatalf("Recv() code = %v, want %v; error = %v", status.Code(err), codes.NotFound, err)
	}
	if store.opened {
		t.Fatal("store.Open was called for an unknown artifact")
	}
}

func newArtifactTestClient(
	t *testing.T,
	run *v1alpha1.Run,
	store artifact.Store,
	runtimeName string,
) artifactv1.ArtifactServiceClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	RegisterArtifactService(server, k8sClient, store, runtimeName)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)

	conn, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return artifactv1.NewArtifactServiceClient(conn)
}

func testArtifactRef(name string, size int64) v1alpha1.ArtifactRef {
	return v1alpha1.ArtifactRef{
		Name:        name,
		Driver:      v1alpha1.ArtifactDriverFilesystem,
		Type:        v1alpha1.ArtifactTypeFile,
		SizeBytes:   size,
		Digest:      "sha256:abc",
		ContentType: "text/plain",
		Location: v1alpha1.ArtifactLocation{
			Filesystem: &v1alpha1.FilesystemArtifactLocation{Path: "artifact"},
		},
	}
}

type artifactReadStore struct {
	content []byte
	opened  bool
}

func (s *artifactReadStore) Put(context.Context, *v1alpha1.Run, string, artifact.PutOptions) (v1alpha1.ArtifactRef, error) {
	return v1alpha1.ArtifactRef{}, nil
}

func (s *artifactReadStore) Open(context.Context, v1alpha1.ArtifactRef) (io.ReadCloser, error) {
	s.opened = true
	return io.NopCloser(bytes.NewReader(s.content)), nil
}

func (s *artifactReadStore) Delete(context.Context, v1alpha1.ArtifactRef) error {
	return nil
}
