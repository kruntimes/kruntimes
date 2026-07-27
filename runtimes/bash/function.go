package bash

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kruntimes/kruntimes/internal/execpath"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

const (
	defaultFunctionInvokeTimeout = 30 * time.Second
	maxFunctionInvokeTimeout     = 5 * time.Minute
	defaultFunctionDrainTimeout  = 30 * time.Second
)

var bashFunctionName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type functionEntry struct {
	mu                   sync.Mutex
	registration         *pb.FunctionRegistration
	attempt              int32
	digest               string
	workingDir           string
	handlerFile          string
	handlerName          string
	env                  map[string]string
	state                pb.FunctionRegistrationState
	inFlight             bool
	lastActivityUnixNano int64
	cancel               context.CancelFunc
	done                 chan struct{}
}

func (s *Server) RegisterFunction(ctx context.Context, req *pb.RegisterFunctionRequest) (*pb.RegisterFunctionResponse, error) {
	workingDir, handlerFile, handlerName, err := s.validateFunctionRegistration(req)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	if existing := s.function(req.RunUid); existing != nil {
		existing.mu.Lock()
		switch {
		case req.RegistrationAttempt < existing.attempt:
			existing.mu.Unlock()
			return nil, status.Error(codes.FailedPrecondition, "registration attempt is stale")
		case req.RegistrationAttempt == existing.attempt:
			if req.RegistrationDigest != existing.digest {
				existing.mu.Unlock()
				return nil, status.Error(codes.AlreadyExists, "registration attempt already exists with a different digest")
			}
			resp := &pb.RegisterFunctionResponse{Registration: cloneFunctionRegistration(existing.registration), State: existing.state}
			existing.mu.Unlock()
			return resp, nil
		default:
			inFlight, cancel, done := existing.inFlight, existing.cancel, existing.done
			existing.mu.Unlock()
			if inFlight {
				cancel()
				if err := waitForFunction(ctx, done); err != nil {
					return nil, err
				}
			}
		}
	}

	registrationID, err := newRegistrationID()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "generate registration id: %v", err)
	}
	entry := &functionEntry{
		registration: &pb.FunctionRegistration{RunUid: req.RunUid, RegistrationId: registrationID},
		attempt:      req.RegistrationAttempt,
		digest:       req.RegistrationDigest,
		workingDir:   workingDir,
		handlerFile:  handlerFile,
		handlerName:  handlerName,
		env:          cloneStringMap(req.Env),
		state:        pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY,
	}
	s.mu.Lock()
	s.functions[req.RunUid] = entry
	s.mu.Unlock()

	return &pb.RegisterFunctionResponse{Registration: cloneFunctionRegistration(entry.registration), State: entry.state}, nil
}

