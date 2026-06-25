package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/controller"
	"github.com/kruntimes/kruntimes/internal/healthcheck"
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
		metricsAddr                string
		probeAddr                  string
		enableLeaderElection       bool
		staleThreshold             time.Duration
		defaultDaemonImage         string
		runtimedServiceAccountName string
		artifactFilesystemRoot     string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8083", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.DurationVar(&staleThreshold, "stale-threshold", 30*time.Second, "Threshold for marking a Run as stale when its assigned pod is unhealthy.")
	flag.StringVar(&defaultDaemonImage, "default-daemon-image", "", "Default runtimed daemon image injected into Runtime Pods.")
	flag.StringVar(&runtimedServiceAccountName, "runtimed-service-account-name", "", "ServiceAccount name injected into Runtime Pods for the runtimed sidecar.")
	flag.StringVar(&artifactFilesystemRoot, "artifact-filesystem-root", "/var/lib/kruntimes/artifacts", "Mounted filesystem artifact store root used for artifact finalizer cleanup.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	skipNameValidation := true
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "kruntimes-controller.kruntimes.com",
		Controller: config.Controller{
			SkipNameValidation: &skipNameValidation,
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to register health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck(
		"kubernetes-api",
		healthcheck.KubernetesAPI(mgr.GetAPIReader(), &v1alpha1.RuntimeList{}),
	); err != nil {
		setupLog.Error(err, "unable to register readiness check")
		os.Exit(1)
	}

	reconciler := &controller.RuntimeReconciler{
		Client:                     mgr.GetClient(),
		Log:                        ctrl.Log.WithName("controllers").WithName("Runtime"),
		Scheme:                     mgr.GetScheme(),
		DefaultDaemonImage:         defaultDaemonImage,
		RuntimedServiceAccountName: runtimedServiceAccountName,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Runtime")
		os.Exit(1)
	}

	artifactCleanup := &controller.ArtifactCleanupReconciler{
		Client:              mgr.GetClient(),
		Log:                 ctrl.Log.WithName("controllers").WithName("ArtifactCleanup"),
		Recorder:            mgr.GetEventRecorderFor("artifact-cleanup"),
		FilesystemStoreRoot: artifactFilesystemRoot,
	}
	if err := artifactCleanup.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ArtifactCleanup")
		os.Exit(1)
	}

	wfReconciler := &controller.WorkflowReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Workflow"),
		Scheme: mgr.GetScheme(),
	}
	if err := wfReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Workflow")
		os.Exit(1)
	}

	staleReaper := &controller.StaleRunReaper{
		Client:             mgr.GetClient(),
		Log:                ctrl.Log.WithName("controllers").WithName("StaleReaper"),
		Recorder:           mgr.GetEventRecorderFor("stale-reaper"),
		StalenessThreshold: staleThreshold,
	}
	if err := staleReaper.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "StaleReaper")
		os.Exit(1)
	}

	completedRunGC := &controller.CompletedRunGC{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("CompletedRunGC"),
	}
	if err := completedRunGC.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "CompletedRunGC")
		os.Exit(1)
	}

	setupLog.Info("starting controller manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
