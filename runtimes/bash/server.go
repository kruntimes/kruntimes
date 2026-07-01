package bash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kruntimes/kruntimes/internal/execpath"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

const (
	defaultOutputLimitBytes = 1024 * 1024
	processTerminationGrace = 2 * time.Second
	outputTruncatedMarker   = "\n[output truncated]\n"
)

type boundedBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func newBoundedBuffer(limit int) boundedBuffer {
	return boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.truncated = b.truncated || len(p) > 0
		return len(p), nil
	}
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		writeBytes := min(remaining, len(p))
		_, _ = b.buffer.Write(p[:writeBytes])
	}
	if len(p) > remaining {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	output := b.buffer.String()
	if b.truncated {
		output += outputTruncatedMarker
	}
	return output
}

type executionEntry struct {
	mu       sync.RWMutex
	state    pb.ExecutionState
	exitCode int32
	errMsg   string
	stdout   boundedBuffer
	stderr   boundedBuffer
	cancel   context.CancelFunc
	done     chan struct{}
}

type executionOutput struct {
	entry  *executionEntry
	stderr bool
}

func (w executionOutput) Write(p []byte) (int, error) {
	w.entry.mu.Lock()
	defer w.entry.mu.Unlock()
	if w.stderr {
		return w.entry.stderr.Write(p)
	}
	return w.entry.stdout.Write(p)
}

func newExecutionEntry(cancel context.CancelFunc, outputLimit int) *executionEntry {
	return &executionEntry{
		state:  pb.ExecutionState_EXECUTION_STATE_RUNNING,
		stdout: newBoundedBuffer(outputLimit),
		stderr: newBoundedBuffer(outputLimit),
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

func (e *executionEntry) complete(state pb.ExecutionState, exitCode int32, errMsg string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = state
	e.exitCode = exitCode
	e.errMsg = errMsg
}

func (e *executionEntry) snapshot(id string) *pb.StatusResponse {
	e.mu.RLock()
	defer e.mu.RUnlock()

	return &pb.StatusResponse{
		Id:           id,
		State:        e.state,
		ExitCode:     e.exitCode,
		Stdout:       e.stdout.String(),
		Stderr:       e.stderr.String(),
		ErrorMessage: e.errMsg,
	}
}

// Server implements the Runtime gRPC service by executing bash commands.
type Server struct {
	pb.UnimplementedRuntimeServer

	operationMu sync.Mutex
	mu          sync.RWMutex
	executions  map[string]*executionEntry
	workDir     string
	outputLimit int
}

func NewServer(workDir string) *Server {
	return NewServerWithOutputLimit(workDir, defaultOutputLimitBytes)
}

func NewServerWithOutputLimit(workDir string, outputLimit int) *Server {
	if workDir == "" {
		workDir = "/workspace"
	}
	return &Server{
		executions:  make(map[string]*executionEntry),
		workDir:     workDir,
		outputLimit: outputLimit,
	}
}

func (s *Server) Execute(ctx context.Context, req *pb.ExecuteRequest) (*pb.ExecuteResponse, error) {
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "request id is required")
	}

	// Serialize replacement and cancellation without blocking Status or List.
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	if existing := s.execution(req.Id); existing != nil {
		existing.cancel()
		if err := waitForExecution(ctx, existing.done); err != nil {
			return nil, err
		}
	}

	executionCtx, cancel := executionContext(req.TimeoutSeconds)
	entry := newExecutionEntry(cancel, s.outputLimit)
	s.mu.Lock()
	s.executions[req.Id] = entry
	s.mu.Unlock()

	go s.execute(executionCtx, req, entry)
	return &pb.ExecuteResponse{Id: req.Id}, nil
}

func (s *Server) Status(_ context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	entry := s.execution(req.Id)
	if entry == nil {
		return nil, status.Errorf(codes.NotFound, "request %s not found", req.Id)
	}
	return entry.snapshot(req.Id), nil
}

func (s *Server) List(context.Context, *pb.ListRequest) (*pb.ListResponse, error) {
	s.mu.RLock()
	entries := make(map[string]*executionEntry, len(s.executions))
	for id, entry := range s.executions {
		entries[id] = entry
	}
	s.mu.RUnlock()

	resp := &pb.ListResponse{Entries: make([]*pb.StatusResponse, 0, len(entries))}
	for id, entry := range entries {
		resp.Entries = append(resp.Entries, entry.snapshot(id))
	}
	return resp, nil
}

func (s *Server) Cancel(ctx context.Context, req *pb.CancelRequest) (*pb.CancelResponse, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	entry := s.execution(req.Id)
	if entry == nil {
		return nil, status.Errorf(codes.NotFound, "request %s not found", req.Id)
	}
	entry.cancel()
	if err := waitForExecution(ctx, entry.done); err != nil {
		return nil, err
	}

	s.mu.Lock()
	if s.executions[req.Id] == entry {
		delete(s.executions, req.Id)
	}
	s.mu.Unlock()
	return &pb.CancelResponse{}, nil
}

