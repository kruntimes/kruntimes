package runextensionapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"k8s.io/client-go/rest"
)

const (
	sessionFrameTypeSend      = "send"
	sessionFrameTypeCancel    = "cancel"
	sessionFrameTypeHeartbeat = "heartbeat"
)

// SessionDialer opens the persistent Session WebSocket.
type SessionDialer interface {
	DialSession(context.Context, string, http.Header) (*websocket.Conn, *http.Response, error)
}

type restSessionDialer struct {
	dialer *websocket.Dialer
	config *rest.Config
}

func newSessionDialer(config *rest.Config) (SessionDialer, error) {
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, fmt.Errorf("create Session TLS config: %w", err)
	}
	return restSessionDialer{dialer: &websocket.Dialer{
		TLSClientConfig:  tlsConfig,
		HandshakeTimeout: 15 * time.Second,
		Proxy:            config.Proxy,
		NetDialContext:   config.Dial,
	}, config: rest.CopyConfig(config)}, nil
}

func (d restSessionDialer) DialSession(ctx context.Context, endpoint string, headers http.Header) (*websocket.Conn, *http.Response, error) {
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
	dialer     *websocket.Dialer
	endpoint   string
	connection *websocket.Conn
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

// SessionOptions configures OpenSession.
type SessionOptions struct {
	// HeartbeatInterval enables application heartbeat frames that renew a
	// Session lease. Zero disables heartbeats.
	HeartbeatInterval time.Duration
}

// OpenSession opens a persistent Session connection to a ready Session Run
// through the aggregated Session API.
func (c *Client) OpenSession(ctx context.Context, namespace, runName string, options *SessionOptions) (*Session, error) {
	if c == nil || c.sessionDialer == nil {
		return nil, errors.New("Run extension API Session connection is not configured")
	}
	if err := requireRun(namespace, runName); err != nil {
		return nil, err
	}
	websocketEndpoint, err := sessionWebSocketURL(c.sessionRequest(namespace, runName).URL().String())
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	if c.bearerToken != "" {
		headers.Set("Authorization", "Bearer "+c.bearerToken)
	}
	connection, response, err := c.sessionDialer.DialSession(ctx, websocketEndpoint, headers)
	if err != nil {
		if response != nil {
			return nil, &APIError{StatusCode: response.StatusCode, Message: "open Session connection"}
		}
		return nil, fmt.Errorf("open Session connection: %w", err)
	}
	session := &Session{connection: connection}
	if options != nil && options.HeartbeatInterval > 0 {
		session.startHeartbeat(options.HeartbeatInterval)
	}
	return session, nil
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

// Session is one replaceable, persistent connection to a ready Session Run.
// Closing it does not terminate the Run or release Runtime capacity.
type Session struct {
	connection *websocket.Conn

	writeMu sync.Mutex
	readMu  sync.Mutex
	stateMu sync.Mutex
	active  string
	sending bool
	closed  bool

	heartbeatStop chan struct{}
	heartbeatDone chan struct{}
}

type sessionSendFrame struct {
	Type           string    `json:"type"`
	Operation      Operation `json:"operation"`
	IdempotencyKey string    `json:"idempotencyKey,omitempty"`
}

// Send submits one operation and waits for its accepted event. Only one
// operation may be active on one Session connection at a time.
func (s *Session) Send(ctx context.Context, operation Operation, idempotencyKey ...string) (string, error) {
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
	s.writeMu.Lock()
	deadline := deadlineFromContext(ctx)
	if deadline.IsZero() {
		deadline = time.Now().Add(30 * time.Second)
	}
	_ = s.connection.SetWriteDeadline(deadline)
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		_ = s.connection.SetWriteDeadline(time.Now())
	})
	err = s.connection.WriteMessage(websocket.TextMessage, payload)
	if !stop() {
		<-callbackDone
	}
	_ = s.connection.SetWriteDeadline(time.Time{})
	s.writeMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("send Session operation: %w", err)
	}
	event, err := s.readEvent(ctx)
	if err != nil {
		return "", err
	}
	if event.Type != "accepted" || event.Accepted == nil || event.Accepted.OperationID == "" {
		return "", errors.New("Session did not return an accepted operation")
	}
	s.stateMu.Lock()
	s.active = event.Accepted.OperationID
	s.stateMu.Unlock()
	return event.Accepted.OperationID, nil
}

// Receive returns the next event after Send. It returns a transport error for
// post-upgrade API errors rather than presenting them as operation events.
func (s *Session) Receive(ctx context.Context) (OperationEvent, error) {
	if s == nil || s.connection == nil {
		return OperationEvent{}, errors.New("Session connection is not configured")
	}
	event, err := s.readEvent(ctx)
	if err != nil {
		// An API error terminates the active operation but not necessarily the
		// persistent Session connection. Permit another operation.
		s.stateMu.Lock()
		s.active = ""
		s.stateMu.Unlock()
		return OperationEvent{}, err
	}
	if event.Completed != nil || event.Failed != nil {
		s.stateMu.Lock()
		s.active = ""
		s.stateMu.Unlock()
	}
	return event, nil
}

// Cancel requests cancellation of the active operation.
func (s *Session) Cancel(ctx context.Context, operationID string) error {
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.connection.SetWriteDeadline(deadlineFromContext(ctx)); err != nil {
		return fmt.Errorf("set Session write deadline: %w", err)
	}
	defer s.connection.SetWriteDeadline(time.Time{})
	if err := s.connection.WriteMessage(websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("cancel Session operation: %w", err)
	}
	return nil
}

// Close closes only the persistent connection. It does not terminate the
// backing Session Run or release capacity.
func (s *Session) Close() error {
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
	err := s.connection.Close()
	if s.heartbeatStop != nil {
		close(s.heartbeatStop)
		<-s.heartbeatDone
	}
	return err
}

func (s *Session) startHeartbeat(interval time.Duration) {
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

func (s *Session) writeHeartbeat() error {
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

func (s *Session) readEvent(ctx context.Context) (OperationEvent, error) {
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
