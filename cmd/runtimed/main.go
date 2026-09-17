package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
	artifactfs "github.com/kruntimes/kruntimes/internal/artifact/filesystem"
	artifacts3 "github.com/kruntimes/kruntimes/internal/artifact/s3"
	"github.com/kruntimes/kruntimes/internal/healthcheck"
	"github.com/kruntimes/kruntimes/internal/runtimed"
	"github.com/kruntimes/kruntimes/internal/toolcache"
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
	if filepath.Base(os.Args[0]) == "kruntime-cache" {
		if err := toolcache.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "kruntime-cache:", err)
			os.Exit(1)
		}
		return
	}
	var (
		metricsAddr         string
		probeAddr           string
		runtimeEndpoint     string
		statusAddr          string
		workers             int
		runtimeName         string
		gatewayURL          string
		gatewayCAFile       string
		artifactStoreDriver string
		artifactStoreRoot   string
		artifactVolumeClaim string
		artifactS3Bucket    string
		artifactS3Prefix    string
		artifactS3Region    string
		artifactS3Endpoint  string
		artifactS3PathStyle bool
		artifactS3Secret    string
		artifactS3PartSize  int64
		artifactS3Workers   int
		maxArtifactBytes    int64
		maxArtifactsBytes   int64
		sessionMaxQueueSize int
		sessionMaxTimeout   time.Duration
		sessionCloseTimeout time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":9090", "Metrics endpoint address.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":9094", "Health probe endpoint.")
	flag.StringVar(&statusAddr, "status-addr", ":9093", "gRPC address for the status proxy (for krt logs).")
	flag.StringVar(&runtimeEndpoint, "runtime-endpoint", "localhost:9091", "gRPC endpoint of the runtime server.")
	flag.StringVar(&runtimeName, "runtime-name", "", "Runtime resource name served by this pod.")
	flag.StringVar(&gatewayURL, "gateway-url", "", "Cluster-local Runtime gateway base URL for Session Run endpoints.")
	flag.StringVar(&gatewayCAFile, "gateway-ca-file", "", "PEM trust bundle file for HTTPS Session Run endpoints.")
	flag.IntVar(&workers, "workers", int(v1alpha1.RuntimeDefaultRunsCapacity), "Max concurrent run executions.")
	flag.StringVar(&artifactStoreDriver, "artifact-store-driver", "", "Artifact store driver: filesystem or s3.")
	flag.StringVar(&artifactStoreRoot, "artifact-store-root", "", "Filesystem artifact store root. Empty disables artifact collection.")
	flag.StringVar(&artifactVolumeClaim, "artifact-volume-claim", "", "PVC name backing the filesystem artifact store.")
	flag.StringVar(&artifactS3Bucket, "artifact-s3-bucket", "", "S3 bucket backing the artifact store.")
	flag.StringVar(&artifactS3Prefix, "artifact-s3-prefix", "", "S3 object key prefix.")
	flag.StringVar(&artifactS3Region, "artifact-s3-region", "", "S3 region override.")
	flag.StringVar(&artifactS3Endpoint, "artifact-s3-endpoint", "", "S3-compatible endpoint override.")
	flag.BoolVar(&artifactS3PathStyle, "artifact-s3-force-path-style", false, "Use path-style S3 addressing.")
	flag.StringVar(&artifactS3Secret, "artifact-s3-credentials-secret-name", "", "Secret containing S3 credentials (recorded for centralized cleanup).")
	flag.Int64Var(&artifactS3PartSize, "artifact-s3-upload-part-size", 0, "S3 multipart upload part size.")
	flag.IntVar(&artifactS3Workers, "artifact-s3-upload-concurrency", 0, "S3 multipart upload concurrency.")
	flag.Int64Var(&maxArtifactBytes, "max-artifact-bytes", artifact.DefaultMaxArtifactBytes, "Maximum bytes allowed for one artifact.")
	flag.Int64Var(&maxArtifactsBytes, "max-artifacts-bytes", artifact.DefaultMaxArtifactsBytes, "Maximum total artifact bytes allowed per Run.")
	flag.IntVar(&sessionMaxQueueSize, "session-max-queue-size", 0, "Maximum queued mutations per Session Run. Non-positive uses the default.")
	flag.DurationVar(&sessionMaxTimeout, "session-max-operation-timeout", 0, "Maximum duration of one Session operation. Non-positive uses the default.")
	flag.DurationVar(&sessionCloseTimeout, "session-close-timeout", 0, "Maximum time to wait for a Session Runtime to close. Non-positive uses the default.")
	klog.InitFlags(nil)
	flag.Parse()
	var gatewayCABundle []byte
	if gatewayCAFile != "" {
		var err error
		gatewayCABundle, err = os.ReadFile(gatewayCAFile)
		if err != nil {
			setupLog.Error(err, "read gateway CA file", "path", gatewayCAFile)
			os.Exit(1)
		}
	}

	podName := os.Getenv("POD_NAME")
	if podName == "" {
		setupLog.Error(errors.New("POD_NAME is required"), "unable to identify runtimed Pod")
		os.Exit(1)
	}
	runtimeNamespace := os.Getenv("POD_NAMESPACE")
	if runtimeNamespace == "" {
		runtimeNamespace = "default"
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{DefaultNamespaces: map[string]cache.Config{
			runtimeNamespace: {},
		}},
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &v1alpha1.Run{}, "spec.runtime", func(object client.Object) []string {
		run, ok := object.(*v1alpha1.Run)
		if !ok || run.Spec.Runtime == "" {
			return nil
		}
		return []string{run.Spec.Runtime}
	}); err != nil {
		setupLog.Error(err, "unable to index Runs by Runtime")
		os.Exit(1)
	}
	runtimeHealthConn, err := grpc.NewClient(
		runtimeEndpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		setupLog.Error(err, "unable to create runtime health client")
		os.Exit(1)
	}
	defer runtimeHealthConn.Close()
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to register health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck(
		"kubernetes-api",
		healthcheck.KubernetesAPI(
			mgr.GetAPIReader(),
			&v1alpha1.RunList{},
			client.InNamespace(runtimeNamespace),
		),
	); err != nil {
		setupLog.Error(err, "unable to register Kubernetes readiness check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck(
		"runtime",
		healthcheck.Runtime(pb.NewRuntimeClient(runtimeHealthConn), 2*time.Second),
	); err != nil {
		setupLog.Error(err, "unable to register runtime readiness check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck(
		"runtime-proxy",
		healthcheck.TCPListener(statusAddr, 2*time.Second),
	); err != nil {
		setupLog.Error(err, "unable to register runtime proxy readiness check")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var artifactStore artifact.Store
	var artifactStoreSpec *v1alpha1.RuntimeArtifactStoreSpec
	switch artifactStoreDriver {
	case "", string(v1alpha1.ArtifactDriverFilesystem):
		if artifactStoreRoot != "" || artifactVolumeClaim != "" {
			artifactStore, err = artifactfs.NewWithLimit(artifactStoreRoot, artifactVolumeClaim, maxArtifactBytes)
			artifactStoreSpec = &v1alpha1.RuntimeArtifactStoreSpec{
				Driver: v1alpha1.ArtifactDriverFilesystem,
				Filesystem: &v1alpha1.FilesystemArtifactStoreSpec{
					VolumeClaimName: artifactVolumeClaim,
				},
			}
		}
	case string(v1alpha1.ArtifactDriverS3):
		artifactStore, err = artifacts3.New(ctx, artifacts3.Config{
			Bucket:            artifactS3Bucket,
			Prefix:            artifactS3Prefix,
			Region:            artifactS3Region,
			Endpoint:          artifactS3Endpoint,
			ForcePathStyle:    artifactS3PathStyle,
			UploadPartSize:    artifactS3PartSize,
			UploadConcurrency: artifactS3Workers,
		})
		artifactStoreSpec = &v1alpha1.RuntimeArtifactStoreSpec{
			Driver: v1alpha1.ArtifactDriverS3,
			S3: &v1alpha1.S3ArtifactStoreSpec{
				Bucket:                artifactS3Bucket,
				Prefix:                artifactS3Prefix,
				Region:                artifactS3Region,
				Endpoint:              artifactS3Endpoint,
				ForcePathStyle:        artifactS3PathStyle,
				CredentialsSecretName: artifactS3Secret,
				UploadPartSize:        artifactS3PartSize,
				UploadConcurrency:     int32(artifactS3Workers),
			},
		}
	default:
		err = fmt.Errorf("unsupported artifact store driver %q", artifactStoreDriver)
	}
	if err != nil {
		setupLog.Error(err, "unable to configure artifact store")
		os.Exit(1)
	}
	if artifactStore != nil && runtimeName == "" {
		setupLog.Error(errors.New("runtime name is required"), "unable to configure artifact service")
		os.Exit(1)
	}

	sessionOperations := runtimed.NewSessionOperationQueue(sessionMaxQueueSize, sessionMaxTimeout)

	runtimedCtrl := &runtimed.Controller{
		Client:              mgr.GetClient(),
		PodReader:           mgr.GetAPIReader(),
		RunReader:           mgr.GetAPIReader(),
		Log:                 ctrl.Log.WithName("controllers").WithName("Runtimed"),
		PodName:             podName,
		RuntimeName:         runtimeName,
		RuntimeNamespace:    runtimeNamespace,
		RuntimeEndpoint:     runtimeEndpoint,
		Workers:             workers,
		ArtifactStore:       artifactStore,
		ArtifactStoreSpec:   artifactStoreSpec,
		MaxArtifactBytes:    maxArtifactBytes,
		MaxArtifactsBytes:   maxArtifactsBytes,
		SessionOperations:   sessionOperations,
		SessionCloseTimeout: sessionCloseTimeout,
		GatewayURL:          gatewayURL,
		GatewayCABundle:     gatewayCABundle,
		Recorder:            mgr.GetEventRecorderFor("runtimed"),
	}

	// Bind the gRPC proxy before the manager can claim a Run. A Session/Function
	// Run becomes Ready after its local registration, so starting this server in
	// the background would otherwise expose a short window where Gateway routes
	// a ready Run to an unbound :9093 endpoint.
	proxyReady := make(chan error, 1)
	go func() {
		if err := runtimed.StartRuntimeProxyServer(
			ctx,
			runtimeEndpoint,
			statusAddr,
			mgr.GetAPIReader(),
			mgr.GetCache(),
			artifactStore,
			sessionOperations,
			runtimedCtrl,
			runtimeNamespace,
			runtimeName,
			podName,
			proxyReady,
		); err != nil {
			klog.Errorf("Status proxy: %v", err)
		}
	}()
	if err := <-proxyReady; err != nil {
		setupLog.Error(err, "unable to start runtime proxy")
		os.Exit(1)
	}

	if err := runtimedCtrl.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Runtimed")
		os.Exit(1)
	}
	workspaceCleanupCtrl := &runtimed.PersistentWorkspaceCleanupReconciler{
		Client:    mgr.GetClient(),
		PodReader: mgr.GetAPIReader(),
		PodName:   podName,
		Namespace: runtimeNamespace,
	}
	if err := workspaceCleanupCtrl.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PersistentWorkspaceCleanup")
		os.Exit(1)
	}

	setupLog.Info("starting runtimed", "pod", podName, "runtime", runtimeEndpoint)
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
	klog.Info("Runtimed shut down gracefully")
}
