package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/gorilla/websocket"
	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

const (
	sessionConnectionMessageSend   = "send"
	sessionConnectionMessageCancel = "cancel"
)

// sessionConnectionMessage is one client frame on a persistent Session
// connection. The transport remains private to the SDK.
type sessionConnectionMessage struct {
	Type           string          `json:"type"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	Operation      json.RawMessage `json:"operation,omitempty"`
	OperationID    string          `json:"operationID,omitempty"`
}

type sessionConnectionFrame struct {
	message sessionConnectionMessage
	err     error
}
type sessionOperationReceiveResult struct {
	event *pb.SessionOperationEvent
	err   error
}

func readSessionConnectionFrames(connection *websocket.Conn, done <-chan struct{}) <-chan sessionConnectionFrame {
	frames := make(chan sessionConnectionFrame)
	go func() {
		defer close(frames)
		for {
			messageType, payload, err := connection.ReadMessage()
			if err == nil && messageType != websocket.TextMessage {
				err = errors.New("Session connection frames must be JSON text")
			}
			var message sessionConnectionMessage
			if err == nil {
				message, err = decodeSessionConnectionMessage(payload)
			}
			select {
			case frames <- sessionConnectionFrame{message: message, err: err}:
			case <-done:
			}
			if err != nil {
				return
			}
		}
	}()
	return frames
}

func receiveSessionOperationEvents(ctx context.Context, stream pb.SessionRuntime_StreamSessionOperationClient) <-chan sessionOperationReceiveResult {
	events := make(chan sessionOperationReceiveResult)
	go func() {
		defer close(events)
		for {
			event, err := stream.Recv()
			select {
			case events <- sessionOperationReceiveResult{event: event, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return events
}

func decodeSessionConnectionMessage(payload []byte) (sessionConnectionMessage, error) {
	var message sessionConnectionMessage
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&message); err != nil {
		return sessionConnectionMessage{}, fmt.Errorf("decode Session connection frame: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return sessionConnectionMessage{}, errors.New("Session connection frame must contain one JSON value")
	}
	switch message.Type {
	case sessionConnectionMessageSend:
		if len(message.Operation) == 0 || message.OperationID != "" {
			return sessionConnectionMessage{}, errors.New("send frame requires operation and must not include operationID")
		}
	case sessionConnectionMessageCancel:
		if message.OperationID == "" || len(message.Operation) != 0 || message.IdempotencyKey != "" {
			return sessionConnectionMessage{}, errors.New("cancel frame requires operationID only")
		}
	default:
		return sessionConnectionMessage{}, fmt.Errorf("unsupported Session connection frame type %q", message.Type)
	}
	return message, nil
}
