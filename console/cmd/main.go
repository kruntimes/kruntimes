package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	console "github.com/kruntimes/kruntimes/console/backend"
	"github.com/kruntimes/kruntimes/internal/gateway"
	"github.com/kruntimes/kruntimes/internal/logapi"
)

func main() {
	var (
		address                    string
		assetsDirectory            string
		certificateFile            string
		privateKeyFile             string
		clientCAFile               string
		publicRead                 bool
		authorizationCacheTTL      time.Duration
		authorizationCacheCapacity int
		maxConcurrentRequests      int
		maxRequestBodyBytes        int64
		maxResponseBodyBytes       int64
		maxHeaderBytes             int
	)
	flag.StringVar(&address, "bind-address", ":8443", "The HTTPS address the Kruntimes Console binds to.")
	flag.StringVar(&assetsDirectory, "assets-dir", "/console/assets", "Directory containing the Console frontend assets.")
	flag.StringVar(&certificateFile, "tls-certificate-file", "", "PEM TLS certificate file for the Console. Required.")
	flag.StringVar(&privateKeyFile, "tls-private-key-file", "", "PEM TLS private key file for the Console. Required.")
	flag.StringVar(&clientCAFile, "tls-client-ca-file", "", "Optional PEM client CA bundle for Kubernetes client-certificate authentication.")
	flag.BoolVar(&publicRead, "public-read", true, "Allow unauthenticated namespace and Run list requests using the Console ServiceAccount.")
	defaultAuthorizationCache := gateway.DefaultAuthorizationCacheOptions()
	flag.DurationVar(&authorizationCacheTTL, "authorization-cache-ttl", defaultAuthorizationCache.TTL, "How long successful access authorization decisions remain cached; zero disables caching.")
	flag.IntVar(&authorizationCacheCapacity, "authorization-cache-capacity", defaultAuthorizationCache.Capacity, "Maximum successful authorization decisions retained; zero disables caching.")
	flag.IntVar(&maxConcurrentRequests, "max-concurrent-requests", gateway.DefaultMaxConcurrentRequests, "Maximum Runtime access API requests handled by one Console Pod.")
	flag.Int64Var(&maxRequestBodyBytes, "max-request-body-bytes", gateway.DefaultMaxRequestBodyBytes, "Maximum Runtime access API JSON request body size in bytes.")
	flag.Int64Var(&maxResponseBodyBytes, "max-response-body-bytes", gateway.DefaultMaxResponseBodyBytes, "Maximum Runtime access API JSON response size in bytes.")
	flag.IntVar(&maxHeaderBytes, "max-header-bytes", gateway.DefaultMaxHeaderBytes, "Maximum HTTP request header size in bytes.")
	flag.Parse()
	if certificateFile == "" || privateKeyFile == "" {
		fmt.Fprintln(os.Stderr, "both --tls-certificate-file and --tls-private-key-file are required")
		os.Exit(2)
	}
	if maxRequestBodyBytes <= 0 || maxResponseBodyBytes <= 0 || maxHeaderBytes <= 0 {
		fmt.Fprintln(os.Stderr, "Console Runtime access API request body, response body, and header limits must be positive")
		os.Exit(2)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	config := ctrl.GetConfigOrDie()
	factory, err := console.NewRequestClientFactory(config, scheme)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	authenticator, err := console.NewTokenReviewAuthenticator(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console TokenReview client: %v\n", err)
		os.Exit(1)
	}
	logs, err := logapi.NewClient(rest.AnonymousClientConfig(config))
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console aggregated Run log API: %v\n", err)
		os.Exit(1)
	}
	logClient, err := console.NewAggregatedRunLogClient(logs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console aggregated Run log API: %v\n", err)
		os.Exit(1)
	}
	runClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console Runtime access client: %v\n", err)
		os.Exit(1)
	}
	kubernetesClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console authorization client: %v\n", err)
		os.Exit(1)
	}
	consoleServer := &console.Server{
		Clients:       factory,
		Authenticator: authenticator,
		Logs:          logClient,
		Assets:        os.DirFS(assetsDirectory),
		RuntimeAccess: &gateway.Server{
			Runs: runClient,
			Authorizer: gateway.NewCachingAuthorizer(
				gateway.KubernetesAuthorizer{Client: kubernetesClient},
				gateway.AuthorizationCacheOptions{Capacity: authorizationCacheCapacity, TTL: authorizationCacheTTL},
			),
			Dialer:                gateway.GRPCDialer{},
			FunctionDialer:        gateway.GRPCDialer{},
			MaxConcurrentRequests: maxConcurrentRequests,
			MaxRequestBodyBytes:   maxRequestBodyBytes,
			MaxResponseBodyBytes:  maxResponseBodyBytes,
			MaxHeaderBytes:        maxHeaderBytes,
		},
	}
	if publicRead {
		publicClient, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			fmt.Fprintf(os.Stderr, "configure Console public-read client: %v\n", err)
			os.Exit(1)
		}
		consoleServer.PublicReadClient = publicClient
	}
	tlsConfig, err := consoleTLSConfig(certificateFile, privateKeyFile, clientCAFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console TLS: %v\n", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              address,
		Handler:           consoleServer,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    maxHeaderBytes,
		TLSConfig:         tlsConfig,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()

	if err := server.ListenAndServeTLS(certificateFile, privateKeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(os.Stderr, "serve Console: %v\n", err)
		os.Exit(1)
	}
}

func consoleTLSConfig(certificateFile, privateKeyFile, clientCAFile string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(certificateFile, privateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	if clientCAFile == "" {
		return config, nil
	}
	clientCA, err := os.ReadFile(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("read TLS client CA: %w", err)
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(clientCA) {
		return nil, errors.New("TLS client CA contains no certificates")
	}
	config.ClientAuth = tls.VerifyClientCertIfGiven
	config.ClientCAs = clientCAs
	return config, nil
}
