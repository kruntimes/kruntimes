package runtimed

import (
	"context"
	"fmt"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

// StartRuntimeProxyServer starts the gRPC proxies exposed by runtimed on addr.
// Runtime status calls are proxied to the colocated Runtime Server.
// SessionRuntime calls route to the owner runtimed, which proxies to its local
// Runtime Server. Artifact downloads are served from this Runtime Pod's
// ArtifactStore.
func StartRuntimeProxyServer(
	ctx context.Context,
	runtimeEndpoint, addr string,
	apiWriter client.Client,
	apiReader, sessionReader client.Reader,
	store artifact.Store,
	operations *SessionOperationQueue,
	functionController *Controller,
	runtimeNamespace,
	runtimeName,
	podName string,
	listenerReady chan<- error,
) error {
	notifyReady := func(err error) {
		if listenerReady == nil {
			return
		}
		listenerReady <- err
		listenerReady = nil
	}
	_, statusPort, err := net.SplitHostPort(addr)
	if err != nil {
		err = fmt.Errorf("parse runtimed proxy address %q: %w", addr, err)
		notifyReady(err)
		return err
	}
	conn, err := grpc.NewClient(runtimeEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		notifyReady(err)
		return err
	}
	defer conn.Close()

	srv := grpc.NewServer()
	pb.RegisterRuntimeServer(srv, &runtimeStatusProxy{runtimeCli: pb.NewRuntimeClient(conn)})
	sessionProxy := newSessionRuntimeProxy(
		sessionReader,
		apiReader,
		pb.NewSessionRuntimeClient(conn),
		runtimeNamespace,
		runtimeName,
		podName,
		statusPort,
	)
	if operations != nil {
		sessionProxy.operations = operations
	}
	sessionProxy.operationStore = newSessionOperationStore(apiWriter, apiReader)
	pb.RegisterSessionRuntimeServer(srv, sessionProxy)
	pb.RegisterFunctionRuntimeServer(srv, newFunctionRuntimeProxy(
		sessionReader,
		apiReader,
		pb.NewFunctionRuntimeClient(conn),
		functionController,
		runtimeNamespace,
		runtimeName,
		podName,
		statusPort,
	))
	if store != nil {
		RegisterArtifactService(srv, apiReader, store, runtimeNamespace, runtimeName)
	}

	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()

	lis, err := net.Listen("tcp", addr)
	if err != nil {
		notifyReady(err)
		return err
	}
	notifyReady(nil)
	klog.Infof("Runtime proxy server listening on %s", addr)
	return srv.Serve(lis)
}
