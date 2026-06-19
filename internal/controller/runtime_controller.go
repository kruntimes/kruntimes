package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

const (
	runtimeLabel         = "runtime"
	runtimedDefaultImage = "kruntimes-runtimed:latest"
	runtimedDefaultSA    = "kruntimes-runtimed"
	runtimedRoleName     = "kruntimes-runtimed"
	runtimedRBACNameMax  = 63
	workspaceVolume      = "workspace"
	workspacePath        = "/workspace"
	artifactStoreVolume  = "artifact-store"
	artifactStorePath    = "/var/lib/kruntimes/artifacts"
)

// RuntimeReconciler watches Runtime CRs and creates Deployments with runtimed sidecar.
type RuntimeReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	DefaultDaemonImage         string
	RuntimedServiceAccountName string
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *RuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("runtime", req.NamespacedName)

	var rt v1alpha1.Runtime
	if err := r.Get(ctx, req.NamespacedName, &rt); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get runtime: %w", err)
	}

	runtimedServiceAccountName := r.runtimedServiceAccountName(&rt)
	serviceAccount := r.buildRuntimedServiceAccount(&rt, runtimedServiceAccountName)
	role := r.buildRuntimedRole(&rt)
	roleBinding := r.buildRuntimedRoleBinding(&rt, runtimedServiceAccountName)
	deploy := r.buildDeployment(&rt)
	networkPolicy := r.buildNetworkPolicy(&rt)

	if changed, err := r.reconcileServiceAccount(ctx, serviceAccount); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled runtimed ServiceAccount", "serviceAccount", serviceAccount.Name)
		return ctrl.Result{}, nil
	}
	if changed, err := r.reconcileRole(ctx, role); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled runtimed Role", "role", role.Name)
		return ctrl.Result{}, nil
	}
	if changed, err := r.reconcileRoleBinding(ctx, roleBinding); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled runtimed RoleBinding", "roleBinding", roleBinding.Name)
		return ctrl.Result{}, nil
	}
	if changed, err := r.reconcileDeployment(ctx, &rt, deploy); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled Deployment", "deployment", deploy.Name)
		return ctrl.Result{}, nil
	}
	if changed, err := r.reconcileNetworkPolicy(ctx, &rt, networkPolicy); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled NetworkPolicy", "networkPolicy", networkPolicy.Name)
		return ctrl.Result{}, nil
	}

	// Propagate Deployment status back to Runtime.
	var existing appsv1.Deployment
	err := r.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: deploy.Namespace}, &existing)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("get deployment for status: %w", err)
	}
	if rt.Status.ReadyReplicas != existing.Status.ReadyReplicas {
		rt.Status.ReadyReplicas = existing.Status.ReadyReplicas
		if err := r.Status().Update(ctx, &rt); err != nil {
			return ctrl.Result{}, fmt.Errorf("update runtime status: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *RuntimeReconciler) buildDeployment(rt *v1alpha1.Runtime) *appsv1.Deployment {
	name := rt.Name
	ns := rt.Namespace
	runtimeLabelVal := name
	replicas := rt.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	port := rt.Spec.Port
	if port == 0 {
		port = 9091
	}
	daemonImage := rt.Spec.DaemonImage
	if daemonImage == "" {
		daemonImage = runtimedDefaultImage
	}
	if r.DefaultDaemonImage != "" {
		daemonImage = r.DefaultDaemonImage
	}
	runtimedServiceAccountName := r.runtimedServiceAccountName(rt)

	labels := map[string]string{
		runtimeLabel: runtimeLabelVal,
		"app":        "kruntimes-" + name,
	}
	annotations := runtimepod.CapacityAnnotations(rt)
	runsCapacity := runtimepod.RunsCapacityFromRuntime(rt, 0)

	runtimeContainer := corev1.Container{
		Name:            "runtime",
		Image:           rt.Spec.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            rt.Spec.Command,
		Ports: []corev1.ContainerPort{
			{Name: "grpc", ContainerPort: port, Protocol: corev1.ProtocolTCP},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       5,
		},
		Env: rt.Spec.Env,
		Resources: corev1.ResourceRequirements{
			Requests: rt.Spec.Resources.Requests,
			Limits:   rt.Spec.Resources.Limits,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolume, MountPath: workspacePath},
		},
		SecurityContext: defaultContainerSecurityContext(),
	}
	if runtimeContainer.Resources.Requests == nil {
		runtimeContainer.Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("250m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		}
	}
	if runtimeContainer.Resources.Limits == nil {
		runtimeContainer.Resources.Limits = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		}
	}

	daemonContainer := corev1.Container{
		Name:            "runtimed",
		Image:           daemonImage,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args: []string{
			fmt.Sprintf("--runtime-endpoint=127.0.0.1:%d", port),
			"--status-addr=:9093",
		},
		Ports: []corev1.ContainerPort{
			{Name: "health", ContainerPort: 9094, Protocol: corev1.ProtocolTCP},
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(9094)},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(9094)},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       5,
		},
		Env: []corev1.EnvVar{
			{
				Name: "HOSTNAME",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
				},
			},
			{
				Name: "POD_NAMESPACE",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
				},
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: workspaceVolume, MountPath: workspacePath},
		},
		SecurityContext: defaultContainerSecurityContext(),
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
	}
	daemonContainer.Args = append(daemonContainer.Args, fmt.Sprintf("--runtime-name=%s", name))
	if runsCapacity > 0 {
		daemonContainer.Args = append(daemonContainer.Args, fmt.Sprintf("--workers=%d", runsCapacity))
	}

	volumes := []corev1.Volume{
		{
			Name: workspaceVolume,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: workspaceVolumeSource(rt.Spec.Workspace),
			},
		},
	}
	podSecurityContext := configureArtifactStore(rt.Spec.ArtifactStore, &daemonContainer, &volumes)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-" + name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: annotations},
				Spec: corev1.PodSpec{
					ServiceAccountName: runtimedServiceAccountName,
					SecurityContext:    podSecurityContext,
					Containers:         []corev1.Container{runtimeContainer, daemonContainer},
					Volumes:            volumes,
				},
			},
		},
	}
}

