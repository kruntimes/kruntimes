package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"strings"
	"time"

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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
	gatewayCAVolume      = "gateway-ca"
	gatewayCAPath        = "/var/run/kruntimes/gateway-ca"
	gatewayCAFile        = "ca.crt"
	gatewayCAAnnotation  = "kruntimes.io/gateway-ca-bundle"
)

// RuntimeReconciler watches Runtime CRs and creates Deployments with runtimed sidecar.
type RuntimeReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme

	DefaultDaemonImage         string
	RuntimedServiceAccountName string
	GatewayNamespace           string
	GatewaySelectorLabels      map[string]string
	GatewayURL                 string
	GatewayCABundle            []byte
	SessionMaxQueueSize        int
	SessionMaxOperationTimeout time.Duration
	SessionCloseTimeout        time.Duration
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runtimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;patch
// +kubebuilder:rbac:groups=core,resources=configmaps,verbs=create;get;list;update;patch;delete
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
	service := r.buildService(&rt)
	deploy := r.buildDeployment(&rt)
	networkPolicy := r.buildNetworkPolicy(&rt)

	if changed, err := r.reconcileServiceAccount(ctx, serviceAccount); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled runtimed ServiceAccount", "serviceAccount", serviceAccount.Name)
	}
	if changed, err := r.reconcileRole(ctx, role); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled runtimed Role", "role", role.Name)
	}
	if changed, err := r.reconcileRoleBinding(ctx, roleBinding); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled runtimed RoleBinding", "roleBinding", roleBinding.Name)
	}
	if changed, err := r.reconcileService(ctx, &rt, service); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled Runtime Service", "service", service.Name)
	}
	if changed, err := r.reconcileDeployment(ctx, &rt, deploy); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled Deployment", "deployment", deploy.Name)
	}
	if changed, err := r.reconcileNetworkPolicy(ctx, &rt, networkPolicy); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		log.Info("Reconciled NetworkPolicy", "networkPolicy", networkPolicy.Name)
	}

	return r.reconcileRuntimeStatus(ctx, &rt, client.ObjectKeyFromObject(deploy))
}

