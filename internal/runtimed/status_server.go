package runtimed

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

// StartRuntimeServices starts the gRPC services exposed by runtimed on addr.
// It proxies runtime status calls to runtimeEndpoint (typically localhost:9091)
// and, when configured, also serves artifact download requests for runs
// executing on this pod.
func StartRuntimeServices(
	ctx context.Context,
	runtimeEndpoint, addr string,
	reader client.Reader,
	store artifact.Store,
	runtimeNamespace,
	runtimeName string,
) error {
	conn, err := grpc.NewClient(runtimeEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	srv := grpc.NewServer()
	pb.RegisterRuntimeServer(srv, &statusProxy{runtimeCli: pb.NewRuntimeClient(conn)})
	if store != nil {
		RegisterArtifactService(srv, reader, store, runtimeNamespace, runtimeName)
	}

	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	klog.Infof("Runtime services listening on %s", addr)
	return srv.Serve(lis)
}

type statusProxy struct {
	pb.UnimplementedRuntimeServer
	runtimeCli pb.RuntimeClient
}

func (s *statusProxy) Status(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	return s.runtimeCli.Status(ctx, req)
}
