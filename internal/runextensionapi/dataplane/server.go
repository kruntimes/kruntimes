// Package dataplane translates Run data-plane HTTP requests into Runtime
// Service gRPC calls. It owns the Session operation protocol, the Session file
// adapters, and Function invocation. Callers must authenticate and authorize a
// request before invoking these handlers; this package makes no authorization
// decision.
package dataplane

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	defaultRuntimeServicePort = 9093

	// DefaultMaxRequestBodyBytes is the maximum size of a data-plane JSON request.
	DefaultMaxRequestBodyBytes int64 = 1 << 20
	// DefaultMaxResponseBodyBytes is the maximum size of a data-plane JSON response.
	DefaultMaxResponseBodyBytes int64 = 1 << 20
)

var errRequestBodyTooLarge = errors.New("data-plane request body exceeds configured limit")

// SessionRuntimeDialer creates a SessionRuntime client for a Runtime Service.
type SessionRuntimeDialer interface {
	Dial(context.Context, string) (pb.SessionRuntimeClient, io.Closer, error)
}

// FunctionRuntimeDialer creates the runtimed FunctionRuntime proxy client.
type FunctionRuntimeDialer interface {
	DialFunction(context.Context, string) (pb.FunctionRuntimeClient, io.Closer, error)
}

// Server translates already-authorized Run data-plane requests into Runtime
// Service gRPC calls.
type Server struct {
	Dialer         SessionRuntimeDialer
	FunctionDialer FunctionRuntimeDialer
	RuntimePort    int
	// MaxRequestBodyBytes bounds data-plane JSON request bodies. Values less
	// than one use the default.
	MaxRequestBodyBytes int64
	// MaxResponseBodyBytes bounds data-plane generated JSON responses. Values
	// less than one use the default.
	MaxResponseBodyBytes int64
}

// StreamSession serves a Session Run after the caller has authenticated and
// authorized the request.
func (s *Server) StreamSession(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	if run == nil || run.Spec.Mode.Session == nil {
		s.writeError(w, http.StatusNotFound, "Session Run not found")
		return
	}
	if run.Status.Phase != v1alpha1.RunReady || run.Status.AssignedPodUID == "" {
		s.writeError(w, http.StatusConflict, "Session Run is not Ready")
		return
	}
	if s.Dialer == nil {
		s.writeError(w, http.StatusServiceUnavailable, "Session Runtime dialer is not configured")
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		s.writeError(w, http.StatusUpgradeRequired, "WebSocket upgrade is required")
		return
	}
	s.streamOperationWebSocket(w, r, run)
}

// GetSessionStatus returns readiness and bounded metadata for a Session Run.
func (s *Server) GetSessionStatus(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	client, closer, err := s.runtimeClient(r.Context(), run)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	defer closer.Close()
	response, err := client.GetSessionStatus(r.Context(), &pb.GetSessionStatusRequest{Identity: sessionIdentity(run)})
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, newSessionStatusResponse(response))
}

// ServeFunction invokes one Function Run after the caller has authenticated and
// authorized the request.
func (s *Server) ServeFunction(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}
	if run == nil || run.Spec.Mode.Function == nil {
		s.writeError(w, http.StatusNotFound, "function Run not found")
		return
	}
	if run.Status.Phase != v1alpha1.RunReady || run.Status.AssignedPodUID == "" {
		s.writeError(w, http.StatusConflict, "function Run is not Ready")
		return
	}
	if s.FunctionDialer == nil {
		s.writeError(w, http.StatusServiceUnavailable, "Function Runtime dialer is not configured")
		return
	}
	input, err := s.functionInput(r)
	if err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.Header.Get("X-Kruntime-Invocation-ID")
	if len(id) > 128 {
		s.writeError(w, http.StatusBadRequest, "invocation ID exceeds 128 bytes")
		return
	}
	client, closer, err := s.functionClient(r.Context(), run)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	defer closer.Close()
	response, err := client.InvokeFunction(r.Context(), &pb.InvokeFunctionRequest{Registration: &pb.FunctionRegistration{RunUid: string(run.UID)}, InvocationId: id, Input: input, ContentType: "application/json"})
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, functionInvokeResponse{InvocationID: response.GetInvocationId(), Output: response.GetOutput(), ContentType: response.GetContentType(), Outputs: response.GetOutputs()})
}

// ServeSessionFiles serves one bounded, paginated page of a Session Run's
// workspace.
func (s *Server) ServeSessionFiles(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	s.listSessionFiles(w, r, run)
}