// reconcileRuntimeStatus observes the Deployment after its desired spec has
// been reconciled. Child-resource writes must not prevent Runtime status from
// reflecting the most recently observed Deployment state.
func (r *RuntimeReconciler) reconcileRuntimeStatus(ctx context.Context, rt *v1alpha1.Runtime, deploymentKey client.ObjectKey) (ctrl.Result, error) {
	var existing appsv1.Deployment
	if err := r.Get(ctx, deploymentKey, &existing); err != nil {
		return ctrl.Result{}, fmt.Errorf("get deployment for status: %w", err)
	}
	if rt.Status.ReadyReplicas == existing.Status.ReadyReplicas {
		return ctrl.Result{}, nil
	}

	base := rt.DeepCopy()
	rt.Status.ReadyReplicas = existing.Status.ReadyReplicas
	if err := r.Status().Patch(ctx, rt, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch runtime status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *RuntimeReconciler) buildService(rt *v1alpha1.Runtime) *corev1.Service {
	selectorLabels := map[string]string{
		runtimeLabel: rt.Name,
		"app":        "kruntimes-" + rt.Name,
	}
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-" + rt.Name,
			Namespace: rt.Namespace,
			Labels:    selectorLabels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: selectorLabels,
			Ports: []corev1.ServicePort{{
				Name:       "session-runtime",
				Port:       9093,
				TargetPort: intstr.FromString("session-runtime"),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
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
		daemonImage = r.DefaultDaemonImage
	}
	if daemonImage == "" {
		daemonImage = runtimedDefaultImage
	}
	runtimedServiceAccountName := r.runtimedServiceAccountName(rt)
	template := rt.Spec.Template.DeepCopy()
	selectorLabels := map[string]string{
		runtimeLabel: runtimeLabelVal,
		"app":        "kruntimes-" + name,
	}
	labels := maps.Clone(template.Labels)
	if labels == nil {
		labels = make(map[string]string, 2)
	}
	maps.Copy(labels, selectorLabels)
	annotations := maps.Clone(template.Annotations)
	for key, value := range runtimepod.CapacityAnnotations(rt) {
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[key] = value
	}
	runsCapacity := runtimepod.RunsCapacityFromRuntime(rt, 0)

	runtimeContainer, additionalContainers := runtimeContainers(template.Spec.Containers)
	runtimeContainer.VolumeMounts = withoutVolumeMount(runtimeContainer.VolumeMounts, artifactStoreVolume)
	for i := range additionalContainers {
		additionalContainers[i].VolumeMounts = withoutVolumeMount(additionalContainers[i].VolumeMounts, artifactStoreVolume)
	}
	for i := range template.Spec.InitContainers {
		template.Spec.InitContainers[i].VolumeMounts = withoutVolumeMount(template.Spec.InitContainers[i].VolumeMounts, artifactStoreVolume)
	}
	runtimeContainer.Ports = upsertContainerPort(runtimeContainer.Ports, corev1.ContainerPort{
		Name: "grpc", ContainerPort: port, Protocol: corev1.ProtocolTCP,
	})
	if runtimeContainer.LivenessProbe == nil {
		runtimeContainer.LivenessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
			},
			InitialDelaySeconds: 5,
			PeriodSeconds:       10,
		}
	}
	if runtimeContainer.ReadinessProbe == nil {
		runtimeContainer.ReadinessProbe = &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(port)},
			},
			InitialDelaySeconds: 1,
			PeriodSeconds:       5,
		}
	}
	runtimeContainer.VolumeMounts = upsertVolumeMount(runtimeContainer.VolumeMounts, corev1.VolumeMount{
		Name: workspaceVolume, MountPath: workspacePath,
	})
	if runtimeContainer.SecurityContext == nil {
		runtimeContainer.SecurityContext = defaultContainerSecurityContext()
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
			{Name: "session-runtime", ContainerPort: 9093, Protocol: corev1.ProtocolTCP},
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
				Name: "POD_NAME",
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
	if r.GatewayURL != "" {
		daemonContainer.Args = append(daemonContainer.Args, fmt.Sprintf("--gateway-url=%s", r.GatewayURL))
	}
	if len(r.GatewayCABundle) > 0 {
		daemonContainer.Args = append(daemonContainer.Args, fmt.Sprintf("--gateway-ca-file=%s/%s", gatewayCAPath, gatewayCAFile))
		daemonContainer.VolumeMounts = append(daemonContainer.VolumeMounts, corev1.VolumeMount{Name: gatewayCAVolume, MountPath: gatewayCAPath, ReadOnly: true})
		if annotations == nil {
			annotations = make(map[string]string)
		}
		annotations[gatewayCAAnnotation] = string(r.GatewayCABundle)
	}
	if r.SessionMaxQueueSize > 0 {
		daemonContainer.Args = append(daemonContainer.Args, fmt.Sprintf("--session-max-queue-size=%d", r.SessionMaxQueueSize))
	}
	if r.SessionMaxOperationTimeout > 0 {
		daemonContainer.Args = append(daemonContainer.Args, fmt.Sprintf("--session-max-operation-timeout=%s", r.SessionMaxOperationTimeout))
	}
	if r.SessionCloseTimeout > 0 {
		daemonContainer.Args = append(daemonContainer.Args, fmt.Sprintf("--session-close-timeout=%s", r.SessionCloseTimeout))
	}
	if runsCapacity > 0 {
		daemonContainer.Args = append(daemonContainer.Args, fmt.Sprintf("--workers=%d", runsCapacity))
	}

	volumes := make([]corev1.Volume, 0, len(template.Spec.Volumes)+2)
	for _, volume := range template.Spec.Volumes {
		if volume.Name != workspaceVolume && volume.Name != artifactStoreVolume && volume.Name != gatewayCAVolume {
			volumes = append(volumes, volume)
		}
	}
	volumes = append(volumes,
		corev1.Volume{
			Name:         workspaceVolume,
			VolumeSource: workspaceVolumeSource(rt.Spec.Workspace),
		},
	)
	if len(r.GatewayCABundle) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: gatewayCAVolume,
			VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{
				Items: []corev1.DownwardAPIVolumeFile{{
					Path:     gatewayCAFile,
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.annotations['" + gatewayCAAnnotation + "']"},
				}},
			}},
		})
	}
	artifactSecurityContext := configureArtifactStore(rt.Spec.ArtifactStore, &daemonContainer, &volumes)
	podSecurityContext := template.Spec.SecurityContext
	if podSecurityContext == nil {
		podSecurityContext = artifactSecurityContext
	} else if artifactSecurityContext != nil && podSecurityContext.FSGroup == nil {
		podSecurityContext.FSGroup = artifactSecurityContext.FSGroup
		if podSecurityContext.FSGroupChangePolicy == nil {
			podSecurityContext.FSGroupChangePolicy = artifactSecurityContext.FSGroupChangePolicy
		}
	}

	containers := make([]corev1.Container, 0, 2+len(additionalContainers))
	containers = append(containers, runtimeContainer)
	containers = append(containers, additionalContainers...)
	containers = append(containers, daemonContainer)
	template.Spec.Containers = containers
	template.Spec.Volumes = volumes
	template.Spec.ServiceAccountName = runtimedServiceAccountName
	template.Spec.SecurityContext = podSecurityContext
	template.ObjectMeta = metav1.ObjectMeta{Labels: labels, Annotations: annotations}

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runtime-" + name,
			Namespace: ns,
			Labels:    selectorLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: selectorLabels},
			Template: *template,
		},
	}
}

