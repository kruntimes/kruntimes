package runtimed

import (
	"errors"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	artifactv1 "github.com/kruntimes/kruntimes/api/artifact/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

const artifactDownloadChunkBytes = 64 * 1024

// RegisterArtifactService registers artifact access on an existing runtimed
// gRPC server. The server must be reachable only through Kubernetes
// port-forwarding or an equivalently authenticated transport.
func RegisterArtifactService(
	registrar grpc.ServiceRegistrar,
	reader client.Reader,
	store artifact.Store,
	runtimeName string,
) {
	artifactv1.RegisterArtifactServiceServer(registrar, &artifactServer{
		reader:      reader,
		store:       store,
		runtimeName: runtimeName,
	})
}

type artifactServer struct {
	artifactv1.UnimplementedArtifactServiceServer
	reader      client.Reader
	store       artifact.Store
	runtimeName string
}

func (s *artifactServer) Download(
	req *artifactv1.DownloadRequest,
	stream grpc.ServerStreamingServer[artifactv1.DownloadResponse],
) error {
	if req.GetNamespace() == "" || req.GetRunName() == "" || req.GetArtifactName() == "" {
		return status.Error(codes.InvalidArgument, "namespace, run_name, and artifact_name are required")
	}
	if s.reader == nil || s.store == nil || s.runtimeName == "" {
		return status.Error(codes.FailedPrecondition, "artifact service is not configured")
	}

	run := &v1alpha1.Run{}
	key := client.ObjectKey{Namespace: req.GetNamespace(), Name: req.GetRunName()}
	if err := s.reader.Get(stream.Context(), key, run); err != nil {
		if apierrors.IsNotFound(err) {
			return status.Errorf(codes.NotFound, "Run %s/%s not found", key.Namespace, key.Name)
		}
		return status.Errorf(codes.Internal, "get Run %s/%s: %v", key.Namespace, key.Name, err)
	}
	if run.Spec.Runtime != s.runtimeName {
		return status.Errorf(codes.PermissionDenied, "Run uses runtime %q, not %q", run.Spec.Runtime, s.runtimeName)
	}

	ref, found := findArtifactRef(run.Status.ArtifactRefs, req.GetArtifactName())
	if !found {
		return status.Errorf(codes.NotFound, "artifact %q not found on Run %s/%s", req.GetArtifactName(), run.Namespace, run.Name)
	}

	content, err := s.store.Open(stream.Context(), ref)
	if err != nil {
		return status.Errorf(codes.Internal, "open artifact %q: %v", ref.Name, err)
	}
	defer content.Close()

	if err := stream.Send(&artifactv1.DownloadResponse{
		Metadata: &artifactv1.ArtifactMetadata{
			Name:        ref.Name,
			Type:        string(ref.Type),
			ContentType: ref.ContentType,
			SizeBytes:   ref.SizeBytes,
			Digest:      ref.Digest,
		},
	}); err != nil {
		return err
	}

	buffer := make([]byte, artifactDownloadChunkBytes)
	for {
		n, readErr := content.Read(buffer)
		if n > 0 {
			data := append([]byte(nil), buffer[:n]...)
			if err := stream.Send(&artifactv1.DownloadResponse{Data: data}); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return status.Errorf(codes.Internal, "read artifact %q: %v", ref.Name, readErr)
		}
	}
}

func findArtifactRef(refs []v1alpha1.ArtifactRef, name string) (v1alpha1.ArtifactRef, bool) {
	for _, ref := range refs {
		if ref.Name == name {
			return ref, true
		}
	}
	return v1alpha1.ArtifactRef{}, false
}
