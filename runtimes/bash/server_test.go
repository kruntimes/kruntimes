package bash

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

func startTestServer(t *testing.T) (pb.RuntimeClient, func()) {
	return startTestServerWithOutputLimit(t, defaultOutputLimitBytes)
}

func startTestServerWithOutputLimit(t *testing.T, outputLimit int) (pb.RuntimeClient, func()) {
	t.Helper()

	srv := grpc.NewServer()
	pb.RegisterRuntimeServer(srv, NewServerWithOutputLimit(t.TempDir(), outputLimit))

	lis, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() { _ = srv.Serve(lis) }()

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

func TestForgetTerminalExecution(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "forget-terminal",
		Args: []string{"echo done"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	waitForTerminalStatus(t, client, "forget-terminal")

	if _, err := client.Forget(ctx, &pb.ForgetRequest{Id: "forget-terminal"}); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := client.Status(ctx, &pb.StatusRequest{Id: "forget-terminal"}); status.Code(err) != codes.NotFound {
		t.Fatalf("Status error = %v, want NotFound", err)
	}
}

func TestForgetRejectsRunningExecution(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "forget-running",
		Args: []string{"sleep 30"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if _, err := client.Forget(ctx, &pb.ForgetRequest{Id: "forget-running"}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Forget error = %v, want FailedPrecondition", err)
	}
	if _, err := client.Cancel(ctx, &pb.CancelRequest{Id: "forget-running"}); err != nil {
		t.Fatalf("Cancel: %v", err)
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

	// Duplicate Execute should succeed (cancels the old execution for retry).
	_, err = client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "dup-1",
		Args: []string{"echo second"},
	})
	if err != nil {
		t.Fatalf("second CreateTask (retry): %v", err)
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

func TestExecute_InlineSource(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	// Simulate what runtimed does: write inline code to script in a temp dir,
	// then pass working_dir to the ExecuteRequest.
	workDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(workDir, "script"), []byte("echo hello_from_inline"), 0o644)

	ctx := context.Background()
	_, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:         "inline-1",
		WorkingDir: workDir,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var resp *pb.StatusResponse
	for i := 0; i < 50; i++ {
		resp, err = client.Status(ctx, &pb.StatusRequest{Id: "inline-1"})
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if resp.State == pb.ExecutionState_EXECUTION_STATE_SUCCEEDED || resp.State == pb.ExecutionState_EXECUTION_STATE_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Errorf("expected SUCCEEDED, got %v (stderr=%s)", resp.State, resp.Stderr)
	}
	if resp.Stdout != "hello_from_inline\n" {
		t.Errorf("expected 'hello_from_inline\n', got %q", resp.Stdout)
	}
}

func TestHealth(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	resp, err := client.Health(context.Background(), &pb.HealthRequest{})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !resp.Healthy {
		t.Error("expected healthy=true")
	}
}

func TestOutputIsBounded(t *testing.T) {
	const outputLimit = 128
	client, cleanup := startTestServerWithOutputLimit(t, outputLimit)
	defer cleanup()

	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id: "bounded-output",
		Args: []string{
			"head -c 4096 /dev/zero | tr '\\0' x",
			"head -c 4096 /dev/zero | tr '\\0' y >&2",
		},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	resp := waitForTerminalStatus(t, client, "bounded-output")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Fatalf("state = %v, want succeeded: %s", resp.State, resp.ErrorMessage)
	}
	if !strings.HasSuffix(resp.Stdout, outputTruncatedMarker) {
		t.Fatalf("stdout does not contain truncation marker: %q", resp.Stdout)
	}
	if got, want := len(strings.TrimSuffix(resp.Stdout, outputTruncatedMarker)), outputLimit; got != want {
		t.Fatalf("retained stdout bytes = %d, want %d", got, want)
	}
	if !strings.HasSuffix(resp.Stderr, outputTruncatedMarker) {
		t.Fatalf("stderr does not contain truncation marker: %q", resp.Stderr)
	}
	if got, want := len(strings.TrimSuffix(resp.Stderr, outputTruncatedMarker)), outputLimit; got != want {
		t.Fatalf("retained stderr bytes = %d, want %d", got, want)
	}
}

func TestStatusSnapshotIsImmutable(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	entry := newExecutionEntry(cancel, 64)
	stdout := executionOutput{entry: entry}

	if _, err := stdout.Write([]byte("before")); err != nil {
		t.Fatalf("write initial output: %v", err)
	}
	snapshot := entry.snapshot("immutable")
	if _, err := stdout.Write([]byte("-after")); err != nil {
		t.Fatalf("write later output: %v", err)
	}
	entry.complete(pb.ExecutionState_EXECUTION_STATE_SUCCEEDED, 0, "")

	if snapshot.State != pb.ExecutionState_EXECUTION_STATE_RUNNING {
		t.Fatalf("snapshot state = %v, want running", snapshot.State)
	}
	if snapshot.Stdout != "before" {
		t.Fatalf("snapshot stdout = %q, want immutable initial output", snapshot.Stdout)
	}
	current := entry.snapshot("immutable")
	if current.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Fatalf("current state = %v, want succeeded", current.State)
	}
	if current.Stdout != "before-after" {
		t.Fatalf("current stdout = %q, want all output", current.Stdout)
	}
}

func TestConcurrentStatusListAndReplacement(t *testing.T) {
	client, cleanup := startTestServerWithOutputLimit(t, 256)
	defer cleanup()

	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "concurrent",
		Args: []string{"while :; do echo stdout; echo stderr >&2; done"},
	}); err != nil {
		t.Fatalf("initial Execute: %v", err)
	}

	start := make(chan struct{})
	errCh := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(list bool) {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				var err error
				if list {
					_, err = client.List(ctx, &pb.ListRequest{})
				} else {
					_, err = client.Status(ctx, &pb.StatusRequest{Id: "concurrent"})
				}
				if err != nil {
					errCh <- err
					return
				}
			}
		}(i%2 == 0)
	}
	close(start)

	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:   "concurrent",
		Args: []string{"printf replacement"},
	}); err != nil {
		t.Fatalf("replacement Execute: %v", err)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent status operation: %v", err)
	}

	resp := waitForTerminalStatus(t, client, "concurrent")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
		t.Fatalf("replacement state = %v, want succeeded: %s", resp.State, resp.ErrorMessage)
	}
	if resp.Stdout != "replacement" {
		t.Fatalf("replacement stdout = %q", resp.Stdout)
	}
}

