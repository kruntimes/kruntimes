package krt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	artifactv1 "github.com/kruntimes/kruntimes/api/artifact/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestArtifactListCommand(t *testing.T) {
	run := artifactTestRun()
	k8sClient := newKRTTestClient(t, run)
	command := newArtifactCmdWithClient(k8sClient, nil)
	output := &bytes.Buffer{}
	command.SetOut(output)
	command.SetArgs([]string{"list", "run-1"})

	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	got := output.String()
	for _, want := range []string{"NAME", "result.txt", "file", "text/plain", "sha256:abc"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q does not contain %q", got, want)
		}
	}
}

func TestArtifactDownloadPrefersReadyAssignedPod(t *testing.T) {
	run := artifactTestRun()
	run.Status.AssignedPod = "runtime-b"
	objects := []runtime.Object{
		run,
		readyRuntimePod("runtime-a", "default", "bash"),
		readyRuntimePod("runtime-b", "default", "bash"),
		readyRuntimePod("other-runtime", "default", "python"),
	}
	k8sClient := newKRTTestClient(t, objects...)
	var selectedPod, selectedPath string
	downloader := func(
		_ context.Context,
		podName string,
		_ *v1alpha1.Run,
		_ string,
		outputPath string,
		_ int,
	) (*artifactv1.ArtifactMetadata, error) {
		selectedPod = podName
		selectedPath = outputPath
		return &artifactv1.ArtifactMetadata{
			Name: "result.txt", Type: "file", SizeBytes: 7,
		}, nil
	}
	command := newArtifactCmdWithClient(k8sClient, downloader)
	output := &bytes.Buffer{}
	command.SetOut(output)
	command.SetArgs([]string{"download", "run-1", "result.txt", "-o", "download.txt"})

	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if selectedPod != "runtime-b" {
		t.Fatalf("selected pod = %q, want runtime-b", selectedPod)
	}
	if selectedPath != "download.txt" {
		t.Fatalf("selected path = %q", selectedPath)
	}
}

func TestArtifactDownloadUsesTarGzDefaultForDirectory(t *testing.T) {
	run := artifactTestRun()
	run.Status.ArtifactRefs[0].Name = "bundle"
	run.Status.ArtifactRefs[0].Type = v1alpha1.ArtifactTypeDirectory
	run.Status.AssignedPod = "runtime-a"
	k8sClient := newKRTTestClient(t, run, readyRuntimePod("runtime-a", "default", "bash"))
	var selectedPath string
	downloader := func(
		_ context.Context,
		_ string,
		_ *v1alpha1.Run,
		_ string,
		outputPath string,
		_ int,
	) (*artifactv1.ArtifactMetadata, error) {
		selectedPath = outputPath
		return &artifactv1.ArtifactMetadata{Name: "bundle", Type: "directory"}, nil
	}
	command := newArtifactCmdWithClient(k8sClient, downloader)
	command.SetOut(&bytes.Buffer{})
	command.SetArgs([]string{"download", "run-1", "bundle"})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("ExecuteContext() error = %v", err)
	}
	if selectedPath != "bundle.tar.gz" {
		t.Fatalf("output path = %q, want bundle.tar.gz", selectedPath)
	}
}

func TestSelectRuntimePodFallsBackToReadyPod(t *testing.T) {
	run := artifactTestRun()
	run.Status.AssignedPod = "runtime-old"
	notReady := readyRuntimePod("runtime-old", "default", "bash")
	notReady.Status.Conditions[0].Status = corev1.ConditionFalse
	k8sClient := newKRTTestClient(
		t,
		run,
		notReady,
		readyRuntimePod("runtime-new", "default", "bash"),
	)

	pod, err := selectRuntimePod(t.Context(), k8sClient, run)
	if err != nil {
		t.Fatalf("selectRuntimePod() error = %v", err)
	}
	if pod.Name != "runtime-new" {
		t.Fatalf("pod = %q, want runtime-new", pod.Name)
	}
}