func (r *RuntimeReconciler) runtimedServiceAccountName(rt *v1alpha1.Runtime) string {
	if rt.Spec.RuntimedServiceAccountName != "" {
		return rt.Spec.RuntimedServiceAccountName
	}
	if r.RuntimedServiceAccountName != "" {
		return r.RuntimedServiceAccountName
	}
	return runtimedDefaultSA
}

func (r *RuntimeReconciler) buildRuntimedServiceAccount(rt *v1alpha1.Runtime, name string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: rt.Namespace,
			Labels:    runtimedRBACLabels(),
		},
	}
}

func (r *RuntimeReconciler) buildRuntimedRole(rt *v1alpha1.Runtime) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runtimedRoleName,
			Namespace: rt.Namespace,
			Labels:    runtimedRBACLabels(),
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"kruntimes.io"},
				Resources: []string{"runs"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
			},
			{
				APIGroups: []string{"kruntimes.io"},
				Resources: []string{"runs/status"},
				Verbs:     []string{"get", "update", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"pods/status"},
				Verbs:     []string{"get", "patch"},
			},
			{
				APIGroups: []string{""},
				Resources: []string{"events"},
				Verbs:     []string{"create", "patch"},
			},
		},
	}
}

func (r *RuntimeReconciler) buildRuntimedRoleBinding(rt *v1alpha1.Runtime, serviceAccountName string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runtimedRoleBindingName(serviceAccountName),
			Namespace: rt.Namespace,
			Labels:    runtimedRBACLabels(),
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     runtimedRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      serviceAccountName,
				Namespace: rt.Namespace,
			},
		},
	}
}

func runtimedRBACLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":      "kruntimes",
		"app.kubernetes.io/component": "runtimed",
		"app":                         "kruntimes-runtimed",
	}
}