func (s *Server) FunctionStatus(_ context.Context, req *pb.FunctionStatusRequest) (*pb.FunctionStatusResponse, error) {
	entry, err := s.matchFunction(req.GetRegistration())
	if err != nil {
		return nil, err
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()

	inFlight := int32(0)
	if entry.inFlight {
		inFlight = 1
	}
	return &pb.FunctionStatusResponse{
		Registration:         cloneFunctionRegistration(entry.registration),
		State:                entry.state,
		InFlight:             inFlight,
		LastActivityUnixNano: entry.lastActivityUnixNano,
	}, nil
}

func (s *Server) InvokeFunction(ctx context.Context, req *pb.InvokeFunctionRequest) (*pb.InvokeFunctionResponse, error) {
	if req.InvocationId == "" || len(req.InvocationId) > 128 {
		return nil, status.Error(codes.InvalidArgument, "invocation id must be between 1 and 128 bytes")
	}
	if req.ContentType != "application/json" {
		return nil, status.Error(codes.InvalidArgument, "Bash functions support only application/json input")
	}
	if len(req.Input) == 0 || len(req.Input) > defaultOutputLimitBytes || !json.Valid(req.Input) || bytes.IndexByte(req.Input, 0) >= 0 {
		return nil, status.Error(codes.InvalidArgument, "input must be valid JSON no larger than 1 MiB")
	}

	// Registration changes and invocation admission share this lock. Once an
	// invocation is admitted, a newer registration or unregistration fences it
	// by cancelling and waiting for this invocation to finish.
	s.operationMu.Lock()
	entry, err := s.matchFunction(req.GetRegistration())
	if err != nil {
		s.operationMu.Unlock()
		return nil, err
	}
	invokeCtx, cancel := functionInvokeContext(ctx, req.TimeoutMillis)

	entry.mu.Lock()
	if entry.state != pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY {
		entry.mu.Unlock()
		s.operationMu.Unlock()
		cancel()
		return nil, status.Error(codes.FailedPrecondition, "function registration is not ready")
	}
	if entry.inFlight {
		entry.mu.Unlock()
		s.operationMu.Unlock()
		cancel()
		return nil, status.Error(codes.ResourceExhausted, "function registration already has an in-flight invocation")
	}
	entry.inFlight = true
	entry.lastActivityUnixNano = time.Now().UnixNano()
	entry.cancel = cancel
	entry.done = make(chan struct{})
	workingDir, handlerFile, handlerName, env, registration := entry.workingDir, entry.handlerFile, entry.handlerName, cloneStringMap(entry.env), cloneFunctionRegistration(entry.registration)
	done := entry.done
	entry.mu.Unlock()
	s.operationMu.Unlock()
	defer cancel()

	defer func() {
		entry.mu.Lock()
		entry.inFlight = false
		entry.cancel = nil
		entry.lastActivityUnixNano = time.Now().UnixNano()
		close(done)
		entry.mu.Unlock()
	}()

	output, err := invokeBashFunction(invokeCtx, workingDir, handlerFile, handlerName, string(req.Input), env, s.outputLimit)
	if err != nil {
		if errorsIsContextError(err) {
			return nil, status.FromContextError(err).Err()
		}
		if errorsIsOutputLimit(err) {
			return nil, status.Error(codes.ResourceExhausted, err.Error())
		}
		return nil, status.Errorf(codes.Internal, "invoke handler: %v", err)
	}
	return &pb.InvokeFunctionResponse{
		Registration: registration,
		InvocationId: req.InvocationId,
		Output:       output,
		ContentType:  "application/json",
	}, nil
}

func (s *Server) UnregisterFunction(ctx context.Context, req *pb.UnregisterFunctionRequest) (*pb.UnregisterFunctionResponse, error) {
	registration := req.GetRegistration()
	if registration == nil || registration.RunUid == "" || registration.RegistrationId == "" {
		return nil, status.Error(codes.InvalidArgument, "registration run uid and id are required")
	}

	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	entry := s.function(registration.RunUid)
	if entry == nil {
		return &pb.UnregisterFunctionResponse{Registration: cloneFunctionRegistration(registration)}, nil
	}

	entry.mu.Lock()
	if entry.registration.RegistrationId != registration.RegistrationId {
		entry.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "function registration is stale")
	}
	entry.state = pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_DRAINING
	inFlight, cancel, done := entry.inFlight, entry.cancel, entry.done
	entry.mu.Unlock()

	if inFlight {
		if req.CancelInFlight {
			cancel()
		}
		drainCtx, cancel := functionDrainContext(ctx, req.DrainTimeoutMillis)
		err := waitForFunction(drainCtx, done)
		cancel()
		if err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if s.functions[registration.RunUid] == entry {
		delete(s.functions, registration.RunUid)
	}
	s.mu.Unlock()
	return &pb.UnregisterFunctionResponse{Registration: cloneFunctionRegistration(registration)}, nil
}

func (s *Server) function(runUID string) *functionEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.functions[runUID]
}

