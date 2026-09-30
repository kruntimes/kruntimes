package gateway

import "testing"

func TestDecodeSessionConnectionMessage(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{name: "send", payload: `{"type":"send","idempotencyKey":"turn-1","operation":{"command":{"shell":"pwd"}}}`},
		{name: "cancel", payload: `{"type":"cancel","operationID":"operation-1"}`},
		{name: "unknown field", payload: `{"type":"cancel","operationID":"operation-1","extra":true}`, wantErr: true},
		{name: "send without operation", payload: `{"type":"send"}`, wantErr: true},
		{name: "cancel with operation", payload: `{"type":"cancel","operationID":"operation-1","operation":{}}`, wantErr: true},
		{name: "unknown type", payload: `{"type":"heartbeat"}`, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeSessionConnectionMessage([]byte(test.payload))
			if (err != nil) != test.wantErr {
				t.Fatalf("decode error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
