// Package gateway implements the Runtime Gateway HTTP entrypoint.
package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	defaultRuntimeServicePort = 9093
	runtimeIndexField         = "spec.runtime"

	// DefaultMaxRequestBodyBytes is the maximum size of a gateway JSON request.
	DefaultMaxRequestBodyBytes int64 = 1 << 20
	// DefaultMaxResponseBodyBytes is the maximum size of a gateway JSON response.
	DefaultMaxResponseBodyBytes int64 = 1 << 20
	// DefaultMaxHeaderBytes is the maximum size of a gateway HTTP request header.
	DefaultMaxHeaderBytes = 1 << 20
	// DefaultMaxConcurrentRequests is the per-gateway-Pod HTTP request limit.
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
	HTTPAddress    string
	HTTPSAddress   string
	ReadTimeout    time.Duration
	WriteTimeout   time.Duration

	// TLSCertificateFile and TLSPrivateKeyFile enable TLS when both paths are
	// configured. Supplying only one is a startup error; the gateway never
	// silently falls back to plain HTTP.
	TLSCertificateFile string
	TLSPrivateKeyFile  string
	// TLSClientCAFile optionally enables mTLS. Client certificates signed by
	// this CA are verified and authorized with their Kubernetes CN/O identity.
	TLSClientCAFile string

	// MaxConcurrentRequests bounds requests handled by one gateway Pod. Values
	// less than one use the default. Health checks do not consume this limit.
	MaxConcurrentRequests int
	// MaxRequestBodyBytes bounds gateway JSON request bodies. Values less than
	// one use the default.
	MaxRequestBodyBytes int64
	// MaxResponseBodyBytes bounds gateway-generated JSON responses. Values less
	// than one use the default.
	MaxResponseBodyBytes int64
	// MaxHeaderBytes bounds HTTP request headers before routing. Values less than
	// one use the default.
	MaxHeaderBytes int
	requestLimiter gatewayRequestLimiter
}

// Start implements manager.Runnable and serves the HTTP gateway until ctx ends.
func (s *Server) Start(ctx context.Context) error {
	httpAddress := s.HTTPAddress
	if httpAddress == "" && s.HTTPSAddress == "" {
		httpAddress = ":8084"
	}
	tlsConfig, err := s.tlsConfig()
	if err != nil {
		return err
	}
	if s.HTTPSAddress != "" && tlsConfig == nil {
		return errors.New("Runtime gateway HTTPS address requires TLS certificate and private key files")
	}
	type listenerConfig struct {
		listener net.Listener
		tls      bool
	}
	listeners := make([]listenerConfig, 0, 2)
	if httpAddress != "" {
		listener, err := net.Listen("tcp", httpAddress)
		if err != nil {
			return fmt.Errorf("listen for Runtime gateway HTTP: %w", err)
		}
		listeners = append(listeners, listenerConfig{listener: listener})
	}
	if s.HTTPSAddress != "" {
		listener, err := net.Listen("tcp", s.HTTPSAddress)
		if err != nil {
			for _, open := range listeners {
				_ = open.listener.Close()
			}
			return fmt.Errorf("listen for Runtime gateway HTTPS: %w", err)
		}
		listeners = append(listeners, listenerConfig{listener: listener, tls: true})
	}
	servers := make([]*http.Server, 0, len(listeners))
	results := make(chan error, len(listeners))
	for _, configuredListener := range listeners {
		server := s.httpServer()
		servers = append(servers, server)
		go func(server *http.Server, configuredListener listenerConfig) {
			var err error
			if configuredListener.tls {
				// ServeTLS configures ALPN and Go's HTTP/2 support. tlsConfig
				// already holds the loaded certificate, so empty file paths do
				// not cause it to reload certificate material.
				server.TLSConfig = tlsConfig
				err = server.ServeTLS(configuredListener.listener, "", "")
			} else {
				err = server.Serve(configuredListener.listener)
			}
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			results <- err
		}(server, configuredListener)
	}
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, server := range servers {
			_ = server.Shutdown(shutdownCtx)
		}
		for range servers {
			<-results
		}
		return nil
	case err := <-results:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, server := range servers {
			_ = server.Shutdown(shutdownCtx)
		}
		return err
	}
}

func (s *Server) httpServer() *http.Server {
	return &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       s.ReadTimeout,
		WriteTimeout:      s.WriteTimeout,
		MaxHeaderBytes:    s.maxHeaderBytes(),
	}
}

