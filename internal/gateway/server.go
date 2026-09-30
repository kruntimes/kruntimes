// Package gateway implements the Runtime access HTTP handler embedded by the
// Console. It owns protocol translation only; the Console owns the public
// listener, TLS, and lifecycle.
package gateway

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
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	defaultRuntimeServicePort = 9093

	// DefaultMaxRequestBodyBytes is the maximum size of a gateway JSON request.
	DefaultMaxRequestBodyBytes int64 = 1 << 20
	// DefaultMaxResponseBodyBytes is the maximum size of a gateway JSON response.
	DefaultMaxResponseBodyBytes int64 = 1 << 20
	// DefaultMaxHeaderBytes is the maximum size of a gateway HTTP request header.
	DefaultMaxHeaderBytes = 1 << 20
	// DefaultMaxConcurrentRequests is the per-Console-Pod HTTP request limit.
	DefaultMaxConcurrentRequests = 128
)

var errRequestBodyTooLarge = errors.New("gateway request body exceeds configured limit")

// Authorizer verifies that a request principal may access a Session Run.
type Authorizer interface {
	Authorize(context.Context, *http.Request, *v1alpha1.Run) error
}

// SessionRuntimeDialer creates a SessionRuntime client for a Runtime Service.
type SessionRuntimeDialer interface {
	Dial(context.Context, string) (pb.SessionRuntimeClient, io.Closer, error)
}

// FunctionRuntimeDialer creates the runtimed FunctionRuntime proxy client.
type FunctionRuntimeDialer interface {
	DialFunction(context.Context, string) (pb.FunctionRuntimeClient, io.Closer, error)
}

// Server exposes a versioned HTTP API for already-ready Session Runs.
// Run reads are served through the configured controller-runtime cache.
type Server struct {
	Runs           client.Reader
	Authorizer     Authorizer
	Dialer         SessionRuntimeDialer
	FunctionDialer FunctionRuntimeDialer
	RuntimePort    int
	// MaxConcurrentRequests bounds requests handled by one Console Pod. Values
	// less than one use the default. Health checks do not consume this limit.
	MaxConcurrentRequests int
	// MaxRequestBodyBytes bounds access API JSON request bodies. Values less than
	// one use the default.
	MaxRequestBodyBytes int64
	// MaxResponseBodyBytes bounds access API generated JSON responses. Values less
	// than one use the default.
	MaxResponseBodyBytes int64
	// MaxHeaderBytes bounds HTTP request headers before routing. Values less than
	// one use the default.
	MaxHeaderBytes int
	requestLimiter gatewayRequestLimiter
	routesOnce     sync.Once
	routes         http.Handler
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.routesOnce.Do(s.registerRoutes)
	s.routes.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/functions/{invocation}", s.withRequestLimit(s.handleFunctionInvoke))

	withSessionRun := func(handler sessionRouteHandler) http.HandlerFunc {
		return s.withRequestLimit(s.withSessionRun(handler))
	}
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}", withSessionRun(s.handleSessionStatus))
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:execute", withSessionRun(s.handleSessionExecute))
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:stream", withSessionRun(s.handleSessionStream))
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:ws", withSessionRun(s.handleSessionWebSocket))
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations/{operation}", withSessionRun(s.handleSessionResume))
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/files", withSessionRun(s.handleSessionListFiles))
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/files/{path...}", withSessionRun(s.handleSessionReadFile))
	mux.HandleFunc("/", s.endpointNotFound)

	s.routes = mux
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) endpointNotFound(w http.ResponseWriter, _ *http.Request) {
	s.writeError(w, http.StatusNotFound, "endpoint not found")
}

func (s *Server) withRequestLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.requestLimiter.tryAcquire(s.maxConcurrentRequests()) {
			s.writeError(w, http.StatusTooManyRequests, "gateway request concurrency limit reached")
			return
		}
		defer s.requestLimiter.release()
		next(w, r)
	}
}

