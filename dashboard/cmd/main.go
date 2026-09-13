package main

import (
	"context"
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

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/dashboard/backend"
	"github.com/kruntimes/kruntimes/internal/logapi"
)

func main() {
	var (
		address         string
		assetsDirectory string
		certificateFile string
		privateKeyFile  string
		publicRead      bool
	)
	flag.StringVar(&address, "bind-address", ":8443", "The HTTPS address the Dashboard binds to.")
	flag.StringVar(&assetsDirectory, "assets-dir", "/dashboard/assets", "Directory containing the Dashboard frontend assets.")
	flag.StringVar(&certificateFile, "tls-certificate-file", "", "PEM TLS certificate file for the Dashboard. Required.")
	flag.StringVar(&privateKeyFile, "tls-private-key-file", "", "PEM TLS private key file for the Dashboard. Required.")
	flag.BoolVar(&publicRead, "public-read", true, "Allow unauthenticated namespace and Run list requests using the Dashboard ServiceAccount.")
	flag.Parse()
	if certificateFile == "" || privateKeyFile == "" {
		fmt.Fprintln(os.Stderr, "both --tls-certificate-file and --tls-private-key-file are required")
		os.Exit(2)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	config := ctrl.GetConfigOrDie()
	factory, err := dashboard.NewRequestClientFactory(config, scheme)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Dashboard Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	authenticator, err := dashboard.NewTokenReviewAuthenticator(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Dashboard TokenReview client: %v\n", err)
		os.Exit(1)
	}
	logs, err := logapi.NewClient(rest.AnonymousClientConfig(config))
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Dashboard aggregated Run log API: %v\n", err)
		os.Exit(1)
	}
	logClient, err := dashboard.NewAggregatedRunLogClient(logs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "configure Dashboard aggregated Run log API: %v\n", err)
		os.Exit(1)
	}
	dashboardServer := &dashboard.Server{Clients: factory, Authenticator: authenticator, Logs: logClient, Assets: os.DirFS(assetsDirectory)}
	if publicRead {
		publicClient, err := client.New(config, client.Options{Scheme: scheme})
		if err != nil {
			fmt.Fprintf(os.Stderr, "configure Dashboard public-read client: %v\n", err)
			os.Exit(1)
		}
		dashboardServer.PublicReadClient = publicClient
	}
	server := &http.Server{
		Addr:              address,
		Handler:           dashboardServer,
		ReadHeaderTimeout: 5 * time.Second,
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
		fmt.Fprintf(os.Stderr, "serve Dashboard: %v\n", err)
		os.Exit(1)
	}
}
