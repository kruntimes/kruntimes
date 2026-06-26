package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
	artifactfs "github.com/kruntimes/kruntimes/internal/artifact/filesystem"
	artifacts3 "github.com/kruntimes/kruntimes/internal/artifact/s3"
)

func main() {
	var (
		namespace      string
		runUID         string
		driver         string
		filesystemRoot string
		volumeClaim    string
		s3Bucket       string
		s3Prefix       string
		s3Region       string
		s3Endpoint     string
		s3PathStyle    bool
		storeHash      string
		interval       time.Duration
	)
	flag.StringVar(&namespace, "run-namespace", "", "Namespace of the Run being cleaned.")
	flag.StringVar(&runUID, "run-uid", "", "UID of the Run being cleaned.")
	flag.StringVar(&driver, "driver", "", "Artifact store driver: filesystem or s3.")
	flag.StringVar(&filesystemRoot, "filesystem-root", "/var/lib/kruntimes/artifacts", "Mounted filesystem artifact root.")
	flag.StringVar(&volumeClaim, "filesystem-volume-claim", "", "PVC backing the filesystem artifact store.")
	flag.StringVar(&s3Bucket, "s3-bucket", "", "S3 bucket backing the artifact store.")
	flag.StringVar(&s3Prefix, "s3-prefix", "", "S3 object key prefix.")
	flag.StringVar(&s3Region, "s3-region", "", "S3 region override.")
	flag.StringVar(&s3Endpoint, "s3-endpoint", "", "S3-compatible endpoint override.")
	flag.BoolVar(&s3PathStyle, "s3-force-path-style", false, "Use path-style S3 addressing.")
	flag.StringVar(&storeHash, "store-hash", "", "Artifact store hash handled by this long-running worker.")
	flag.DurationVar(&interval, "interval", 10*time.Second, "Worker polling interval.")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	cfg := cleanupConfig{
		namespace: namespace, runUID: runUID, driver: v1alpha1.ArtifactDriver(driver),
		filesystemRoot: filesystemRoot, volumeClaim: volumeClaim,
		s3Bucket: s3Bucket, s3Prefix: s3Prefix, s3Region: s3Region,
		s3Endpoint: s3Endpoint, s3PathStyle: s3PathStyle,
		storeHash: storeHash, interval: interval,
	}
	if cfg.runUID == "" {
		if err := runWorker(ctx, cfg); err != nil {
			fmt.Fprintf(os.Stderr, "runtime maintainer failed: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := clean(ctx, cfg); err != nil {
		fmt.Fprintf(os.Stderr, "artifact cleanup failed: %v\n", err)
		os.Exit(1)
	}
}

type cleanupConfig struct {
	namespace      string
	runUID         string
	driver         v1alpha1.ArtifactDriver
	filesystemRoot string
	volumeClaim    string
	s3Bucket       string
	s3Prefix       string
	s3Region       string
	s3Endpoint     string
	s3PathStyle    bool
	storeHash      string
	interval       time.Duration
}

func clean(ctx context.Context, cfg cleanupConfig) error {
	if cfg.namespace == "" || cfg.runUID == "" {
		return fmt.Errorf("run namespace and UID are required")
	}

	var store artifact.RunStore
	var err error
	switch cfg.driver {
	case v1alpha1.ArtifactDriverFilesystem:
		if cfg.volumeClaim == "" {
			return fmt.Errorf("filesystem volume claim is required")
		}
		store, err = artifactfs.NewWithLimit(cfg.filesystemRoot, cfg.volumeClaim, artifact.DefaultMaxArtifactBytes)
	case v1alpha1.ArtifactDriverS3:
		store, err = artifacts3.New(ctx, artifacts3.Config{
			Bucket: cfg.s3Bucket, Prefix: cfg.s3Prefix, Region: cfg.s3Region,
			Endpoint: cfg.s3Endpoint, ForcePathStyle: cfg.s3PathStyle,
		})
	default:
		return fmt.Errorf("unsupported artifact store driver %q", cfg.driver)
	}
	if err != nil {
		return fmt.Errorf("configure artifact store: %w", err)
	}

	run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{
		Namespace: cfg.namespace,
		UID:       types.UID(cfg.runUID),
	}}
	if err := store.DeleteRun(ctx, run); err != nil {
		return err
	}
	return nil
}

func runWorker(ctx context.Context, cfg cleanupConfig) error {
	if cfg.namespace == "" {
		cfg.namespace = os.Getenv("POD_NAMESPACE")
	}
	if cfg.namespace == "" || cfg.storeHash == "" {
		return fmt.Errorf("run namespace and store hash are required")
	}
	store, err := storeForConfig(ctx, cfg)
	if err != nil {
		return err
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	k8sClient, err := client.New(config.GetConfigOrDie(), client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}
	if cfg.interval <= 0 {
		cfg.interval = 10 * time.Second
	}

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for {
		if err := cleanMatchingRuns(ctx, k8sClient, store, cfg.namespace, cfg.storeHash); err != nil {
			log.Printf("artifact cleanup pass failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func cleanMatchingRuns(ctx context.Context, k8sClient client.Client, store artifact.RunStore, namespace, storeHash string) error {
	var runs v1alpha1.RunList
	if err := k8sClient.List(ctx, &runs, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("list Runs: %w", err)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if run.DeletionTimestamp.IsZero() ||
			!controllerutil.ContainsFinalizer(run, artifact.RunArtifactFinalizer) ||
			run.Status.ArtifactStore == nil {
			continue
		}
		hash, err := artifact.StoreHash(run.Status.ArtifactStore)
		if err != nil || hash != storeHash {
			continue
		}
		if err := store.DeleteRun(ctx, run); err != nil {
			log.Printf("delete artifacts for Run %s/%s: %v", run.Namespace, run.Name, err)
			continue
		}
		base := run.DeepCopy()
		controllerutil.RemoveFinalizer(run, artifact.RunArtifactFinalizer)
		if err := k8sClient.Patch(ctx, run, client.MergeFrom(base)); err != nil {
			log.Printf("remove artifact cleanup finalizer for Run %s/%s: %v", run.Namespace, run.Name, err)
		}
	}
	return nil
}

func storeForConfig(ctx context.Context, cfg cleanupConfig) (artifact.RunStore, error) {
	switch cfg.driver {
	case v1alpha1.ArtifactDriverFilesystem:
		if cfg.volumeClaim == "" {
			return nil, fmt.Errorf("filesystem volume claim is required")
		}
		store, err := artifactfs.NewWithLimit(cfg.filesystemRoot, cfg.volumeClaim, artifact.DefaultMaxArtifactBytes)
		if err != nil {
			return nil, fmt.Errorf("configure artifact store: %w", err)
		}
		return store, nil
	case v1alpha1.ArtifactDriverS3:
		store, err := artifacts3.New(ctx, artifacts3.Config{
			Bucket: cfg.s3Bucket, Prefix: cfg.s3Prefix, Region: cfg.s3Region,
			Endpoint: cfg.s3Endpoint, ForcePathStyle: cfg.s3PathStyle,
		})
		if err != nil {
			return nil, fmt.Errorf("configure artifact store: %w", err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("unsupported artifact store driver %q", cfg.driver)
	}
}
