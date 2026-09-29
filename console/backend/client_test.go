package console

import (
	"net/http"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestRequestClientFactoryConfigForTokenUsesOnlyRequestIdentity(t *testing.T) {
	scheme := runtime.NewScheme()
	factory, err := NewRequestClientFactory(&rest.Config{
		Host:            "https://kubernetes.example",
		BearerToken:     "service-account-token",
		BearerTokenFile: "/var/run/secrets/token",
		Username:        "service-account-user",
		Password:        "service-account-password",
		AuthProvider:    &clientcmdapi.AuthProviderConfig{Name: "gcp"},
		ExecProvider:    &clientcmdapi.ExecConfig{Command: "credential-plugin"},
		Impersonate:     rest.ImpersonationConfig{UserName: "administrator"},
		TLSClientConfig: rest.TLSClientConfig{
			CAFile:   "/var/run/secrets/ca.crt",
			CertFile: "/var/run/secrets/client.crt",
			KeyFile:  "/var/run/secrets/client.key",
			CertData: []byte("client-certificate"),
			KeyData:  []byte("client-key"),
		},
		Transport: http.DefaultTransport,
	}, scheme)
	if err != nil {
		t.Fatalf("NewRequestClientFactory() error = %v", err)
	}

	config := factory.configForToken("caller-token")
	if config.Host != "https://kubernetes.example" || config.TLSClientConfig.CAFile != "/var/run/secrets/ca.crt" {
		t.Fatalf("transport config = %#v, want base API endpoint and CA", config)
	}
	if config.BearerToken != "caller-token" || config.BearerTokenFile != "" {
		t.Fatalf("bearer credential = token %q, file %q, want only caller token", config.BearerToken, config.BearerTokenFile)
	}
	if config.Username != "" || config.Password != "" || config.AuthProvider != nil || config.ExecProvider != nil || config.Impersonate.UserName != "" || config.Transport != nil {
		t.Fatalf("fallback credential fields must be cleared: %#v", config)
	}
	if config.TLSClientConfig.CertFile != "" || config.TLSClientConfig.KeyFile != "" || len(config.TLSClientConfig.CertData) != 0 || len(config.TLSClientConfig.KeyData) != 0 {
		t.Fatalf("TLS client credential must be cleared: %#v", config.TLSClientConfig)
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
		err    error
	}{
		{name: "bearer", header: "Bearer token-value", want: "token-value"},
		{name: "case insensitive scheme", header: "bearer token-value", want: "token-value"},
		{name: "missing", err: ErrMissingBearerToken},
		{name: "wrong scheme", header: "Basic credentials", err: ErrMissingBearerToken},
		{name: "multiple values", header: "Bearer first second", err: ErrMissingBearerToken},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, "https://dashboard.example", nil)
			if err != nil {
				t.Fatalf("NewRequest() error = %v", err)
			}
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}
			got, err := bearerToken(request)
			if err != tt.err {
				t.Fatalf("bearerToken() error = %v, want %v", err, tt.err)
			}
			if got != tt.want {
				t.Fatalf("bearerToken() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBearerTokenAcceptsHTTPSessionCookie(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "https://dashboard.example", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	request.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "cookie-token"})
	got, err := bearerToken(request)
	if err != nil || got != "cookie-token" {
		t.Fatalf("bearerToken() = %q, %v", got, err)
	}
}