func (r *RuntimeReconciler) runtimedServiceAccountName(rt *v1alpha1.Runtime) string {
	if rt.Spec.Template.Spec.ServiceAccountName != "" {
		return rt.Spec.Template.Spec.ServiceAccountName
	}
	if r.RuntimedServiceAccountName != "" {
		return r.RuntimedServiceAccountName
	}
	return runtimedDefaultSA
}

func runtimeContainers(containers []corev1.Container) (corev1.Container, []corev1.Container) {
	runtimeContainer := corev1.Container{Name: "runtime"}
	additional := make([]corev1.Container, 0, len(containers))
	for _, container := range containers {
		switch container.Name {
		case "runtime":
			runtimeContainer = container
		case "runtimed":
			// The controller owns the runtimed sidecar and ignores user overrides.
		default:
			additional = append(additional, container)
		}
	}
	return runtimeContainer, additional
}

func upsertContainerPort(ports []corev1.ContainerPort, required corev1.ContainerPort) []corev1.ContainerPort {
	for i := range ports {
		if ports[i].Name == required.Name {
			ports[i] = required
			return ports
		}
	}
	return append(ports, required)
}

func upsertVolumeMount(mounts []corev1.VolumeMount, required corev1.VolumeMount) []corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == required.Name {
			mounts[i] = required
			return mounts
		}
	}
	return append(mounts, required)
}

func withoutVolumeMount(mounts []corev1.VolumeMount, name string) []corev1.VolumeMount {
	result := mounts[:0]
	for _, mount := range mounts {
		if mount.Name != name {
			result = append(result, mount)
		}
	}
	return result
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
				APIGroups: []string{"kruntimes.io"},
				Resources: []string{"persistentworkspaces"},
				Verbs:     []string{"get", "list", "watch", "update", "patch"},
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
				Resources: []string{"configmaps"},
				Verbs:     []string{"create", "get", "list", "update", "patch", "delete"},
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
	ingress := []networkingv1.NetworkPolicyIngressRule(nil)
	if r.GatewayNamespace != "" {
		ingress = []networkingv1.NetworkPolicyIngressRule{
			{
				From: []networkingv1.NetworkPolicyPeer{{
					PodSelector: &metav1.LabelSelector{MatchLabels: labels},
				}},
				Ports: sessionRuntimeNetworkPolicyPorts(),
			},
			{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"kubernetes.io/metadata.name": r.GatewayNamespace,
					}},
					PodSelector: &metav1.LabelSelector{MatchLabels: r.gatewaySelectorLabels()},
				}},
				Ports: sessionRuntimeNetworkPolicyPorts(),
			},
		}
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
			Ingress: ingress,
		},
	}
}

func (r *RuntimeReconciler) gatewaySelectorLabels() map[string]string {
	if len(r.GatewaySelectorLabels) != 0 {
		return maps.Clone(r.GatewaySelectorLabels)
	}
	return map[string]string{"app.kubernetes.io/component": "runtime-gateway"}
}

