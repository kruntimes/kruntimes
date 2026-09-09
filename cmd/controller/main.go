package main

import (
	"flag"
	"fmt"
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
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	workspaceadmission "github.com/kruntimes/kruntimes/internal/admission"
	"github.com/kruntimes/kruntimes/internal/controller"
	"github.com/kruntimes/kruntimes/internal/healthcheck"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const maxGatewayCABundleBytes = 64 << 10

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr                              string
		probeAddr                                string
		webhookPort                              int
		webhookCertDir                           string
		webhookControllerServiceAccountName      string
		webhookControllerServiceAccountNamespace string
		enableLeaderElection                     bool
		staleThreshold                           time.Duration
		defaultDaemonImage                       string
		runtimedServiceAccountName               string
		runtimeMaintainerImage                   string
		runtimeMaintainerImagePullPolicy         string
		runtimeMaintainerPullSecrets             string
		gatewayNamespace                         string
		gatewaySelectorLabels                    string
		gatewayURL                               string
		gatewayCAFile                            string
		sessionMaxQueueSize                      int
		sessionMaxOperationTimeout               time.Duration
		sessionCloseTimeout                      time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8082", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8083", "The address the probe endpoint binds to.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The HTTPS port for admission webhooks.")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs", "Directory containing tls.crt and tls.key for admission webhooks.")
	flag.StringVar(&webhookControllerServiceAccountName, "webhook-controller-service-account-name", "", "ServiceAccount name trusted to create verified WorkflowRun child Runs with workspaces.")
	flag.StringVar(&webhookControllerServiceAccountNamespace, "webhook-controller-service-account-namespace", "", "Namespace of the ServiceAccount trusted to create verified WorkflowRun child Runs with workspaces.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.DurationVar(&staleThreshold, "stale-threshold", 30*time.Second, "Threshold for marking a Run as stale when its assigned pod is unhealthy.")
	flag.StringVar(&defaultDaemonImage, "default-daemon-image", "", "Default runtimed daemon image injected into Runtime Pods.")
	flag.StringVar(&runtimedServiceAccountName, "runtimed-service-account-name", "", "ServiceAccount name injected into Runtime Pods for the runtimed sidecar.")
	flag.StringVar(&runtimeMaintainerImage, "runtime-maintainer-image", "", "Image containing the long-running runtime maintainer.")
	flag.StringVar(&runtimeMaintainerImagePullPolicy, "runtime-maintainer-image-pull-policy", "IfNotPresent", "Image pull policy for long-running runtime maintainers.")
	flag.StringVar(&runtimeMaintainerPullSecrets, "runtime-maintainer-image-pull-secrets", "", "Comma-separated image pull Secret names for runtime maintainers.")
	flag.StringVar(&gatewayNamespace, "gateway-namespace", "", "Namespace of the enabled Runtime gateway. Empty keeps Runtime Pod ingress denied.")
	flag.StringVar(&gatewaySelectorLabels, "gateway-selector-labels", "", "Comma-separated key=value labels selecting Runtime gateway Pods.")
	flag.StringVar(&gatewayURL, "gateway-url", "", "Cluster-local Runtime gateway base URL written to ready Session Run endpoints.")
	flag.StringVar(&gatewayCAFile, "gateway-ca-file", "", "PEM trust bundle file for HTTPS Session Run endpoints.")
	flag.IntVar(&sessionMaxQueueSize, "session-max-queue-size", 0, "Maximum queued mutations per Session Run. A non-positive value uses runtimed defaults.")
	flag.DurationVar(&sessionMaxOperationTimeout, "session-max-operation-timeout", 0, "Maximum duration of one Session operation. A non-positive value uses runtimed defaults.")
	flag.DurationVar(&sessionCloseTimeout, "session-close-timeout", 0, "Maximum time runtimed waits for a Session Runtime to close. A non-positive value uses runtimed defaults.")
	flag.Parse()
	var gatewayCABundle []byte
	if gatewayCAFile != "" {
		var err error
		gatewayCABundle, err = os.ReadFile(gatewayCAFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read gateway CA file: %v\n", err)
			os.Exit(1)
		}
		if len(gatewayCABundle) > maxGatewayCABundleBytes {
			fmt.Fprintf(os.Stderr, "gateway CA file exceeds %d bytes\n", maxGatewayCABundleBytes)
			os.Exit(1)
		}
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	if webhookControllerServiceAccountName == "" || webhookControllerServiceAccountNamespace == "" {
		setupLog.Error(fmt.Errorf("both controller service account name and namespace are required"), "admission webhook configuration is incomplete")
		os.Exit(1)
	}

	skipNameValidation := true
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		WebhookServer: webhookserver.NewServer(webhookserver.Options{
			Port:    webhookPort,
			CertDir: webhookCertDir,
		}),
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "kruntimes-controller.kruntimes.com",
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
	if err := mgr.AddReadyzCheck("admission-webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to register readiness check", "check", "admission-webhook")
		os.Exit(1)
	}
	workspaceadmission.RegisterRunAdmissionValidator(
		mgr.GetWebhookServer(),
		mgr.GetAPIReader(),
		workspaceadmission.KubernetesSubjectAccessReviewer{Client: mgr.GetClient()},
		workspaceadmission.ServiceAccountIdentity{
			Name:      webhookControllerServiceAccountName,
			Namespace: webhookControllerServiceAccountNamespace,
		},
		mgr.GetScheme(),
	)

	reconciler := &controller.RuntimeReconciler{
		Client:                     mgr.GetClient(),
		Log:                        ctrl.Log.WithName("controllers").WithName("Runtime"),
		Scheme:                     mgr.GetScheme(),
		DefaultDaemonImage:         defaultDaemonImage,
		RuntimedServiceAccountName: runtimedServiceAccountName,
		GatewayNamespace:           gatewayNamespace,
		GatewaySelectorLabels:      parseLabels(gatewaySelectorLabels),
		GatewayURL:                 gatewayURL,
		GatewayCABundle:            gatewayCABundle,
		SessionMaxQueueSize:        sessionMaxQueueSize,
		SessionMaxOperationTimeout: sessionMaxOperationTimeout,
		SessionCloseTimeout:        sessionCloseTimeout,
	}
	if err := reconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Runtime")
		os.Exit(1)
	}

	artifactCleanup := &controller.ArtifactCleanupReconciler{
		Client:                    mgr.GetClient(),
		Log:                       ctrl.Log.WithName("controllers").WithName("ArtifactCleanup"),
		Recorder:                  mgr.GetEventRecorderFor("artifact-cleanup"),
		MaintainerImage:           runtimeMaintainerImage,
		MaintainerImagePullPolicy: corev1.PullPolicy(runtimeMaintainerImagePullPolicy),
		ImagePullSecrets:          localObjectReferences(runtimeMaintainerPullSecrets),
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

func parseLabels(value string) map[string]string {
	if value == "" {
		return nil
	}
	labels := make(map[string]string)
	for _, item := range strings.Split(value, ",") {
		key, labelValue, ok := strings.Cut(item, "=")
		if !ok || key == "" || labelValue == "" {
			continue
		}
		labels[key] = labelValue
	}
	return labels
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
