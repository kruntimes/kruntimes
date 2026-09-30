package sandbox

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

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

type defaultSessionDialer struct{}

const (
	sessionFrameTypeSend      = "send"
	sessionFrameTypeCancel    = "cancel"
	sessionFrameTypeHeartbeat = "heartbeat"
)

func (defaultSessionDialer) DialSession(ctx context.Context, endpoint string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	return websocket.DefaultDialer.DialContext(ctx, endpoint, headers)
}

type restSessionDialer struct{ dialer *websocket.Dialer }

func newRESTSessionDialer(config *rest.Config) (SessionDialer, error) {
	tlsConfig, err := rest.TLSConfigFor(config)
	if err != nil {
		return nil, fmt.Errorf("create Session TLS config: %w", err)
	}
	return restSessionDialer{dialer: &websocket.Dialer{TLSClientConfig: tlsConfig}}, nil
}

func (d restSessionDialer) DialSession(ctx context.Context, endpoint string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	if d.dialer == nil {
		return nil, nil, errors.New("Session WebSocket dialer is not configured")
	}
	return d.dialer.DialContext(ctx, endpoint, headers)
}

// Session is one replaceable, persistent SDK connection to a ready Sandbox.
// Closing it does not terminate the Sandbox or release Runtime capacity.
type Session struct {
	connection *websocket.Conn

	writeMu sync.Mutex
	readMu  sync.Mutex
	stateMu sync.Mutex
	active  string
	closed  bool

	heartbeatStop chan struct{}
	heartbeatDone chan struct{}
}

type sessionSendFrame struct {
	Type      string                  `json:"type"`
	Operation sessionCommandOperation `json:"operation"`
}

type sessionCommandOperation struct {
	Command Command `json:"command"`
}

// OpenSession opens a persistent connection to this ready Sandbox. It does not
// create a Run or acquire capacity.
func (s *Sandbox) OpenSession(ctx context.Context) (*Session, error) {
	if s == nil {
		return nil, &StateError{Message: "Sandbox is not available"}
	}
	if s.client == nil || s.client.sessionDialer == nil {
		return nil, &StateError{Run: s.Run(), Message: "Sandbox Session connection is not configured"}
	}
	endpoint, err := s.endpoint("operations:ws")
	if err != nil {
		return nil, err
	}
	websocketEndpoint, err := sessionWebSocketURL(endpoint)
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	if s.client.bearerToken != "" {
		headers.Set("Authorization", "Bearer "+s.client.bearerToken)
	}
	connection, response, err := s.client.sessionDialer.DialSession(ctx, websocketEndpoint, headers)
	if err != nil {
		if response != nil {
			return nil, &APIError{StatusCode: response.StatusCode, Message: "open Sandbox Session"}
		}
		return nil, fmt.Errorf("open Sandbox Session: %w", err)
	}
	session := &Session{connection: connection}
	if interval := sessionHeartbeatInterval(s.run); interval > 0 {
		session.startHeartbeat(interval)
	}
	return session, nil
}

func sessionWebSocketURL(endpoint string) (string, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "", errors.New("Sandbox Session endpoint is invalid")
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	default:
		return "", errors.New("Sandbox Session endpoint must use HTTP or HTTPS")
	}
	return parsed.String(), nil
}

