package controller

import (
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

func TestBuildDeploymentAddsCapacityAnnotationsAndWorkers(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Image: "bash-runtime:latest",
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

func TestBuildDeploymentAppliesWorkspaceSizeLimit(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Image: "bash-runtime:latest",
			Workspace: &v1alpha1.RuntimeWorkspaceSpec{
				SizeLimit: quantityPtr(resource.MustParse("10Gi")),
			},
		},
	}

	deploy := (&RuntimeReconciler{}).buildDeployment(rt)
	workspace := deploy.Spec.Template.Spec.Volumes[0]
	if workspace.EmptyDir == nil || workspace.EmptyDir.SizeLimit == nil {
		t.Fatalf("workspace emptyDir = %#v, want size limit", workspace.EmptyDir)
	}
	if got := workspace.EmptyDir.SizeLimit.String(); got != "10Gi" {
		t.Fatalf("workspace sizeLimit = %q, want 10Gi", got)
	}
}

func TestBuildDeploymentLeavesWorkspaceSizeLimitUnsetByDefault(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Image: "bash-runtime:latest",
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
			Image: "bash-runtime:latest",
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

	rt.Spec.RuntimedServiceAccountName = "custom-runtime-runtimed"
	deploy = (&RuntimeReconciler{RuntimedServiceAccountName: "team-a-kruntimes-runtimed"}).buildDeployment(rt)
	if got := deploy.Spec.Template.Spec.ServiceAccountName; got != "custom-runtime-runtimed" {
		t.Fatalf("serviceAccountName = %q, want Runtime spec override", got)
	}
}

func TestBuildNetworkPolicyDeniesRuntimePodIngressByDefault(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Image: "bash-runtime:latest",
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

func TestBuildDeploymentMountsFilesystemArtifactStoreOnlyIntoRuntimed(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Image: "bash-runtime:latest",
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
			Image: "bash-runtime:latest",
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
			Image: "bash-runtime:latest",
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

func quantityPtr(q resource.Quantity) *resource.Quantity {
	return &q
}
