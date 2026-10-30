package runextensionapi

import (
	"context"
	"encoding/json"
	"errors"
)

// FunctionsInterface exposes Function Run operations.
type FunctionsInterface struct {
	client *Client
}

// FunctionInvocationOptions is one bounded Function invocation request.
type FunctionInvocationOptions struct {
	// Input is one JSON value passed to the Function handler.
	Input []byte
	// InvocationID is an optional idempotency key bounded to 128 bytes by the
	// backend.
	InvocationID string
}

// Invoke invokes one ready Function Run through the aggregated Function API.
func (f FunctionsInterface) Invoke(ctx context.Context, namespace, runName string, options *FunctionInvocationOptions) (*FunctionInvocationResult, error) {
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
	if f.client == nil || f.client.functions == nil {
		return nil, errors.New("Run extension API Function client is not configured")
	}
	request := f.client.functionRequest(namespace, runName)
	request.Body(options.Input)
	request.SetHeader("Content-Type", "application/json")
	if options.InvocationID != "" {
		request.SetHeader("X-Kruntime-Invocation-ID", options.InvocationID)
	}
	body, err := f.client.do(ctx, request)
	if err != nil {
		return nil, err
	}
	var result FunctionInvocationResult
	if err := decodeResponse(body, &result); err != nil {
		return nil, err
	}
	return &result, nil
}
