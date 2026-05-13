package main

import (
	"context"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/airconduct/kruntime/api/v1alpha1"
	"github.com/airconduct/kruntime/internal/agent"
)

func main() {
	var (
		metricsAddr string
		workers     int
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":9090", "The address the metrics endpoint binds to.")
	flag.IntVar(&workers, "workers", 2, "Number of concurrent task execution workers.")
	klog.InitFlags(nil)
	flag.Parse()

	hostname := os.Getenv("HOSTNAME")
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	klog.Infof("Starting kruntime agent, hostname=%s", hostname)

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = 50
	restConfig.Burst = 100

	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		klog.Fatalf("Failed to create client: %v", err)
	}

	ctrl := &agent.Controller{
		Client:   c,
		Hostname: hostname,
		Executor: &agent.Executor{},
		Workers:  workers,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		klog.Infof("Metrics server listening on %s", metricsAddr)
		if err := http.ListenAndServe(metricsAddr, nil); err != nil {
			klog.Errorf("Metrics server: %v", err)
		}
	}()

	if err := ctrl.Run(ctx); err != nil {
		klog.Fatalf("Controller error: %v", err)
	}
	klog.Info("Agent shut down gracefully")
}
