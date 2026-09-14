package logapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

var groupVersion = schema.GroupVersion{Group: Group, Version: Version}

// RunLogOptions controls a Run-log API request. The returned request supports
// the standard client-go forms: Stream(ctx) for follow and Do(ctx).Raw() for
// a bounded snapshot.
type RunLogOptions struct {
	TailLines      int64
	Follow         bool
	Cursor         string
	TimeoutSeconds int
}

// Client builds Kubernetes aggregation Run-log requests.
type Client interface {
	GetLogs(namespace, runName string, options *RunLogOptions) *Request
}

// RESTClient is a client-go REST implementation of Client.
type RESTClient struct {
	restClient  rest.Interface
	httpClient  *http.Client
	bearerToken string
}

// Request is a Run-log request with the familiar client-go terminal forms.
// It preserves Kubernetes Status response bodies from the aggregated API,
// which rest.Request otherwise reduces to a generic HTTP error for this
// custom API group.
type Request struct {
	httpClient  *http.Client
	url         *url.URL
	bearerToken string
}

// Result is the bounded snapshot result returned by Request.Do.
type Result struct {
	response *http.Response
	err      error
}

// NewClient creates an aggregation API client using the authentication from
// config, including bearer tokens, exec credentials, and client certificates.
func NewClient(config *rest.Config) (*RESTClient, error) {
	if config == nil || config.Host == "" {
		return nil, fmt.Errorf("Kubernetes REST configuration is required for the aggregated Run log API")
	}
	configured := rest.CopyConfig(config)
	configured.GroupVersion = &groupVersion
	configured.APIPath = "/apis"
	configured.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	if configured.UserAgent == "" {
		configured.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	restClient, err := rest.RESTClientFor(configured)
	if err != nil {
		return nil, fmt.Errorf("configure aggregated Run log API client: %w", err)
	}
	httpClient, err := rest.HTTPClientFor(configured)
	if err != nil {
		return nil, fmt.Errorf("configure aggregated Run log HTTP client: %w", err)
	}
	return &RESTClient{restClient: restClient, httpClient: httpClient}, nil
}

// WithBearerToken returns a request client that forwards token to the API
// server. It is intended for server-side relays such as Dashboard; the token
// is never exposed to browser JavaScript.
func (c *RESTClient) WithBearerToken(token string) *RESTClient {
	return &RESTClient{restClient: c.restClient, httpClient: c.httpClient, bearerToken: token}
}

// GetLogs builds GET .../namespaces/{namespace}/runs/{name}/log. Callers may
// use Stream(ctx) for a follow response or Do(ctx).Raw() for a bounded
// snapshot.
func (c *RESTClient) GetLogs(namespace, runName string, options *RunLogOptions) *Request {
	request := c.restClient.Get().Namespace(namespace).Resource("runs").Name(runName).SubResource("log")
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
	return &Request{httpClient: c.httpClient, url: request.URL(), bearerToken: c.bearerToken}
}

// Stream executes a follow request and returns its response body.
func (r *Request) Stream(ctx context.Context) (io.ReadCloser, error) {
	response, err := r.execute(ctx)
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}

// Do executes a bounded snapshot request. Call Raw to obtain its body.
func (r *Request) Do(ctx context.Context) Result {
	response, err := r.execute(ctx)
	return Result{response: response, err: err}
}

// Raw reads and closes the bounded snapshot response body.
func (r Result) Raw() ([]byte, error) {
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

func (r *Request) execute(ctx context.Context) (*http.Response, error) {
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
