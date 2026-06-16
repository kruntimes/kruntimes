package healthcheck

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestKubernetesAPI(t *testing.T) {
	wantErr := errors.New("api unavailable")
	reader := &readerStub{listErr: wantErr}
	check := KubernetesAPI(reader, &v1alpha1.RunList{})

	err := check(httptest.NewRequest("GET", "/readyz", nil))
	if !errors.Is(err, wantErr) {
		t.Fatalf("check error = %v, want %v", err, wantErr)
	}
	if reader.list == nil {
		t.Fatal("readiness check did not list a resource")
	}
}

func TestRuntime(t *testing.T) {
	tests := []struct {
		name    string
		resp    *pb.HealthResponse
		err     error
		wantErr bool
	}{
		{name: "healthy", resp: &pb.HealthResponse{Healthy: true}},
		{name: "nil response", wantErr: true},
		{name: "unhealthy", resp: &pb.HealthResponse{Message: "not ready"}, wantErr: true},
		{name: "rpc error", err: errors.New("connection refused"), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check := Runtime(&runtimeHealthStub{resp: tt.resp, err: tt.err}, time.Second)
			err := check(httptest.NewRequest("GET", "/readyz", nil))
			if (err != nil) != tt.wantErr {
				t.Fatalf("check error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

type readerStub struct {
	listErr error
	list    client.ObjectList
}

func (r *readerStub) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return nil
}

func (r *readerStub) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	r.list = list
	return r.listErr
}

type runtimeHealthStub struct {
	resp *pb.HealthResponse
	err  error
}

func (r *runtimeHealthStub) Health(
	context.Context,
	*pb.HealthRequest,
	...grpc.CallOption,
) (*pb.HealthResponse, error) {
	return r.resp, r.err
}
