package bash

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sessionEntry is Runtime Server-local state. runtimed owns the operation
// queue, so this entry only records the fenced workspace lifecycle.
type sessionEntry struct {
	mu         sync.RWMutex
	identity   *pb.SessionIdentity
	workDir    string
	sessionEnv map[string]string
	state      pb.SessionState
	activity   time.Time
}

func (s *Server) RegisterSession(_ context.Context, registration *pb.RegisterSessionRequest) (*pb.SessionStatus, error) {
	identity, workingDir, err := s.validateSessionRegistration(registration)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	if existing := s.session(identity.RunUid); existing != nil {
		if !existing.matches(identity) {
			return nil, status.Error(codes.FailedPrecondition, "session assignment is stale")
		}
		return existing.status(), nil
	}

	entry := &sessionEntry{
		identity:   cloneSessionIdentity(identity),
		workDir:    workingDir,
		sessionEnv: maps.Clone(registration.Env),
		state:      pb.SessionState_SESSION_STATE_READY,
		activity:   time.Now(),
	}
	s.mu.Lock()
	s.sessions[identity.RunUid] = entry
	s.mu.Unlock()
	return entry.status(), nil
}

func (s *Server) GetSessionStatus(_ context.Context, req *pb.GetSessionStatusRequest) (*pb.SessionStatus, error) {
	entry, err := s.matchSession(req.GetIdentity())
	if err != nil {
		return nil, err
	}
	return entry.status(), nil
}

// ExecuteSessionOperation executes one mutation already serialized and
// admitted by runtimed. The Runtime Server deliberately does not assign
// operation IDs or own a queue.
func (s *Server) ExecuteSessionOperation(ctx context.Context, req *pb.ExecuteSessionOperationRequest) (*pb.ExecuteSessionOperationResponse, error) {
	entry, err := s.matchSession(req.GetIdentity())
	if err != nil {
		return nil, err
	}
	switch {
	case req.GetCommand() != nil:
		result, err := s.executeSessionCommand(ctx, entry, req.GetCommand())
		if err != nil {
			return nil, err
		}
		return &pb.ExecuteSessionOperationResponse{Command: result}, nil
	case req.GetWriteFile() != nil:
		if err := s.writeSessionFile(entry, req.GetWriteFile()); err != nil {
			return nil, err
		}
	case req.GetCreateDirectory() != nil:
		if err := s.createSessionDirectory(entry, req.GetCreateDirectory()); err != nil {
			return nil, err
		}
	case req.GetDeleteFile() != nil:
		if err := s.deleteSessionFile(entry, req.GetDeleteFile()); err != nil {
			return nil, err
		}
	case req.GetRenameFile() != nil:
		if err := s.renameSessionFile(entry, req.GetRenameFile()); err != nil {
			return nil, err
		}
	default:
		return nil, status.Error(codes.InvalidArgument, "exactly one session operation is required")
	}
	return &pb.ExecuteSessionOperationResponse{}, nil
}

func (s *Server) executeSessionCommand(ctx context.Context, entry *sessionEntry, commandRequest *pb.SessionCommand) (*pb.SessionCommandResult, error) {
	if (len(commandRequest.Argv) == 0) == (commandRequest.Shell == "") {
		return nil, status.Error(codes.InvalidArgument, "exactly one of argv or shell is required")
	}
	workingDir, err := sessionPath(entry, commandRequest.WorkingDirectory, true)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	commandCtx, cancel := sessionCommandContext(ctx, commandRequest.TimeoutMillis)
	defer cancel()
	command := sessionCommand(commandCtx, commandRequest)
	command.Dir = workingDir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdin = bytes.NewReader(commandRequest.Stdin)
	stdout := newBoundedBuffer(s.outputLimit)
	stderr := newBoundedBuffer(s.outputLimit)
	command.Stdout = &stdout
	command.Stderr = &stderr
	command.Env = sessionCommandEnv(entry.sessionEnv, commandRequest.Env)

	waitCh, err := startManagedCommand(command)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "start session command: %v", err)
	}
	defer entry.touch()

	select {
	case result := <-waitCh:
		return &pb.SessionCommandResult{
			ExitCode: sessionCommandExitCode(result),
			Stdout:   []byte(stdout.String()),
			Stderr:   []byte(stderr.String()),
		}, nil
	case <-commandCtx.Done():
		_ = terminateProcessGroupAndWait(command.Process.Pid, waitCh, s.sessionTerminationGrace)
		if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
			return &pb.SessionCommandResult{
				ExitCode: -1,
				Stdout:   []byte(stdout.String()),
				Stderr:   []byte(stderr.String()),
				TimedOut: true,
			}, nil
		}
		return nil, status.FromContextError(commandCtx.Err()).Err()
	}
}