func runtimedRoleBindingName(serviceAccountName string) string {
	name := fmt.Sprintf("%s-%s", runtimedRoleName, serviceAccountName)
	if len(name) <= runtimedRBACNameMax {
		return name
	}
	sum := sha256.Sum256([]byte(serviceAccountName))
	suffix := hex.EncodeToString(sum[:])[:10]
	prefixLength := runtimedRBACNameMax - len(suffix) - 1
	prefix := strings.TrimRight(name[:prefixLength], "-.")
	return fmt.Sprintf("%s-%s", prefix, suffix)
}

func (r *RuntimeReconciler) buildNetworkPolicy(rt *v1alpha1.Runtime) *networkingv1.NetworkPolicy {
	labels := map[string]string{
		runtimeLabel: rt.Name,
		"app":        "kruntimes-" + rt.Name,
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-" + rt.Name,
			Namespace: rt.Namespace,
			Labels:    labels,
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: labels,
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
			},
		},
	}
}

func defaultContainerSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		ReadOnlyRootFilesystem:   ptr(true),
		RunAsNonRoot:             ptr(true),
		AllowPrivilegeEscalation: ptr(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

func workspaceVolumeSource(workspace *v1alpha1.RuntimeWorkspaceSpec) *corev1.EmptyDirVolumeSource {
	emptyDir := &corev1.EmptyDirVolumeSource{}
	if workspace == nil || workspace.SizeLimit == nil {
		return emptyDir
	}
	sizeLimit := workspace.SizeLimit.DeepCopy()
	emptyDir.SizeLimit = &sizeLimit
	return emptyDir
}

func configureArtifactStore(store *v1alpha1.RuntimeArtifactStoreSpec, daemon *corev1.Container, volumes *[]corev1.Volume) *corev1.PodSecurityContext {
	if store == nil {
		return nil
	}

	switch store.Driver {
	case v1alpha1.ArtifactDriverFilesystem:
		if store.Filesystem == nil {
			return nil
		}
		daemon.Args = append(daemon.Args,
			"--artifact-store-driver=filesystem",
			fmt.Sprintf("--artifact-store-root=%s", artifactStorePath),
			fmt.Sprintf("--artifact-volume-claim=%s", store.Filesystem.VolumeClaimName),
		)
		daemon.VolumeMounts = append(daemon.VolumeMounts, corev1.VolumeMount{
			Name:      artifactStoreVolume,
			MountPath: artifactStorePath,
		})
		*volumes = append(*volumes, corev1.Volume{
			Name: artifactStoreVolume,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: store.Filesystem.VolumeClaimName,
				},
			},
		})
		return &corev1.PodSecurityContext{
			FSGroup:             ptr[int64](65532),
			FSGroupChangePolicy: ptr(corev1.FSGroupChangeOnRootMismatch),
		}
	case v1alpha1.ArtifactDriverS3:
		if store.S3 == nil {
			return nil
		}
		configureS3ArtifactStore(store.S3, daemon)
	}

	return nil
}

