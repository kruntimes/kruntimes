package controller

import (
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

func TestBuildDeploymentAddsCapacityAnnotationsAndWorkers(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
			Capacity: &v1alpha1.RuntimeCapacity{
				Resources: corev1.ResourceList{
					corev1.ResourceName(v1alpha1.RuntimeResourceRuns): resource.MustParse("3"),
					corev1.ResourceName("gpu"):                        resource.MustParse("1"),
				},
			},
		},
	}

	deploy := (&RuntimeReconciler{}).buildDeployment(rt)
	annotations := deploy.Spec.Template.Annotations
	if annotations[runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns)] != "3" {
		t.Fatalf("runs capacity annotation = %q, want 3", annotations[runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns)])
	}
	if annotations[runtimepod.CapacityAnnotation("gpu")] != "1" {
		t.Fatalf("gpu capacity annotation = %q, want 1", annotations[runtimepod.CapacityAnnotation("gpu")])
	}

	daemon := deploy.Spec.Template.Spec.Containers[1]
	if !slices.Contains(daemon.Args, "--workers=3") {
		t.Fatalf("daemon args = %v, want --workers=3", daemon.Args)
	}
	if !slices.Contains(daemon.Args, "--runtime-endpoint=127.0.0.1:9091") {
		t.Fatalf("daemon args = %v, want IPv4 loopback runtime endpoint", daemon.Args)
	}
	if !slices.Contains(daemon.Args, "--runtime-name=bash") {
		t.Fatalf("daemon args = %v, want runtime name", daemon.Args)
	}
	if len(daemon.Ports) != 1 || daemon.Ports[0].Name != "health" || daemon.Ports[0].ContainerPort != 9094 {
		t.Fatalf("daemon ports = %v, want health port 9094", daemon.Ports)
	}
	if daemon.LivenessProbe == nil ||
		daemon.LivenessProbe.HTTPGet == nil ||
		daemon.LivenessProbe.HTTPGet.Path != "/healthz" {
		t.Fatalf("daemon liveness probe = %#v, want /healthz", daemon.LivenessProbe)
	}
	if daemon.ReadinessProbe == nil ||
		daemon.ReadinessProbe.HTTPGet == nil ||
		daemon.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("daemon readiness probe = %#v, want /readyz", daemon.ReadinessProbe)
	}

	for _, container := range deploy.Spec.Template.Spec.Containers {
		if container.SecurityContext == nil {
			t.Fatalf("container %s missing security context", container.Name)
		}
		if container.SecurityContext.RunAsNonRoot == nil || !*container.SecurityContext.RunAsNonRoot {
			t.Fatalf("container %s runAsNonRoot = %#v, want true", container.Name, container.SecurityContext.RunAsNonRoot)
		}
		if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
			t.Fatalf("container %s allowPrivilegeEscalation = %#v, want false", container.Name, container.SecurityContext.AllowPrivilegeEscalation)
		}
		if container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
			t.Fatalf("container %s readOnlyRootFilesystem = %#v, want true", container.Name, container.SecurityContext.ReadOnlyRootFilesystem)
		}
		if container.SecurityContext.SeccompProfile == nil || container.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Fatalf("container %s seccomp = %#v, want RuntimeDefault", container.Name, container.SecurityContext.SeccompProfile)
		}
		if container.SecurityContext.Capabilities == nil || len(container.SecurityContext.Capabilities.Drop) != 1 || container.SecurityContext.Capabilities.Drop[0] != corev1.Capability("ALL") {
			t.Fatalf("container %s capabilities = %#v, want drop ALL", container.Name, container.SecurityContext.Capabilities)
		}
	}
}

func TestBuildDeploymentRuntimeDaemonImageOverridesDefault(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template:    runtimePodTemplate("bash-runtime:latest"),
			DaemonImage: "custom-runtimed:v1",
		},
	}

	deploy := (&RuntimeReconciler{DefaultDaemonImage: "default-runtimed:v1"}).buildDeployment(rt)
	daemon := deploy.Spec.Template.Spec.Containers[1]
	if daemon.Image != "custom-runtimed:v1" {
		t.Fatalf("daemon image = %q, want runtime override", daemon.Image)
	}
}

