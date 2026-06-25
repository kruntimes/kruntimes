package controller

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
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
	r := &ArtifactCleanupReconciler{Client: k8sClient}

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

func TestArtifactCleanupDeletesFilesystemArtifactsInline(t *testing.T) {
	root := t.TempDir()
	scheme := artifactCleanupScheme(t)
	run := artifactCleanupRun(true, filesystemStoreSpec())
	artifactPath := filepath.Join(root, "namespaces", run.Namespace, "runs", string(run.UID), "report")
	if err := os.MkdirAll(filepath.Dir(artifactPath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactPath, []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	r := &ArtifactCleanupReconciler{Client: k8sClient, FilesystemStoreRoot: root}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, err := os.Stat(artifactPath); !os.IsNotExist(err) {
		t.Fatalf("artifact path still exists or stat failed unexpectedly: %v", err)
	}
	var current v1alpha1.Run
	err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &current)
	if err == nil && slices.Contains(current.Finalizers, artifact.RunArtifactFinalizer) {
		t.Fatalf("finalizer was not removed: %v", current.Finalizers)
	}
}

func TestArtifactCleanupRetriesWhenS3CredentialsSecretMissing(t *testing.T) {
	scheme := artifactCleanupScheme(t)
	run := artifactCleanupRun(true, s3StoreSpec("missing-credentials"))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	r := &ArtifactCleanupReconciler{Client: k8sClient}

	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != artifactCleanupRetry {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, artifactCleanupRetry)
	}
	var current v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(current.Finalizers, artifact.RunArtifactFinalizer) {
		t.Fatalf("finalizer was removed despite missing credentials: %v", current.Finalizers)
	}
}

func TestArtifactCleanupRejectsIncompleteS3CredentialsSecret(t *testing.T) {
	scheme := artifactCleanupScheme(t)
	run := artifactCleanupRun(true, s3StoreSpec("s3-credentials"))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s3-credentials", Namespace: run.Namespace},
		Data:       map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("access")},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run, secret).Build()
	r := &ArtifactCleanupReconciler{Client: k8sClient}

	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != artifactCleanupRetry {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, artifactCleanupRetry)
	}
}

func artifactCleanupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
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

func s3StoreSpec(secretName string) *v1alpha1.RuntimeArtifactStoreSpec {
	return &v1alpha1.RuntimeArtifactStoreSpec{
		Driver: v1alpha1.ArtifactDriverS3,
		S3: &v1alpha1.S3ArtifactStoreSpec{
			Bucket: "artifacts", Prefix: "prod", Endpoint: "http://minio:9000",
			ForcePathStyle: true, CredentialsSecretName: secretName,
		},
	}
}
