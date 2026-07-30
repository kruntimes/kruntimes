package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"
	"k8s.io/klog/v2"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/runtimes/bash"
)

func main() {
	var (
		port    int
		workDir string
	)

	flag.IntVar(&port, "port", 9091, "gRPC listen port")
	flag.StringVar(&workDir, "work-dir", "", "Workspace directory (default /workspace)")
	klog.InitFlags(nil)
	flag.Parse()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		klog.Fatalf("Failed to listen: %v", err)
	}

	srv := grpc.NewServer()
	runtimeServer := bash.NewServer(workDir)
	pb.RegisterRuntimeServer(srv, runtimeServer)
	pb.RegisterFunctionRuntimeServer(srv, runtimeServer)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	klog.Infof("Bash runtime listening on :%d, workDir=%s", port, workDir)
	if err := srv.Serve(lis); err != nil {
		klog.Fatalf("Failed to serve: %v", err)
	}
	klog.Info("Bash runtime stopped")
}
