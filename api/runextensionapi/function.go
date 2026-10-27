package runextensionapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// FunctionInvocationOptions is one bounded Function invocation request.
type FunctionInvocationOptions struct {
	// Input is one JSON value passed to the Function handler.
	Input []byte
	// InvocationID is an optional idempotency key bounded to 128 bytes by the
	// backend.
	InvocationID string
}

// InvokeFunction invokes one ready Function Run through the aggregated
// Function API.
func (c *Client) InvokeFunction(ctx context.Context, namespace, runName string, options *FunctionInvocationOptions) (*FunctionInvocationResult, error) {
	if options == nil || len(options.Input) == 0 {
		return nil, errors.New("Function invocation input is required")
	}
	if !json.Valid(options.Input) {
		return nil, errors.New("Function invocation input must be valid JSON")
	}
	if len(options.InvocationID) > 128 {
		return nil, errors.New("Function invocation ID exceeds 128 bytes")
	}
	if err := requireRun(namespace, runName); err != nil {
		return nil, err
	}
	request := c.functionRequest(namespace, runName)
	headers := http.Header{}
	if options.InvocationID != "" {
		headers.Set("X-Kruntime-Invocation-ID", options.InvocationID)
	}
	var result FunctionInvocationResult
	if err := c.execute(ctx, http.MethodPost, request.URL().String(), options.Input, headers, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