// ServeSessionFile serves one bounded workspace-relative file from a Session
// Run.
func (s *Server) ServeSessionFile(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run, path string) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	if path == "" {
		s.endpointNotFound(w)
		return
	}
	s.readSessionFile(w, r, run, path)
}

// streamOperationWebSocket upgrades one authorized request to a persistent
// Session connection. It serializes send frames and accepts cancellation only
// for the active operation.
func (s *Server) streamOperationWebSocket(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	connection, err := sessionOperationWebSocketUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	connection.SetReadLimit(s.maxRequestBodyBytes())
	if err := s.touchSession(r.Context(), run); err != nil {
		s.writeWebSocketError(connection, err)
		return
	}
	done := make(chan struct{})
	defer close(done)
	frames := readSessionConnectionFrames(connection, done)
	for frame := range frames {
		if frame.err != nil {
			s.closeWebSocket(connection, websocket.ClosePolicyViolation, "valid Session frame is required")
			return
		}
		if frame.message.Type == sessionConnectionMessageHeartbeat {
			if err := s.touchSession(r.Context(), run); err != nil {
				s.writeWebSocketError(connection, err)
				return
			}
			continue
		}
		if frame.message.Type == sessionConnectionMessageCancel {
			s.writeWebSocketError(connection, errors.New("no active Session operation to cancel"))
			continue
		}
		if frame.message.Type != sessionConnectionMessageSend || !s.streamSessionConnectionOperation(r.Context(), connection, frames, run, frame.message) {
			return
		}
	}
}

func (s *Server) streamSessionConnectionOperation(parent context.Context, connection *websocket.Conn, frames <-chan sessionConnectionFrame, run *v1alpha1.Run, message sessionConnectionMessage) bool {
	operation, err := s.websocketOperation(message.Operation)
	if err != nil {
		s.closeWebSocket(connection, websocket.ClosePolicyViolation, err.Error())
		return false
	}
	operationID, err := sessionOperationID(message.IdempotencyKey)
	if err != nil {
		s.closeWebSocket(connection, websocket.ClosePolicyViolation, err.Error())
		return false
	}
	ctx, cancel := context.WithCancel(parent)
	client, closer, err := s.runtimeClient(ctx, run)
	if err != nil {
		cancel()
		s.writeWebSocketError(connection, err)
		return true
	}
	// Stop the stream receiver before closing its ClientConn. Closing a gRPC
	// connection with a still-active Recv can wait for transport teardown and
	// delay the next operation on this persistent WebSocket.
	defer func() {
		cancel()
		// A gRPC ClientConn may wait for transport teardown in Close. The next
		// operation on this persistent WebSocket must not wait behind that work.
		go func() { _ = closer.Close() }()
	}()
	operation.Identity, operation.IdempotencyKey = sessionIdentity(run), operationID
	stream, err := client.StreamSessionOperation(ctx, operation)
	if err != nil {
		s.writeWebSocketError(connection, err)
		return true
	}
	events := receiveSessionOperationEvents(ctx, stream)
	for {
		select {
		case frame, ok := <-frames:
			if !ok || frame.err != nil {
				s.closeWebSocket(connection, websocket.ClosePolicyViolation, "only cancellation of the active operation is allowed")
				return false
			}
			if frame.message.Type == sessionConnectionMessageHeartbeat {
				if err := s.touchSession(ctx, run); err != nil {
					s.writeWebSocketError(connection, err)
					return true
				}
				continue
			}
			if frame.message.Type != sessionConnectionMessageCancel || frame.message.OperationID != operationID {
				s.closeWebSocket(connection, websocket.ClosePolicyViolation, "only cancellation of the active operation is allowed")
				return false
			}
			cancel()
		case result, ok := <-events:
			if !ok || result.err == io.EOF {
				s.writeWebSocketError(connection, errors.New("Runtime Server stream ended without a terminal event"))
				return true
			}
			if result.err != nil {
				s.writeWebSocketError(connection, result.err)
				return true
			}
			response, err := newSessionOperationEventResponse(result.event)
			if err != nil {
				s.writeWebSocketError(connection, err)
				return true
			}
			encoded, err := json.Marshal(response)
			if err != nil || int64(len(encoded)) > s.maxResponseBodyBytes() {
				s.writeWebSocketError(connection, errors.New("gateway stream event exceeds configured limit"))
				return true
			}
			if err := connection.WriteMessage(websocket.TextMessage, encoded); err != nil {
				return false
			}
			if response.Completed != nil || response.Failed != nil {
				return true
			}
		}
	}
}