func configureS3ArtifactStore(store *v1alpha1.S3ArtifactStoreSpec, daemon *corev1.Container) {
	daemon.Args = append(daemon.Args,
		"--artifact-store-driver=s3",
		fmt.Sprintf("--artifact-s3-bucket=%s", store.Bucket),
	)
	if store.Prefix != "" {
		daemon.Args = append(daemon.Args, fmt.Sprintf("--artifact-s3-prefix=%s", store.Prefix))
	}
	if store.Region != "" {
		daemon.Args = append(daemon.Args, fmt.Sprintf("--artifact-s3-region=%s", store.Region))
	}
	if store.Endpoint != "" {
		daemon.Args = append(daemon.Args, fmt.Sprintf("--artifact-s3-endpoint=%s", store.Endpoint))
	}
	if store.ForcePathStyle {
		daemon.Args = append(daemon.Args, "--artifact-s3-force-path-style=true")
	}
	if store.UploadPartSize > 0 {
		daemon.Args = append(daemon.Args, fmt.Sprintf("--artifact-s3-upload-part-size=%d", store.UploadPartSize))
	}
	if store.UploadConcurrency > 0 {
		daemon.Args = append(daemon.Args, fmt.Sprintf("--artifact-s3-upload-concurrency=%d", store.UploadConcurrency))
	}
	if store.CredentialsSecretName != "" {
		daemon.EnvFrom = append(daemon.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: store.CredentialsSecretName},
			},
		})
	}
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *RuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Runtime{}).
		Owns(&appsv1.Deployment{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Complete(r)
}

func (r *RuntimeReconciler) reconcileDeployment(
	ctx context.Context,
	rt *v1alpha1.Runtime,
	desired *appsv1.Deployment,
) (bool, error) {
	var existing appsv1.Deployment
	if err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get deployment: %w", err)
		}
		if err := controllerutil.SetControllerReference(rt, desired, r.Scheme); err != nil {
			return false, fmt.Errorf("set deployment owner ref: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("create deployment: %w", err)
		}
		return true, nil
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return false, nil
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	if err := controllerutil.SetControllerReference(rt, &existing, r.Scheme); err != nil {
		return false, fmt.Errorf("set deployment owner ref: %w", err)
	}
	if err := r.Update(ctx, &existing); err != nil {
		return false, fmt.Errorf("update deployment: %w", err)
	}
	return true, nil
}

func (r *RuntimeReconciler) reconcileServiceAccount(
	ctx context.Context,
	desired *corev1.ServiceAccount,
) (bool, error) {
	var existing corev1.ServiceAccount
	if err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get serviceaccount: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("create serviceaccount: %w", err)
		}
		return true, nil
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) {
		return false, nil
	}
	existing.Labels = desired.Labels
	if err := r.Update(ctx, &existing); err != nil {
		return false, fmt.Errorf("update serviceaccount: %w", err)
	}
	return true, nil
}

func (r *RuntimeReconciler) reconcileRole(ctx context.Context, desired *rbacv1.Role) (bool, error) {
	var existing rbacv1.Role
	if err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get role: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("create role: %w", err)
		}
		return true, nil
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(existing.Rules, desired.Rules) {
		return false, nil
	}
	existing.Labels = desired.Labels
	existing.Rules = desired.Rules
	if err := r.Update(ctx, &existing); err != nil {
		return false, fmt.Errorf("update role: %w", err)
	}
	return true, nil
}

func (r *RuntimeReconciler) reconcileRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) (bool, error) {
	var existing rbacv1.RoleBinding
	if err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get rolebinding: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("create rolebinding: %w", err)
		}
		return true, nil
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(existing.RoleRef, desired.RoleRef) &&
		equality.Semantic.DeepEqual(existing.Subjects, desired.Subjects) {
		return false, nil
	}
	existing.Labels = desired.Labels
	existing.RoleRef = desired.RoleRef
	existing.Subjects = desired.Subjects
	if err := r.Update(ctx, &existing); err != nil {
		return false, fmt.Errorf("update rolebinding: %w", err)
	}
	return true, nil
}

func (r *RuntimeReconciler) reconcileNetworkPolicy(
	ctx context.Context,
	rt *v1alpha1.Runtime,
	desired *networkingv1.NetworkPolicy,
) (bool, error) {
	var existing networkingv1.NetworkPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get networkpolicy: %w", err)
		}
		if err := controllerutil.SetControllerReference(rt, desired, r.Scheme); err != nil {
			return false, fmt.Errorf("set networkpolicy owner ref: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("create networkpolicy: %w", err)
		}
		return true, nil
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(existing.Spec, desired.Spec) {
		return false, nil
	}
	existing.Labels = desired.Labels
	existing.Spec = desired.Spec
	if err := controllerutil.SetControllerReference(rt, &existing, r.Scheme); err != nil {
		return false, fmt.Errorf("set networkpolicy owner ref: %w", err)
	}
	if err := r.Update(ctx, &existing); err != nil {
		return false, fmt.Errorf("update networkpolicy: %w", err)
	}
	return true, nil
}

func ptr[T any](v T) *T { return &v }