func (s *Server) Forget(_ context.Context, req *pb.ForgetRequest) (*pb.ForgetResponse, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	entry := s.execution(req.Id)
	if entry == nil {
		return nil, status.Errorf(codes.NotFound, "request %s not found", req.Id)
	}
	select {
	case <-entry.done:
	default:
		return nil, status.Errorf(codes.FailedPrecondition, "request %s is still running", req.Id)
	}

	s.mu.Lock()
	if s.executions[req.Id] == entry {
		delete(s.executions, req.Id)
	}
	s.mu.Unlock()
	return &pb.ForgetResponse{}, nil
}

func (s *Server) Health(context.Context, *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Healthy: true}, nil
}

func (s *Server) execution(id string) *executionEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.executions[id]
}

func (s *Server) execute(ctx context.Context, req *pb.ExecuteRequest, entry *executionEntry) {
	defer close(entry.done)
	defer entry.cancel()

	workDir := req.WorkingDir
	if workDir == "" {
		workDir = filepath.Join(s.workDir, req.Id)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		entry.complete(pb.ExecutionState_EXECUTION_STATE_FAILED, 0, fmt.Sprintf("mkdir: %v", err))
		return
	}

	cmd, err := buildCommand(req, workDir)
	if err != nil {
		entry.complete(pb.ExecutionState_EXECUTION_STATE_FAILED, 0, err.Error())
		return
	}
	cmd.Dir = workDir
	cmd.Stdout = executionOutput{entry: entry}
	cmd.Stderr = executionOutput{entry: entry, stderr: true}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = append(cmd.Env, os.Environ()...)

	if err := ctx.Err(); err != nil {
		entry.complete(cancelledResult(err))
		return
	}
	if err := cmd.Start(); err != nil {
		entry.complete(pb.ExecutionState_EXECUTION_STATE_FAILED, 0, err.Error())
		return
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var runErr error
	select {
	case runErr = <-waitCh:
		entry.complete(commandResult(runErr))
	case <-ctx.Done():
		runErr = terminateProcessGroupAndWait(cmd.Process.Pid, waitCh, processTerminationGrace)
		entry.complete(cancelledResult(ctx.Err()))
	}

	result := entry.snapshot(req.Id)
	klog.V(3).Infof("Run %s finished: state=%v exit=%d", req.Id, result.State, result.ExitCode)
}

func buildCommand(req *pb.ExecuteRequest, workDir string) (*exec.Cmd, error) {
	entrypoint, err := execpath.ResolveEntrypoint(req.Entrypoint, "script")
	if err != nil {
		return nil, err
	}
	scriptPath := filepath.Join(workDir, entrypoint)
	if _, err := os.Stat(scriptPath); err == nil {
		return exec.Command("bash", append([]string{scriptPath}, req.Args...)...), nil
	}
	if isExplicitShellCommand(req.Args) {
		return exec.Command(req.Args[0], req.Args[1:]...), nil
	}
	switch len(req.Args) {
	case 0:
		return nil, errors.New("no args or script provided")
	case 1:
		return exec.Command("bash", "-c", req.Args[0]), nil
	default:
		return exec.Command("bash", "-c", strings.Join(req.Args, "\n")+"\n"), nil
	}
}

func isExplicitShellCommand(args []string) bool {
	return len(args) >= 3 && (args[0] == "sh" || args[0] == "bash") && args[1] == "-c"
}

func executionContext(timeoutSeconds int64) (context.Context, context.CancelFunc) {
	if timeoutSeconds <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), time.Duration(timeoutSeconds)*time.Second)
}

func waitForExecution(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func commandResult(err error) (pb.ExecutionState, int32, string) {
	if err == nil {
		return pb.ExecutionState_EXECUTION_STATE_SUCCEEDED, 0, ""
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return pb.ExecutionState_EXECUTION_STATE_FAILED, int32(exitErr.ExitCode()), exitErr.Error()
	}
	return pb.ExecutionState_EXECUTION_STATE_FAILED, 0, err.Error()
}

func cancelledResult(err error) (pb.ExecutionState, int32, string) {
	if errors.Is(err, context.DeadlineExceeded) {
		return pb.ExecutionState_EXECUTION_STATE_FAILED, -1, "timeout"
	}
	return pb.ExecutionState_EXECUTION_STATE_FAILED, -1, "cancelled"
}

func terminateProcessGroup(pid int, signal syscall.Signal) {
	if pid <= 0 {
		return
	}
	if err := syscall.Kill(-pid, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		klog.V(2).Infof("Failed to signal process group %d: %v", pid, err)
	}
}

func terminateProcessGroupAndWait(pid int, waitCh <-chan error, grace time.Duration) error {
	terminateProcessGroup(pid, syscall.SIGTERM)

	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	var (
		commandDone bool
		groupDone   bool
		waitErr     error
		graceC      = timer.C
	)
	for !commandDone || !groupDone {
		select {
		case waitErr = <-waitCh:
			commandDone = true
			waitCh = nil
			groupDone = !processGroupExists(pid)
		case <-ticker.C:
			groupDone = !processGroupExists(pid)
		case <-graceC:
			terminateProcessGroup(pid, syscall.SIGKILL)
			graceC = nil
		}
	}
	return waitErr
}

func processGroupExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
