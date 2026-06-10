package controller

import (
	"slices"
	"strings"
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
