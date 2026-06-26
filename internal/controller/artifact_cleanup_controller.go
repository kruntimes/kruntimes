package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	kptr "k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

const (
	artifactStoreMountPath          = "/var/lib/kruntimes/artifacts"
	artifactCleanupRetry            = 30 * time.Second
	runtimeMaintainerServiceAccount = "kruntimes-runtime-maintainer"
	runtimeMaintainerRole           = "kruntimes-runtime-maintainer"
)

// ArtifactCleanupReconciler snapshots artifact store config and ensures a
// long-running runtime maintainer exists for each store snapshot referenced by a
// deleting Run. Workers are intentionally not owned by Runtime objects so they
// survive Runtime deletion and artifactStore changes.
type ArtifactCleanupReconciler struct {
	client.Client
	Log              logr.Logger
	Recorder         record.EventRecorder
	MaintainerImage  string
	ImagePullSecrets []corev1.LocalObjectReference
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=runs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *ArtifactCleanupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Run{}).
		Complete(r)
}

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
	if r.MaintainerImage == "" {
		return ctrl.Result{}, fmt.Errorf("runtime maintainer image is not configured")
	}
	if err := r.ensureCleanupWorker(ctx, &run); err != nil {
		r.record(&run, corev1.EventTypeWarning, "ArtifactCleanupRetry", "Ensure runtime maintainer failed: %v", err)
		return ctrl.Result{RequeueAfter: artifactCleanupRetry}, nil
	}
	return ctrl.Result{RequeueAfter: artifactCleanupRetry}, nil
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

func (r *ArtifactCleanupReconciler) ensureCleanupWorker(ctx context.Context, run *v1alpha1.Run) error {
	serviceAccount := cleanupServiceAccount(run.Namespace)
	role := cleanupRole(run.Namespace)
	roleBinding := cleanupRoleBinding(run.Namespace)
	deploy, err := r.buildCleanupWorkerDeployment(run)
	if err != nil {
		return err
	}
	if err := r.reconcileServiceAccount(ctx, serviceAccount); err != nil {
		return err
	}
	if err := r.reconcileRole(ctx, role); err != nil {
		return err
	}
	if err := r.reconcileRoleBinding(ctx, roleBinding); err != nil {
		return err
	}
	if err := r.reconcileCleanupDeployment(ctx, deploy); err != nil {
		return err
	}
	r.record(run, corev1.EventTypeNormal, "RuntimeMaintainerEnsured", "Ensured runtime maintainer %s", deploy.Name)
	return nil
}

func (r *ArtifactCleanupReconciler) buildCleanupWorkerDeployment(run *v1alpha1.Run) (*appsv1.Deployment, error) {
	store := run.Status.ArtifactStore
	if store == nil {
		return nil, fmt.Errorf("Run has no artifact store cleanup configuration")
	}
	storeHash, err := artifact.StoreHash(store)
	if err != nil {
		return nil, err
	}
	args := []string{
		"--run-namespace=" + run.Namespace,
		"--store-hash=" + storeHash,
		"--driver=" + string(store.Driver),
	}
	podSpec := corev1.PodSpec{
		ServiceAccountName:           runtimeMaintainerServiceAccount,
		AutomountServiceAccountToken: kptr.To(true),
		ImagePullSecrets:             append([]corev1.LocalObjectReference(nil), r.ImagePullSecrets...),
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
	}
	container := corev1.Container{
		Name:    "cleaner",
		Image:   r.MaintainerImage,
		Command: []string{"/runtime-maintainer"},
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
			return nil, fmt.Errorf("filesystem artifact store configuration is incomplete")
		}
		args = append(args,
			"--filesystem-root="+artifactStoreMountPath,
			"--filesystem-volume-claim="+store.Filesystem.VolumeClaimName,
		)
		container.VolumeMounts = []corev1.VolumeMount{{Name: "artifact-store", MountPath: artifactStoreMountPath}}
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
			return nil, fmt.Errorf("S3 artifact store configuration is incomplete")
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
		return nil, fmt.Errorf("unsupported artifact store driver %q", store.Driver)
	}
	container.Args = args
	podSpec.Containers = []corev1.Container{container}

	labels := cleanupWorkerLabels(storeHash)
	// Do not set Runtime owner references here. Runtime maintainers are keyed by
	// immutable artifact-store snapshots so they can clean deleting Runs even
	// after the Runtime is deleted or its artifactStore spec changes.
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      artifact.RuntimeMaintainerName(storeHash),
			Namespace: run.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: kptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       podSpec,
			},
		},
	}, nil
}

func cleanupWorkerLabels(storeHash string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "kruntimes",
		"app.kubernetes.io/component":  "runtime-maintainer",
		artifact.CleanupStoreHashLabel: storeHash,
	}
}

func cleanupServiceAccount(namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{
		Name:      runtimeMaintainerServiceAccount,
		Namespace: namespace,
		Labels:    cleanupWorkerLabels("rbac"),
	}}
}

func cleanupRole(namespace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: runtimeMaintainerRole, Namespace: namespace, Labels: cleanupWorkerLabels("rbac")},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"kruntimes.io"}, Resources: []string{"runs"}, Verbs: []string{"get", "list", "watch", "update", "patch"}},
			{APIGroups: []string{"kruntimes.io"}, Resources: []string{"runs/status"}, Verbs: []string{"get", "update", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
		},
	}
}

func cleanupRoleBinding(namespace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: runtimeMaintainerRole, Namespace: namespace, Labels: cleanupWorkerLabels("rbac")},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: runtimeMaintainerRole},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      runtimeMaintainerServiceAccount,
			Namespace: namespace,
		}},
	}
}

func (r *ArtifactCleanupReconciler) reconcileServiceAccount(ctx context.Context, desired *corev1.ServiceAccount) error {
	var existing corev1.ServiceAccount
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return nil
	}
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

func (r *ArtifactCleanupReconciler) reconcileRole(ctx context.Context, desired *rbacv1.Role) error {
	var existing rbacv1.Role
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) && equality.Semantic.DeepEqual(existing.Rules, desired.Rules) {
		return nil
	}
	existing.Labels = desired.Labels
	existing.Rules = desired.Rules
	return r.Update(ctx, &existing)
}

func (r *ArtifactCleanupReconciler) reconcileRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) error {
	var existing rbacv1.RoleBinding
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(existing.RoleRef, desired.RoleRef) &&
		equality.Semantic.DeepEqual(existing.Subjects, desired.Subjects) {
		return nil
	}
	existing.Labels = desired.Labels
	existing.RoleRef = desired.RoleRef
	existing.Subjects = desired.Subjects
	return r.Update(ctx, &existing)
}

func (r *ArtifactCleanupReconciler) reconcileCleanupDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	var existing appsv1.Deployment
	if err := r.Get(ctx, client.ObjectKeyFromObject(desired), &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return r.Create(ctx, desired)
		}
		return err
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return nil
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	return r.Update(ctx, &existing)
}

func (r *ArtifactCleanupReconciler) record(run *v1alpha1.Run, eventType, reason, message string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(run, eventType, reason, message, args...)
	}
}
