package runextensionapi

import (
	"context"
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

	gwebsocket "github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

const (
	sessionFrameTypeSend      = "send"
	sessionFrameTypeCancel    = "cancel"
	sessionFrameTypeHeartbeat = "heartbeat"
	sessionEventBufferSize    = 64
)

// SessionsInterface exposes Session operations and Session workspace files.
type SessionsInterface struct {
	client *Client
}

// SessionOptions configures a Session executor.
type SessionOptions struct {
	// HeartbeatInterval enables application heartbeat frames that renew a
	// Session lease. Zero disables heartbeats.
	HeartbeatInterval time.Duration
}

// NewExecutor opens a persistent Session connection to a ready Session Run
// through the aggregated Session API. The context bounds the executor's
// lifetime; Receive closes when that context is done or Close is called.
func (s SessionsInterface) NewExecutor(ctx context.Context, namespace, runName string, options *SessionOptions) (*SessionExecutor, error) {
	if s.client == nil || s.client.sessions == nil || s.client.sessionDialer == nil {
		return nil, errors.New("Run extension API Session connection is not configured")
	}
	if err := requireRun(namespace, runName); err != nil {
		return nil, err
	}
	endpoint, err := sessionWebSocketURL(s.client.sessionRequest(namespace, runName).URL().String())
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	if s.client.bearerToken != "" {
		headers.Set("Authorization", "Bearer "+s.client.bearerToken)
	}
	connection, response, err := s.client.sessionDialer.DialSession(ctx, endpoint, headers)
	if err != nil {
		if response != nil {
			return nil, sessionHandshakeError(response)
		}
		return nil, fmt.Errorf("open Session connection: %w", err)
	}
	executorContext, cancel := context.WithCancel(ctx)
	executor := &SessionExecutor{
		connection: connection,
		ctx:        executorContext,
		cancel:     cancel,
		events:     make(chan OperationEvent, sessionEventBufferSize),
		accepted:   make(chan OperationEvent, 1),
		readerDone: make(chan struct{}),
	}
	if options != nil && options.HeartbeatInterval > 0 {
		executor.startHeartbeat(options.HeartbeatInterval)
	}
	go executor.readLoop()
	return executor, nil
}

func sessionWebSocketURL(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "", errors.New("Session endpoint is invalid")
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", errors.New("Session endpoint must use HTTP or HTTPS")
	}
	return parsed.String(), nil
}

// SessionDialer opens the persistent Session WebSocket using the caller's
// Kubernetes transport configuration.
type SessionDialer interface {
	DialSession(context.Context, string, http.Header) (*gwebsocket.Conn, *http.Response, error)
}

type restSessionDialer struct {
	dialer *gwebsocket.Dialer
	config *rest.Config
}

func newSessionDialer(config *rest.Config) (SessionDialer, error) {
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, fmt.Errorf("create Session TLS config: %w", err)
	}
	return restSessionDialer{dialer: &gwebsocket.Dialer{
		TLSClientConfig:  tlsConfig,
		HandshakeTimeout: 15 * time.Second,
		Proxy:            config.Proxy,
		NetDialContext:   config.Dial,
	}, config: rest.CopyConfig(config)}, nil
}