func (s *Server) touchSession(ctx context.Context, run *v1alpha1.Run) error {
	client, closer, err := s.runtimeClient(ctx, run)
	if err != nil {
		return err
	}
	defer closer.Close()
	_, err = client.RenewSessionLease(ctx, &pb.RenewSessionLeaseRequest{Identity: sessionIdentity(run)})
	return err
}

var sessionOperationWebSocketUpgrader = websocket.Upgrader{
	CheckOrigin: sameOriginWebSocketRequest,
}

func sameOriginWebSocketRequest(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients normally omit Origin and authenticate with a bearer
		// token or client certificate during the HTTP upgrade request.
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host == r.Host
}

func (s *Server) websocketOperation(payload []byte) (*pb.SessionOperationRequest, error) {
	if int64(len(payload)) > s.maxRequestBodyBytes() {
		return nil, errRequestBodyTooLarge
	}
	var request executeOperationRequest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("request must contain one JSON value")
	}
	return request.protobuf()
}

func (s *Server) writeWebSocketError(connection *websocket.Conn, err error) {
	_ = connection.WriteJSON(struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}{Type: "error", Error: status.Convert(err).Message()})
}

func (s *Server) closeWebSocket(connection *websocket.Conn, code int, message string) {
	_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, message), time.Now().Add(time.Second))
}

func sessionOperationID(value string) (string, error) {
	if value == "" {
		bytes := make([]byte, 16)
		if _, err := rand.Read(bytes); err != nil {
			return "", fmt.Errorf("generate session operation ID: %w", err)
		}
		return hex.EncodeToString(bytes), nil
	}
	if len(value) > 128 {
		return "", errors.New("idempotency key exceeds 128 bytes")
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') && (character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return "", errors.New("idempotency key contains unsupported characters")
		}
	}
	return value, nil
}

// listSessionFiles reads one bounded page of a Session Run's direct workspace
// children from the owning runtimed.
func (s *Server) listSessionFiles(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	request, err := sessionFileListRequest(r.URL.Query())
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	client, closer, err := s.runtimeClient(r.Context(), run)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	defer closer.Close()
	request.Identity = sessionIdentity(run)
	response, err := client.ListSessionFiles(r.Context(), request)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	entries := make([]sessionFileInfoResponse, 0, len(response.GetEntries()))
	for _, entry := range response.GetEntries() {
		entries = append(entries, sessionFileInfoResponse{Path: entry.GetPath(), Directory: entry.GetDirectory(), SizeBytes: entry.GetSizeBytes()})
	}
	s.writeJSON(w, http.StatusOK, struct {
		Entries       []sessionFileInfoResponse `json:"entries"`
		NextPageToken string                    `json:"nextPageToken"`
	}{Entries: entries, NextPageToken: response.GetNextPageToken()})
}

func sessionFileListRequest(query url.Values) (*pb.ListSessionFilesRequest, error) {
	request := &pb.ListSessionFilesRequest{Path: query.Get("path"), PageToken: query.Get("pageToken")}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.ParseInt(value, 10, 32)
		if err != nil || limit <= 0 {
			return nil, errors.New("limit must be a positive integer")
		}
		request.Limit = int32(limit)
	}
	return request, nil
}

// readSessionFile reads one bounded workspace-relative file from the owning
// runtimed.
func (s *Server) readSessionFile(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run, path string) {
	maxBytes := int64(1 << 20)
	if value := r.URL.Query().Get("maxBytes"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			s.writeError(w, http.StatusBadRequest, "maxBytes must be a positive integer")
			return
		}
		maxBytes = parsed
	}
	client, closer, err := s.runtimeClient(r.Context(), run)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	defer closer.Close()
	response, err := client.ReadSessionFile(r.Context(), &pb.ReadSessionFileRequest{Identity: sessionIdentity(run), Path: path, MaxBytes: maxBytes})
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, struct {
		Contents  []byte `json:"contents"`
		Truncated bool   `json:"truncated"`
	}{Contents: response.GetContents(), Truncated: response.GetTruncated()})
}

