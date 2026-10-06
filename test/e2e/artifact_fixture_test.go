package e2e

// Artifact store assertion helpers and filesystem runtime teardown.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func assertFilesystemArtifactMissing(t *testing.T, claimName, relativePath string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "artifact-inspector-", Namespace: testNamespace},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name: "inspector", Image: bashRuntimeImage(),
				Command:      []string{"test"},
				Args:         []string{"!", "-e", "/artifacts/" + relativePath},
				VolumeMounts: []corev1.VolumeMount{{Name: "artifacts", MountPath: "/artifacts"}},
			}},
			Volumes: []corev1.Volume{{
				Name: "artifacts",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: claimName,
				}},
			}},
		},
	}
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		t.Fatalf("create artifact inspector Pod: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), pod) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil {
			t.Fatalf("get artifact inspector Pod: %v", err)
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded:
			return
		case corev1.PodFailed:
			t.Fatalf("artifact inspector found path %s", relativePath)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for artifact inspector Pod, phase=%s", pod.Status.Phase)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func assertTarGzFile(t *testing.T, path, name, wantContent string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			t.Fatalf("archive does not contain %s", name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name != name {
			continue
		}
		content, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatal(err)
		}
		if string(content) != wantContent {
			t.Fatalf("archive %s = %q, want %q", name, content, wantContent)
		}
		return
	}
}