func (d restSessionDialer) DialSession(ctx context.Context, endpoint string, headers http.Header) (*gwebsocket.Conn, *http.Response, error) {
	if d.dialer == nil || d.config == nil {
		return nil, nil, errors.New("Session WebSocket dialer is not configured")
	}
	requestURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, err
	}
	switch requestURL.Scheme {
	case "ws":
		requestURL.Scheme = "http"
	case "wss":
		requestURL.Scheme = "https"
	default:
		return nil, nil, errors.New("Session WebSocket endpoint is invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header = headers.Clone()
	// Kubernetes REST wrappers supply rotating token-file, exec-plugin, and
	// auth-provider credentials before the WebSocket handshake.
	handshake := &websocketHandshakeTransport{dialer: d.dialer, endpoint: endpoint}
	transport, err := rest.HTTPWrappersForConfig(d.config, handshake)
	if err != nil {
		return nil, nil, fmt.Errorf("configure Session credentials: %w", err)
	}
	response, err := transport.RoundTrip(request)
	if handshake.connection != nil {
		return handshake.connection, response, nil
	}
	if err != nil {
		return nil, response, err
	}
	return nil, response, errors.New("Session WebSocket handshake failed")
}

type websocketHandshakeTransport struct {
	dialer     *gwebsocket.Dialer
	endpoint   string
	connection *gwebsocket.Conn
}

func (t *websocketHandshakeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	connection, response, err := t.dialer.DialContext(request.Context(), t.endpoint, request.Header)
	if err != nil {
		if response != nil {
			return response, nil
		}
		return nil, err
	}
	t.connection = connection
	return response, nil
}

// sessionHandshakeError maps the HTTP response that rejected the WebSocket
// upgrade to APIError, preserving the Kubernetes status or backend error body.
func sessionHandshakeError(response *http.Response) error {
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var status metav1.Status
	if json.Unmarshal(payload, &status) == nil && status.Message != "" {
		return &APIError{StatusCode: response.StatusCode, Message: status.Message}
	}
	var responseError struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(payload, &responseError) == nil && responseError.Error != "" {
		return &APIError{StatusCode: response.StatusCode, Message: responseError.Error}
	}
	return &APIError{StatusCode: response.StatusCode, Message: http.StatusText(response.StatusCode)}
}

// ListFiles returns one bounded page of direct children below a
// workspace-relative path.
func (s SessionsInterface) ListFiles(ctx context.Context, namespace, runName string, options *ListSessionFilesOptions) (*SessionFilePage, error) {
	if options == nil {
		options = &ListSessionFilesOptions{}
	}
	if options.Limit < 0 {
		return nil, errors.New("file page limit cannot be negative")
	}
	if err := requireRun(namespace, runName); err != nil {
		return nil, err
	}
	request := s.client.sessionFilesRequest(namespace, runName)
	if options.Directory != "" {
		request.Param("path", options.Directory)
	}
	if options.Limit > 0 {
		request.Param("limit", strconv.Itoa(options.Limit))
	}
	if options.PageToken != "" {
		request.Param("pageToken", options.PageToken)
	}
	body, err := s.client.do(ctx, request)
	if err != nil {
		return nil, err
	}
	var page SessionFilePage
	if err := decodeResponse(body, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

// ReadFile returns bounded content at a workspace-relative path.
func (s SessionsInterface) ReadFile(ctx context.Context, namespace, runName, path string, maxBytes int64) ([]byte, bool, error) {
	if path == "" {
		return nil, false, errors.New("Session file path is required")
	}
	if err := requireRun(namespace, runName); err != nil {
		return nil, false, err
	}
	request := s.client.sessionFilesRequest(namespace, runName)
	if maxBytes > 0 {
		request.Param("maxBytes", strconv.FormatInt(maxBytes, 10))
	}
	// Suffix is the client-go canonical way to address one file below the
	// files collection. Reject traversal before the request is sent: client-go
	// normalizes the URL, which would otherwise silently address a different
	// endpoint instead of the intended workspace file.
	cleanPath, err := sessionFilePath(path)
	if err != nil {
		return nil, false, err
	}
	request.Suffix(cleanPath)
	body, err := s.client.do(ctx, request)
	if err != nil {
		return nil, false, err
	}
	var response struct {
		Contents  []byte `json:"contents"`
		Truncated bool   `json:"truncated"`
	}
	if err := decodeResponse(body, &response); err != nil {
		return nil, false, err
	}
	return response.Contents, response.Truncated, nil
}

func sessionFilePath(path string) (string, error) {
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return "", fmt.Errorf("Session file path %q escapes the workspace", path)
		}
	}
	return strings.TrimPrefix(path, "/"), nil
}

// SessionExecutor is one replaceable, persistent connection to a ready Session
// Run. Closing it does not terminate the Run or release Runtime capacity.
//
// Send submits one operation and Receive exposes the ordered event channel:
//
//	executor, err := client.Sessions().NewExecutor(ctx, namespace, runName, nil)
//	operationID, err := executor.Send(ctx, operation)
//	for event := range executor.Receive() {
//	    // output, progress, completed, or failed
//	}
//	if err := executor.Err(); err != nil { ... }
type SessionExecutor struct {
	connection *gwebsocket.Conn
	ctx        context.Context
	cancel     context.CancelFunc

	events     chan OperationEvent
	accepted   chan OperationEvent
	readerDone chan struct{}

	writeMu sync.Mutex
	readMu  sync.Mutex
	stateMu sync.Mutex
	active  string
	sending bool
	closed  bool
	err     error

	heartbeatStop chan struct{}
	heartbeatDone chan struct{}
}

type sessionSendFrame struct {
	Type           string    `json:"type"`
	Operation      Operation `json:"operation"`
	IdempotencyKey string    `json:"idempotencyKey,omitempty"`
}

// Receive returns the ordered event stream. The channel closes when the
// connection ends; call Err after it closes to distinguish a normal Close from
// a transport failure.
func (s *SessionExecutor) Receive() <-chan OperationEvent {
	if s == nil {
		closed := make(chan OperationEvent)
		close(closed)
		return closed
	}
	return s.events
}

// Err returns the terminal stream error. It is nil after a normal Close.
func (s *SessionExecutor) Err() error {
	if s == nil {
		return nil
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return nil
	}
	return s.err
}

// Send submits one operation and waits for its accepted event. Only one
// operation may be active on one Session connection at a time. The accepted
// event is consumed by Send; Receive yields the remaining stream events.
func (s *SessionExecutor) Send(ctx context.Context, operation Operation, idempotencyKey ...string) (string, error) {
	if s == nil || s.connection == nil {
		return "", errors.New("Session connection is not configured")
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return "", errors.New("Session connection is closed")
	}
	if s.active != "" || s.sending {
		s.stateMu.Unlock()
		return "", errors.New("Session connection already has an active operation")
	}
	s.sending = true
	s.stateMu.Unlock()
	defer func() {
		s.stateMu.Lock()
		s.sending = false
		s.stateMu.Unlock()
	}()
	if len(idempotencyKey) > 1 {
		return "", errors.New("at most one Session idempotency key is allowed")
	}
	frame := sessionSendFrame{Type: sessionFrameTypeSend, Operation: operation}
	if len(idempotencyKey) == 1 {
		frame.IdempotencyKey = idempotencyKey[0]
	}
	payload, err := json.Marshal(frame)
	if err != nil {
		return "", fmt.Errorf("encode Session send: %w", err)
	}
	if err := s.writeText(ctx, payload); err != nil {
		return "", fmt.Errorf("send Session operation: %w", err)
	}
	select {
	case event := <-s.accepted:
		if event.Accepted == nil || event.Accepted.OperationID == "" {
			if err := s.Err(); err != nil {
				return "", err
			}
			if s.ctx.Err() != nil {
				return "", s.ctx.Err()
			}
			return "", errors.New("Session connection is closed")
		}
		// The read loop records the active operation before publishing accepted,
		// so a fast terminal event cannot clear it before Send observes the ID.
		return event.Accepted.OperationID, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-s.readerDone:
		if err := s.Err(); err != nil {
			return "", err
		}
		return "", errors.New("Session connection is closed")
	}
}

// Cancel requests cancellation of the active operation.
func (s *SessionExecutor) Cancel(ctx context.Context, operationID string) error {
	if s == nil || s.connection == nil {
		return errors.New("Session connection is not configured")
	}
	s.stateMu.Lock()
	active, closed := s.active, s.closed
	s.stateMu.Unlock()
	if closed || active == "" || active != operationID {
		return errors.New("Session operation is not active")
	}
	payload, err := json.Marshal(struct {
		Type        string `json:"type"`
		OperationID string `json:"operationID"`
	}{Type: sessionFrameTypeCancel, OperationID: operationID})
	if err != nil {
		return fmt.Errorf("encode Session cancellation: %w", err)
	}
	if err := s.writeText(ctx, payload); err != nil {
		return fmt.Errorf("cancel Session operation: %w", err)
	}
	return nil
}

// Close closes only the persistent connection. It does not terminate the
// backing Session Run or release capacity.
func (s *SessionExecutor) Close() error {
	if s == nil || s.connection == nil {
		return nil
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return nil
	}
	s.closed = true
	s.stateMu.Unlock()
	if s.heartbeatStop != nil {
		close(s.heartbeatStop)
		<-s.heartbeatDone
	}
	s.cancel()
	err := s.connection.Close()
	<-s.readerDone
	return err
}

func (s *SessionExecutor) writeText(ctx context.Context, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	deadline := deadlineFromContext(ctx)
	if deadline.IsZero() {
		deadline = time.Now().Add(30 * time.Second)
	}
	if err := s.connection.SetWriteDeadline(deadline); err != nil {
		return err
	}
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		_ = s.connection.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stop() {
			<-callbackDone
		}
		_ = s.connection.SetWriteDeadline(time.Time{})
	}()
	return s.connection.WriteMessage(gwebsocket.TextMessage, payload)
}

