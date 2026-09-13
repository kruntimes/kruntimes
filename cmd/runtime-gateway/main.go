package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/gateway"
)

const runtimeIndexField = "spec.runtime"

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr                string
		probeAddr                  string
		httpAddr                   string
		httpsAddr                  string
		tlsCertificateFile         string
		tlsPrivateKeyFile          string
		tlsClientCAFile            string
		authorizationCacheTTL      time.Duration
		authorizationCacheCapacity int
		maxConcurrentRequests      int
		maxRequestBodyBytes        int64
		maxResponseBodyBytes       int64
		maxHeaderBytes             int
	)
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8085", "The address the metrics endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8086", "The address the health probe endpoint binds to.")
	flag.StringVar(&httpAddr, "http-bind-address", ":8084", "The address the Runtime gateway HTTP API binds to.")
	flag.StringVar(&httpsAddr, "https-bind-address", "", "The address the Runtime gateway HTTPS API binds to. Empty disables HTTPS.")
	flag.StringVar(&tlsCertificateFile, "tls-certificate-file", "", "PEM TLS certificate file for the Runtime gateway HTTP API. Both TLS file flags are required to enable HTTPS.")
	flag.StringVar(&tlsPrivateKeyFile, "tls-private-key-file", "", "PEM TLS private key file for the Runtime gateway HTTP API. Both TLS file flags are required to enable HTTPS.")
	flag.StringVar(&tlsClientCAFile, "tls-client-ca-file", "", "PEM client CA bundle for optional mTLS Kubernetes client-certificate authentication. Requires HTTPS.")
	defaultAuthorizationCache := gateway.DefaultAuthorizationCacheOptions()
	flag.DurationVar(&authorizationCacheTTL, "authorization-cache-ttl", defaultAuthorizationCache.TTL, "How long successful bearer-token authorization decisions remain cached; zero disables caching.")
	flag.IntVar(&authorizationCacheCapacity, "authorization-cache-capacity", defaultAuthorizationCache.Capacity, "Maximum successful bearer-token authorization decisions retained; zero disables caching.")
	flag.IntVar(&maxConcurrentRequests, "max-concurrent-requests", gateway.DefaultMaxConcurrentRequests, "Maximum concurrent HTTP requests handled by one gateway Pod.")
	flag.Int64Var(&maxRequestBodyBytes, "max-request-body-bytes", gateway.DefaultMaxRequestBodyBytes, "Maximum gateway JSON request body size in bytes.")
	flag.Int64Var(&maxResponseBodyBytes, "max-response-body-bytes", gateway.DefaultMaxResponseBodyBytes, "Maximum gateway-generated JSON response size in bytes.")
	flag.IntVar(&maxHeaderBytes, "max-header-bytes", gateway.DefaultMaxHeaderBytes, "Maximum HTTP request header size in bytes.")
	flag.Parse()
	if maxRequestBodyBytes <= 0 || maxResponseBodyBytes <= 0 || maxHeaderBytes <= 0 {
		fmt.Fprintln(os.Stderr, "gateway request body, response body, and header limits must be positive")
		os.Exit(2)
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	config := ctrl.GetConfigOrDie()
	manager, err := ctrl.NewManager(config, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to create Runtime gateway manager")
		os.Exit(1)
	}
	if err := manager.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.Run{}, runtimeIndexField, func(object client.Object) []string {
		run, ok := object.(*v1alpha1.Run)
		if !ok || run.Spec.Runtime == "" {
			return nil
		}
		return []string{run.Spec.Runtime}
	}); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to index Runs by Runtime")
		os.Exit(1)
	}
	if err := manager.AddHealthzCheck("ping", healthz.Ping); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to register health check")
		os.Exit(1)
	}
	if err := manager.AddReadyzCheck("ping", healthz.Ping); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to register readiness check")
		os.Exit(1)
	}
	kubernetesClient := kubernetes.NewForConfigOrDie(config)
	if err := manager.Add(&gateway.Server{
		Runs: manager.GetCache(),
		Authorizer: gateway.NewCachingAuthorizer(
			gateway.KubernetesAuthorizer{Client: kubernetesClient},
			gateway.AuthorizationCacheOptions{Capacity: authorizationCacheCapacity, TTL: authorizationCacheTTL},
		),
		Dialer:                gateway.GRPCDialer{},
		FunctionDialer:        gateway.GRPCDialer{},
		HTTPAddress:           httpAddr,
		HTTPSAddress:          httpsAddr,
		TLSCertificateFile:    tlsCertificateFile,
		TLSPrivateKeyFile:     tlsPrivateKeyFile,
		TLSClientCAFile:       tlsClientCAFile,
		MaxConcurrentRequests: maxConcurrentRequests,
		MaxRequestBodyBytes:   maxRequestBodyBytes,
		MaxResponseBodyBytes:  maxResponseBodyBytes,
		MaxHeaderBytes:        maxHeaderBytes,
	}); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to add Runtime gateway server")
		os.Exit(1)
	}

	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.WithName("setup").Error(err, "Runtime gateway stopped")
		os.Exit(1)
	}
}
