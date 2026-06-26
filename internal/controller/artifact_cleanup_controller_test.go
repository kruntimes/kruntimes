package controller

import (
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

func TestArtifactCleanupSnapshotsRuntimeStore(t *testing.T) {
	scheme := artifactCleanupScheme(t)
	run := artifactCleanupRun(false, nil)
	runtimeResource := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: run.Spec.Runtime, Namespace: run.Namespace},
		Spec:       v1alpha1.RuntimeSpec{ArtifactStore: filesystemStoreSpec()},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).WithObjects(run, runtimeResource).Build()
	r := &ArtifactCleanupReconciler{Client: k8sClient, MaintainerImage: "controller:test"}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var current v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.ArtifactStore == nil || current.Status.ArtifactStore.Filesystem == nil ||
		current.Status.ArtifactStore.Filesystem.VolumeClaimName != "artifacts-pvc" {
		t.Fatalf("artifact store snapshot = %#v", current.Status.ArtifactStore)
	}
}

func TestArtifactCleanupEnsuresFilesystemWorkerWithoutRuntime(t *testing.T) {
	scheme := artifactCleanupScheme(t)
	run := artifactCleanupRun(true, filesystemStoreSpec())
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	r := &ArtifactCleanupReconciler{
		Client: k8sClient, MaintainerImage: "controller:test",
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry"}},
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	storeHash, err := artifact.StoreHash(run.Status.ArtifactStore)
	if err != nil {
		t.Fatal(err)
	}
	var deploy appsv1.Deployment
	if err := k8sClient.Get(t.Context(), client.ObjectKey{
		Namespace: run.Namespace, Name: artifact.RuntimeMaintainerName(storeHash),
	}, &deploy); err != nil {
		t.Fatal(err)
	}
	if deploy.Spec.Template.Spec.ServiceAccountName != runtimeMaintainerServiceAccount {
		t.Fatalf("serviceAccountName = %q", deploy.Spec.Template.Spec.ServiceAccountName)
	}
	container := deploy.Spec.Template.Spec.Containers[0]
	if container.Image != "controller:test" || container.Command[0] != "/runtime-maintainer" {
		t.Fatalf("cleaner container = %#v", container)
	}
	for _, want := range []string{"--store-hash=" + storeHash, "--filesystem-volume-claim=artifacts-pvc"} {
		if !slices.Contains(container.Args, want) {
			t.Fatalf("cleaner args = %v, missing %s", container.Args, want)
		}
	}
	if len(deploy.Spec.Template.Spec.Volumes) != 1 ||
		deploy.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "artifacts-pvc" {
		t.Fatalf("cleanup volumes = %#v", deploy.Spec.Template.Spec.Volumes)
	}
	if len(deploy.Spec.Template.Spec.ImagePullSecrets) != 1 || deploy.Spec.Template.Spec.ImagePullSecrets[0].Name != "registry" {
		t.Fatalf("imagePullSecrets = %#v", deploy.Spec.Template.Spec.ImagePullSecrets)
	}

	for _, object := range []client.Object{&corev1.ServiceAccount{}, &rbacv1.Role{}, &rbacv1.RoleBinding{}} {
		if err := k8sClient.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: runtimeMaintainerRole}, object); err != nil {
			if _, ok := object.(*corev1.ServiceAccount); ok {
				err = k8sClient.Get(t.Context(), client.ObjectKey{Namespace: run.Namespace, Name: runtimeMaintainerServiceAccount}, object)
			}
			if err != nil {
				t.Fatalf("get cleanup RBAC object %T: %v", object, err)
			}
		}
	}
}

func TestBuildS3ArtifactCleanupWorkerUsesSecretReference(t *testing.T) {
	store := &v1alpha1.RuntimeArtifactStoreSpec{
		Driver: v1alpha1.ArtifactDriverS3,
		S3: &v1alpha1.S3ArtifactStoreSpec{
			Bucket: "artifacts", Prefix: "prod", Endpoint: "http://minio:9000",
			ForcePathStyle: true, CredentialsSecretName: "s3-credentials",
		},
	}
	run := artifactCleanupRun(true, store)
	r := &ArtifactCleanupReconciler{MaintainerImage: "controller:test"}

	deploy, err := r.buildCleanupWorkerDeployment(run)
	if err != nil {
		t.Fatalf("buildCleanupWorkerDeployment: %v", err)
	}
	container := deploy.Spec.Template.Spec.Containers[0]
	for _, want := range []string{
		"--s3-bucket=artifacts", "--s3-prefix=prod", "--s3-endpoint=http://minio:9000", "--s3-force-path-style=true",
	} {
		if !slices.Contains(container.Args, want) {
			t.Fatalf("cleaner args = %v, missing %s", container.Args, want)
		}
	}
	if len(container.EnvFrom) != 1 || container.EnvFrom[0].SecretRef.Name != "s3-credentials" {
		t.Fatalf("cleaner envFrom = %#v", container.EnvFrom)
	}
}

func TestArtifactCleanupReusesWorkerForSameStoreHash(t *testing.T) {
	scheme := artifactCleanupScheme(t)
	first := artifactCleanupRun(true, filesystemStoreSpec())
	first.Name = "first"
	second := artifactCleanupRun(true, filesystemStoreSpec())
	second.Name = "second"
	second.UID = "second-uid"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(first, second).Build()
	r := &ArtifactCleanupReconciler{Client: k8sClient, MaintainerImage: "controller:test"}

	for _, run := range []*v1alpha1.Run{first, second} {
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
			t.Fatalf("Reconcile(%s): %v", run.Name, err)
		}
	}
	var deployments appsv1.DeploymentList
	if err := k8sClient.List(t.Context(), &deployments, client.InNamespace(first.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(deployments.Items) != 1 {
		t.Fatalf("worker deployments = %d, want 1", len(deployments.Items))
	}
}

func artifactCleanupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func artifactCleanupRun(deleting bool, store *v1alpha1.RuntimeArtifactStoreSpec) *v1alpha1.Run {
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name: "artifact-run", Namespace: "workloads", UID: "artifact-run-uid",
			Finalizers: []string{artifact.RunArtifactFinalizer},
		},
		Spec:   v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{ArtifactStore: store},
	}
	if deleting {
		now := metav1.Now()
		run.DeletionTimestamp = &now
	}
	return run
}

func filesystemStoreSpec() *v1alpha1.RuntimeArtifactStoreSpec {
	return &v1alpha1.RuntimeArtifactStoreSpec{
		Driver: v1alpha1.ArtifactDriverFilesystem,
		Filesystem: &v1alpha1.FilesystemArtifactStoreSpec{
			VolumeClaimName: "artifacts-pvc",
		},
	}
}