func (s *Server) runtimeClient(ctx context.Context, run *v1alpha1.Run) (pb.SessionRuntimeClient, io.Closer, error) {
	if s.Dialer == nil {
		return nil, nil, status.Error(codes.Unavailable, "Session Runtime dialer is not configured")
	}
	port := s.RuntimePort
	if port == 0 {
		port = defaultRuntimeServicePort
	}
	address := fmt.Sprintf("runtime-%s.%s.svc:%d", run.Spec.Runtime, run.Namespace, port)
	client, closer, err := s.Dialer.Dial(ctx, address)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "dial Runtime Service: %v", err)
	}
	return client, closer, nil
}

func (s *Server) functionClient(ctx context.Context, run *v1alpha1.Run) (pb.FunctionRuntimeClient, io.Closer, error) {
	port := s.RuntimePort
	if port == 0 {
		port = defaultRuntimeServicePort
	}
	client, closer, err := s.FunctionDialer.DialFunction(ctx, fmt.Sprintf("runtime-%s.%s.svc:%d", run.Spec.Runtime, run.Namespace, port))
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "dial Runtime Service: %v", err)
	}
	return client, closer, nil
}

func (s *Server) functionInput(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	body := &io.LimitedReader{R: r.Body, N: s.maxRequestBodyBytes() + 1}
	input, err := io.ReadAll(body)
	if err != nil {
		return nil, fmt.Errorf("read request: %w", err)
	}
	if body.N == 0 {
		return nil, errRequestBodyTooLarge
	}
	if !json.Valid(input) {
		return nil, errors.New("function input must be valid JSON")
	}
	return input, nil
}

func sessionIdentity(run *v1alpha1.Run) *pb.SessionIdentity {
	return &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: run.Status.AssignedPodUID}
}

func (s *Server) maxRequestBodyBytes() int64 {
	if s.MaxRequestBodyBytes > 0 {
		return s.MaxRequestBodyBytes
	}
	return DefaultMaxRequestBodyBytes
}

func (s *Server) maxResponseBodyBytes() int64 {
	if s.MaxResponseBodyBytes > 0 {
		return s.MaxResponseBodyBytes
	}
	return DefaultMaxResponseBodyBytes
}