func (s *Server) tlsConfig() (*tls.Config, error) {
	if s.TLSCertificateFile == "" && s.TLSPrivateKeyFile == "" {
		if s.TLSClientCAFile != "" {
			return nil, errors.New("Runtime gateway TLS client CA requires TLS certificate and private key files")
		}
		return nil, nil
	}
	if s.TLSCertificateFile == "" || s.TLSPrivateKeyFile == "" {
		return nil, errors.New("both Runtime gateway TLS certificate and private key files are required")
	}
	certificate, err := tls.LoadX509KeyPair(s.TLSCertificateFile, s.TLSPrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load Runtime gateway TLS certificate: %w", err)
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	if s.TLSClientCAFile == "" {
		return config, nil
	}
	clientCA, err := os.ReadFile(s.TLSClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read Runtime gateway TLS client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(clientCA) {
		return nil, errors.New("Runtime gateway TLS client CA contains no certificates")
	}
	config.ClientAuth = tls.VerifyClientCertIfGiven
	config.ClientCAs = clientCAs
	return config, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			s.methodNotAllowed(w)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if !s.requestLimiter.tryAcquire(s.maxConcurrentRequests()) {
		s.writeError(w, http.StatusTooManyRequests, "gateway request concurrency limit reached")
		return
	}
	defer s.requestLimiter.release()

	if namespace, runtimeName, runUID, ok := functionRoute(r.URL.Path); ok {
		s.serveFunctionInvoke(w, r, namespace, runtimeName, runUID)
		return
	}
	namespace, runtimeName, runUID, suffix, ok := sessionRoute(r.URL.Path)
	if !ok {
		s.writeError(w, http.StatusNotFound, "endpoint not found")
		return
	}
	run, err := s.sessionRun(r.Context(), namespace, runtimeName, runUID)
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

	switch {
	case len(suffix) == 0 && r.Method == http.MethodGet:
		s.getSessionStatus(w, r, run)
	case len(suffix) == 1 && suffix[0] == "operations:execute" && r.Method == http.MethodPost:
		s.executeOperation(w, r, run)
	case len(suffix) == 1 && suffix[0] == "files" && r.Method == http.MethodGet:
		s.listFiles(w, r, run)
	case len(suffix) > 1 && suffix[0] == "files" && r.Method == http.MethodGet:
		s.readFile(w, r, run, strings.Join(suffix[1:], "/"))
	default:
		s.methodNotAllowed(w)
	}
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
	if err := s.Runs.List(ctx, &runs, client.InNamespace(namespace), client.MatchingFields{runtimeIndexField: runtimeName}); err != nil {
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

func functionRoute(path string) (namespace, runtimeName, runUID string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 7 || parts[0] != "v1" || parts[1] != "namespaces" || parts[3] != "runtimes" || parts[5] != "functions" {
		return "", "", "", false
	}
	var err error
	if namespace, err = url.PathUnescape(parts[2]); err != nil || namespace == "" {
		return "", "", "", false
	}
	if runtimeName, err = url.PathUnescape(parts[4]); err != nil || runtimeName == "" {
		return "", "", "", false
	}
	if !strings.HasSuffix(parts[6], ":invoke") {
		return "", "", "", false
	}
	runUID, err = url.PathUnescape(strings.TrimSuffix(parts[6], ":invoke"))
	if err != nil || runUID == "" {
		return "", "", "", false
	}
	return namespace, runtimeName, runUID, true
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
	if err := s.Runs.List(ctx, &runs, client.InNamespace(namespace), client.MatchingFields{runtimeIndexField: runtimeName}); err != nil {
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

func sessionRoute(path string) (namespace, runtimeName, runUID string, suffix []string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 7 || parts[0] != "v1" || parts[1] != "namespaces" || parts[3] != "runtimes" || parts[5] != "sessions" {
		return "", "", "", nil, false
	}
	namespace, err := url.PathUnescape(parts[2])
	if err != nil || namespace == "" {
		return "", "", "", nil, false
	}
	runtimeName, err = url.PathUnescape(parts[4])
	if err != nil || runtimeName == "" {
		return "", "", "", nil, false
	}
	runUID, err = url.PathUnescape(parts[6])
	if err != nil || runUID == "" {
		return "", "", "", nil, false
	}
	return namespace, runtimeName, runUID, parts[7:], true
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
