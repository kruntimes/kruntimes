package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
	artifactfs "github.com/kruntimes/kruntimes/internal/artifact/filesystem"
	artifacts3 "github.com/kruntimes/kruntimes/internal/artifact/s3"
)

const (
	defaultArtifactCleanupFilesystemRoot = "/var/lib/kruntimes/artifacts"
	artifactCleanupRetry                 = 30 * time.Second
)

// ArtifactCleanupReconciler owns artifact finalizer removal independently of Runtime Pods.
type ArtifactCleanupReconciler struct {
	client.Client
	Log                 logr.Logger
	Recorder            record.EventRecorder
	FilesystemStoreRoot string
	MaxArtifactBytes    int64
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=runs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// SetupWithManager registers the Run watch.
func (r *ArtifactCleanupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Run{}).
		Complete(r)
}

// Reconcile snapshots store configuration and removes external artifacts for deleting Runs.
func (r *ArtifactCleanupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var run v1alpha1.Run
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !controllerutil.ContainsFinalizer(&run, artifact.RunArtifactFinalizer) {
		return ctrl.Result{}, nil
	}
	if run.Status.ArtifactStore == nil {
		return r.snapshotArtifactStore(ctx, &run)
	}
	if run.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	store, err := r.storeForRun(ctx, &run)
	if err != nil {
		r.record(&run, corev1.EventTypeWarning, "ArtifactCleanupRetry", "Artifact cleanup configuration is not ready: %v", err)
		return ctrl.Result{RequeueAfter: artifactCleanupRetry}, nil
	}

	if err := store.DeleteRun(ctx, &run); err != nil {
		r.record(&run, corev1.EventTypeWarning, "ArtifactCleanupRetry", "Artifact cleanup failed: %v", err)
		return ctrl.Result{RequeueAfter: artifactCleanupRetry}, nil
	}
	base := run.DeepCopy()
	controllerutil.RemoveFinalizer(&run, artifact.RunArtifactFinalizer)
	if err := r.Patch(ctx, &run, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove artifact cleanup finalizer: %w", err)
	}
	r.record(&run, corev1.EventTypeNormal, "ArtifactCleanupComplete", "Deleted external artifacts")
	return ctrl.Result{}, nil
}

func (r *ArtifactCleanupReconciler) snapshotArtifactStore(ctx context.Context, run *v1alpha1.Run) (ctrl.Result, error) {
	var runtimeResource v1alpha1.Runtime
	key := client.ObjectKey{Namespace: run.Namespace, Name: run.Spec.Runtime}
	if err := r.Get(ctx, key, &runtimeResource); err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve artifact store from Runtime %s: %w", key, err)
	}
	if runtimeResource.Spec.ArtifactStore == nil {
		return ctrl.Result{}, fmt.Errorf("Runtime %s has no artifact store configuration", key)
	}
	base := run.DeepCopy()
	run.Status.ArtifactStore = runtimeResource.Spec.ArtifactStore.DeepCopy()
	if err := r.Status().Patch(ctx, run, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("snapshot artifact store configuration: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *ArtifactCleanupReconciler) storeForRun(ctx context.Context, run *v1alpha1.Run) (artifact.RunStore, error) {
	store := run.Status.ArtifactStore
	if store == nil {
		return nil, fmt.Errorf("Run has no artifact store cleanup configuration")
	}

	switch store.Driver {
	case v1alpha1.ArtifactDriverFilesystem:
		if store.Filesystem == nil || store.Filesystem.VolumeClaimName == "" {
			return nil, fmt.Errorf("filesystem artifact store configuration is incomplete")
		}
		root := r.FilesystemStoreRoot
		if root == "" {
			root = defaultArtifactCleanupFilesystemRoot
		}
		maxArtifactBytes := r.MaxArtifactBytes
		if maxArtifactBytes <= 0 {
			maxArtifactBytes = artifact.DefaultMaxArtifactBytes
		}
		return artifactfs.NewWithLimit(root, store.Filesystem.VolumeClaimName, maxArtifactBytes)
	case v1alpha1.ArtifactDriverS3:
		if store.S3 == nil || store.S3.Bucket == "" {
			return nil, fmt.Errorf("S3 artifact store configuration is incomplete")
		}
		cfg := artifacts3.Config{
			Bucket:         store.S3.Bucket,
			Prefix:         store.S3.Prefix,
			Region:         store.S3.Region,
			Endpoint:       store.S3.Endpoint,
			ForcePathStyle: store.S3.ForcePathStyle,
		}
		if store.S3.CredentialsSecretName != "" {
			secret, err := r.s3CredentialsSecret(ctx, run.Namespace, store.S3.CredentialsSecretName)
			if err != nil {
				return nil, err
			}
			cfg.AccessKeyID = string(secret.Data["AWS_ACCESS_KEY_ID"])
			cfg.SecretAccessKey = string(secret.Data["AWS_SECRET_ACCESS_KEY"])
			cfg.SessionToken = string(secret.Data["AWS_SESSION_TOKEN"])
		}
		return artifacts3.New(ctx, cfg)
	default:
		return nil, fmt.Errorf("unsupported artifact store driver %q", store.Driver)
	}
}

func (r *ArtifactCleanupReconciler) s3CredentialsSecret(ctx context.Context, namespace, name string) (*corev1.Secret, error) {
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := r.Get(ctx, key, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("S3 credentials Secret %s does not exist", key)
		}
		return nil, fmt.Errorf("read S3 credentials Secret %s: %w", key, err)
	}
	if len(secret.Data["AWS_ACCESS_KEY_ID"]) == 0 || len(secret.Data["AWS_SECRET_ACCESS_KEY"]) == 0 {
		return nil, fmt.Errorf("S3 credentials Secret %s must contain AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY", key)
	}
	return &secret, nil
}

func (r *ArtifactCleanupReconciler) record(run *v1alpha1.Run, eventType, reason, message string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(run, eventType, reason, message, args...)
	}
}
