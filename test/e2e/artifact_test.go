package e2e

// Artifact scenarios: filesystem export, staging inputs, and artifact cleanup on Run deletion.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/krt"
)

func TestFilesystemArtifacts(t *testing.T) {
	t.Parallel()
	runtimeName := "bash-filesystem-artifacts"
	claimName := "e2e-filesystem-artifacts"
	ensureFilesystemRuntime(t, runtimeName, claimName)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-artifacts-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode: taskMode(
				`mkdir -p "$KRUNTIME_ARTIFACTS_DIR/bundle"; printf report > "$KRUNTIME_ARTIFACTS_DIR/report.txt"; printf nested > "$KRUNTIME_ARTIFACTS_DIR/bundle/data.txt"`,
			),
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create artifact run: %v", err)
	}
	waitForRun(t, run, 30*time.Second)

	if len(run.Status.ArtifactRefs) != 2 {
		t.Fatalf("artifact refs = %#v, want 2", run.Status.ArtifactRefs)
	}
	if run.Status.ArtifactStore == nil || run.Status.ArtifactStore.Filesystem == nil ||
		run.Status.ArtifactStore.Filesystem.VolumeClaimName != claimName {
		t.Fatalf("artifact store cleanup snapshot = %#v", run.Status.ArtifactStore)
	}
	var report, bundle *v1alpha1.ArtifactRef
	for i := range run.Status.ArtifactRefs {
		ref := &run.Status.ArtifactRefs[i]
		if ref.Driver != v1alpha1.ArtifactDriverFilesystem ||
			ref.Location.Filesystem == nil ||
			ref.Location.Filesystem.VolumeClaimName != claimName {
			t.Fatalf("invalid filesystem artifact ref: %#v", ref)
		}
		if ref.Name == "report.txt" {
			report = ref
		}
		if ref.Name == "bundle" {
			bundle = ref
		}
	}
	if report == nil || report.SizeBytes != int64(len("report")) || !strings.HasPrefix(report.Digest, "sha256:") {
		t.Fatalf("report ref = %#v", report)
	}
	if bundle == nil || bundle.Type != v1alpha1.ArtifactTypeDirectory ||
		bundle.ContentType != "application/gzip" ||
		!strings.HasPrefix(bundle.Digest, "sha256:") {
		t.Fatalf("bundle ref = %#v", bundle)
	}

	downloadDir := t.TempDir()
	reportPath := filepath.Join(downloadDir, "report.txt")
	if _, err := krt.DownloadArtifact(context.Background(), k8sClient, restConfig, testNamespace, run.Name, report.Name, reportPath, 19093); err != nil {
		t.Fatalf("download report artifact: %v", err)
	}
	reportContent, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(reportContent) != "report" {
		t.Fatalf("downloaded report = %q, want report", reportContent)
	}

	bundlePath := filepath.Join(downloadDir, "bundle.tar.gz")
	if _, err := krt.DownloadArtifact(context.Background(), k8sClient, restConfig, testNamespace, run.Name, bundle.Name, bundlePath, 19094); err != nil {
		t.Fatalf("download directory artifact: %v", err)
	}
	assertTarGzFile(t, bundlePath, "data.txt", "nested")

	deleteRuntimeAndWait(t, runtimeName, 30*time.Second)

	ttlSeconds := int32(1)
	for i := 0; i < 10; i++ {
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get artifact run for TTL: %v", err)
		}
		run.Spec.TTLSecondsAfterFinished = &ttlSeconds
		if err := k8sClient.Update(context.Background(), run); err == nil {
			break
		}
		if i == 9 {
			t.Fatal("failed to set artifact Run TTL")
		}
		time.Sleep(100 * time.Millisecond)
	}
	waitForRunDeleted(t, run, 30*time.Second)
	assertFilesystemArtifactMissing(t, claimName, report.Location.Filesystem.Path)
}

