package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