func (s *Server) ReadSessionFile(_ context.Context, req *pb.ReadSessionFileRequest) (*pb.ReadSessionFileResponse, error) {
	entry, err := s.matchSession(req.GetIdentity())
	if err != nil {
		return nil, err
	}
	path, err := sessionPath(entry, req.Path, false)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	maxBytes := req.MaxBytes
	if maxBytes <= 0 {
		return nil, status.Error(codes.InvalidArgument, "max bytes must be positive")
	}
	maxBytes = min(maxBytes, int64(s.outputLimit))
	file, err := os.Open(path)
	if err != nil {
		return nil, sessionFileError("open", err)
	}
	defer file.Close()
	contents, err := ioReadAllLimit(file, maxBytes)
	if err != nil {
		return nil, sessionFileError("read", err)
	}
	return &pb.ReadSessionFileResponse{Contents: contents[:min(int64(len(contents)), maxBytes)], Truncated: int64(len(contents)) > maxBytes}, nil
}

func (s *Server) ListSessionFiles(_ context.Context, req *pb.ListSessionFilesRequest) (*pb.ListSessionFilesResponse, error) {
	entry, err := s.matchSession(req.GetIdentity())
	if err != nil {
		return nil, err
	}
	page, err := sessionFilePageRequest(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	directory, err := sessionPath(entry, req.Path, true)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	entries, hasMore, err := readSessionFilePage(directory, page.after, page.limit)
	if err != nil {
		return nil, sessionFileError("list", err)
	}
	response := &pb.ListSessionFilesResponse{Entries: make([]*pb.SessionFileInfo, 0, len(entries))}
	for _, item := range entries {
		info, err := item.Info()
		if err != nil {
			return nil, sessionFileError("stat", err)
		}
		response.Entries = append(response.Entries, &pb.SessionFileInfo{Path: item.Name(), Directory: item.IsDir(), SizeBytes: info.Size()})
	}
	if hasMore {
		response.NextPageToken, err = page.nextToken(entries[len(entries)-1].Name())
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode next page token: %v", err)
		}
	}
	return response, nil
}

func (s *Server) writeSessionFile(entry *sessionEntry, req *pb.SessionFileWrite) error {
	if len(req.Contents) > s.outputLimit {
		return status.Error(codes.ResourceExhausted, "file contents exceed the Runtime Server transfer limit")
	}
	path, err := sessionPath(entry, req.Path, false)
	if err != nil && req.CreateParents {
		path, err = sessionPathWithMissingParent(entry, req.Path, false)
		if err == nil {
			err = createSessionParents(entry, path)
		}
		if err == nil {
			path, err = sessionPath(entry, req.Path, false)
		}
	}
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := os.WriteFile(path, req.Contents, 0o644); err != nil {
		return sessionFileError("write", err)
	}
	entry.touch()
	return nil
}

func (s *Server) createSessionDirectory(entry *sessionEntry, req *pb.SessionDirectoryCreate) error {
	path, err := sessionPath(entry, req.Path, false)
	if err != nil {
		path, err = sessionPathWithMissingParent(entry, req.Path, false)
		if err == nil {
			err = createSessionParents(entry, path)
		}
		if err == nil {
			path, err = sessionPath(entry, req.Path, false)
		}
	}
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return sessionFileError("create directory", err)
	}
	entry.touch()
	return nil
}