func TestSessionRunExportsArtifactsOnDrain(t *testing.T) {
	t.Parallel()
	runtimeName := "bash-session-artifacts"
	claimName := "e2e-session-artifacts"
	ensureFilesystemRuntime(t, runtimeName, claimName)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-artifacts-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create Session artifact Run: %v", err)
	}
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunReady)

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	operation, err := executeSessionOperation(t.Context(), baseURL, token, json.RawMessage(`{"command":{"argv":["sh","-c","printf session-report > \"$KRUNTIME_ARTIFACTS_DIR/report.txt\"; printf done"]}}`))
	if err != nil || operation.ExitCode != 0 || string(operation.Stdout) != "done" {
		t.Fatalf("Session artifact command = %#v, %v; want successful done output", operation, err)
	}

	requestRunDrain(t, run)
	waitForRunPhase(t, run, 30*time.Second, v1alpha1.RunSucceeded)
	if run.Status.ArtifactStore == nil || run.Status.ArtifactStore.Filesystem == nil ||
		run.Status.ArtifactStore.Filesystem.VolumeClaimName != claimName {
		t.Fatalf("artifact store cleanup snapshot = %#v", run.Status.ArtifactStore)
	}
	if len(run.Status.ArtifactRefs) != 1 || run.Status.ArtifactRefs[0].Name != "report.txt" {
		t.Fatalf("artifact refs = %#v, want report.txt", run.Status.ArtifactRefs)
	}

	destination := filepath.Join(t.TempDir(), "report.txt")
	if _, err := krt.DownloadArtifact(t.Context(), k8sClient, restConfig, testNamespace, run.Name, "report.txt", destination, 19095); err != nil {
		t.Fatalf("download Session artifact: %v", err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "session-report" {
		t.Fatalf("downloaded Session artifact = %q, want session-report", contents)
	}
}

func TestRunStagesArtifactInputs(t *testing.T) {
	t.Parallel()
	runtimeName := "bash-artifact-inputs"
	claimName := "e2e-artifact-inputs"
	ensureFilesystemRuntime(t, runtimeName, claimName)

	producer := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-artifact-producer-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			Mode:    taskMode(`mkdir -p "$KRUNTIME_ARTIFACTS_DIR/bundle"; printf report > "$KRUNTIME_ARTIFACTS_DIR/report.txt"; printf nested > "$KRUNTIME_ARTIFACTS_DIR/bundle/data.txt"`),
		},
	}
	if err := k8sClient.Create(context.Background(), producer); err != nil {
		t.Fatalf("create artifact producer: %v", err)
	}
	waitForRun(t, producer, 30*time.Second)

	refs := make(map[string]v1alpha1.ArtifactRef, len(producer.Status.ArtifactRefs))
	for _, ref := range producer.Status.ArtifactRefs {
		refs[ref.Name] = ref
	}
	report, reportFound := refs["report.txt"]
	bundle, bundleFound := refs["bundle"]
	if !reportFound || !bundleFound {
		t.Fatalf("producer artifact refs = %#v, want report.txt and bundle", producer.Status.ArtifactRefs)
	}

	consumer := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-artifact-consumer-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			ArtifactInputs: []v1alpha1.ArtifactInput{
				{Ref: report, Path: "inputs/report.txt"},
				{Ref: bundle, Path: "inputs/bundle"},
			},
			Mode: taskMode(`test "$(cat inputs/report.txt)" = report && test "$(cat inputs/bundle/data.txt)" = nested`),
		},
	}
	if err := k8sClient.Create(context.Background(), consumer); err != nil {
		t.Fatalf("create artifact consumer: %v", err)
	}
	waitForRun(t, consumer, 30*time.Second)

	sessionConsumer := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-session-artifact-consumer-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime: runtimeName,
			ArtifactInputs: []v1alpha1.ArtifactInput{
				{Ref: report, Path: "inputs/report.txt"},
				{Ref: bundle, Path: "inputs/bundle"},
			},
			Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
		},
	}
	if err := k8sClient.Create(context.Background(), sessionConsumer); err != nil {
		t.Fatalf("create Session artifact consumer: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), sessionConsumer) })
	waitForRunPhase(t, sessionConsumer, 30*time.Second, v1alpha1.RunReady)
	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), sessionConsumer.Status.Endpoint.URL)
	token := sessionGatewayToken(t, sessionConsumer)
	operation, err := executeSessionOperation(t.Context(), baseURL, token, json.RawMessage(`{"command":{"argv":["sh","-c","printf '%s:%s' \"$(cat inputs/report.txt)\" \"$(cat inputs/bundle/data.txt)\""]}}`))
	if err != nil || operation.ExitCode != 0 || string(operation.Stdout) != "report:nested" {
		t.Fatalf("Session artifact input result = %#v, %v; want report:nested", operation, err)
	}
	requestRunCancel(t, sessionConsumer)
	waitForRunPhase(t, sessionConsumer, 20*time.Second, v1alpha1.RunCancelled)

	missing := report.DeepCopy()
	missing.Name = "missing.txt"
	missing.Location.Filesystem.Path = filepath.ToSlash(filepath.Join("namespaces", testNamespace, "runs", "missing", missing.Name))
	missingConsumer := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-missing-artifact-consumer-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:        runtimeName,
			ArtifactInputs: []v1alpha1.ArtifactInput{{Ref: *missing, Path: "inputs/missing.txt"}},
			Mode:           taskMode(`exit 1`),
		},
	}
	if err := k8sClient.Create(context.Background(), missingConsumer); err != nil {
		t.Fatalf("create missing-artifact consumer: %v", err)
	}
	waitForRunPhase(t, missingConsumer, 30*time.Second, v1alpha1.RunFailed)
	if !strings.Contains(missingConsumer.Status.Message, "open artifact input") {
		t.Fatalf("missing artifact consumer message = %q, want artifact input error", missingConsumer.Status.Message)
	}
}
