package bash

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

func startTestServer(t *testing.T) (pb.RuntimeClient, func()) {
	t.Helper()

	srv := grpc.NewServer()
	pb.RegisterRuntimeServer(srv, NewServer(t.TempDir()))

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go srv.Serve(lis)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	return pb.NewRuntimeClient(conn), func() {
		conn.Close()
		srv.Stop()
	}
}

func TestCreateAndGetTask_Success(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "test-1",
		Args: []string{"echo hello"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var resp *pb.StatusResponse
	for i := 0; i < 50; i++ {
		resp, err = client.Status(ctx, &pb.StatusRequest{Id: "test-1"})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED || resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Errorf("expected SUCCEEDED, got %v (stderr=%s err=%s)", resp.State, resp.Stderr, resp.ErrorMessage)
	}
}

func TestCreateAndGetTask_Failure(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "test-2",
		Args: []string{"exit 42"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var resp *pb.StatusResponse
	for i := 0; i < 50; i++ {
		resp, err = client.Status(ctx, &pb.StatusRequest{Id: "test-2"})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED || resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.ExecutionState_EXECUTION_STATE_FAILED {
		t.Errorf("expected FAILED, got %v", resp.State)
	}
	if resp.ExitCode != 42 {
		t.Errorf("expected exit code 42, got %d", resp.ExitCode)
	}
}

func TestListAndDeleteTask(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "test-3",
		Args: []string{"sleep 10"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	listResp, err := client.List(ctx, &pb.ListRequest{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(listResp.Entries) != 1 {
		t.Errorf("expected 1 request, got %d", len(listResp.Entries))
	}

	_, err = client.Cancel(ctx, &pb.CancelRequest{Id: "test-3"})
	if err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	listResp, err = client.List(ctx, &pb.ListRequest{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(listResp.Entries) != 0 {
		t.Errorf("expected 0 tasks after delete, got %d", len(listResp.Entries))
	}
}

func TestGetTask_NotFound(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.Status(ctx, &pb.StatusRequest{Id: "nonexistent"})
	if err == nil {
		t.Error("expected error for nonexistent request")
	}
}

func TestCreateTask_Duplicate(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "dup-1",
		Args: []string{"echo first"},
	})
	if err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	_, err = client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "dup-1",
		Args: []string{"echo second"},
	})
	if err == nil {
		t.Error("expected error for duplicate run ID")
	}
}

func TestCreateTask_MultipleCommands(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "multi-1",
		Args: []string{"export FOO=bar", "echo $FOO"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var resp *pb.StatusResponse
	for i := 0; i < 50; i++ {
		resp, err = client.Status(ctx, &pb.StatusRequest{Id: "multi-1"})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED || resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Errorf("expected SUCCEEDED, got %v (stderr=%s)", resp.State, resp.Stderr)
	}
	fmt.Printf("stdout: %s\n", resp.Stdout)
}
