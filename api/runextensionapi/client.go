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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// Client talks to the aggregated Run extension API. All HTTP requests execute
// through client-go REST clients built from the caller's Kubernetes REST
// configuration, so authentication, TLS, proxies, exec credentials, and token
// rotation behave exactly like built-in Kubernetes clients.
type Client struct {
	config        *rest.Config
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
	sessionDialer, err := newSessionDialer(configured)
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
		config:        configured,
		logs:          logs,
		sessions:      sessions,
		functions:     functions,
		sessionDialer: sessionDialer,
	}, nil
}

// Runs returns the Run-scoped extension API operations.
func (c *Client) Runs() RunsInterface { return RunsInterface{client: c} }

// Sessions returns the Session extension API operations.
func (c *Client) Sessions() SessionsInterface { return SessionsInterface{client: c} }

// Functions returns the Function extension API operations.
func (c *Client) Functions() FunctionsInterface { return FunctionsInterface{client: c} }

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

// do executes request through client-go and returns the raw response body.
// Raw bodies are kept so both Kubernetes metav1.Status responses and the
// backend JSON error shape can be mapped to APIError.
func (c *Client) do(ctx context.Context, request *rest.Request) ([]byte, error) {
	c.authorize(request)
	result := request.Do(ctx)
	body, err := result.Raw()
	statusCode := 0
	result.StatusCode(&statusCode)
	if err != nil {
		return nil, newAPIError(statusCode, body, err)
	}
	return body, nil
}

// stream executes request through client-go and returns the unbounded response
// body for follow-style APIs.
func (c *Client) stream(ctx context.Context, request *rest.Request) (io.ReadCloser, error) {
	c.authorize(request)
	return request.Stream(ctx)
}

func (c *Client) authorize(request *rest.Request) {
	if c.bearerToken != "" {
		request.SetHeader("Authorization", "Bearer "+c.bearerToken)
	}
}

func decodeResponse(body []byte, response any) error {
	if response == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, response); err != nil {
		return fmt.Errorf("decode Run extension API response: %w", err)
	}
	return nil
}

// newAPIError maps both the Kubernetes metav1.Status used for API-server
// rejections and the backend JSON error body to APIError.
func newAPIError(statusCode int, body []byte, requestErr error) error {
	if statusCode == 0 && requestErr != nil {
		return fmt.Errorf("call Run extension API: %w", requestErr)
	}
	var status metav1.Status
	if json.Unmarshal(body, &status) == nil && status.Message != "" {
		return &APIError{StatusCode: statusCode, Message: status.Message}
	}
	var responseError struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &responseError) == nil && responseError.Error != "" {
		return &APIError{StatusCode: statusCode, Message: responseError.Error}
	}
	var statusErr *apierrors.StatusError
	if errors.As(requestErr, &statusErr) {
		code := int(statusErr.ErrStatus.Code)
		if code == 0 {
			code = statusCode
		}
		return &APIError{StatusCode: code, Message: statusErr.ErrStatus.Message}
	}
	if statusCode == 0 {
		return fmt.Errorf("call Run extension API: %w", requestErr)
	}
	return &APIError{StatusCode: statusCode, Message: requestErr.Error()}
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
