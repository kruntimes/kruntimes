package bash

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	pb "github.com/airconduct/kruntime/api/runtime/v1"
)

type taskEntry struct {
	cmd      *exec.Cmd
	state    pb.ExecutionState
	exitCode int32
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	errMsg   string
	done     chan struct{}
}

// Server implements the TaskRuntime gRPC service by executing bash commands.
type Server struct {
	pb.UnimplementedRuntimeServer

	mu      sync.Mutex
	tasks   map[string]*taskEntry
	workDir string
}

func NewServer(workDir string) *Server {
	if workDir == "" {
		workDir = "/workspace"
	}
	return &Server{
		tasks:   make(map[string]*taskEntry),
		workDir: workDir,
	}
}

func (s *Server) Execute(ctx context.Context, req *pb.ExecuteRequest) (*pb.ExecuteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.tasks[req.Id]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "task %s already exists", req.Id)
	}

	entry := &taskEntry{
		state: pb.ExecutionState_EXECUTION_STATE_RUNNING,
		done:  make(chan struct{}),
	}
	s.tasks[req.Id] = entry

	go s.execute(req, entry)
	return &pb.ExecuteResponse{Id: req.Id}, nil
}

func (s *Server) Status(ctx context.Context, req *pb.StatusRequest) (*pb.StatusResponse, error) {
	s.mu.Lock()
	entry, ok := s.tasks[req.Id]
	s.mu.Unlock()

	if !ok {
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.Id)
	}

	select {
	case <-entry.done:
	default:
	}

	return &pb.StatusResponse{
		Id:           req.Id,
		State:        entry.state,
		ExitCode:     entry.exitCode,
		Stdout:       entry.stdout.String(),
		Stderr:       entry.stderr.String(),
		ErrorMessage: entry.errMsg,
	}, nil
}

func (s *Server) List(ctx context.Context, req *pb.ListRequest) (*pb.ListResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp := &pb.ListResponse{}
	for id, entry := range s.tasks {
		resp.Entries = append(resp.Entries, &pb.StatusResponse{
			Id:           id,
			State:        entry.state,
			ExitCode:     entry.exitCode,
			Stdout:       entry.stdout.String(),
			Stderr:       entry.stderr.String(),
			ErrorMessage: entry.errMsg,
		})
	}
	return resp, nil
}

func (s *Server) Cancel(ctx context.Context, req *pb.CancelRequest) (*pb.CancelResponse, error) {
	s.mu.Lock()
	entry, ok := s.tasks[req.Id]
	if !ok {
		s.mu.Unlock()
		return nil, status.Errorf(codes.NotFound, "task %s not found", req.Id)
	}
	s.mu.Unlock() // unlock before killing to avoid deadlock

	if entry.cmd != nil && entry.cmd.Process != nil {
		entry.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-entry.done:
		case <-time.After(5 * time.Second):
			entry.cmd.Process.Kill()
		}
	}

	s.mu.Lock()
	delete(s.tasks, req.Id)
	s.mu.Unlock()

	return &pb.CancelResponse{}, nil
}

func (s *Server) execute(req *pb.ExecuteRequest, entry *taskEntry) {
	defer close(entry.done)

	workDir := req.WorkingDir
	if workDir == "" {
		workDir = filepath.Join(s.workDir, req.Id)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		entry.state = pb.ExecutionState_EXECUTION_STATE_FAILED
		entry.errMsg = fmt.Sprintf("mkdir: %v", err)
		return
	}

	var cmd *exec.Cmd
	if len(req.Commands) == 1 {
		cmd = exec.Command("bash", "-c", req.Commands[0])
	} else if len(req.Commands) > 1 {
		script := ""
		for _, c := range req.Commands {
			script += c + "\n"
		}
		cmd = exec.Command("bash", "-c", script)
	} else {
		entry.state = pb.ExecutionState_EXECUTION_STATE_FAILED
		entry.errMsg = "no commands provided"
		return
	}

	cmd.Dir = workDir
	cmd.Stdout = &entry.stdout
	cmd.Stderr = &entry.stderr

	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = append(cmd.Env, os.Environ()...)

	entry.cmd = cmd

	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = 600 // default 10 min
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()

	select {
	case err := <-done:
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				entry.exitCode = int32(exitErr.ExitCode())
				entry.state = pb.ExecutionState_EXECUTION_STATE_FAILED
				entry.errMsg = exitErr.Error()
			} else {
				entry.state = pb.ExecutionState_EXECUTION_STATE_FAILED
				entry.errMsg = err.Error()
			}
		} else {
			entry.exitCode = 0
			entry.state = pb.ExecutionState_EXECUTION_STATE_SUCCEEDED
		}
	case <-ctx.Done():
		cmd.Process.Kill()
		entry.state = pb.ExecutionState_EXECUTION_STATE_FAILED
		entry.errMsg = "timeout"
		entry.exitCode = -1
	}

	klog.V(3).Infof("Task %s finished: state=%v exit=%d", req.Id, entry.state, entry.exitCode)
}
