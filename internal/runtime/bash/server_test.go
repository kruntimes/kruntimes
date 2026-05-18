package bash

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/airconduct/kruntime/api/taskruntime/v1"
)

func startTestServer(t *testing.T) (pb.TaskRuntimeClient, func()) {
	t.Helper()

	srv := grpc.NewServer()
	pb.RegisterTaskRuntimeServer(srv, NewServer(t.TempDir()))

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

	return pb.NewTaskRuntimeClient(conn), func() {
		conn.Close()
		srv.Stop()
	}
}

func TestCreateAndGetTask_Success(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.CreateTask(ctx, &pb.CreateTaskRequest{
		Id:       "test-1",
		Commands: []string{"echo hello"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var resp *pb.GetTaskResponse
	for i := 0; i < 50; i++ {
		resp, err = client.GetTask(ctx, &pb.GetTaskRequest{Id: "test-1"})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if resp.State == pb.TaskState_TASK_STATE_SUCCEEDED || resp.State == pb.TaskState_TASK_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.TaskState_TASK_STATE_SUCCEEDED {
		t.Errorf("expected SUCCEEDED, got %v (stderr=%s err=%s)", resp.State, resp.Stderr, resp.ErrorMessage)
	}
}

func TestCreateAndGetTask_Failure(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()

	_, err := client.CreateTask(ctx, &pb.CreateTaskRequest{
		Id:       "test-2",
		Commands: []string{"exit 42"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var resp *pb.GetTaskResponse
	for i := 0; i < 50; i++ {
		resp, err = client.GetTask(ctx, &pb.GetTaskRequest{Id: "test-2"})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if resp.State == pb.TaskState_TASK_STATE_SUCCEEDED || resp.State == pb.TaskState_TASK_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.TaskState_TASK_STATE_FAILED {
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

	_, err := client.CreateTask(ctx, &pb.CreateTaskRequest{
		Id:       "test-3",
		Commands: []string{"sleep 10"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	listResp, err := client.ListTasks(ctx, &pb.ListTasksRequest{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(listResp.Tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(listResp.Tasks))
	}

	_, err = client.DeleteTask(ctx, &pb.DeleteTaskRequest{Id: "test-3"})
	if err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	listResp, err = client.ListTasks(ctx, &pb.ListTasksRequest{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(listResp.Tasks) != 0 {
		t.Errorf("expected 0 tasks after delete, got %d", len(listResp.Tasks))
	}
}

func TestGetTask_NotFound(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.GetTask(ctx, &pb.GetTaskRequest{Id: "nonexistent"})
	if err == nil {
		t.Error("expected error for nonexistent task")
	}
}

func TestCreateTask_Duplicate(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.CreateTask(ctx, &pb.CreateTaskRequest{
		Id:       "dup-1",
		Commands: []string{"echo first"},
	})
	if err != nil {
		t.Fatalf("first CreateTask: %v", err)
	}

	_, err = client.CreateTask(ctx, &pb.CreateTaskRequest{
		Id:       "dup-1",
		Commands: []string{"echo second"},
	})
	if err == nil {
		t.Error("expected error for duplicate task ID")
	}
}

func TestCreateTask_MultipleCommands(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	_, err := client.CreateTask(ctx, &pb.CreateTaskRequest{
		Id:       "multi-1",
		Commands: []string{"export FOO=bar", "echo $FOO"},
	})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	var resp *pb.GetTaskResponse
	for i := 0; i < 50; i++ {
		resp, err = client.GetTask(ctx, &pb.GetTaskRequest{Id: "multi-1"})
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if resp.State == pb.TaskState_TASK_STATE_SUCCEEDED || resp.State == pb.TaskState_TASK_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.TaskState_TASK_STATE_SUCCEEDED {
		t.Errorf("expected SUCCEEDED, got %v (stderr=%s)", resp.State, resp.Stderr)
	}
	fmt.Printf("stdout: %s\n", resp.Stdout)
}
