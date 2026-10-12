package gateway

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

// GRPCDialer dials the SessionRuntime service exposed by a Runtime Service.
// The hop remains inside the Kubernetes cluster and is protected by the
// Runtime's NetworkPolicy and the gateway's authenticated HTTP boundary.
type GRPCDialer struct{}

// Runtime Services can be freshly created when a Session first becomes Ready.
// Leave enough time for cluster DNS/endpoints convergence while still being
// bounded by the caller's context deadline.
const runtimeDialTimeout = 15 * time.Second

func (GRPCDialer) Dial(ctx context.Context, address string) (pb.SessionRuntimeClient, io.Closer, error) {
	connection, err := dialReadyRuntime(ctx, address)
	if err != nil {
		return nil, nil, fmt.Errorf("create SessionRuntime client: %w", err)
	}
	return pb.NewSessionRuntimeClient(connection), connection, nil
}

func (GRPCDialer) DialFunction(ctx context.Context, address string) (pb.FunctionRuntimeClient, io.Closer, error) {
	connection, err := dialReadyRuntime(ctx, address)
	if err != nil {
		return nil, nil, fmt.Errorf("create FunctionRuntime client: %w", err)
	}
	return pb.NewFunctionRuntimeClient(connection), connection, nil
}

// dialReadyRuntime waits briefly for a Runtime Service endpoint to accept the
// connection. grpc.NewClient is intentionally lazy; without this wait a Run
// can become Ready just before kube-proxy publishes its runtimed :9093
// listener, causing the first gateway operation to fail spuriously.
func dialReadyRuntime(ctx context.Context, address string) (*grpc.ClientConn, error) {
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, runtimeDialTimeout)
	defer cancel()
	connection.Connect()
	for {
		switch state := connection.GetState(); state {
		case connectivity.Ready:
			return connection, nil
		case connectivity.Shutdown:
			_ = connection.Close()
			return nil, fmt.Errorf("Runtime Service connection shut down")
		default:
			if !connection.WaitForStateChange(deadlineCtx, state) {
				_ = connection.Close()
				return nil, deadlineCtx.Err()
			}
		}
	}
}
