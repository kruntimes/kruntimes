package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"k8s.io/client-go/rest"
)

// RunExtensionAPIProxy relays caller-authenticated Console requests to the
// Kubernetes aggregated Run extension API. It forwards the caller credential
// and makes no authorization decision: kube-apiserver authenticates the caller
// and authorizes the aggregated resource.
type RunExtensionAPIProxy struct {
	target    *url.URL
	transport http.RoundTripper
}

func NewRunExtensionAPIProxy(baseConfig *rest.Config) (*RunExtensionAPIProxy, error) {
	if baseConfig == nil || baseConfig.Host == "" {
		return nil, errors.New("Console Run extension API proxy requires a Kubernetes API host")
	}
	// The transport carries only TLS and connection details. Each request
	// supplies the caller credential directly.
	transport, err := rest.TransportFor(rest.AnonymousClientConfig(baseConfig))
	if err != nil {
		return nil, fmt.Errorf("configure Console Run extension API proxy transport: %w", err)
	}
	target, err := url.Parse(baseConfig.Host)
	if err != nil || target.Scheme == "" || target.Host == "" {
		return nil, errors.New("Console Run extension API proxy requires a valid Kubernetes API host")
	}
	return &RunExtensionAPIProxy{target: target, transport: transport}, nil
}

// Proxy relays one request to targetPath under the Kubernetes API server.
// Callers resolve the aggregated path from the Console route before calling it.
func (p *RunExtensionAPIProxy) Proxy(writer http.ResponseWriter, request *http.Request, targetPath string) {
	if p == nil || p.target == nil || p.transport == nil {
		writeRunExtensionAPIProxyError(writer, http.StatusServiceUnavailable, "Run extension API proxy is not configured")
		return
	}
	token, err := bearerToken(request)
	if err != nil {
		writeRunExtensionAPIProxyError(writer, http.StatusUnauthorized, "Console request requires a Kubernetes credential")
		return
	}
	proxy := &httputil.ReverseProxy{
		Director: func(out *http.Request) {
			out.URL.Scheme = p.target.Scheme
			out.URL.Host = p.target.Host
			out.URL.Path = targetPath
			out.URL.RawPath = ""
			out.Host = p.target.Host
			out.Header.Set("Authorization", "Bearer "+token)
			// The aggregation backend authorizes the Kubernetes identity, not the
			// browser's cookies or origin. Strip both before forwarding.
			out.Header.Del("Cookie")
			out.Header.Del("Origin")
		},
		Transport:     p.transport,
		FlushInterval: -1,
		ErrorHandler: func(writer http.ResponseWriter, _ *http.Request, _ error) {
			writeRunExtensionAPIProxyError(writer, http.StatusBadGateway, "aggregated Run extension API is unavailable")
		},
	}
	proxy.ServeHTTP(writer, request)
}

func writeRunExtensionAPIProxyError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(errorResponse{Error: message})
}