func (s *SessionExecutor) startHeartbeat(interval time.Duration) {
	s.heartbeatStop = make(chan struct{})
	s.heartbeatDone = make(chan struct{})
	go func() {
		defer close(s.heartbeatDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.heartbeatStop:
				return
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				if err := s.writeHeartbeat(); err != nil {
					s.stateMu.Lock()
					s.closed = true
					s.stateMu.Unlock()
					_ = s.connection.Close()
					return
				}
			}
		}
	}()
}

func (s *SessionExecutor) writeHeartbeat() error {
	s.stateMu.Lock()
	closed := s.closed
	s.stateMu.Unlock()
	if closed {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.connection.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	defer s.connection.SetWriteDeadline(time.Time{})
	if err := s.connection.WriteJSON(struct {
		Type string `json:"type"`
	}{Type: sessionFrameTypeHeartbeat}); err != nil {
		return fmt.Errorf("send Session heartbeat: %w", err)
	}
	return nil
}

func (s *SessionExecutor) readLoop() {
	defer func() {
		close(s.events)
		close(s.accepted)
		close(s.readerDone)
	}()
	for {
		event, err := s.readEvent(s.ctx)
		if err != nil {
			s.stateMu.Lock()
			if !s.closed {
				s.err = err
			}
			s.stateMu.Unlock()
			return
		}
		if event.Type == "accepted" {
			if event.Accepted != nil {
				s.stateMu.Lock()
				s.active = event.Accepted.OperationID
				s.stateMu.Unlock()
			}
			select {
			case s.accepted <- event:
			case <-s.ctx.Done():
				return
			}
			continue
		}
		if event.Completed != nil || event.Failed != nil {
			s.stateMu.Lock()
			s.active = ""
			s.stateMu.Unlock()
		}
		select {
		case s.events <- event:
		case <-s.ctx.Done():
			return
		}
	}
}

func (s *SessionExecutor) readEvent(ctx context.Context) (OperationEvent, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	if err := s.connection.SetReadDeadline(deadlineFromContext(ctx)); err != nil {
		return OperationEvent{}, fmt.Errorf("set Session read deadline: %w", err)
	}
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		_ = s.connection.SetReadDeadline(time.Now())
	})
	defer func() {
		if !stop() {
			<-callbackDone
		}
		_ = s.connection.SetReadDeadline(time.Time{})
	}()
	var raw json.RawMessage
	if err := s.connection.ReadJSON(&raw); err != nil {
		return OperationEvent{}, fmt.Errorf("read Session event: %w", err)
	}
	var wire struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return OperationEvent{}, fmt.Errorf("decode Session frame: %w", err)
	}
	if wire.Type == "error" {
		return OperationEvent{}, errors.New(wire.Error)
	}
	var event OperationEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return OperationEvent{}, fmt.Errorf("decode Session event: %w", err)
	}
	return event, nil
}

func deadlineFromContext(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Time{}
}