type sessionRouteHandler func(http.ResponseWriter, *http.Request, *v1alpha1.Run)

func (s *Server) withSessionRun(next sessionRouteHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run, ok := s.authorizedSessionRun(w, r)
		if !ok {
			return
		}
		next(w, r, run)
	}
}

func (s *Server) authorizedSessionRun(w http.ResponseWriter, r *http.Request) (*v1alpha1.Run, bool) {
	namespace, runtimeName, runUID, ok := routeRunIdentity(r)
	if !ok {
		s.endpointNotFound(w, r)
		return nil, false
	}
	run, err := s.sessionRun(r.Context(), namespace, runtimeName, runUID)
	if err != nil {
		s.writeGatewayError(w, err)
		return nil, false
	}
	if s.Authorizer == nil {
		s.writeError(w, http.StatusServiceUnavailable, "gateway authorization is not configured")
		return nil, false
	}
	if err := s.Authorizer.Authorize(r.Context(), r, run); err != nil {
		s.writeGatewayError(w, err)
		return nil, false
	}
	return run, true
}

func routeRunIdentity(r *http.Request) (namespace, runtimeName, runUID string, ok bool) {
	namespace = r.PathValue("namespace")
	runtimeName = r.PathValue("runtime")
	runUID = r.PathValue("runUID")
	return namespace, runtimeName, runUID, namespace != "" && runtimeName != "" && runUID != ""
}

func (s *Server) handleFunctionInvoke(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	runtimeName := r.PathValue("runtime")
	runUID, found := strings.CutSuffix(r.PathValue("invocation"), ":invoke")
	if namespace == "" || runtimeName == "" || !found || runUID == "" {
		s.endpointNotFound(w, r)
		return
	}
	s.serveFunctionInvoke(w, r, namespace, runtimeName, runUID)
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	s.getSessionStatus(w, r, run)
}

func (s *Server) handleSessionExecute(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}
	s.executeOperation(w, r, run)
}

func (s *Server) handleSessionStream(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}
	s.streamOperation(w, r, run)
}

func (s *Server) handleSessionWebSocket(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	if !websocket.IsWebSocketUpgrade(r) {
		s.writeError(w, http.StatusUpgradeRequired, "WebSocket upgrade is required")
		return
	}
	s.streamOperationWebSocket(w, r, run)
}

func (s *Server) handleSessionResume(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	operationID, found := strings.CutSuffix(r.PathValue("operation"), ":stream")
	if !found || operationID == "" {
		s.endpointNotFound(w, r)
		return
	}
	s.resumeOperation(w, r, run, operationID)
}

func (s *Server) handleSessionListFiles(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	s.listFiles(w, r, run)
}

func (s *Server) handleSessionReadFile(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	path := r.PathValue("path")
	if path == "" {
		s.endpointNotFound(w, r)
		return
	}
	s.readFile(w, r, run, path)
}

func (s *Server) serveFunctionInvoke(w http.ResponseWriter, r *http.Request, namespace, runtimeName, runUID string) {
	if r.Method != http.MethodPost {
		s.methodNotAllowed(w)
		return
	}
	run, err := s.functionRun(r.Context(), namespace, runtimeName, runUID)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	if s.Authorizer == nil {
		s.writeError(w, http.StatusServiceUnavailable, "gateway authorization is not configured")
		return
	}
	if err := s.Authorizer.Authorize(r.Context(), r, run); err != nil {
		s.writeGatewayError(w, err)
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

func (s *Server) functionRun(ctx context.Context, namespace, runtimeName, runUID string) (*v1alpha1.Run, error) {
	if s.Runs == nil || s.FunctionDialer == nil {
		return nil, status.Error(codes.FailedPrecondition, "gateway is not configured")
	}
	var runs v1alpha1.RunList
	if err := s.Runs.List(ctx, &runs, client.InNamespace(namespace)); err != nil {
		return nil, status.Errorf(codes.Internal, "list Runtime Runs: %v", err)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if string(run.UID) == runUID {
			if run.Spec.Mode.Function == nil || run.Spec.Runtime != runtimeName {
				return nil, status.Error(codes.NotFound, "function Run not found")
			}
			if run.Status.Phase != v1alpha1.RunReady || run.Status.AssignedPodUID == "" {
				return nil, status.Errorf(codes.FailedPrecondition, "function Run is %s, not Ready", run.Status.Phase)
			}
			return run, nil
		}
	}
	return nil, status.Error(codes.NotFound, "function Run not found")
}

func (s *Server) functionClient(ctx context.Context, run *v1alpha1.Run) (pb.FunctionRuntimeClient, io.Closer, error) {
	port := s.RuntimePort
	if port == 0 {
		port = defaultRuntimeServicePort
	}
	client, closer, err := s.FunctionDialer.DialFunction(ctx, fmt.Sprintf("runtime-%s.%s:%d", run.Spec.Runtime, run.Namespace, port))
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "dial Runtime Service: %v", err)
	}
	return client, closer, nil
}

