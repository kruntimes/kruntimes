package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
}
