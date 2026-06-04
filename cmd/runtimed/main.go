package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimed"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr     string
		probeAddr       string
		runtimeEndpoint string
		statusAddr      string
		workers         int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":9090", "Metrics endpoint address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":9094", "Health probe endpoint.")
	flag.StringVar(&statusAddr, "status-addr", ":9093", "gRPC address for the status proxy (for krt logs).")
	flag.StringVar(&runtimeEndpoint, "runtime-endpoint", "localhost:9091", "gRPC endpoint of the runtime server.")
	flag.IntVar(&workers, "workers", int(v1alpha1.RuntimeDefaultRunsCapacity), "Max concurrent run executions.")
	klog.InitFlags(nil)
	flag.Parse()

	hostname := os.Getenv("HOSTNAME")
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start status proxy for krt logs.
	go func() {
		if err := runtimed.StartStatusProxy(ctx, runtimeEndpoint, statusAddr); err != nil {
			klog.Errorf("Status proxy: %v", err)
		}
	}()

	runtimedCtrl := &runtimed.Controller{
		Client:          mgr.GetClient(),
		PodReader:       mgr.GetAPIReader(),
		Log:             ctrl.Log.WithName("controllers").WithName("Runtimed"),
		Hostname:        hostname,
		RuntimeEndpoint: runtimeEndpoint,
		Workers:         workers,
		Recorder:        mgr.GetEventRecorderFor("runtimed"),
	}

	if err := runtimedCtrl.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Runtimed")
		os.Exit(1)
	}

	setupLog.Info("starting runtimed", "hostname", hostname, "runtime", runtimeEndpoint)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
	klog.Info("Runtimed shut down gracefully")
}