func (s *Server) deleteSessionFile(entry *sessionEntry, req *pb.SessionFileDelete) error {
	path, err := sessionPath(entry, req.Path, false)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	info, err := os.Lstat(path)
	if err != nil {
		return sessionFileError("stat", err)
	}
	if info.IsDir() && !req.Recursive {
		return status.Error(codes.FailedPrecondition, "recursive must be true to delete a directory")
	}
	if req.Recursive {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return sessionFileError("delete", err)
	}
	entry.touch()
	return nil
}

func (s *Server) renameSessionFile(entry *sessionEntry, req *pb.SessionFileRename) error {
	source, err := sessionPath(entry, req.SourcePath, false)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	destination, err := sessionPath(entry, req.DestinationPath, false)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if _, err := os.Lstat(destination); err == nil {
		if !req.Overwrite {
			return status.Error(codes.AlreadyExists, "destination already exists")
		}
		if err := os.RemoveAll(destination); err != nil {
			return sessionFileError("replace destination", err)
		}
	} else if !os.IsNotExist(err) {
		return sessionFileError("stat destination", err)
	}
	if err := os.Rename(source, destination); err != nil {
		return sessionFileError("rename", err)
	}
	entry.touch()
	return nil
}

func (s *Server) CloseSession(_ context.Context, req *pb.CloseSessionRequest) (*pb.CloseSessionResponse, error) {
	identity := req.GetIdentity()
	if err := validateSessionIdentity(identity); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	entry := s.session(identity.RunUid)
	if entry == nil {
		return &pb.CloseSessionResponse{Identity: cloneSessionIdentity(identity)}, nil
	}
	if !entry.matches(identity) {
		return nil, status.Error(codes.FailedPrecondition, "session assignment is stale")
	}
	entry.close()
	s.mu.Lock()
	if s.sessions[identity.RunUid] == entry {
		delete(s.sessions, identity.RunUid)
	}
	s.mu.Unlock()
	return &pb.CloseSessionResponse{Identity: cloneSessionIdentity(identity)}, nil
}

func (s *Server) validateSessionRegistration(req *pb.RegisterSessionRequest) (*pb.SessionIdentity, string, error) {
	if req == nil {
		return nil, "", fmt.Errorf("session registration is required")
	}
	if err := validateSessionIdentity(req.GetIdentity()); err != nil {
		return nil, "", err
	}
	workingDir, err := s.runtimeWorkingDir(req.WorkingDir)
	if err != nil {
		return nil, "", err
	}
	return cloneSessionIdentity(req.Identity), workingDir, nil
}

func validateSessionIdentity(identity *pb.SessionIdentity) error {
	if identity == nil || identity.RunUid == "" || identity.AssignedPodUid == "" {
		return fmt.Errorf("session run uid and assigned pod uid are required")
	}
	return nil
}

func (s *Server) session(runUID string) *sessionEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[runUID]
}

func (s *Server) matchSession(identity *pb.SessionIdentity) (*sessionEntry, error) {
	if err := validateSessionIdentity(identity); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	entry := s.session(identity.RunUid)
	if entry == nil {
		return nil, status.Error(codes.NotFound, "session not found")
	}
	if !entry.matches(identity) {
		return nil, status.Error(codes.FailedPrecondition, "session assignment is stale")
	}
	return entry, nil
}

func (e *sessionEntry) matches(identity *pb.SessionIdentity) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.identity.RunUid == identity.RunUid && e.identity.AssignedPodUid == identity.AssignedPodUid
}

func (e *sessionEntry) status() *pb.SessionStatus {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return &pb.SessionStatus{
		Identity:             cloneSessionIdentity(e.identity),
		State:                e.state,
		LastActivityUnixNano: e.activity.UnixNano(),
	}
}

func (e *sessionEntry) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = pb.SessionState_SESSION_STATE_CLOSED
	e.activity = time.Now()
}

func (e *sessionEntry) touch() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.activity = time.Now()
}

func sessionCommand(ctx context.Context, req *pb.SessionCommand) *exec.Cmd {
	if req.Shell != "" {
		return exec.CommandContext(ctx, "bash", "-c", req.Shell)
	}
	return exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
}

