package controller

import (
	"slices"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	r := &ArtifactCleanupReconciler{Client: k8sClient, CleanerImage: "controller:test"}

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

func TestArtifactCleanupCreatesFilesystemJobWithoutRuntime(t *testing.T) {
	scheme := artifactCleanupScheme(t)
	run := artifactCleanupRun(true, filesystemStoreSpec())
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run).Build()
	r := &ArtifactCleanupReconciler{
		Client: k8sClient, CleanerImage: "controller:test",
		ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry"}},
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var job batchv1.Job
	if err := k8sClient.Get(t.Context(), types.NamespacedName{
		Namespace: run.Namespace, Name: artifact.CleanupJobName(run.UID),
	}, &job); err != nil {
		t.Fatal(err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Image != "controller:test" || container.Command[0] != "/artifact-cleaner" {
		t.Fatalf("cleaner container = %#v", container)
	}
	if !slices.Contains(container.Args, "--filesystem-volume-claim=artifacts-pvc") {
		t.Fatalf("cleaner args = %v", container.Args)
	}
	if len(job.Spec.Template.Spec.Volumes) != 1 ||
		job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "artifacts-pvc" {
		t.Fatalf("cleanup volumes = %#v", job.Spec.Template.Spec.Volumes)
	}
	if len(job.Spec.Template.Spec.ImagePullSecrets) != 1 || job.Spec.Template.Spec.ImagePullSecrets[0].Name != "registry" {
		t.Fatalf("imagePullSecrets = %#v", job.Spec.Template.Spec.ImagePullSecrets)
	}
}

func TestBuildS3ArtifactCleanupJobUsesSecretReference(t *testing.T) {
	store := &v1alpha1.RuntimeArtifactStoreSpec{
		Driver: v1alpha1.ArtifactDriverS3,
		S3: &v1alpha1.S3ArtifactStoreSpec{
			Bucket: "artifacts", Prefix: "prod", Endpoint: "http://minio:9000",
			ForcePathStyle: true, CredentialsSecretName: "s3-credentials",
		},
	}
	run := artifactCleanupRun(true, store)
	r := &ArtifactCleanupReconciler{CleanerImage: "controller:test"}

	job, err := r.buildCleanupJob(run)
	if err != nil {
		t.Fatalf("buildCleanupJob: %v", err)
	}
	container := job.Spec.Template.Spec.Containers[0]
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

func TestArtifactCleanupCompletedJobRemovesFinalizer(t *testing.T) {
	scheme := artifactCleanupScheme(t)
	run := artifactCleanupRun(true, filesystemStoreSpec())
	r := &ArtifactCleanupReconciler{CleanerImage: "controller:test"}
	job, err := r.buildCleanupJob(run)
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run, &job).Build()
	r.Client = k8sClient

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var current v1alpha1.Run
	err = k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &current)
	if err == nil && slices.Contains(current.Finalizers, artifact.RunArtifactFinalizer) {
		t.Fatalf("finalizer was not removed: %v", current.Finalizers)
	}
}

func TestArtifactCleanupFailedJobIsDeletedForRetry(t *testing.T) {
	scheme := artifactCleanupScheme(t)
	run := artifactCleanupRun(true, filesystemStoreSpec())
	r := &ArtifactCleanupReconciler{CleanerImage: "controller:test"}
	job, err := r.buildCleanupJob(run)
	if err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(run, &job).Build()
	r.Client = k8sClient

	result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(run)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.RequeueAfter != artifactCleanupRetry {
		t.Fatalf("RequeueAfter = %s, want %s", result.RequeueAfter, artifactCleanupRetry)
	}
	var currentJob batchv1.Job
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(&job), &currentJob); err == nil {
		t.Fatal("failed cleanup Job was not deleted")
	}
}

func artifactCleanupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
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