func TestBuildDeploymentMergesPodTemplate(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "python", Namespace: "workloads"},
		Spec: v1alpha1.RuntimeSpec{
			Port: 8080,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      map[string]string{"team": "compute", runtimeLabel: "ignored"},
					Annotations: map[string]string{"example.com/owner": "platform"},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: "custom-runtime",
					NodeSelector:       map[string]string{"accelerator": "gpu"},
					ImagePullSecrets:   []corev1.LocalObjectReference{{Name: "registry"}},
					InitContainers: []corev1.Container{{
						Name: "init", Image: "init:v1",
						VolumeMounts: []corev1.VolumeMount{{Name: artifactStoreVolume, MountPath: "/artifacts"}},
					}},
					Containers: []corev1.Container{
						{
							Name:  "runtime",
							Image: "python-runtime:v1",
							Args:  []string{"serve"},
							Env:   []corev1.EnvVar{{Name: "MODE", Value: "worker"}},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Path: "/ready", Port: intstr.FromInt32(8080)},
								},
							},
						},
						{
							Name: "telemetry", Image: "telemetry:v1",
							VolumeMounts: []corev1.VolumeMount{{Name: artifactStoreVolume, MountPath: "/artifacts"}},
						},
						{Name: "runtimed", Image: "ignored:v1"},
					},
					Volumes: []corev1.Volume{
						{Name: "cache", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: workspaceVolume},
						{Name: artifactStoreVolume},
					},
				},
			},
		},
	}

	deploy := (&RuntimeReconciler{}).buildDeployment(rt)
	if !maps.Equal(deploy.Spec.Selector.MatchLabels, map[string]string{
		runtimeLabel: "python", "app": "kruntimes-python",
	}) {
		t.Fatalf("selector labels = %v, want controller-owned stable labels", deploy.Spec.Selector.MatchLabels)
	}
	if deploy.Spec.Template.Labels["team"] != "compute" || deploy.Spec.Template.Labels[runtimeLabel] != "python" {
		t.Fatalf("pod labels = %v, want custom and controller-owned labels", deploy.Spec.Template.Labels)
	}
	if deploy.Spec.Template.Annotations["example.com/owner"] != "platform" {
		t.Fatalf("pod annotations = %v, want custom annotation", deploy.Spec.Template.Annotations)
	}
	podSpec := deploy.Spec.Template.Spec
	if podSpec.ServiceAccountName != "custom-runtime" || podSpec.NodeSelector["accelerator"] != "gpu" {
		t.Fatalf("pod spec customization was not preserved: %#v", podSpec)
	}
	if len(podSpec.ImagePullSecrets) != 1 || podSpec.ImagePullSecrets[0].Name != "registry" {
		t.Fatalf("imagePullSecrets = %v, want registry", podSpec.ImagePullSecrets)
	}
	if len(podSpec.Containers) != 3 || podSpec.Containers[0].Name != "runtime" ||
		podSpec.Containers[1].Name != "telemetry" || podSpec.Containers[2].Name != "runtimed" {
		t.Fatalf("containers = %v, want runtime, telemetry, runtimed", podSpec.Containers)
	}
	if len(podSpec.Volumes) != 2 || podSpec.Volumes[0].Name != "cache" || podSpec.Volumes[1].Name != workspaceVolume {
		t.Fatalf("volumes = %v, want custom cache and controller workspace", podSpec.Volumes)
	}
	if len(podSpec.Containers[1].VolumeMounts) != 0 || len(podSpec.InitContainers[0].VolumeMounts) != 0 {
		t.Fatal("artifact-store mounts from the user template must be removed")
	}
	runtimeContainer := podSpec.Containers[0]
	if runtimeContainer.Image != "python-runtime:v1" || !slices.Equal(runtimeContainer.Args, []string{"serve"}) {
		t.Fatalf("runtime container = %#v, want template image and args", runtimeContainer)
	}
	if runtimeContainer.ReadinessProbe == nil || runtimeContainer.ReadinessProbe.HTTPGet == nil ||
		runtimeContainer.ReadinessProbe.HTTPGet.Path != "/ready" {
		t.Fatalf("runtime readiness probe = %#v, want custom probe", runtimeContainer.ReadinessProbe)
	}
	if !slices.ContainsFunc(runtimeContainer.Ports, func(port corev1.ContainerPort) bool {
		return port.Name == "grpc" && port.ContainerPort == 8080
	}) {
		t.Fatalf("runtime ports = %v, want injected grpc port", runtimeContainer.Ports)
	}
	if rt.Spec.Template.Labels[runtimeLabel] != "ignored" || len(rt.Spec.Template.Spec.Volumes) != 3 ||
		len(rt.Spec.Template.Spec.Containers) != 3 ||
		len(rt.Spec.Template.Spec.Containers[1].VolumeMounts) != 1 ||
		len(rt.Spec.Template.Spec.InitContainers[0].VolumeMounts) != 1 {
		t.Fatal("buildDeployment mutated the Runtime template")
	}
}

