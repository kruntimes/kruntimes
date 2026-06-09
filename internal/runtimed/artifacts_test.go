package runtimed

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

func TestPrepareArtifactStagingClearsPreviousAttempt(t *testing.T) {
	setTestWorkspace(t)
	store := &fakeArtifactStore{}
	c := &Controller{ArtifactStore: store}
	run := artifactTestRun()
	staging := artifactStagingDir(run)
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "old"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := c.prepareArtifactStaging(run)
	if err != nil {
		t.Fatalf("prepareArtifactStaging: %v", err)
	}
	if got != staging {
		t.Fatalf("staging = %q, want %q", got, staging)
	}
	entries, err := os.ReadDir(staging)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("staging entries = %v, want empty", entries)
	}
}

func TestCollectArtifactsStoresSortedTopLevelEntries(t *testing.T) {
	setTestWorkspace(t)
	store := &fakeArtifactStore{}
	c := &Controller{ArtifactStore: store}
	run := artifactTestRun()
	staging := artifactStagingDir(run)
	if err := os.MkdirAll(filepath.Join(staging, "bundle"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "z.txt"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "bundle", "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}

	refs, err := c.collectArtifacts(t.Context(), run)
	if err != nil {
		t.Fatalf("collectArtifacts: %v", err)
	}
	if len(refs) != 2 || refs[0].Name != "bundle" || refs[1].Name != "z.txt" {
		t.Fatalf("refs = %#v, want sorted bundle,z.txt", refs)
	}
	if store.puts[0].options.Type != v1alpha1.ArtifactTypeDirectory {
		t.Fatalf("bundle type = %q", store.puts[0].options.Type)
	}
	if store.puts[1].options.Type != v1alpha1.ArtifactTypeFile {
		t.Fatalf("z.txt type = %q", store.puts[1].options.Type)
	}
}

func TestApplySuccessWritesArtifactRefsOnlyAfterAllUploadsSucceed(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	run := artifactTestRun()
	run.Status.Phase = v1alpha1.RunRunning
	staging := artifactStagingDir(run)
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := os.WriteFile(filepath.Join(staging, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	store := &fakeArtifactStore{failAt: 2}
	c := &Controller{Client: k8sClient, ArtifactStore: store}
	ar := &activeRun{run: run, workDir: filepath.Join(workspacePath, string(run.UID))}

	result, err := c.applySuccess(t.Context(), ar, &pb.StatusResponse{Stdout: "stdout"})
	if err != nil {
		t.Fatalf("applySuccess transient failure: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("requeueAfter = %s, want positive", result.RequeueAfter)
	}
	var unchanged v1alpha1.Run
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &unchanged); err != nil {
		t.Fatal(err)
	}
	if unchanged.Status.Phase != v1alpha1.RunRunning || len(unchanged.Status.ArtifactRefs) != 0 {
		t.Fatalf("status after failed collection = %#v", unchanged.Status)
	}

	store.failAt = 0
	store.puts = nil
	if _, err := c.applySuccess(t.Context(), ar, &pb.StatusResponse{Stdout: "stdout"}); err != nil {
		t.Fatalf("applySuccess retry: %v", err)
	}
	var completed v1alpha1.Run
	if err := k8sClient.Get(t.Context(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Status.Phase != v1alpha1.RunSucceeded || len(completed.Status.ArtifactRefs) != 2 {
		t.Fatalf("completed status = %#v", completed.Status)
	}
	if completed.Status.Message != "execution completed" {
		t.Fatalf("message = %q, want stable success summary", completed.Status.Message)
	}
}

func TestCollectArtifactsRejectsSymlinkBeforeStore(t *testing.T) {
	setTestWorkspace(t)
	store := &fakeArtifactStore{}
	c := &Controller{ArtifactStore: store}
	run := artifactTestRun()
	staging := artifactStagingDir(run)
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(staging, "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(staging, "link")); err != nil {
		t.Fatal(err)
	}

	if _, err := c.collectArtifacts(t.Context(), run); err == nil {
		t.Fatal("collectArtifacts accepted symlink")
	}
	if len(store.puts) != 0 {
		t.Fatalf("store was called %d times", len(store.puts))
	}
}

func TestCollectArtifactsEnforcesSingleAndTotalLimits(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		maxSingle int64
		maxTotal  int64
	}{
		{
			name:      "single",
			files:     map[string]string{"large": "12345"},
			maxSingle: 4,
			maxTotal:  10,
		},
		{
			name:      "total",
			files:     map[string]string{"a": "123", "b": "456"},
			maxSingle: 4,
			maxTotal:  5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setTestWorkspace(t)
			store := &fakeArtifactStore{}
			c := &Controller{
				ArtifactStore:     store,
				MaxArtifactBytes:  tt.maxSingle,
				MaxArtifactsBytes: tt.maxTotal,
			}
			run := artifactTestRun()
			staging := artifactStagingDir(run)
			if err := os.MkdirAll(staging, 0o750); err != nil {
				t.Fatal(err)
			}
			for name, content := range tt.files {
				if err := os.WriteFile(filepath.Join(staging, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := c.collectArtifacts(t.Context(), run)
			if err == nil || !isArtifactInvalid(err) {
				t.Fatalf("error = %v, want ArtifactInvalid", err)
			}
		})
	}
}

func TestApplySuccessTransientStoreFailureRequeuesWithoutChangingRun(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	run := artifactTestRun()
	run.Status.Phase = v1alpha1.RunRunning
	staging := artifactStagingDir(run)
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "a"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	c := &Controller{
		Client:        k8sClient,
		ArtifactStore: &fakeArtifactStore{failAt: 1},
	}
	ar := &activeRun{run: run, workDir: filepath.Join(workspacePath, string(run.UID))}

	result, err := c.applySuccess(t.Context(), ar, &pb.StatusResponse{Stdout: "stdout"})
	if err != nil {
		t.Fatalf("applySuccess: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("requeueAfter = %s, want positive", result.RequeueAfter)
	}
	var current v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != v1alpha1.RunRunning || current.Status.Attempt != 0 {
		t.Fatalf("status changed after transient failure: %#v", current.Status)
	}
}

func TestApplySuccessInvalidArtifactTerminatesWithoutRetry(t *testing.T) {
	setTestWorkspace(t)
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	run := artifactTestRun()
	run.Status.Phase = v1alpha1.RunRunning
	staging := artifactStagingDir(run)
	if err := os.MkdirAll(staging, 0o750); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(staging, "target")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(staging, "link")); err != nil {
		t.Fatal(err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()
	c := &Controller{Client: k8sClient, ArtifactStore: &fakeArtifactStore{}}
	ar := &activeRun{run: run, workDir: filepath.Join(workspacePath, string(run.UID))}

	if _, err := c.applySuccess(t.Context(), ar, &pb.StatusResponse{Stdout: "stdout"}); err != nil {
		t.Fatalf("applySuccess: %v", err)
	}
	var current v1alpha1.Run
	if err := k8sClient.Get(t.Context(), client.ObjectKeyFromObject(run), &current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != v1alpha1.RunFailed {
		t.Fatalf("phase = %s, want Failed", current.Status.Phase)
	}
	completed := findCondition(current.Status.Conditions, "Completed")
	if completed == nil || completed.Reason != "ArtifactInvalid" {
		t.Fatalf("Completed condition = %#v, want ArtifactInvalid", completed)
	}
}

type fakeArtifactStore struct {
	puts   []fakeArtifactPut
	failAt int
}

type fakeArtifactPut struct {
	path    string
	options artifact.PutOptions
}

func (s *fakeArtifactStore) Put(_ context.Context, run *v1alpha1.Run, localPath string, opts artifact.PutOptions) (v1alpha1.ArtifactRef, error) {
	s.puts = append(s.puts, fakeArtifactPut{path: localPath, options: opts})
	if s.failAt > 0 && len(s.puts) == s.failAt {
		return v1alpha1.ArtifactRef{}, errors.New("store unavailable")
	}
	return v1alpha1.ArtifactRef{
		Name:      opts.Name,
		Driver:    v1alpha1.ArtifactDriverFilesystem,
		Type:      opts.Type,
		SizeBytes: 1,
		CreatedAt: metav1.Now(),
		Location: v1alpha1.ArtifactLocation{
			Filesystem: &v1alpha1.FilesystemArtifactLocation{
				Path:            filepath.ToSlash(filepath.Join("namespaces", run.Namespace, "runs", string(run.UID), opts.Name)),
				VolumeClaimName: "artifacts-pvc",
			},
		},
	}, nil
}

func (s *fakeArtifactStore) Open(context.Context, v1alpha1.ArtifactRef) (io.ReadCloser, error) {
	return nil, errors.New("not implemented")
}

func (s *fakeArtifactStore) Delete(context.Context, v1alpha1.ArtifactRef) error {
	return nil
}

func artifactTestRun() *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "artifact-run",
			Namespace: "default",
			UID:       "artifact-run-uid",
		},
		Spec: v1alpha1.RunSpec{Runtime: "bash"},
	}
}

func setTestWorkspace(t *testing.T) {
	t.Helper()
	previous := workspacePath
	workspacePath = t.TempDir()
	t.Cleanup(func() { workspacePath = previous })
}

func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
