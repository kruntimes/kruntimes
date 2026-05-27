package runtimed

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

// StartStatusProxy starts a gRPC server on addr that forwards to the runtime
// server on runtimeEndpoint (typically localhost:9091). It only serves runs
// executing on this pod; krt logs is responsible for port-forwarding to the
// correct pod.
func StartStatusProxy(ctx context.Context, runtimeEndpoint, addr string) error {
	conn, err := grpc.NewClient(runtimeEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return err
	}
	defer conn.Close()

	srv := grpc.NewServer()
	pb.RegisterRuntimeServer(srv, &statusProxy{runtimeCli: pb.NewRuntimeClient(conn)})

	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	klog.Infof("Status proxy listening on %s", addr)
	return srv.Serve(lis)
}

type statusProxy struct {
	pb.UnimplementedRuntimeServer
	runtimeCli pb.RuntimeClient
}

func (s *statusProxy) Status(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	return s.runtimeCli.Status(ctx, req)
}