func TestBuildDeploymentAppliesWorkspacePVC(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
			Workspace: &v1alpha1.RuntimeWorkspaceSpec{
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: "bash-workspace",
					},
				},
			},
		},
	}

	deploy := (&RuntimeReconciler{}).buildDeployment(rt)
	workspace := deploy.Spec.Template.Spec.Volumes[0]
	if workspace.PersistentVolumeClaim == nil {
		t.Fatalf("workspace volume = %#v, want persistentVolumeClaim", workspace.VolumeSource)
	}
	if got := workspace.PersistentVolumeClaim.ClaimName; got != "bash-workspace" {
		t.Fatalf("workspace claimName = %q, want bash-workspace", got)
	}
	if workspace.EmptyDir != nil {
		t.Fatalf("workspace emptyDir = %#v, want nil when PVC is configured", workspace.EmptyDir)
	}
}

func TestBuildDeploymentAppliesWorkspaceEmptyDirVolumeSource(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
			Workspace: &v1alpha1.RuntimeWorkspaceSpec{
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						Medium:    corev1.StorageMediumMemory,
						SizeLimit: resource.NewQuantity(5*1024*1024*1024, resource.BinarySI),
					},
				},
			},
		},
	}

	deploy := (&RuntimeReconciler{}).buildDeployment(rt)
	workspace := deploy.Spec.Template.Spec.Volumes[0]
	if workspace.EmptyDir == nil {
		t.Fatalf("workspace emptyDir = nil, want configured emptyDir")
	}
	if workspace.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("workspace emptyDir medium = %q, want Memory", workspace.EmptyDir.Medium)
	}
	if workspace.EmptyDir.SizeLimit == nil || workspace.EmptyDir.SizeLimit.String() != "5Gi" {
		t.Fatalf("workspace emptyDir sizeLimit = %v, want 5Gi", workspace.EmptyDir.SizeLimit)
	}
}

func TestBuildDeploymentUsesEmptyDirWorkspaceByDefault(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
		},
	}

	deploy := (&RuntimeReconciler{}).buildDeployment(rt)
	workspace := deploy.Spec.Template.Spec.Volumes[0]
	if workspace.EmptyDir == nil {
		t.Fatalf("workspace emptyDir = nil, want emptyDir volume")
	}
	if workspace.EmptyDir.SizeLimit != nil {
		t.Fatalf("workspace sizeLimit = %v, want nil by default", workspace.EmptyDir.SizeLimit)
	}
}