func TestReceiveArtifactPublishesAtomically(t *testing.T) {
	content := bytes.Repeat([]byte("payload"), 20_000)
	digest := sha256.Sum256(content)
	artifactClient := newDownloadTestClient(t, &downloadTestServer{
		metadata: &artifactv1.ArtifactMetadata{
			Name: "result.bin", Type: "blob", ContentType: "application/octet-stream",
			SizeBytes: int64(len(content)), Digest: fmt.Sprintf("sha256:%x", digest),
		},
		content: content,
	})
	outputPath := filepath.Join(t.TempDir(), "result.bin")

	metadata, err := receiveArtifact(
		t.Context(),
		artifactClient,
		"default",
		"run-1",
		"result.bin",
		outputPath,
	)
	if err != nil {
		t.Fatalf("receiveArtifact() error = %v", err)
	}
	if metadata.GetType() != "blob" || metadata.GetContentType() != "application/octet-stream" {
		t.Fatalf("metadata = %#v", metadata)
	}
	got, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("downloaded content differs")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(outputPath), ".*.part-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain: %v", matches)
	}
}

func TestReceiveArtifactRejectsDigestMismatch(t *testing.T) {
	content := []byte("content")
	artifactClient := newDownloadTestClient(t, &downloadTestServer{
		metadata: &artifactv1.ArtifactMetadata{
			Name: "result.bin", Type: "blob", SizeBytes: int64(len(content)),
			Digest: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
		content: content,
	})
	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "result.bin")

	if _, err := receiveArtifact(
		t.Context(),
		artifactClient,
		"default",
		"run-1",
		"result.bin",
		outputPath,
	); err == nil {
		t.Fatal("receiveArtifact() error = nil")
	}
	if entries, err := os.ReadDir(outputDir); err != nil || len(entries) != 0 {
		t.Fatalf("partial files remain after digest mismatch: entries=%v err=%v", entries, err)
	}
}

func TestReceiveArtifactRemovesPartialFile(t *testing.T) {
	artifactClient := newDownloadTestClient(t, &downloadTestServer{
		metadata: &artifactv1.ArtifactMetadata{
			Name: "result.bin", Type: "blob", SizeBytes: 100,
		},
		content: []byte("short"),
	})
	outputDir := t.TempDir()
	outputPath := filepath.Join(outputDir, "result.bin")

	if _, err := receiveArtifact(
		t.Context(),
		artifactClient,
		"default",
		"run-1",
		"result.bin",
		outputPath,
	); err == nil {
		t.Fatal("receiveArtifact() error = nil")
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("output path exists after failure: %v", err)
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial files remain: %v", entries)
	}
}

func artifactTestRun() *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status: v1alpha1.RunStatus{
			ArtifactRefs: []v1alpha1.ArtifactRef{{
				Name:        "result.txt",
				Type:        v1alpha1.ArtifactTypeFile,
				Driver:      v1alpha1.ArtifactDriverFilesystem,
				SizeBytes:   7,
				ContentType: "text/plain",
				Digest:      "sha256:abc",
				Location: v1alpha1.ArtifactLocation{
					Filesystem: &v1alpha1.FilesystemArtifactLocation{Path: "result.txt"},
				},
			}},
		},
	}
}

func readyRuntimePod(name, namespace, runtimeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, Labels: map[string]string{runtimeLabel: runtimeName},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: v1alpha1.RuntimePodRuntimedReadyCondition, Status: corev1.ConditionTrue},
			},
		},
	}
}

func newKRTTestClient(t *testing.T, objects ...runtime.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
}

type downloadTestServer struct {
	artifactv1.UnimplementedArtifactServiceServer
	metadata *artifactv1.ArtifactMetadata
	content  []byte
}

func (s *downloadTestServer) Download(
	_ *artifactv1.DownloadRequest,
	stream grpc.ServerStreamingServer[artifactv1.DownloadResponse],
) error {
	if err := stream.Send(&artifactv1.DownloadResponse{Metadata: s.metadata}); err != nil {
		return err
	}
	for offset := 0; offset < len(s.content); {
		end := min(offset+17_000, len(s.content))
		if err := stream.Send(&artifactv1.DownloadResponse{Data: s.content[offset:end]}); err != nil {
			return err
		}
		offset = end
	}
	return nil
}

func newDownloadTestClient(
	t *testing.T,
	serverImpl artifactv1.ArtifactServiceServer,
) artifactv1.ArtifactServiceClient {
	t.Helper()
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	artifactv1.RegisterArtifactServiceServer(server, serverImpl)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(server.Stop)

	connection, err := grpc.NewClient(
		"passthrough:///bufconn",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return artifactv1.NewArtifactServiceClient(connection)
}