func sessionCommandContext(ctx context.Context, timeoutMillis int64) (context.Context, context.CancelFunc) {
	if timeoutMillis <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(timeoutMillis)*time.Millisecond)
}

func sessionCommandEnv(sessionEnv, commandEnv map[string]string) []string {
	env := make(map[string]string)
	for _, item := range os.Environ() {
		name, value, ok := strings.Cut(item, "=")
		if ok {
			env[name] = value
		}
	}
	for name, value := range sessionEnv {
		env[name] = value
	}
	for name, value := range commandEnv {
		env[name] = value
	}
	names := slices.Sorted(maps.Keys(env))
	values := make([]string, 0, len(names))
	for _, name := range names {
		values = append(values, name+"="+env[name])
	}
	return values
}

func sessionCommandExitCode(result commandWaitResult) int32 {
	if result.err == nil && result.status.Exited() && result.status.ExitStatus() == 0 {
		return 0
	}
	if result.err == nil && result.status.Exited() {
		return int32(result.status.ExitStatus())
	}
	return -1
}

func sessionPath(entry *sessionEntry, requested string, allowRoot bool) (string, error) {
	candidate, root, err := sessionPathWithRoot(entry, requested, allowRoot)
	if err != nil {
		return "", err
	}
	if candidate == root {
		return root, nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(candidate))
	if err != nil {
		return "", fmt.Errorf("resolve file parent: %w", err)
	}
	if err := ensureWithinSessionWorkspace(root, parent); err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
		if err := ensureWithinSessionWorkspace(root, resolved); err != nil {
			return "", err
		}
	}
	return candidate, nil
}

func sessionPathWithMissingParent(entry *sessionEntry, requested string, allowRoot bool) (string, error) {
	candidate, root, err := sessionPathWithRoot(entry, requested, allowRoot)
	if err != nil {
		return "", err
	}
	ancestor := filepath.Dir(candidate)
	for {
		if _, err := os.Lstat(ancestor); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect file parent: %w", err)
		}
		next := filepath.Dir(ancestor)
		if next == ancestor {
			return "", fmt.Errorf("file path must not escape the session workspace")
		}
		ancestor = next
	}
	resolvedAncestor, err := filepath.EvalSymlinks(ancestor)
	if err != nil {
		return "", fmt.Errorf("resolve file parent: %w", err)
	}
	if err := ensureWithinSessionWorkspace(root, resolvedAncestor); err != nil {
		return "", err
	}
	return candidate, nil
}

func sessionPathWithRoot(entry *sessionEntry, requested string, allowRoot bool) (string, string, error) {
	entry.mu.RLock()
	root := entry.workDir
	entry.mu.RUnlock()
	if requested == "" {
		if allowRoot {
			return root, root, nil
		}
		return "", "", fmt.Errorf("file path is required")
	}
	if filepath.IsAbs(requested) {
		return "", "", fmt.Errorf("file path must be workspace-relative")
	}
	cleaned := filepath.Clean(requested)
	if cleaned == "." {
		if allowRoot {
			return root, root, nil
		}
		return "", "", fmt.Errorf("file path must not be the workspace root")
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("file path must not escape the workspace")
	}
	return filepath.Join(root, cleaned), root, nil
}

func createSessionParents(entry *sessionEntry, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	return nil
}

func ensureWithinSessionWorkspace(root, path string) error {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve session workspace: %w", err)
	}
	relative, err := filepath.Rel(resolvedRoot, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("file path must not escape the session workspace")
	}
	return nil
}

func ioReadAllLimit(file *os.File, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(file, limit+1))
}

func sessionFileError(operation string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return status.Errorf(codes.NotFound, "%s session file: %v", operation, err)
	}
	return status.Errorf(codes.Internal, "%s session file: %v", operation, err)
}

func cloneSessionIdentity(identity *pb.SessionIdentity) *pb.SessionIdentity {
	if identity == nil {
		return nil
	}
	return &pb.SessionIdentity{RunUid: identity.RunUid, AssignedPodUid: identity.AssignedPodUid}
}