func TestBuildDeploymentUsesRuntimedServiceAccount(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
		},
	}

	defaultDeploy := (&RuntimeReconciler{}).buildDeployment(rt)
	if got := defaultDeploy.Spec.Template.Spec.ServiceAccountName; got != runtimedDefaultSA {
		t.Fatalf("default serviceAccountName = %q, want %q", got, runtimedDefaultSA)
	}

	deploy := (&RuntimeReconciler{RuntimedServiceAccountName: "team-a-kruntimes-runtimed"}).buildDeployment(rt)
	if got := deploy.Spec.Template.Spec.ServiceAccountName; got != "team-a-kruntimes-runtimed" {
		t.Fatalf("serviceAccountName = %q, want configured name", got)
	}

	rt.Spec.Template.Spec.ServiceAccountName = "custom-runtime-runtimed"
	deploy = (&RuntimeReconciler{RuntimedServiceAccountName: "team-a-kruntimes-runtimed"}).buildDeployment(rt)
	if got := deploy.Spec.Template.Spec.ServiceAccountName; got != "custom-runtime-runtimed" {
		t.Fatalf("serviceAccountName = %q, want Runtime spec override", got)
	}
}

func TestBuildRuntimedRBACUsesNamespaceScopedRole(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "workloads"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
		},
	}
	reconciler := &RuntimeReconciler{}

	serviceAccountName := reconciler.runtimedServiceAccountName(rt)
	serviceAccount := reconciler.buildRuntimedServiceAccount(rt, serviceAccountName)
	if serviceAccount.Name != runtimedDefaultSA || serviceAccount.Namespace != "workloads" {
		t.Fatalf("serviceAccount = %s/%s, want workloads/%s", serviceAccount.Namespace, serviceAccount.Name, runtimedDefaultSA)
	}

	role := reconciler.buildRuntimedRole(rt)
	if role.Name != runtimedRoleName || role.Namespace != "workloads" {
		t.Fatalf("role = %s/%s, want workloads/%s", role.Namespace, role.Name, runtimedRoleName)
	}
	assertPolicyRule(t, role.Rules, "kruntimes.io", "runs", "get", "list", "watch", "update", "patch")
	assertPolicyRule(t, role.Rules, "kruntimes.io", "runs/status", "get", "update", "patch")
	assertPolicyRule(t, role.Rules, "", "pods", "get")
	assertPolicyRule(t, role.Rules, "", "pods/status", "get", "patch")
	assertPolicyRule(t, role.Rules, "", "events", "create", "patch")

	binding := reconciler.buildRuntimedRoleBinding(rt, serviceAccountName)
	if binding.Name != "kruntimes-runtimed-kruntimes-runtimed" || binding.Namespace != "workloads" {
		t.Fatalf("roleBinding = %s/%s, want workloads/kruntimes-runtimed-kruntimes-runtimed", binding.Namespace, binding.Name)
	}
	if binding.RoleRef.APIGroup != rbacv1.GroupName || binding.RoleRef.Kind != "Role" || binding.RoleRef.Name != runtimedRoleName {
		t.Fatalf("roleRef = %#v, want runtimed Role", binding.RoleRef)
	}
	if len(binding.Subjects) != 1 ||
		binding.Subjects[0].Kind != rbacv1.ServiceAccountKind ||
		binding.Subjects[0].Name != runtimedDefaultSA ||
		binding.Subjects[0].Namespace != "workloads" {
		t.Fatalf("subjects = %#v, want workloads/%s ServiceAccount", binding.Subjects, runtimedDefaultSA)
	}
}

func TestBuildRuntimedRBACUsesRuntimeServiceAccountOverride(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "workloads"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
		},
	}
	rt.Spec.Template.Spec.ServiceAccountName = "runtime-specific-runtimed"
	reconciler := &RuntimeReconciler{RuntimedServiceAccountName: "controller-default-runtimed"}

	serviceAccountName := reconciler.runtimedServiceAccountName(rt)
	if serviceAccountName != "runtime-specific-runtimed" {
		t.Fatalf("serviceAccountName = %q, want Runtime spec override", serviceAccountName)
	}

	binding := reconciler.buildRuntimedRoleBinding(rt, serviceAccountName)
	if binding.Subjects[0].Name != "runtime-specific-runtimed" {
		t.Fatalf("subjects = %#v, want Runtime spec service account", binding.Subjects)
	}
}