func (s *Server) methodNotAllowed(w http.ResponseWriter) {
	s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func (s *Server) endpointNotFound(w http.ResponseWriter) {
	s.writeError(w, http.StatusNotFound, "endpoint not found")
}

func (s *Server) writeGatewayError(w http.ResponseWriter, err error) {
	code := status.Code(err)
	httpStatus := http.StatusInternalServerError
	switch code {
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
	case codes.Unauthenticated:
		httpStatus = http.StatusUnauthorized
	case codes.PermissionDenied:
		httpStatus = http.StatusForbidden
	case codes.NotFound:
		httpStatus = http.StatusNotFound
	case codes.FailedPrecondition:
		httpStatus = http.StatusConflict
	case codes.ResourceExhausted:
		httpStatus = http.StatusTooManyRequests
	case codes.DeadlineExceeded:
		httpStatus = http.StatusGatewayTimeout
	case codes.Unavailable:
		httpStatus = http.StatusServiceUnavailable
	}
	s.writeError(w, httpStatus, status.Convert(err).Message())
}

func (s *Server) writeError(w http.ResponseWriter, httpStatus int, message string) {
	s.writeJSON(w, httpStatus, struct {
		Error string `json:"error"`
	}{Error: message})
}

func (s *Server) writeJSON(w http.ResponseWriter, httpStatus int, value any) {
	var response bytes.Buffer
	if err := json.NewEncoder(&response).Encode(value); err != nil {
		s.writeUnboundedError(w, http.StatusInternalServerError, "encode data-plane response")
		return
	}
	if int64(response.Len()) > s.maxResponseBodyBytes() {
		s.writeUnboundedError(w, http.StatusRequestEntityTooLarge, "data-plane response exceeds configured limit")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(response.Bytes())
}

func (s *Server) writeUnboundedError(w http.ResponseWriter, httpStatus int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_, _ = w.Write([]byte(fmt.Sprintf("{\"error\":%q}\n", message)))
}

type executeOperationRequest struct {
	Command         *sessionCommandRequest         `json:"command,omitempty"`
	WriteFile       *sessionFileWriteRequest       `json:"writeFile,omitempty"`
	CreateDirectory *sessionDirectoryCreateRequest `json:"createDirectory,omitempty"`
	DeleteFile      *sessionFileDeleteRequest      `json:"deleteFile,omitempty"`
	RenameFile      *sessionFileRenameRequest      `json:"renameFile,omitempty"`
}

func (r executeOperationRequest) protobuf() (*pb.SessionOperationRequest, error) {
	count := 0
	if r.Command != nil {
		count++
	}
	if r.WriteFile != nil {
		count++
	}
	if r.CreateDirectory != nil {
		count++
	}
	if r.DeleteFile != nil {
		count++
	}
	if r.RenameFile != nil {
		count++
	}
	if count != 1 {
		return nil, errors.New("exactly one session operation is required")
	}
	if r.Command != nil {
		return &pb.SessionOperationRequest{Operation: &pb.SessionOperationRequest_Command{Command: &pb.SessionCommand{Argv: r.Command.Argv, Shell: r.Command.Shell, WorkingDirectory: r.Command.WorkingDirectory, Env: r.Command.Env, Stdin: r.Command.Stdin, TimeoutMillis: r.Command.TimeoutMillis}}}, nil
	}
	if r.WriteFile != nil {
		return &pb.SessionOperationRequest{Operation: &pb.SessionOperationRequest_WriteFile{WriteFile: &pb.SessionFileWrite{Path: r.WriteFile.Path, Contents: r.WriteFile.Contents, CreateParents: r.WriteFile.CreateParents}}}, nil
	}
	if r.CreateDirectory != nil {
		return &pb.SessionOperationRequest{Operation: &pb.SessionOperationRequest_CreateDirectory{CreateDirectory: &pb.SessionDirectoryCreate{Path: r.CreateDirectory.Path}}}, nil
	}
	if r.DeleteFile != nil {
		return &pb.SessionOperationRequest{Operation: &pb.SessionOperationRequest_DeleteFile{DeleteFile: &pb.SessionFileDelete{Path: r.DeleteFile.Path, Recursive: r.DeleteFile.Recursive}}}, nil
	}
	return &pb.SessionOperationRequest{Operation: &pb.SessionOperationRequest_RenameFile{RenameFile: &pb.SessionFileRename{SourcePath: r.RenameFile.SourcePath, DestinationPath: r.RenameFile.DestinationPath, Overwrite: r.RenameFile.Overwrite}}}, nil
}

type sessionCommandRequest struct {
	Argv             []string          `json:"argv,omitempty"`
	Shell            string            `json:"shell,omitempty"`
	WorkingDirectory string            `json:"workingDirectory,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Stdin            []byte            `json:"stdin,omitempty"`
	TimeoutMillis    int64             `json:"timeoutMillis,omitempty"`
}
type sessionFileWriteRequest struct {
	Path          string `json:"path"`
	Contents      []byte `json:"contents"`
	CreateParents bool   `json:"createParents,omitempty"`
}
type sessionDirectoryCreateRequest struct {
	Path string `json:"path"`
}
type sessionFileDeleteRequest struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}
type sessionFileRenameRequest struct {
	SourcePath      string `json:"sourcePath"`
	DestinationPath string `json:"destinationPath"`
	Overwrite       bool   `json:"overwrite,omitempty"`
}

type sessionStatusResponse struct {
	State                string `json:"state"`
	LastActivityUnixNano int64  `json:"lastActivityUnixNano,omitempty"`
	FatalError           string `json:"fatalError,omitempty"`
}

func newSessionStatusResponse(value *pb.SessionStatus) sessionStatusResponse {
	return sessionStatusResponse{State: value.GetState().String(), LastActivityUnixNano: value.GetLastActivityUnixNano(), FatalError: value.GetFatalError()}
}

type executeOperationResponse struct {
	Command *sessionCommandResultResponse `json:"command,omitempty"`
}

func newExecuteOperationResponse(value *pb.SessionOperationResult) executeOperationResponse {
	if command := value.GetCommand(); command != nil {
		return executeOperationResponse{Command: &sessionCommandResultResponse{ExitCode: command.GetExitCode(), Stdout: command.GetStdout(), Stderr: command.GetStderr(), TimedOut: command.GetTimedOut()}}
	}
	return executeOperationResponse{}
}

type sessionOperationEventResponse struct {
	Sequence  int64                             `json:"sequence"`
	Type      string                            `json:"type"`
	Accepted  *sessionOperationAcceptedResponse `json:"accepted,omitempty"`
	Output    *sessionOperationOutputResponse   `json:"output,omitempty"`
	Progress  *sessionOperationProgressResponse `json:"progress,omitempty"`
	Completed *executeOperationResponse         `json:"completed,omitempty"`
	Failed    *sessionOperationFailureResponse  `json:"failed,omitempty"`
}

type sessionOperationAcceptedResponse struct {
	OperationID string `json:"operationID"`
}

type sessionOperationOutputResponse struct {
	Stream string `json:"stream"`
	Data   []byte `json:"data,omitempty"`
}

type sessionOperationProgressResponse struct {
	Kind        string `json:"kind"`
	Message     string `json:"message,omitempty"`
	ToolCallID  string `json:"toolCallID,omitempty"`
	ToolName    string `json:"toolName,omitempty"`
	Data        []byte `json:"data,omitempty"`
	ContentType string `json:"contentType,omitempty"`
}

type sessionOperationFailureResponse struct {
	Code    int32  `json:"code"`
	Message string `json:"message"`
}

func newSessionOperationEventResponse(value *pb.SessionOperationEvent) (sessionOperationEventResponse, error) {
	if value == nil || value.GetEvent() == nil || value.GetSequence() <= 0 {
		return sessionOperationEventResponse{}, errors.New("Runtime Server emitted an invalid Session event")
	}
	response := sessionOperationEventResponse{Sequence: value.GetSequence()}
	switch {
	case value.GetAccepted() != nil:
		response.Type = "accepted"
		response.Accepted = &sessionOperationAcceptedResponse{OperationID: value.GetAccepted().GetOperationId()}
	case value.GetOutput() != nil:
		output := value.GetOutput()
		response.Type = "output"
		response.Output = &sessionOperationOutputResponse{Stream: sessionOperationOutputStreamName(output.GetStream()), Data: output.GetData()}
	case value.GetProgress() != nil:
		progress := value.GetProgress()
		response.Type = "progress"
		response.Progress = &sessionOperationProgressResponse{
			Kind:        sessionOperationProgressKindName(progress.GetKind()),
			Message:     progress.GetMessage(),
			ToolCallID:  progress.GetToolCallId(),
			ToolName:    progress.GetToolName(),
			Data:        progress.GetData(),
			ContentType: progress.GetContentType(),
		}
	case value.GetCompleted() != nil:
		response.Type = "completed"
		completed := newExecuteOperationResponse(value.GetCompleted())
		response.Completed = &completed
	case value.GetFailed() != nil:
		failed := value.GetFailed()
		response.Type = "failed"
		response.Failed = &sessionOperationFailureResponse{Code: failed.GetCode(), Message: failed.GetMessage()}
	default:
		return sessionOperationEventResponse{}, errors.New("Runtime Server emitted an unknown Session event")
	}
	return response, nil
}

func sessionOperationOutputStreamName(stream pb.SessionOperationOutputStream) string {
	switch stream {
	case pb.SessionOperationOutputStream_SESSION_OPERATION_OUTPUT_STREAM_STDOUT:
		return "stdout"
	case pb.SessionOperationOutputStream_SESSION_OPERATION_OUTPUT_STREAM_STDERR:
		return "stderr"
	default:
		return "unspecified"
	}
}

func sessionOperationProgressKindName(kind pb.SessionOperationProgressKind) string {
	switch kind {
	case pb.SessionOperationProgressKind_SESSION_OPERATION_PROGRESS_KIND_STATUS:
		return "status"
	case pb.SessionOperationProgressKind_SESSION_OPERATION_PROGRESS_KIND_TEXT_DELTA:
		return "text_delta"
	case pb.SessionOperationProgressKind_SESSION_OPERATION_PROGRESS_KIND_TOOL_CALL_STARTED:
		return "tool_call_started"
	case pb.SessionOperationProgressKind_SESSION_OPERATION_PROGRESS_KIND_TOOL_CALL_FINISHED:
		return "tool_call_finished"
	default:
		return "unspecified"
	}
}

type sessionCommandResultResponse struct {
	ExitCode int32  `json:"exitCode"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	TimedOut bool   `json:"timedOut,omitempty"`
}
type functionInvokeResponse struct {
	InvocationID string            `json:"invocationId"`
	Output       []byte            `json:"output,omitempty"`
	ContentType  string            `json:"contentType,omitempty"`
	Outputs      map[string]string `json:"outputs,omitempty"`
}
type sessionFileInfoResponse struct {
	Path      string `json:"path"`
	Directory bool   `json:"directory"`
	SizeBytes int64  `json:"sizeBytes"`
}
