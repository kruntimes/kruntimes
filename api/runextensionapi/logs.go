package runextensionapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunLogOptions controls a Run-log API request. The returned request supports
// the standard client-go forms: Stream(ctx) for follow and Do(ctx).Raw() for a
// bounded snapshot.
type RunLogOptions struct {
	TailLines      int64
	Follow         bool
	Cursor         string
	TimeoutSeconds int
}

// RunLogRequest is a Run-log request with the familiar client-go terminal
// forms. It preserves Kubernetes Status response bodies from the aggregated
// API, which rest.Request otherwise reduces to a generic HTTP error for this
// custom API group.
type RunLogRequest struct {
	httpClient  *http.Client
	url         *url.URL
	bearerToken string
}

// RunLogResult is the bounded snapshot result returned by RunLogRequest.Do.
type RunLogResult struct {
	response *http.Response
	err      error
}

// GetRunLogs builds GET .../namespaces/{namespace}/runs/{name}/log. Callers may
// use Stream(ctx) for a follow response or Do(ctx).Raw() for a bounded
// snapshot.
func (c *Client) GetRunLogs(namespace, runName string, options *RunLogOptions) *RunLogRequest {
	if c == nil || c.logs == nil {
		return &RunLogRequest{}
	}
	request := c.logs.Get().Namespace(namespace).Resource("runs").Name(runName).SubResource("log")
	if options != nil {
		if options.TailLines > 0 {
			request.Param("tailLines", fmt.Sprintf("%d", options.TailLines))
		}
		if options.Follow {
			request.Param("follow", "true")
		}
		if options.Cursor != "" {
			request.Param("cursor", options.Cursor)
		}
		if options.TimeoutSeconds > 0 {
			request.Param("timeoutSeconds", fmt.Sprintf("%d", options.TimeoutSeconds))
		}
	}
	return &RunLogRequest{httpClient: c.httpClient, url: request.URL(), bearerToken: c.bearerToken}
}

// Stream executes a follow request and returns its response body.
func (r *RunLogRequest) Stream(ctx context.Context) (io.ReadCloser, error) {
	response, err := r.execute(ctx)
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}

// Do executes a bounded snapshot request. Call Raw to obtain its body.
func (r *RunLogRequest) Do(ctx context.Context) RunLogResult {
	response, err := r.execute(ctx)
	return RunLogResult{response: response, err: err}
}

// Raw reads and closes the bounded snapshot response body.
func (r RunLogResult) Raw() ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	defer r.response.Body.Close()
	contents, err := io.ReadAll(r.response.Body)
	if err != nil {
		return nil, fmt.Errorf("read aggregated Run log API response: %w", err)
	}
	return contents, nil
}

func (r *RunLogRequest) execute(ctx context.Context) (*http.Response, error) {
	if r == nil || r.httpClient == nil || r.url == nil {
		return nil, fmt.Errorf("aggregated Run log API request is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build aggregated Run log API request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if r.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+r.bearerToken)
	}
	response, err := r.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("request aggregated Run log API: %w", err)
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
		return response, nil
	}
	defer response.Body.Close()
	return nil, decodeStatusError(response)
}

func decodeStatusError(response *http.Response) error {
	status := metav1.Status{}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status); err == nil && status.Message != "" {
		status.Status = metav1.StatusFailure
		if status.Code == 0 {
			status.Code = int32(response.StatusCode)
		}
		if status.Reason == "" {
			status.Reason = metav1.StatusReasonUnknown
		}
		return &apierrors.StatusError{ErrStatus: status}
	}
	return fmt.Errorf("aggregated Run log API returned %s", response.Status)
}
