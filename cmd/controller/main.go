package main

import (
	"flag"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
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
		metricsAddr                  string
		probeAddr                    string
		enableLeaderElection         bool
		staleThreshold               time.Duration
		defaultDaemonImage           string
		runtimedServiceAccountName   string
		runtimeMaintainerImage       string
		runtimeMaintainerPullSecrets string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8083", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.DurationVar(&staleThreshold, "stale-threshold", 30*time.Second, "Threshold for marking a Run as stale when its assigned pod is unhealthy.")
	flag.StringVar(&defaultDaemonImage, "default-daemon-image", "", "Default runtimed daemon image injected into Runtime Pods.")
	flag.StringVar(&runtimedServiceAccountName, "runtimed-service-account-name", "", "ServiceAccount name injected into Runtime Pods for the runtimed sidecar.")
	flag.StringVar(&runtimeMaintainerImage, "runtime-maintainer-image", "", "Image containing the long-running runtime maintainer.")
	flag.StringVar(&runtimeMaintainerPullSecrets, "runtime-maintainer-image-pull-secrets", "", "Comma-separated image pull Secret names for runtime maintainers.")
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
		Client:           mgr.GetClient(),
		Log:              ctrl.Log.WithName("controllers").WithName("ArtifactCleanup"),
		Recorder:         mgr.GetEventRecorderFor("artifact-cleanup"),
		MaintainerImage:  runtimeMaintainerImage,
		ImagePullSecrets: localObjectReferences(runtimeMaintainerPullSecrets),
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

	persistentWorkspaceReconciler := &controller.PersistentWorkspaceReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("PersistentWorkspace"),
		Scheme: mgr.GetScheme(),
	}
	if err := persistentWorkspaceReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PersistentWorkspace")
		os.Exit(1)
	}

	actionReconciler := &controller.ActionReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("Action"),
		Scheme: mgr.GetScheme(),
	}
	if err := actionReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Action")
		os.Exit(1)
	}

	workflowRunReconciler := &controller.WorkflowRunReconciler{
		Client: mgr.GetClient(),
		Log:    ctrl.Log.WithName("controllers").WithName("WorkflowRun"),
		Scheme: mgr.GetScheme(),
	}
	if err := workflowRunReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "WorkflowRun")
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

func localObjectReferences(csv string) []corev1.LocalObjectReference {
	var refs []corev1.LocalObjectReference
	for _, value := range strings.Split(csv, ",") {
		if name := strings.TrimSpace(value); name != "" {
			refs = append(refs, corev1.LocalObjectReference{Name: name})
		}
	}
	return refs
}
