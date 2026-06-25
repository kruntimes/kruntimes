package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	kptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

const (
	artifactCleanerMountPath = "/var/lib/kruntimes/artifacts"
	artifactCleanupRetry     = 30 * time.Second
)

// ArtifactCleanupReconciler owns artifact finalizer removal independently of Runtime Pods.
type ArtifactCleanupReconciler struct {
	client.Client
	Log              logr.Logger
	Recorder         record.EventRecorder
	CleanerImage     string
	ImagePullSecrets []corev1.LocalObjectReference
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=runs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// SetupWithManager registers Run and cleanup Job watches.
func (r *ArtifactCleanupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	jobEvents := predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		UpdateFunc:  func(event.UpdateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Run{}).
		Watches(
			&batchv1.Job{},
			handler.EnqueueRequestsFromMapFunc(r.runForCleanupJob),
			builder.WithPredicates(jobEvents),
		).
		Complete(r)
}

// Reconcile snapshots store configuration and drives cleanup Jobs for deleting Runs.
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
	if r.CleanerImage == "" {
		return ctrl.Result{}, fmt.Errorf("artifact cleaner image is not configured")
	}

	jobKey := client.ObjectKey{Namespace: run.Namespace, Name: artifact.CleanupJobName(run.UID)}
	var job batchv1.Job
	if err := r.Get(ctx, jobKey, &job); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		job, err = r.buildCleanupJob(&run)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Create(ctx, &job); err != nil {
			return ctrl.Result{}, fmt.Errorf("create artifact cleanup Job: %w", err)
		}
		r.record(&run, corev1.EventTypeNormal, "ArtifactCleanupStarted", "Created artifact cleanup Job %s", job.Name)
		return ctrl.Result{}, nil
	}
	if job.Annotations[artifact.CleanupRunAnnotation] != run.Name ||
		job.Annotations[artifact.CleanupRunUIDAnnotation] != string(run.UID) {
		return ctrl.Result{}, fmt.Errorf("artifact cleanup Job %s does not belong to Run %s", jobKey, req.NamespacedName)
	}

	if jobComplete(&job) {
		base := run.DeepCopy()
		controllerutil.RemoveFinalizer(&run, artifact.RunArtifactFinalizer)
		if err := r.Patch(ctx, &run, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("remove artifact cleanup finalizer: %w", err)
		}
		r.record(&run, corev1.EventTypeNormal, "ArtifactCleanupComplete", "Deleted external artifacts")
		return ctrl.Result{}, nil
	}
	if jobFailed(&job) {
		r.record(&run, corev1.EventTypeWarning, "ArtifactCleanupRetry", "Cleanup Job %s failed; retrying", job.Name)
		if err := r.Delete(ctx, &job); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete failed artifact cleanup Job: %w", err)
		}
		return ctrl.Result{RequeueAfter: artifactCleanupRetry}, nil
	}
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

func (r *ArtifactCleanupReconciler) buildCleanupJob(run *v1alpha1.Run) (batchv1.Job, error) {
	store := run.Status.ArtifactStore
	if store == nil {
		return batchv1.Job{}, fmt.Errorf("Run has no artifact store cleanup configuration")
	}
	args := []string{
		"--run-namespace=" + run.Namespace,
		"--run-uid=" + string(run.UID),
		"--driver=" + string(store.Driver),
	}
	podSpec := corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		AutomountServiceAccountToken: kptr.To(false),
		ImagePullSecrets:             append([]corev1.LocalObjectReference(nil), r.ImagePullSecrets...),
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
	container := corev1.Container{
		Name:    "cleaner",
		Image:   r.CleanerImage,
		Command: []string{"/artifact-cleaner"},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: kptr.To(false),
			ReadOnlyRootFilesystem:   kptr.To(true),
			RunAsNonRoot:             kptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
		},
	}

	switch store.Driver {
	case v1alpha1.ArtifactDriverFilesystem:
		if store.Filesystem == nil || store.Filesystem.VolumeClaimName == "" {
			return batchv1.Job{}, fmt.Errorf("filesystem artifact store configuration is incomplete")
		}
		args = append(args,
			"--filesystem-root="+artifactCleanerMountPath,
			"--filesystem-volume-claim="+store.Filesystem.VolumeClaimName,
		)
		container.VolumeMounts = []corev1.VolumeMount{{Name: "artifact-store", MountPath: artifactCleanerMountPath}}
		podSpec.Volumes = []corev1.Volume{{
			Name: "artifact-store",
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: store.Filesystem.VolumeClaimName,
			}},
		}}
		podSpec.SecurityContext.FSGroup = kptr.To[int64](65532)
		podSpec.SecurityContext.FSGroupChangePolicy = kptr.To(corev1.FSGroupChangeOnRootMismatch)
	case v1alpha1.ArtifactDriverS3:
		if store.S3 == nil || store.S3.Bucket == "" {
			return batchv1.Job{}, fmt.Errorf("S3 artifact store configuration is incomplete")
		}
		args = append(args, "--s3-bucket="+store.S3.Bucket)
		if store.S3.Prefix != "" {
			args = append(args, "--s3-prefix="+store.S3.Prefix)
		}
		if store.S3.Region != "" {
			args = append(args, "--s3-region="+store.S3.Region)
		}
		if store.S3.Endpoint != "" {
			args = append(args, "--s3-endpoint="+store.S3.Endpoint)
		}
		if store.S3.ForcePathStyle {
			args = append(args, "--s3-force-path-style=true")
		}
		if store.S3.CredentialsSecretName != "" {
			container.EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: store.S3.CredentialsSecretName},
			}}}
		}
	default:
		return batchv1.Job{}, fmt.Errorf("unsupported artifact store driver %q", store.Driver)
	}
	container.Args = args
	podSpec.Containers = []corev1.Container{container}

	job := batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      artifact.CleanupJobName(run.UID),
			Namespace: run.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "kruntimes",
				"app.kubernetes.io/component": "artifact-cleaner",
				"kruntimes.io/run-uid-hash":   strings.TrimPrefix(artifact.CleanupJobName(run.UID), "artifact-cleanup-"),
			},
			Annotations: map[string]string{
				artifact.CleanupRunAnnotation:    run.Name,
				artifact.CleanupRunUIDAnnotation: string(run.UID),
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            kptr.To[int32](3),
			ActiveDeadlineSeconds:   kptr.To[int64](600),
			TTLSecondsAfterFinished: kptr.To[int32](300),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app.kubernetes.io/name":      "kruntimes",
					"app.kubernetes.io/component": "artifact-cleaner",
				}},
				Spec: podSpec,
			},
		},
	}
	return job, nil
}

func (r *ArtifactCleanupReconciler) runForCleanupJob(_ context.Context, object client.Object) []ctrl.Request {
	job, ok := object.(*batchv1.Job)
	if !ok || job.Labels["app.kubernetes.io/component"] != "artifact-cleaner" {
		return nil
	}
	runName := job.Annotations[artifact.CleanupRunAnnotation]
	if runName == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: job.Namespace, Name: runName}}}
}

func jobComplete(job *batchv1.Job) bool {
	return jobConditionTrue(job, batchv1.JobComplete)
}

func jobFailed(job *batchv1.Job) bool {
	return jobConditionTrue(job, batchv1.JobFailed)
}

func jobConditionTrue(job *batchv1.Job, conditionType batchv1.JobConditionType) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == conditionType && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func (r *ArtifactCleanupReconciler) record(run *v1alpha1.Run, eventType, reason, message string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(run, eventType, reason, message, args...)
	}
}
