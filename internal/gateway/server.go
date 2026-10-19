// Package gateway implements the transitional Runtime access HTTP surface
// embedded by the Console. Data-plane translation lives in
// internal/runextensionapi/dataplane; this package keeps the Console /v1
// routes, per-request limits, and Kubernetes user authorization until clients
// move to the aggregated Run extension API and this package is removed.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runextensionapi/dataplane"
)

const (
	// DefaultMaxRequestBodyBytes is the maximum size of a gateway JSON request.
	DefaultMaxRequestBodyBytes int64 = 1 << 20
	// DefaultMaxResponseBodyBytes is the maximum size of a gateway JSON response.
	DefaultMaxResponseBodyBytes int64 = 1 << 20
	// DefaultMaxHeaderBytes is the maximum size of a gateway HTTP request header.
	DefaultMaxHeaderBytes = 1 << 20
	// DefaultMaxConcurrentRequests is the per-Console-Pod HTTP request limit.
	DefaultMaxConcurrentRequests = 128
	// DefaultMaxSessionConnections isolates long-lived WebSockets from HTTP requests.
	DefaultMaxSessionConnections = 128
)

// GRPCDialer is the data-plane Runtime Service dialer used by the Console.
type GRPCDialer = dataplane.GRPCDialer

// Authorizer verifies that a request principal may access a Session Run.
type Authorizer interface {
	Authorize(context.Context, *http.Request, *v1alpha1.Run) error
}

// Server exposes the transitional Console Runtime access API for already-ready
// Session and Function Runs.
type Server struct {
	Runs           client.Reader
	Authorizer     Authorizer
	Dialer         dataplane.SessionRuntimeDialer
	FunctionDialer dataplane.FunctionRuntimeDialer
	RuntimePort    int
	// MaxConcurrentRequests bounds requests handled by one Console Pod. Values
	// less than one use the default. Health checks do not consume this limit.
	MaxConcurrentRequests int
	// MaxSessionConnections bounds persistent Session WebSockets independently of HTTP requests.
	MaxSessionConnections int
	// MaxRequestBodyBytes bounds access API JSON request bodies. Values less than
	// one use the default.
	MaxRequestBodyBytes int64
	// MaxResponseBodyBytes bounds access API generated JSON responses. Values less
	// than one use the default.
	MaxResponseBodyBytes int64
	// MaxHeaderBytes is retained for Console listener configuration. The gateway
	// does not inspect request headers itself.
	MaxHeaderBytes    int
	requestLimiter    gatewayRequestLimiter
	connectionLimiter gatewayRequestLimiter
	routesOnce        sync.Once
	routes            http.Handler
	dataPlane         *dataplane.Server
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.routesOnce.Do(s.registerRoutes)
	s.routes.ServeHTTP(w, r)
}

func (s *Server) registerRoutes() {
	s.dataPlane = &dataplane.Server{
		Dialer:               s.Dialer,
		FunctionDialer:       s.FunctionDialer,
		RuntimePort:          s.RuntimePort,
		MaxRequestBodyBytes:  s.MaxRequestBodyBytes,
		MaxResponseBodyBytes: s.MaxResponseBodyBytes,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/functions/{invocation}", s.withRequestLimit(s.handleFunctionInvoke))

	withSessionRun := func(handler sessionRouteHandler) http.HandlerFunc {
		return s.withRequestLimit(s.withSessionRun(handler))
	}
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}", withSessionRun(s.handleSessionStatus))
	mux.HandleFunc("/v1/namespaces/{namespace}/runtimes/{runtime}/sessions/{runUID}/operations:ws", s.withSessionRun(s.handleSessionWebSocket))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { s.endpointNotFound(w) })

	s.routes = mux
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) endpointNotFound(w http.ResponseWriter) {
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

func (s *Server) withSessionConnectionLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := s.MaxSessionConnections
		if limit < 1 {
			limit = DefaultMaxSessionConnections
		}
		if !s.connectionLimiter.tryAcquire(limit) {
			s.writeError(w, http.StatusTooManyRequests, "gateway Session connection limit reached")
			return
		}
		defer s.connectionLimiter.release()
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
		s.endpointNotFound(w)
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
		s.endpointNotFound(w)
		return
	}
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
	s.dataPlane.ServeFunction(w, r, run)
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	if r.Method != http.MethodGet {
		s.methodNotAllowed(w)
		return
	}
	s.dataPlane.GetSessionStatus(w, r, run)
}

func (s *Server) handleSessionWebSocket(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	s.StreamSession(w, r, run)
}

// StreamSession serves a Session Run after the caller has authenticated and
// authorized the request.
func (s *Server) StreamSession(w http.ResponseWriter, r *http.Request, run *v1alpha1.Run) {
	s.withSessionConnectionLimit(func(w http.ResponseWriter, r *http.Request) {
		s.dataPlane.StreamSession(w, r, run)
	})(w, r)
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

func (s *Server) maxConcurrentRequests() int {
	if s.MaxConcurrentRequests > 0 {
		return s.MaxConcurrentRequests
	}
	return DefaultMaxConcurrentRequests
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
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_, _ = w.Write(response.Bytes())
}

func (s *Server) writeUnboundedError(w http.ResponseWriter, httpStatus int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_, _ = w.Write([]byte(fmt.Sprintf("{\"error\":%q}\n", message)))
}
