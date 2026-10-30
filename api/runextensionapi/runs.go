package runextensionapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
)

// RunsInterface exposes operations scoped to one Run.
type RunsInterface struct {
	client *Client
}

// RunLogOptions controls a Run-log API request. The returned request supports
// the standard client-go terminal forms: Stream(ctx) for follow and Do(ctx)
// for a bounded snapshot.
type RunLogOptions struct {
	TailLines      int64
	Follow         bool
	Cursor         string
	TimeoutSeconds int
}

// RunLogRequest is one Run-log request built with client-go.
type RunLogRequest struct {
	client  *Client
	request *rest.Request
}

// Logs builds GET .../namespaces/{namespace}/runs/{name}/log. Callers may use
// Stream(ctx) for a follow response or Do(ctx) for a bounded snapshot.
func (r RunsInterface) Logs(namespace, runName string, options *RunLogOptions) *RunLogRequest {
	if r.client == nil || r.client.logs == nil {
		return &RunLogRequest{}
	}
	request := r.client.logs.Get().Namespace(namespace).Resource("runs").Name(runName).SubResource("log")
	if options != nil {
		if options.TailLines > 0 {
			request.Param("tailLines", strconv.FormatInt(options.TailLines, 10))
		}
		if options.Follow {
			request.Param("follow", "true")
		}
		if options.Cursor != "" {
			request.Param("cursor", options.Cursor)
		}
		if options.TimeoutSeconds > 0 {
			request.Param("timeoutSeconds", strconv.Itoa(options.TimeoutSeconds))
		}
	}
	return &RunLogRequest{client: r.client, request: request}
}

// Stream executes a follow request and returns its response body.
func (r *RunLogRequest) Stream(ctx context.Context) (io.ReadCloser, error) {
	if r == nil || r.client == nil || r.request == nil {
		return nil, errors.New("aggregated Run log API request is not configured")
	}
	return r.client.stream(ctx, r.request)
}

// Do executes a bounded snapshot request and returns its raw body. Log API
// failures keep the Kubernetes StatusError shape because krt maps them to
// actionable CLI errors.
func (r *RunLogRequest) Do(ctx context.Context) ([]byte, error) {
	if r == nil || r.client == nil || r.request == nil {
		return nil, errors.New("aggregated Run log API request is not configured")
	}
	r.client.authorize(r.request)
	result := r.request.Do(ctx)
	body, err := result.Raw()
	if err == nil {
		return body, nil
	}
	statusCode := 0
	result.StatusCode(&statusCode)
	return nil, runLogStatusError(statusCode, body, err)
}

func runLogStatusError(statusCode int, body []byte, requestErr error) error {
	message := ""
	var status metav1.Status
	if json.Unmarshal(body, &status) == nil && status.Message != "" {
		message = status.Message
		if status.Code != 0 {
			statusCode = int(status.Code)
		}
	}
	var responseError struct {
		Error string `json:"error"`
	}
	if message == "" && json.Unmarshal(body, &responseError) == nil {
		message = responseError.Error
	}
	if message == "" {
		message = requestErr.Error()
	}
	reason := metav1.StatusReasonUnknown
	switch statusCode {
	case 409:
		reason = metav1.StatusReasonConflict
	case 404:
		reason = metav1.StatusReasonNotFound
	case 403:
		reason = metav1.StatusReasonForbidden
	}
	return &apierrors.StatusError{ErrStatus: metav1.Status{
		Status:  metav1.StatusFailure,
		Code:    int32(statusCode),
		Reason:  reason,
		Message: message,
	}}
}