func TestRuntimedRBACWatchMapsResourcesToRuntimes(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	defaultRuntime := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "default-runtime", Namespace: "workloads"},
		Spec:       v1alpha1.RuntimeSpec{Template: runtimePodTemplate("runtime:v1")},
	}
	customRuntime := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-runtime", Namespace: "workloads"},
		Spec:       v1alpha1.RuntimeSpec{Template: runtimePodTemplate("runtime:v1")},
	}
	customRuntime.Spec.Template.Spec.ServiceAccountName = "custom-runtimed"
	otherNamespace := customRuntime.DeepCopy()
	otherNamespace.Name = "other-runtime"
	otherNamespace.Namespace = "other"

	reconciler := &RuntimeReconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(defaultRuntime, customRuntime, otherNamespace).Build(),
	}

	tests := []struct {
		name   string
		object client.Object
		want   []string
	}{
		{
			name:   "default service account",
			object: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: runtimedDefaultSA, Namespace: "workloads"}},
			want:   []string{"default-runtime"},
		},
		{
			name:   "custom service account",
			object: &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "custom-runtimed", Namespace: "workloads"}},
			want:   []string{"custom-runtime"},
		},
		{
			name:   "shared role",
			object: &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: runtimedRoleName, Namespace: "workloads"}},
			want:   []string{"custom-runtime", "default-runtime"},
		},
		{
			name:   "managed role binding",
			object: &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: runtimedRoleName + "-custom", Namespace: "workloads"}},
			want:   []string{"custom-runtime", "default-runtime"},
		},
		{
			name:   "unrelated role",
			object: &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "workloads"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := reconciler.runtimesForRuntimedRBAC(t.Context(), tt.object)
			got := make([]string, 0, len(requests))
			for _, request := range requests {
				got = append(got, request.Name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("mapped Runtimes = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRuntimedRoleBindingNameTruncatesLongServiceAccountNames(t *testing.T) {
	name := runtimedRoleBindingName(strings.Repeat("a", 253))
	if len(name) > runtimedRBACNameMax {
		t.Fatalf("roleBinding name length = %d, want <= %d: %q", len(name), runtimedRBACNameMax, name)
	}
	if strings.HasSuffix(name, "-") || strings.HasSuffix(name, ".") {
		t.Fatalf("roleBinding name = %q, want DNS-safe suffix", name)
	}
	if name != runtimedRoleBindingName(strings.Repeat("a", 253)) {
		t.Fatalf("roleBinding name must be deterministic")
	}
}

func TestBuildNetworkPolicyDeniesRuntimePodIngressByDefault(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
		},
	}

	networkPolicy := (&RuntimeReconciler{}).buildNetworkPolicy(rt)
	if networkPolicy.Name != "runtime-bash" || networkPolicy.Namespace != "default" {
		t.Fatalf("networkPolicy metadata = %s/%s, want default/runtime-bash", networkPolicy.Namespace, networkPolicy.Name)
	}
	if len(networkPolicy.Spec.Ingress) != 0 {
		t.Fatalf("networkPolicy ingress = %v, want default deny ingress", networkPolicy.Spec.Ingress)
	}
	if !slices.Equal(networkPolicy.Spec.PolicyTypes, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}) {
		t.Fatalf("networkPolicy policyTypes = %v, want ingress only", networkPolicy.Spec.PolicyTypes)
	}
	if networkPolicy.Spec.PodSelector.MatchLabels[runtimeLabel] != "bash" {
		t.Fatalf("networkPolicy selector = %v, want runtime=bash", networkPolicy.Spec.PodSelector.MatchLabels)
	}
	if networkPolicy.Spec.PodSelector.MatchLabels["app"] != "kruntimes-bash" {
		t.Fatalf("networkPolicy selector = %v, want app=kruntimes-bash", networkPolicy.Spec.PodSelector.MatchLabels)
	}
}