func sessionRuntimeNetworkPolicyPorts() []networkingv1.NetworkPolicyPort {
	return []networkingv1.NetworkPolicyPort{{
		Protocol: ptr(corev1.ProtocolTCP),
		Port:     ptr(intstr.FromInt(9093)),
	}}
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

func workspaceVolumeSource(workspace *v1alpha1.RuntimeWorkspaceSpec) corev1.VolumeSource {
	if workspace == nil {
		return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}

	source := workspace.VolumeSource.DeepCopy()
	if source == nil || !hasVolumeSource(*source) {
		return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
	}
	return *source
}

func hasVolumeSource(source corev1.VolumeSource) bool {
	return source.HostPath != nil ||
		source.EmptyDir != nil ||
		source.GCEPersistentDisk != nil ||
		source.AWSElasticBlockStore != nil ||
		source.GitRepo != nil ||
		source.Secret != nil ||
		source.NFS != nil ||
		source.ISCSI != nil ||
		source.Glusterfs != nil ||
		source.PersistentVolumeClaim != nil ||
		source.RBD != nil ||
		source.FlexVolume != nil ||
		source.Cinder != nil ||
		source.CephFS != nil ||
		source.Flocker != nil ||
		source.DownwardAPI != nil ||
		source.FC != nil ||
		source.AzureFile != nil ||
		source.ConfigMap != nil ||
		source.VsphereVolume != nil ||
		source.Quobyte != nil ||
		source.AzureDisk != nil ||
		source.PhotonPersistentDisk != nil ||
		source.Projected != nil ||
		source.PortworxVolume != nil ||
		source.ScaleIO != nil ||
		source.StorageOS != nil ||
		source.CSI != nil ||
		source.Ephemeral != nil ||
		source.Image != nil
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
		daemon.Args = append(daemon.Args, fmt.Sprintf("--artifact-s3-credentials-secret-name=%s", store.CredentialsSecretName))
		daemon.EnvFrom = append(daemon.EnvFrom, corev1.EnvFromSource{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: store.CredentialsSecretName},
			},
		})
	}
}

func (r *RuntimeReconciler) runtimesForRuntimedRBAC(ctx context.Context, object client.Object) []reconcile.Request {
	serviceAccount, isServiceAccount := object.(*corev1.ServiceAccount)
	switch object.(type) {
	case *corev1.ServiceAccount:
	case *rbacv1.Role:
		if object.GetName() != runtimedRoleName {
			return nil
		}
	case *rbacv1.RoleBinding:
		if !strings.HasPrefix(object.GetName(), runtimedRoleName+"-") {
			return nil
		}
	default:
		return nil
	}

	var runtimes v1alpha1.RuntimeList
	if err := r.List(ctx, &runtimes, client.InNamespace(object.GetNamespace())); err != nil {
		r.Log.Error(err, "unable to list Runtimes for runtimed RBAC event", "object", client.ObjectKeyFromObject(object))
		return nil
	}

	requests := make([]reconcile.Request, 0, len(runtimes.Items))
	for i := range runtimes.Items {
		runtime := &runtimes.Items[i]
		if isServiceAccount && r.runtimedServiceAccountName(runtime) != serviceAccount.Name {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(runtime)})
	}
	return requests
}

// SetupWithManager registers the reconciler with the controller manager.
func (r *RuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Runtime{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&corev1.ServiceAccount{}, handler.EnqueueRequestsFromMapFunc(r.runtimesForRuntimedRBAC)).
		Watches(&rbacv1.Role{}, handler.EnqueueRequestsFromMapFunc(r.runtimesForRuntimedRBAC)).
		Watches(&rbacv1.RoleBinding{}, handler.EnqueueRequestsFromMapFunc(r.runtimesForRuntimedRBAC)).
		Complete(r)
}

func (r *RuntimeReconciler) reconcileService(
	ctx context.Context,
	rt *v1alpha1.Runtime,
	desired *corev1.Service,
) (bool, error) {
	var existing corev1.Service
	if err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, &existing); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get service: %w", err)
		}
		if err := controllerutil.SetControllerReference(rt, desired, r.Scheme); err != nil {
			return false, fmt.Errorf("set service owner ref: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil {
			return false, fmt.Errorf("create service: %w", err)
		}
		return true, nil
	}
	if equality.Semantic.DeepEqual(existing.Labels, desired.Labels) &&
		equality.Semantic.DeepEqual(existing.Spec.Selector, desired.Spec.Selector) &&
		equality.Semantic.DeepEqual(existing.Spec.Ports, desired.Spec.Ports) &&
		existing.Spec.Type == desired.Spec.Type {
		return false, nil
	}
	existing.Labels = desired.Labels
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Type = desired.Spec.Type
	if err := controllerutil.SetControllerReference(rt, &existing, r.Scheme); err != nil {
		return false, fmt.Errorf("set service owner ref: %w", err)
	}
	if err := r.Update(ctx, &existing); err != nil {
		return false, fmt.Errorf("update service: %w", err)
	}
	return true, nil
}