func TestCancelTerminatesProcessGroup(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	workDir := t.TempDir()
	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:         "cancel-process-group",
		WorkingDir: workDir,
		Args:       []string{"sleep 30 & echo $! > child.pid; wait"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	pidPath := filepath.Join(workDir, "child.pid")
	var childPID int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		content, err := os.ReadFile(pidPath)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(content)))
			if err != nil {
				t.Fatalf("parse child pid: %v", err)
			}
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child process pid was not written")
	}

	if _, err := client.Cancel(ctx, &pb.CancelRequest{Id: "cancel-process-group"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		err := syscall.Kill(childPID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child process %d still exists after cancellation", childPID)
}

func TestCancelKillsProcessIgnoringSIGTERM(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	workDir := t.TempDir()
	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:         "cancel-stubborn-process",
		WorkingDir: workDir,
		Args:       []string{"(trap '' TERM; while :; do sleep 30; done) & echo $! > child.pid; wait"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	childPID := waitForPIDFile(t, filepath.Join(workDir, "child.pid"))

	started := time.Now()
	if _, err := client.Cancel(ctx, &pb.CancelRequest{Id: "cancel-stubborn-process"}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if elapsed := time.Since(started); elapsed < processTerminationGrace {
		t.Fatalf("Cancel returned after %v, before termination grace %v", elapsed, processTerminationGrace)
	}
	waitForProcessExit(t, childPID)
}

func TestTimeoutKillsProcessGroupAndWaits(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	workDir := t.TempDir()
	ctx := context.Background()
	if _, err := client.Execute(ctx, &pb.ExecuteRequest{
		Id:             "timeout-process-group",
		WorkingDir:     workDir,
		TimeoutSeconds: 1,
		Args:           []string{"(trap '' TERM; while :; do sleep 30; done) & echo $! > child.pid; wait"},
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	childPID := waitForPIDFile(t, filepath.Join(workDir, "child.pid"))

	resp := waitForTerminalStatus(t, client, "timeout-process-group")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_FAILED {
		t.Fatalf("state = %v, want failed", resp.State)
	}
	if resp.ErrorMessage != "timeout" {
		t.Fatalf("error message = %q, want timeout", resp.ErrorMessage)
	}
	if resp.ExitCode != -1 {
		t.Fatalf("exit code = %d, want -1", resp.ExitCode)
	}
	waitForProcessExit(t, childPID)
}

func TestRejectsEscapingEntrypoint(t *testing.T) {
	client, cleanup := startTestServer(t)
	defer cleanup()

	workDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workDir, "script"), []byte("echo ok\n"), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	if _, err := client.Execute(context.Background(), &pb.ExecuteRequest{
		Id:         "bad-entrypoint",
		WorkingDir: workDir,
		Entrypoint: "../escape.sh",
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	resp := waitForTerminalStatus(t, client, "bad-entrypoint")
	if resp.State != pb.ExecutionState_EXECUTION_STATE_FAILED {
		t.Fatalf("state = %v, want failed", resp.State)
	}
	if !strings.Contains(resp.ErrorMessage, "entrypoint") {
		t.Fatalf("error message = %q, want entrypoint validation", resp.ErrorMessage)
	}
}

func waitForTerminalStatus(t *testing.T, client pb.RuntimeClient, id string) *pb.StatusResponse {
	t.Helper()
	ctx := context.Background()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Status(ctx, &pb.StatusRequest{Id: id})
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		switch resp.State {
		case pb.ExecutionState_EXECUTION_STATE_SUCCEEDED, pb.ExecutionState_EXECUTION_STATE_FAILED:
			return resp
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for execution %s", id)
	return nil
}

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		content, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
			if err != nil {
				t.Fatalf("parse pid from %s: %v", path, err)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still exists", pid)
}
