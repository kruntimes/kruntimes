package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := clean(ctx, cleanupConfig{
		namespace: namespace, runUID: runUID, driver: v1alpha1.ArtifactDriver(driver),
		filesystemRoot: filesystemRoot, volumeClaim: volumeClaim,
		s3Bucket: s3Bucket, s3Prefix: s3Prefix, s3Region: s3Region,
		s3Endpoint: s3Endpoint, s3PathStyle: s3PathStyle,
	}); err != nil {
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
