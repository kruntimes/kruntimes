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
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/gateway"
	"github.com/kruntimes/kruntimes/internal/logapi"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var address, certificateFile, privateKeyFile, metricsAddress, probeAddress string
	flag.StringVar(&address, "bind-address", ":8445", "HTTPS address for the aggregated Run log API.")
	flag.StringVar(&certificateFile, "tls-certificate-file", "", "PEM TLS certificate file for the aggregated Run log API.")
	flag.StringVar(&privateKeyFile, "tls-private-key-file", "", "PEM TLS private key file for the aggregated Run log API.")
	flag.StringVar(&metricsAddress, "metrics-bind-address", ":8085", "Metrics bind address.")
	flag.StringVar(&probeAddress, "health-probe-bind-address", ":8086", "Health probe bind address.")
	flag.Parse()
	if certificateFile == "" || privateKeyFile == "" {
		fmt.Fprintln(os.Stderr, "both TLS file flags are required")
		os.Exit(2)
	}
	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	config := ctrl.GetConfigOrDie()
	manager, err := ctrl.NewManager(config, ctrl.Options{Scheme: scheme, Metrics: metricsserver.Options{BindAddress: metricsAddress}, HealthProbeBindAddress: probeAddress})
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to create Run log API manager")
		os.Exit(1)
	}
	if err := manager.AddHealthzCheck("ping", healthz.Ping); err != nil {
		panic(err)
	}
	if err := manager.AddReadyzCheck("ping", healthz.Ping); err != nil {
		panic(err)
	}
	kube := kubernetes.NewForConfigOrDie(config)
	requestHeaderCA, allowedNames, usernameHeaders, groupHeaders, err := logapi.RequestHeaderConfig(context.Background(), kube)
	if err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to configure Kubernetes aggregation authentication")
		os.Exit(1)
	}
	if err := manager.Add(&logapi.Server{Runs: manager.GetCache(), Kubernetes: kube, PodLogs: gateway.KubernetesPodLogReader{Client: kube.CoreV1()}, Address: address, TLSCertificateFile: certificateFile, TLSPrivateKeyFile: privateKeyFile, RequestHeaderCA: requestHeaderCA, AllowedClientNames: allowedNames, UsernameHeaders: usernameHeaders, GroupHeaders: groupHeaders, AuthorizationTimeout: 2 * time.Second}); err != nil {
		ctrl.Log.WithName("setup").Error(err, "unable to add Run log API server")
		os.Exit(1)
	}
	if err := manager.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.WithName("setup").Error(err, "Run log API stopped")
		os.Exit(1)
	}
}