func assertPolicyRule(t *testing.T, rules []rbacv1.PolicyRule, apiGroup string, resource string, verbs ...string) {
	t.Helper()
	for _, rule := range rules {
		if slices.Contains(rule.APIGroups, apiGroup) && slices.Contains(rule.Resources, resource) {
			if slices.Equal(rule.Verbs, verbs) {
				return
			}
			t.Fatalf("rule for %s/%s verbs = %v, want %v", apiGroup, resource, rule.Verbs, verbs)
		}
	}
	t.Fatalf("missing rule for %s/%s", apiGroup, resource)
}

func TestBuildDeploymentMountsFilesystemArtifactStoreOnlyIntoRuntimed(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
			ArtifactStore: &v1alpha1.RuntimeArtifactStoreSpec{
				Driver: v1alpha1.ArtifactDriverFilesystem,
				Filesystem: &v1alpha1.FilesystemArtifactStoreSpec{
					VolumeClaimName: "runtime-artifacts",
				},
			},
		},
	}

	deploy := (&RuntimeReconciler{}).buildDeployment(rt)
	podSpec := deploy.Spec.Template.Spec
	if podSpec.SecurityContext == nil || podSpec.SecurityContext.FSGroup == nil || *podSpec.SecurityContext.FSGroup != 65532 {
		t.Fatalf("pod fsGroup = %v, want 65532", podSpec.SecurityContext)
	}
	if len(podSpec.Volumes) != 2 {
		t.Fatalf("volumes = %v, want workspace and artifact-store", podSpec.Volumes)
	}
	artifactVolume := podSpec.Volumes[1]
	if artifactVolume.Name != artifactStoreVolume ||
		artifactVolume.PersistentVolumeClaim == nil ||
		artifactVolume.PersistentVolumeClaim.ClaimName != "runtime-artifacts" {
		t.Fatalf("artifact volume = %#v", artifactVolume)
	}

	runtimeContainer := podSpec.Containers[0]
	if slices.ContainsFunc(runtimeContainer.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == artifactStoreVolume
	}) {
		t.Fatal("runtime container must not mount the artifact PVC")
	}

	daemon := podSpec.Containers[1]
	if !slices.Contains(daemon.Args, "--artifact-store-driver=filesystem") {
		t.Fatalf("daemon args = %v, missing filesystem artifact driver", daemon.Args)
	}
	if !slices.Contains(daemon.Args, "--artifact-store-root="+artifactStorePath) {
		t.Fatalf("daemon args = %v, missing artifact store root", daemon.Args)
	}
	if !slices.Contains(daemon.Args, "--artifact-volume-claim=runtime-artifacts") {
		t.Fatalf("daemon args = %v, missing artifact PVC name", daemon.Args)
	}
	if !slices.ContainsFunc(daemon.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == artifactStoreVolume && m.MountPath == artifactStorePath
	}) {
		t.Fatalf("daemon mounts = %v, missing artifact store", daemon.VolumeMounts)
	}
	if len(daemon.EnvFrom) != 0 {
		t.Fatalf("daemon envFrom = %v, want none", daemon.EnvFrom)
	}
}

