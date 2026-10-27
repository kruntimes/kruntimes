package main

import (
	"context"
	"crypto/tls"
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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/runextensionapi"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	console "github.com/kruntimes/kruntimes/console/backend"
)

func main() {
	var (
		address         string
		assetsDirectory string
		certificateFile string
		privateKeyFile  string
		publicRead      bool
	)
	flag.StringVar(&address, "bind-address", ":8443", "The HTTPS address the Kruntimes Console binds to.")
	flag.StringVar(&assetsDirectory, "assets-dir", "/console/assets", "Directory containing the Console frontend assets.")
	flag.StringVar(&certificateFile, "tls-certificate-file", "", "PEM TLS certificate file for the Console. Required.")
	flag.StringVar(&privateKeyFile, "tls-private-key-file", "", "PEM TLS private key file for the Console. Required.")
	flag.BoolVar(&publicRead, "public-read", true, "Allow unauthenticated namespace and Run list requests using the Console ServiceAccount.")
	flag.Parse()
	if certificateFile == "" || privateKeyFile == "" {
		fmt.Fprintln(os.Stderr, "both --tls-certificate-file and --tls-private-key-file are required")
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
	logs, err := runextensionapi.NewForConfig(rest.AnonymousClientConfig(config))
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console aggregated Run log API: %v\n", err)
		os.Exit(1)
	}
	logClient, err := console.NewAggregatedRunLogClient(logs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console aggregated Run log API: %v\n", err)
		os.Exit(1)
	}
	extensionAPIProxy, err := console.NewRunExtensionAPIProxy(rest.AnonymousClientConfig(config))
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console Run extension API proxy: %v\n", err)
		os.Exit(1)
	}
	consoleServer := &console.Server{
		Clients:         factory,
		Authenticator:   authenticator,
		Logs:            logClient,
		RunExtensionAPI: extensionAPIProxy,
		Assets:          os.DirFS(assetsDirectory),
	}
	if publicRead {
		publicClient, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			fmt.Fprintf(os.Stderr, "configure Console public-read client: %v\n", err)
			os.Exit(1)
		}
		consoleServer.PublicReadClient = publicClient
	}
	tlsConfig, err := consoleTLSConfig(certificateFile, privateKeyFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Console TLS: %v\n", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              address,
		Handler:           consoleServer,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    consoleMaxHeaderBytes,
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

const consoleMaxHeaderBytes = 1 << 20

func consoleTLSConfig(certificateFile, privateKeyFile string) (*tls.Config, error) {
	certificate, err := tls.LoadX509KeyPair(certificateFile, privateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load TLS certificate: %w", err)
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	return config, nil
}