func (s *Server) matchFunction(registration *pb.FunctionRegistration) (*functionEntry, error) {
	if registration == nil || registration.RunUid == "" || registration.RegistrationId == "" {
		return nil, status.Error(codes.InvalidArgument, "registration run uid and id are required")
	}
	entry := s.function(registration.RunUid)
	if entry == nil {
		return nil, status.Error(codes.NotFound, "function registration not found")
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.registration.RegistrationId != registration.RegistrationId {
		return nil, status.Error(codes.FailedPrecondition, "function registration is stale")
	}
	return entry, nil
}

func (s *Server) validateFunctionRegistration(req *pb.RegisterFunctionRequest) (string, string, string, error) {
	if req.RunUid == "" || req.RegistrationAttempt < 1 || req.RegistrationDigest == "" {
		return "", "", "", fmt.Errorf("run uid, positive registration attempt, and registration digest are required")
	}
	if len(req.RegistrationDigest) > 128 {
		return "", "", "", fmt.Errorf("registration digest must be no larger than 128 bytes")
	}
	workingDir, err := s.functionWorkingDir(req.WorkingDir)
	if err != nil {
		return "", "", "", err
	}
	handlerFile, handlerName, err := parseBashHandler(req.Handler)
	if err != nil {
		return "", "", "", err
	}
	handlerPath, err := functionFilePath(workingDir, handlerFile)
	if err != nil {
		return "", "", "", err
	}
	if err := validateBashFunction(workingDir, handlerPath, handlerName, req.Env); err != nil {
		return "", "", "", err
	}
	return workingDir, handlerFile, handlerName, nil
}

func (s *Server) functionWorkingDir(workingDir string) (string, error) {
	if workingDir == "" {
		return "", fmt.Errorf("working directory is required")
	}
	base, err := filepath.EvalSymlinks(s.workDir)
	if err != nil {
		return "", fmt.Errorf("resolve runtime workspace: %w", err)
	}
	path, err := filepath.EvalSymlinks(workingDir)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	relative, err := filepath.Rel(base, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("working directory must be within the runtime workspace")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory must be a directory")
	}
	return path, nil
}

func functionFilePath(workingDir, handlerFile string) (string, error) {
	path, err := filepath.EvalSymlinks(filepath.Join(workingDir, handlerFile))
	if err != nil {
		return "", fmt.Errorf("handler file: %w", err)
	}
	relative, err := filepath.Rel(workingDir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("handler file must be within the working directory")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("handler file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("handler file must be a regular file")
	}
	return path, nil
}

func parseBashHandler(handler string) (string, string, error) {
	separator := strings.LastIndex(handler, ".")
	if separator <= 0 || separator == len(handler)-1 {
		return "", "", fmt.Errorf("handler must use file.function form")
	}
	file, err := execpath.ResolveEntrypoint(handler[:separator]+".sh", "")
	if err != nil {
		return "", "", err
	}
	function := handler[separator+1:]
	if !bashFunctionName.MatchString(function) {
		return "", "", fmt.Errorf("handler function name is invalid")
	}
	return file, function, nil
}

func validateBashFunction(workingDir, handlerPath, handlerName string, env map[string]string) error {
	cmd := exec.Command("bash", "-c", `source "$1"; declare -F "$2" >/dev/null`, "kruntimes-function-validation", handlerPath, handlerName)
	cmd.Dir = workingDir
	cmd.Env = functionEnvironment(env)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("handler %q is not defined by %s: %w: %s", handlerName, filepath.Base(handlerPath), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func invokeBashFunction(ctx context.Context, workingDir, handlerFile, handlerName, input string, env map[string]string, outputLimit int) ([]byte, error) {
	// The fixed bash program receives the file, function, and JSON as positional
	// arguments. Neither the handler nor request data is interpolated as shell source.
	handlerPath, err := functionFilePath(workingDir, handlerFile)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("bash", "-c", `source "$1"; "$2" "$3"`, "kruntimes-function", handlerPath, handlerName, input)
	cmd.Dir = workingDir
	cmd.Env = functionEnvironment(env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout boundedBuffer
	stdout = newBoundedBuffer(outputLimit)
	var stderr boundedBuffer
	stderr = newBoundedBuffer(outputLimit)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	select {
	case err := <-waitCh:
		if err != nil {
			return nil, fmt.Errorf("%w: %s", err, stderr.String())
		}
	case <-ctx.Done():
		_ = terminateProcessGroupAndWait(cmd.Process.Pid, waitCh, processTerminationGrace)
		return nil, ctx.Err()
	}
	if stdout.truncated {
		return nil, errFunctionOutputLimit{}
	}
	return []byte(stdout.String()), nil
}

type errFunctionOutputLimit struct{}

func (errFunctionOutputLimit) Error() string {
	return "function response exceeds the configured output limit"
}

func errorsIsOutputLimit(err error) bool {
	_, ok := err.(errFunctionOutputLimit)
	return ok
}

func errorsIsContextError(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func functionInvokeContext(parent context.Context, timeoutMillis int64) (context.Context, context.CancelFunc) {
	timeout := defaultFunctionInvokeTimeout
	if timeoutMillis > 0 {
		timeout = time.Duration(timeoutMillis) * time.Millisecond
	}
	if timeout > maxFunctionInvokeTimeout {
		timeout = maxFunctionInvokeTimeout
	}
	return context.WithTimeout(parent, timeout)
}

func functionDrainContext(parent context.Context, timeoutMillis int64) (context.Context, context.CancelFunc) {
	timeout := defaultFunctionDrainTimeout
	if timeoutMillis > 0 {
		timeout = time.Duration(timeoutMillis) * time.Millisecond
	}
	if timeout > maxFunctionInvokeTimeout {
		timeout = maxFunctionInvokeTimeout
	}
	return context.WithTimeout(parent, timeout)
}

func waitForFunction(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return status.FromContextError(ctx.Err()).Err()
	}
}

func newRegistrationID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "reg_" + hex.EncodeToString(bytes), nil
}

func cloneFunctionRegistration(registration *pb.FunctionRegistration) *pb.FunctionRegistration {
	if registration == nil {
		return nil
	}
	return &pb.FunctionRegistration{RunUid: registration.RunUid, RegistrationId: registration.RegistrationId}
}

func cloneStringMap(values map[string]string) map[string]string {
	return maps.Clone(values)
}

func functionEnvironment(values map[string]string) []string {
	overrides := cloneStringMap(values)
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, value := range os.Environ() {
		key, _, found := strings.Cut(value, "=")
		if !found {
			continue
		}
		if _, overridden := overrides[key]; overridden {
			continue
		}
		environment = append(environment, value)
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		environment = append(environment, key+"="+overrides[key])
	}
	return environment
}
