package dashboard

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"k8s.io/client-go/rest"
)

// RunLogGateway reads the logs for one Run through the Runtime Gateway. The
// Dashboard uses this narrow interface so its Kubernetes request client never
// needs pods/log permission.
type RunLogGateway interface {
	RunLogs(context.Context, string, string, string, string, int64, bool) (*http.Response, error)
}

// AggregatedRunLogGateway uses the Kubernetes API aggregation layer. Its
// caller token is authenticated by the API server, while this backend never
// needs pods/log permission.
type AggregatedRunLogGateway struct {
	baseURL *url.URL
	client  *http.Client
}

func NewAggregatedRunLogGateway(config *rest.Config) (*AggregatedRunLogGateway, error) {
	if config == nil || config.Host == "" {
		return nil, errors.New("Dashboard Kubernetes REST configuration is required")
	}
	endpoint, err := url.Parse(config.Host)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("Dashboard Kubernetes API URL must be absolute")
	}
	anonymous := rest.AnonymousClientConfig(config)
	transport, err := rest.TransportFor(anonymous)
	if err != nil {
		return nil, fmt.Errorf("configure Dashboard aggregated Run log transport: %w", err)
	}
	return &AggregatedRunLogGateway{baseURL: endpoint, client: &http.Client{Transport: transport}}, nil
}

func (g *AggregatedRunLogGateway) RunLogs(ctx context.Context, token, namespace, _ string, runName string, tailLines int64, follow bool) (*http.Response, error) {
	if g == nil || g.baseURL == nil {
		return nil, errors.New("Dashboard aggregated Run log API is not configured")
	}
	endpoint := g.baseURL.JoinPath("apis", "logs.kruntimes.io", "v1alpha1", "namespaces", namespace, "runs", runName, "log")
	query := endpoint.Query()
	query.Set("tailLines", fmt.Sprintf("%d", tailLines))
	if follow {
		query.Set("follow", "true")
	}
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create aggregated Run log request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call aggregated Run log API: %w", err)
	}
	return response, nil
}

// UsesRunName tells Server that this API identifies the Run by name rather
// than the direct Gateway's immutable UID route.
func (*AggregatedRunLogGateway) UsesRunName() bool { return true }

// HTTPRunLogGateway is the in-cluster Runtime Gateway client used by the
// Dashboard. A request-scoped Kubernetes bearer token is forwarded only to the
// Gateway, never to frontend JavaScript.
type HTTPRunLogGateway struct {
	baseURL *url.URL
	client  *http.Client
}

func NewHTTPRunLogGateway(rawURL, caFile string) (*HTTPRunLogGateway, error) {
	endpoint, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, errors.New("Dashboard Runtime Gateway URL must be an absolute HTTP URL")
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, errors.New("Dashboard Runtime Gateway URL must use http or https")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read Dashboard Runtime Gateway CA bundle: %w", err)
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("Dashboard Runtime Gateway CA bundle contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return &HTTPRunLogGateway{baseURL: endpoint, client: &http.Client{Transport: transport}}, nil
}

func (g *HTTPRunLogGateway) RunLogs(ctx context.Context, token, namespace, runtimeName, runUID string, tailLines int64, follow bool) (*http.Response, error) {
	if g == nil || g.baseURL == nil || g.client == nil {
		return nil, errors.New("Dashboard Runtime Gateway client is not configured")
	}
	endpoint := g.baseURL.JoinPath("v1", "namespaces", namespace, "runtimes", runtimeName, "runs", runUID, "logs")
	query := endpoint.Query()
	query.Set("tailLines", fmt.Sprintf("%d", tailLines))
	if follow {
		query.Set("follow", "true")
	}
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create Runtime Gateway log request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Runtime Gateway log API: %w", err)
	}
	return response, nil
}