func (r *RuntimeReconciler) reconcileDeployment(
	ctx context.Context,
	rt *v1alpha1.Runtime,
	desired *appsv1.Deployment,
) (bool, error) {
	defaultDeploymentForComparison(desired)

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

// defaultDeploymentForComparison applies the API-server defaults which are
// omitted by the typed client scheme. Without them, the controller would write
// the Deployment on every reconciliation and never reach Runtime status.
func defaultDeploymentForComparison(deployment *appsv1.Deployment) {
	if deployment.Spec.Strategy.Type == "" {
		deployment.Spec.Strategy.Type = appsv1.RollingUpdateDeploymentStrategyType
	}
	if deployment.Spec.Strategy.Type == appsv1.RollingUpdateDeploymentStrategyType && deployment.Spec.Strategy.RollingUpdate == nil {
		maxUnavailable := intstr.FromString("25%")
		maxSurge := intstr.FromString("25%")
		deployment.Spec.Strategy.RollingUpdate = &appsv1.RollingUpdateDeployment{
			MaxUnavailable: &maxUnavailable,
			MaxSurge:       &maxSurge,
		}
	}
	if deployment.Spec.RevisionHistoryLimit == nil {
		value := int32(10)
		deployment.Spec.RevisionHistoryLimit = &value
	}
	if deployment.Spec.ProgressDeadlineSeconds == nil {
		value := int32(600)
		deployment.Spec.ProgressDeadlineSeconds = &value
	}

	podSpec := &deployment.Spec.Template.Spec
	if podSpec.RestartPolicy == "" {
		podSpec.RestartPolicy = corev1.RestartPolicyAlways
	}
	if podSpec.TerminationGracePeriodSeconds == nil {
		value := int64(30)
		podSpec.TerminationGracePeriodSeconds = &value
	}
	if podSpec.DNSPolicy == "" {
		podSpec.DNSPolicy = corev1.DNSClusterFirst
	}
	if podSpec.SchedulerName == "" {
		podSpec.SchedulerName = corev1.DefaultSchedulerName
	}
	if podSpec.SecurityContext == nil {
		podSpec.SecurityContext = &corev1.PodSecurityContext{}
	}
	if podSpec.DeprecatedServiceAccount == "" {
		podSpec.DeprecatedServiceAccount = podSpec.ServiceAccountName
	}
	for i := range podSpec.Volumes {
		if downwardAPI := podSpec.Volumes[i].DownwardAPI; downwardAPI != nil && downwardAPI.DefaultMode == nil {
			value := int32(420)
			downwardAPI.DefaultMode = &value
		}
		if downwardAPI := podSpec.Volumes[i].DownwardAPI; downwardAPI != nil {
			for j := range downwardAPI.Items {
				if fieldRef := downwardAPI.Items[j].FieldRef; fieldRef != nil && fieldRef.APIVersion == "" {
					fieldRef.APIVersion = "v1"
				}
			}
		}
	}
	for i := range podSpec.Containers {
		defaultContainerForComparison(&podSpec.Containers[i])
	}
	for i := range podSpec.InitContainers {
		defaultContainerForComparison(&podSpec.InitContainers[i])
	}
}

func defaultContainerForComparison(container *corev1.Container) {
	if container.TerminationMessagePath == "" {
		container.TerminationMessagePath = "/dev/termination-log"
	}
	if container.TerminationMessagePolicy == "" {
		container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	if container.ImagePullPolicy == "" {
		if strings.HasSuffix(container.Image, ":latest") || !strings.Contains(container.Image, ":") {
			container.ImagePullPolicy = corev1.PullAlways
		} else {
			container.ImagePullPolicy = corev1.PullIfNotPresent
		}
	}
	for _, probe := range []*corev1.Probe{container.LivenessProbe, container.ReadinessProbe, container.StartupProbe} {
		if probe == nil {
			continue
		}
		if probe.TimeoutSeconds == 0 {
			probe.TimeoutSeconds = 1
		}
		if probe.PeriodSeconds == 0 {
			probe.PeriodSeconds = 10
		}
		if probe.SuccessThreshold == 0 {
			probe.SuccessThreshold = 1
		}
		if probe.FailureThreshold == 0 {
			probe.FailureThreshold = 3
		}
		if probe.HTTPGet != nil && probe.HTTPGet.Scheme == "" {
			probe.HTTPGet.Scheme = corev1.URISchemeHTTP
		}
	}
	for i := range container.Env {
		if fieldRef := container.Env[i].ValueFrom; fieldRef != nil && fieldRef.FieldRef != nil && fieldRef.FieldRef.APIVersion == "" {
			fieldRef.FieldRef.APIVersion = "v1"
		}
	}
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
