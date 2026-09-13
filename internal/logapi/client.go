package logapi

import (
	"fmt"

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
	GetLogs(namespace, runName string, options *RunLogOptions) *rest.Request
}

// RESTClient is a client-go REST implementation of Client.
type RESTClient struct {
	restClient  rest.Interface
	bearerToken string
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
	return &RESTClient{restClient: restClient}, nil
}

// WithBearerToken returns a request client that forwards token to the API
// server. It is intended for server-side relays such as Dashboard; the token
// is never exposed to browser JavaScript.
func (c *RESTClient) WithBearerToken(token string) *RESTClient {
	return &RESTClient{restClient: c.restClient, bearerToken: token}
}

// GetLogs builds GET .../namespaces/{namespace}/runs/{name}/log. Callers may
// use Stream(ctx), Do(ctx).Raw(), or any other standard rest.Request terminal
// operation.
func (c *RESTClient) GetLogs(namespace, runName string, options *RunLogOptions) *rest.Request {
	request := c.restClient.Get().Namespace(namespace).Resource("runs").Name(runName).SubResource("log")
	if c.bearerToken != "" {
		request.SetHeader("Authorization", "Bearer "+c.bearerToken)
	}
	if options == nil {
		return request
	}
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
	return request
}