func (s *Server) maxConcurrentRequests() int {
	if s.MaxConcurrentRequests > 0 {
		return s.MaxConcurrentRequests
	}
	return DefaultMaxConcurrentRequests
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

func (s *Server) maxHeaderBytes() int {
	if s.MaxHeaderBytes > 0 {
		return s.MaxHeaderBytes
	}
	return DefaultMaxHeaderBytes
}

type gatewayRequestLimiter struct {
	once   sync.Once
	tokens chan struct{}
}

func (l *gatewayRequestLimiter) tryAcquire(limit int) bool {
	l.once.Do(func() {
		l.tokens = make(chan struct{}, limit)
	})
	select {
	case l.tokens <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *gatewayRequestLimiter) release() {
	<-l.tokens
}

func (s *Server) getSessionStatus(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
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

func (s *Server) executeOperation(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	var request executeOperationRequest
	if err := s.decodeJSON(r, &request); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	operation, err := request.protobuf()
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
	operation.Identity = sessionIdentity(run)
	response, err := client.ExecuteSessionOperation(r.Context(), operation)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, newExecuteOperationResponse(response))
}

// streamOperation writes one complete JSON object per line and flushes it as
// soon as the owner runtimed emits an event. net/http chooses HTTP/1.1 chunked
// transfer encoding automatically because this handler never sets a length.
func (s *Server) streamOperation(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	var request executeOperationRequest
	if err := s.decodeJSON(r, &request); err != nil {
		if errors.Is(err, errRequestBodyTooLarge) {
			s.writeError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	operation, err := request.protobuf()
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	operationID, err := sessionOperationID(r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	afterSequence, err := sessionOperationCursor(r)
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
	operation.Identity = sessionIdentity(run)
	operation.IdempotencyKey = operationID
	operation.ResumeAfterSequence = afterSequence
	stream, err := client.StreamSessionOperation(r.Context(), operation)
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	s.writeSessionOperationStream(w, stream)
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
	connection.SetReadDeadline(time.Now().Add(15 * time.Second))
	connection.SetReadDeadline(time.Time{})
	done := make(chan struct{})
	defer close(done)
	frames := readSessionConnectionFrames(connection, done)
	for frame := range frames {
		if frame.err != nil {
			s.closeWebSocket(connection, websocket.ClosePolicyViolation, "valid send frame is required")
			return
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
	defer cancel()
	client, closer, err := s.runtimeClient(ctx, run)
	if err != nil {
		s.writeWebSocketError(connection, err)
		return false
	}
	defer closer.Close()
	operation.Identity, operation.IdempotencyKey = sessionIdentity(run), operationID
	stream, err := client.StreamSessionOperation(ctx, operation)
	if err != nil {
		s.writeWebSocketError(connection, err)
		return false
	}
	events := receiveSessionOperationEvents(ctx, stream)
	for {
		select {
		case frame, ok := <-frames:
			if !ok || frame.err != nil || frame.message.Type != sessionConnectionMessageCancel || frame.message.OperationID != operationID {
				s.closeWebSocket(connection, websocket.ClosePolicyViolation, "only cancellation of the active operation is allowed")
				return false
			}
			cancel()
		case result, ok := <-events:
			if !ok || result.err == io.EOF {
				s.writeWebSocketError(connection, errors.New("Runtime Server stream ended without a terminal event"))
				return false
			}
			if result.err != nil {
				s.writeWebSocketError(connection, result.err)
				return false
			}
			response, err := newSessionOperationEventResponse(result.event)
			if err != nil {
				s.writeWebSocketError(connection, err)
				return false
			}
			encoded, err := json.Marshal(response)
			if err != nil || int64(len(encoded)) > s.maxResponseBodyBytes() {
				s.writeWebSocketError(connection, errors.New("gateway stream event exceeds configured limit"))
				return false
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

func (s *Server) websocketOperation(payload []byte) (*pb.ExecuteSessionOperationRequest, error) {
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
	s.closeWebSocket(connection, websocket.CloseInternalServerErr, "operation stream failed")
}

func (s *Server) closeWebSocket(connection *websocket.Conn, code int, message string) {
	_ = connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, message), time.Now().Add(time.Second))
}

func (s *Server) resumeOperation(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run, operationID string) {
	decoded, err := url.PathUnescape(operationID)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, "operation ID is invalid")
		return
	}
	operationID, err = sessionOperationID(decoded)
	if err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	afterSequence, err := sessionOperationCursor(r)
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
	stream, err := client.StreamSessionOperation(r.Context(), &pb.ExecuteSessionOperationRequest{
		Identity:            sessionIdentity(run),
		IdempotencyKey:      operationID,
		ResumeAfterSequence: afterSequence,
	})
	if err != nil {
		s.writeGatewayError(w, err)
		return
	}
	s.writeSessionOperationStream(w, stream)
}

func (s *Server) writeSessionOperationStream(w http.ResponseWriter, stream pb.SessionRuntime_StreamSessionOperationClient) {

	// Receive the first event before committing HTTP headers. Queue admission and
	// authorization failures therefore retain the ordinary gateway HTTP status.
	event, err := stream.Recv()
	if err != nil {
		if err != io.EOF {
			s.writeGatewayError(w, err)
			return
		}
		s.writeError(w, http.StatusBadGateway, "Runtime Server stream ended without an event")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.writeError(w, http.StatusInternalServerError, "gateway response streaming is unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	for {
		if err := s.writeSessionOperationEvent(w, event); err != nil {
			return
		}
		flusher.Flush()
		event, err = stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
	}
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

func sessionOperationCursor(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("after")
	if value == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseInt(value, 10, 64)
	if err != nil || cursor < 0 {
		return 0, errors.New("session operation cursor must be a non-negative integer")
	}
	return cursor, nil
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
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

func (s *Server) readFile(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run, path string) {
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

func (s *Server) sessionRun(ctx context.Context, namespace, runtimeName, runUID string) (*v1alpha1.Run, error) {
	if s.Runs == nil || s.Dialer == nil {
		return nil, status.Error(codes.FailedPrecondition, "gateway is not configured")
	}
	var runs v1alpha1.RunList
	if err := s.Runs.List(ctx, &runs, client.InNamespace(namespace)); err != nil {
		return nil, status.Errorf(codes.Internal, "list Runtime Runs: %v", err)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if string(run.UID) != runUID {
			continue
		}
		if run.Spec.Mode.Session == nil || run.Spec.Runtime != runtimeName {
			return nil, status.Error(codes.NotFound, "Session Run not found")
		}
		if run.Status.Phase != v1alpha1.RunReady || run.Status.AssignedPodUID == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "Session Run is %s, not Ready", run.Status.Phase)
		}
		return run, nil
	}
	return nil, status.Error(codes.NotFound, "Session Run not found")
}

func (s *Server) runtimeClient(ctx context.Context, run *v1alpha1.Run) (pb.SessionRuntimeClient, io.Closer, error) {
	port := s.RuntimePort
	if port == 0 {
		port = defaultRuntimeServicePort
	}
	address := fmt.Sprintf("runtime-%s.%s:%d", run.Spec.Runtime, run.Namespace, port)
	client, closer, err := s.Dialer.Dial(ctx, address)
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "dial Runtime Service: %v", err)
	}
	return client, closer, nil
}

func sessionIdentity(run *v1alpha1.Run) *pb.SessionIdentity {
	return &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: run.Status.AssignedPodUID}
}

func (s *Server) decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	body := &io.LimitedReader{R: r.Body, N: s.maxRequestBodyBytes() + 1}
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if body.N == 0 {
			return errRequestBodyTooLarge
		}
		return fmt.Errorf("decode request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if body.N == 0 {
			return errRequestBodyTooLarge
		}
		return errors.New("request must contain one JSON value")
	}
	if _, err := io.Copy(io.Discard, body); err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	if body.N == 0 {
		return errRequestBodyTooLarge
	}
	return nil
}

func (s *Server) methodNotAllowed(w http.ResponseWriter) {
	s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
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
		s.writeUnboundedError(w, http.StatusInternalServerError, "encode gateway response")
		return
	}
	if int64(response.Len()) > s.maxResponseBodyBytes() {
		s.writeUnboundedError(w, http.StatusRequestEntityTooLarge, "gateway response exceeds configured limit")
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

func (r executeOperationRequest) protobuf() (*pb.ExecuteSessionOperationRequest, error) {
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
		return &pb.ExecuteSessionOperationRequest{Operation: &pb.ExecuteSessionOperationRequest_Command{Command: &pb.SessionCommand{Argv: r.Command.Argv, Shell: r.Command.Shell, WorkingDirectory: r.Command.WorkingDirectory, Env: r.Command.Env, Stdin: r.Command.Stdin, TimeoutMillis: r.Command.TimeoutMillis}}}, nil
	}
	if r.WriteFile != nil {
		return &pb.ExecuteSessionOperationRequest{Operation: &pb.ExecuteSessionOperationRequest_WriteFile{WriteFile: &pb.SessionFileWrite{Path: r.WriteFile.Path, Contents: r.WriteFile.Contents, CreateParents: r.WriteFile.CreateParents}}}, nil
	}
	if r.CreateDirectory != nil {
		return &pb.ExecuteSessionOperationRequest{Operation: &pb.ExecuteSessionOperationRequest_CreateDirectory{CreateDirectory: &pb.SessionDirectoryCreate{Path: r.CreateDirectory.Path}}}, nil
	}
	if r.DeleteFile != nil {
		return &pb.ExecuteSessionOperationRequest{Operation: &pb.ExecuteSessionOperationRequest_DeleteFile{DeleteFile: &pb.SessionFileDelete{Path: r.DeleteFile.Path, Recursive: r.DeleteFile.Recursive}}}, nil
	}
	return &pb.ExecuteSessionOperationRequest{Operation: &pb.ExecuteSessionOperationRequest_RenameFile{RenameFile: &pb.SessionFileRename{SourcePath: r.RenameFile.SourcePath, DestinationPath: r.RenameFile.DestinationPath, Overwrite: r.RenameFile.Overwrite}}}, nil
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

func newExecuteOperationResponse(value *pb.ExecuteSessionOperationResponse) executeOperationResponse {
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

func (s *Server) writeSessionOperationEvent(w http.ResponseWriter, value *pb.SessionOperationEvent) error {
	response, err := newSessionOperationEventResponse(value)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if int64(len(encoded)+1) > s.maxResponseBodyBytes() {
		return fmt.Errorf("gateway stream event exceeds configured limit")
	}
	_, err = w.Write(append(encoded, '\n'))
	return err
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