// Send submits one command and waits for its accepted event. Only one command
// may be active on one Session connection at a time.
func (s *Session) Send(ctx context.Context, command Command) (string, error) {
	if s == nil || s.connection == nil {
		return "", errors.New("Sandbox Session is not configured")
	}
	s.stateMu.Lock()
	if s.closed {
		s.stateMu.Unlock()
		return "", errors.New("Sandbox Session is closed")
	}
	if s.active != "" {
		s.stateMu.Unlock()
		return "", errors.New("Sandbox Session already has an active operation")
	}
	s.stateMu.Unlock()
	payload, err := json.Marshal(sessionSendFrame{Type: sessionFrameTypeSend, Operation: sessionCommandOperation{Command: command}})
	if err != nil {
		return "", fmt.Errorf("encode Sandbox Session send: %w", err)
	}
	s.writeMu.Lock()
	err = s.connection.WriteMessage(websocket.TextMessage, payload)
	s.writeMu.Unlock()
	if err != nil {
		return "", fmt.Errorf("send Sandbox Session operation: %w", err)
	}
	if err := s.connection.SetReadDeadline(deadlineFromContext(ctx)); err != nil {
		return "", fmt.Errorf("set Sandbox Session read deadline: %w", err)
	}
	defer s.connection.SetReadDeadline(time.Time{})
	event, err := s.readEvent()
	if err != nil {
		return "", err
	}
	if event.Type != "accepted" || event.Accepted == nil || event.Accepted.OperationID == "" {
		return "", errors.New("Sandbox Session did not return an accepted operation")
	}
	s.stateMu.Lock()
	s.active = event.Accepted.OperationID
	s.stateMu.Unlock()
	return event.Accepted.OperationID, nil
}

// Receive returns the next event after Send. It returns a transport error for
// post-upgrade gateway errors rather than presenting them as operation events.
func (s *Session) Receive(ctx context.Context) (OperationEvent, error) {
	if s == nil || s.connection == nil {
		return OperationEvent{}, errors.New("Sandbox Session is not configured")
	}
	if err := s.connection.SetReadDeadline(deadlineFromContext(ctx)); err != nil {
		return OperationEvent{}, fmt.Errorf("set Sandbox Session read deadline: %w", err)
	}
	defer s.connection.SetReadDeadline(time.Time{})
	event, err := s.readEvent()
	if err != nil {
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
		return errors.New("Sandbox Session is not configured")
	}
	s.stateMu.Lock()
	active, closed := s.active, s.closed
	s.stateMu.Unlock()
	if closed || active == "" || active != operationID {
		return errors.New("Sandbox Session operation is not active")
	}
	payload, err := json.Marshal(struct {
		Type        string `json:"type"`
		OperationID string `json:"operationID"`
	}{Type: sessionFrameTypeCancel, OperationID: operationID})
	if err != nil {
		return fmt.Errorf("encode Sandbox Session cancellation: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.connection.SetWriteDeadline(deadlineFromContext(ctx)); err != nil {
		return fmt.Errorf("set Sandbox Session write deadline: %w", err)
	}
	defer s.connection.SetWriteDeadline(time.Time{})
	if err := s.connection.WriteMessage(websocket.TextMessage, payload); err != nil {
		return fmt.Errorf("cancel Sandbox Session operation: %w", err)
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
	if s.heartbeatStop != nil {
		close(s.heartbeatStop)
		<-s.heartbeatDone
	}
	return s.connection.Close()
}

func sessionHeartbeatInterval(run *v1alpha1.Run) time.Duration {
	if run == nil || run.Spec.Mode.Session == nil || run.Spec.Mode.Session.LeaseTimeoutSeconds == nil || *run.Spec.Mode.Session.LeaseTimeoutSeconds <= 0 {
		return 0
	}
	interval := time.Duration(*run.Spec.Mode.Session.LeaseTimeoutSeconds) * time.Second / 3
	return min(max(interval, 100*time.Millisecond), 30*time.Second)
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
	if err := s.connection.WriteJSON(struct {
		Type string `json:"type"`
	}{Type: sessionFrameTypeHeartbeat}); err != nil {
		return fmt.Errorf("send Sandbox Session heartbeat: %w", err)
	}
	return nil
}

func (s *Session) readEvent() (OperationEvent, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	var raw json.RawMessage
	if err := s.connection.ReadJSON(&raw); err != nil {
		return OperationEvent{}, fmt.Errorf("read Sandbox Session event: %w", err)
	}
	var wire struct {
		Type  string `json:"type"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return OperationEvent{}, fmt.Errorf("decode Sandbox Session frame: %w", err)
	}
	if wire.Type == "error" {
		return OperationEvent{}, errors.New(wire.Error)
	}
	var event OperationEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return OperationEvent{}, fmt.Errorf("decode Sandbox Session event: %w", err)
	}
	return event, nil
}

func deadlineFromContext(ctx context.Context) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Time{}
}
