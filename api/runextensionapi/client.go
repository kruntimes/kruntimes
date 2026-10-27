// Package runextensionapi is a client-go style client for the Kubernetes
// aggregated Run extension API served by the run-extension-api Deployment. It
// covers Run logs, Session operations and Session workspace files, and Function
// invocation.
//
// The client uses the caller's Kubernetes REST configuration, so kube-apiserver
// authenticates every request with the same mechanisms as kubectl: bearer
// tokens, token files, client certificates, and exec credentials. The
// run-extension-api backend only sees the verified aggregation identity, so
// callers never need Console access, Runtime Pod port-forwards, or Pod
// permissions.
package runextensionapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

const (
	// GroupLogs is the aggregated API group for Run logs.
	GroupLogs = "logs.kruntimes.io"
	// GroupSessions is the aggregated API group for Session operations and files.
	GroupSessions = "sessions.kruntimes.io"
	// GroupFunctions is the aggregated API group for Function invocation.
	GroupFunctions = "functions.kruntimes.io"
	// Version is the served API version of every Run extension API group.
	Version = "v1alpha1"
)

// APIError reports a non-success Run extension API response. API-server
// rejections and backend request failures are both surfaced with the HTTP
// status and the server-provided message.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Run extension API returned HTTP %d: %s", e.StatusCode, e.Message)
}

// Client talks to the aggregated Run extension API. The group accessors expose
// client-go REST interfaces so callers can build requests the same way they do
// for built-in Kubernetes APIs; the typed helpers wrap the common operations.
type Client struct {
	httpClient    *http.Client
	logs          rest.Interface
	sessions      rest.Interface
	functions     rest.Interface
	sessionDialer SessionDialer
	bearerToken   string
}

// NewForConfig creates a Run extension API client using the authentication and
// TLS configuration in config.
func NewForConfig(config *rest.Config) (*Client, error) {
	if config == nil || config.Host == "" {
		return nil, errors.New("Kubernetes REST config is required for the Run extension API")
	}
	configured := rest.CopyConfig(config)
	if configured.UserAgent == "" {
		configured.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	httpClient, err := rest.HTTPClientFor(configured)
	if err != nil {
		return nil, fmt.Errorf("configure Run extension API HTTP client: %w", err)
	}
	dialer, err := newSessionDialer(configured)
	if err != nil {
		return nil, err
	}
	logs, err := newGroupRESTClient(configured, GroupLogs)
	if err != nil {
		return nil, err
	}
	sessions, err := newGroupRESTClient(configured, GroupSessions)
	if err != nil {
		return nil, err
	}
	functions, err := newGroupRESTClient(configured, GroupFunctions)
	if err != nil {
		return nil, err
	}
	return &Client{
		httpClient:    httpClient,
		logs:          logs,
		sessions:      sessions,
		functions:     functions,
		sessionDialer: dialer,
	}, nil
}

// Logs returns the client-go REST interface for the logs.kruntimes.io group.
func (c *Client) Logs() rest.Interface { return c.logs }

// Sessions returns the client-go REST interface for the
// sessions.kruntimes.io group.
func (c *Client) Sessions() rest.Interface { return c.sessions }

// Functions returns the client-go REST interface for the
// functions.kruntimes.io group.
func (c *Client) Functions() rest.Interface { return c.functions }

// WithBearerToken returns a client copy that forwards token as the request
// credential. It is intended for server-side relays such as the Console; the
// token is never exposed to browser JavaScript. An empty token returns the
// same client.
func (c *Client) WithBearerToken(token string) *Client {
	if c == nil || token == "" {
		return c
	}
	cloned := *c
	cloned.bearerToken = token
	return &cloned
}

func requireRun(namespace, runName string) error {
	if namespace == "" || runName == "" {
		return errors.New("Run namespace and name are required")
	}
	return nil
}

// sessionRequest builds the client-go request for one named Session Run.
func (c *Client) sessionRequest(namespace, runName string) *rest.Request {
	return c.sessions.Get().Namespace(namespace).Resource("runs").Name(runName).SubResource("session")
}

// sessionFilesRequest builds the client-go request for a Session Run's
// workspace file collection.
func (c *Client) sessionFilesRequest(namespace, runName string) *rest.Request {
	return c.sessionRequest(namespace, runName).Suffix("files")
}

// functionRequest builds the client-go request for one named Function Run.
func (c *Client) functionRequest(namespace, runName string) *rest.Request {
	return c.functions.Post().Namespace(namespace).Resource("runs").Name(runName).SubResource("function")
}

// execute performs one bounded request against a URL built by a client-go REST
// request. Requests deliberately execute through the plain HTTP client so both
// the Kubernetes metav1.Status and the backend JSON error body are preserved.
func (c *Client) execute(ctx context.Context, method, endpoint string, body []byte, headers http.Header, response any) error {
	var content io.Reader
	if body != nil {
		content = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, content)
	if err != nil {
		return fmt.Errorf("build Run extension API request: %w", err)
	}
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json")
	if c.bearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	result, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("call Run extension API: %w", err)
	}
	defer result.Body.Close()
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		return decodeAPIError(result)
	}
	if response != nil {
		if err := json.NewDecoder(io.LimitReader(result.Body, 1<<20)).Decode(response); err != nil {
			return fmt.Errorf("decode Run extension API response: %w", err)
		}
	}
	return nil
}

// decodeAPIError maps both the Kubernetes metav1.Status used for API-server
// rejections and the backend JSON error body to APIError.
func decodeAPIError(result *http.Response) error {
	payload, _ := io.ReadAll(io.LimitReader(result.Body, 1<<20))
	var statusError struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
	}
	if json.Unmarshal(payload, &statusError) == nil && statusError.Kind == "Status" && statusError.Message != "" {
		return &APIError{StatusCode: result.StatusCode, Message: statusError.Message}
	}
	var responseError struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(payload, &responseError)
	return &APIError{StatusCode: result.StatusCode, Message: responseError.Error}
}

// newGroupRESTClient builds a client-go REST client for one aggregated group.
func newGroupRESTClient(config *rest.Config, group string) (rest.Interface, error) {
	configured := rest.CopyConfig(config)
	configured.GroupVersion = &schema.GroupVersion{Group: group, Version: Version}
	configured.APIPath = "/apis"
	configured.NegotiatedSerializer = clientgoscheme.Codecs.WithoutConversion()
	if configured.UserAgent == "" {
		configured.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	client, err := rest.RESTClientFor(configured)
	if err != nil {
		return nil, fmt.Errorf("configure %s client: %w", group, err)
	}
	return client, nil
}