func TestBuildDeploymentConfiguresS3ArtifactStoreOnlyInRuntimed(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
			ArtifactStore: &v1alpha1.RuntimeArtifactStoreSpec{
				Driver: v1alpha1.ArtifactDriverS3,
				S3: &v1alpha1.S3ArtifactStoreSpec{
					Bucket:                "runtime-artifacts",
					Prefix:                "tenant-a",
					Region:                "us-east-1",
					Endpoint:              "http://minio.storage.svc:9000",
					ForcePathStyle:        true,
					CredentialsSecretName: "artifact-credentials",
					UploadPartSize:        8 * 1024 * 1024,
					UploadConcurrency:     4,
				},
			},
		},
	}

	deploy := (&RuntimeReconciler{}).buildDeployment(rt)
	podSpec := deploy.Spec.Template.Spec
	if podSpec.SecurityContext != nil {
		t.Fatalf("pod security context = %v, want nil for S3", podSpec.SecurityContext)
	}
	if len(podSpec.Volumes) != 1 || podSpec.Volumes[0].Name != workspaceVolume {
		t.Fatalf("volumes = %v, want workspace only", podSpec.Volumes)
	}

	runtimeContainer := podSpec.Containers[0]
	if len(runtimeContainer.EnvFrom) != 0 {
		t.Fatalf("runtime envFrom = %v, credentials must only be exposed to runtimed", runtimeContainer.EnvFrom)
	}
	if slices.ContainsFunc(runtimeContainer.VolumeMounts, func(m corev1.VolumeMount) bool {
		return m.Name == artifactStoreVolume
	}) {
		t.Fatal("runtime container must not mount an artifact volume")
	}

	daemon := podSpec.Containers[1]
	wantArgs := []string{
		"--artifact-store-driver=s3",
		"--artifact-s3-bucket=runtime-artifacts",
		"--artifact-s3-prefix=tenant-a",
		"--artifact-s3-region=us-east-1",
		"--artifact-s3-endpoint=http://minio.storage.svc:9000",
		"--artifact-s3-force-path-style=true",
		"--artifact-s3-upload-part-size=8388608",
		"--artifact-s3-upload-concurrency=4",
		"--artifact-s3-credentials-secret-name=artifact-credentials",
	}
	for _, arg := range wantArgs {
		if !slices.Contains(daemon.Args, arg) {
			t.Errorf("daemon args = %v, missing %q", daemon.Args, arg)
		}
	}
	if len(daemon.EnvFrom) != 1 ||
		daemon.EnvFrom[0].SecretRef == nil ||
		daemon.EnvFrom[0].SecretRef.Name != "artifact-credentials" {
		t.Fatalf("daemon envFrom = %#v, want artifact-credentials Secret", daemon.EnvFrom)
	}
}

func TestBuildDeploymentOmitsUnsetS3ArtifactStoreOptions(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate("bash-runtime:latest"),
			ArtifactStore: &v1alpha1.RuntimeArtifactStoreSpec{
				Driver: v1alpha1.ArtifactDriverS3,
				S3: &v1alpha1.S3ArtifactStoreSpec{
					Bucket: "runtime-artifacts",
				},
			},
		},
	}

	daemon := (&RuntimeReconciler{}).buildDeployment(rt).Spec.Template.Spec.Containers[1]
	wantArgs := []string{
		"--artifact-store-driver=s3",
		"--artifact-s3-bucket=runtime-artifacts",
	}
	for _, arg := range wantArgs {
		if !slices.Contains(daemon.Args, arg) {
			t.Errorf("daemon args = %v, missing %q", daemon.Args, arg)
		}
	}
	for _, arg := range daemon.Args {
		if slices.ContainsFunc([]string{
			"--artifact-s3-prefix=",
			"--artifact-s3-region=",
			"--artifact-s3-endpoint=",
			"--artifact-s3-force-path-style=",
			"--artifact-s3-upload-part-size=",
			"--artifact-s3-upload-concurrency=",
		}, func(prefix string) bool {
			return strings.HasPrefix(arg, prefix)
		}) {
			t.Errorf("daemon args = %v, contains unset S3 option %q", daemon.Args, arg)
		}
	}
	if len(daemon.EnvFrom) != 0 {
		t.Fatalf("daemon envFrom = %v, want none", daemon.EnvFrom)
	}
}

func runtimePodTemplate(image string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runtime", Image: image}},
		},
	}
}
