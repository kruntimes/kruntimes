package s3

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

type mockUploader struct {
	input *awss3.PutObjectInput
	body  []byte
	err   error
}

func (m *mockUploader) Upload(_ context.Context, input *awss3.PutObjectInput, _ ...func(*manager.Uploader)) (*manager.UploadOutput, error) {
	m.input = input
	if input.Body != nil {
		m.body, _ = io.ReadAll(input.Body)
	}
	return &manager.UploadOutput{}, m.err
}

type mockObjectClient struct {
	getInput    *awss3.GetObjectInput
	deleteInput *awss3.DeleteObjectInput
	listInput   *awss3.ListObjectsV2Input
	body        string
	getErr      error
	deleteErr   error
	listErr     error
	listOutput  *awss3.ListObjectsV2Output
	deletedKeys []string
}

func (m *mockObjectClient) GetObject(_ context.Context, input *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	m.getInput = input
	if m.getErr != nil {
		return nil, m.getErr
	}
	return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewBufferString(m.body))}, nil
}

func (m *mockObjectClient) DeleteObject(_ context.Context, input *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	m.deleteInput = input
	m.deletedKeys = append(m.deletedKeys, aws.ToString(input.Key))
	return &awss3.DeleteObjectOutput{}, m.deleteErr
}

func (m *mockObjectClient) ListObjectsV2(_ context.Context, input *awss3.ListObjectsV2Input, _ ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error) {
	m.listInput = input
	if m.listErr != nil {
		return nil, m.listErr
	}
	if m.listOutput != nil {
		return m.listOutput, nil
	}
	return &awss3.ListObjectsV2Output{}, nil
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr bool
	}{
		{name: "valid AWS", config: Config{Bucket: "artifacts", Region: "us-east-1"}},
		{name: "valid MinIO", config: Config{Bucket: "artifacts", Endpoint: "http://localhost:9000", ForcePathStyle: true}},
		{name: "missing bucket", config: Config{}, wantErr: true},
		{name: "bucket with slash", config: Config{Bucket: "bad/name"}, wantErr: true},
		{name: "invalid endpoint", config: Config{Bucket: "artifacts", Endpoint: "localhost:9000"}, wantErr: true},
		{name: "negative part size", config: Config{Bucket: "artifacts", UploadPartSize: -1}, wantErr: true},
		{name: "part size below S3 minimum", config: Config{Bucket: "artifacts", UploadPartSize: 1024}, wantErr: true},
		{name: "negative concurrency", config: Config{Bucket: "artifacts", UploadConcurrency: -1}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPut(t *testing.T) {
	const content = "artifact content\n"
	localPath := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(localPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	uploader := &mockUploader{}
	objects := &mockObjectClient{}
	store := newStore(Config{Bucket: "artifacts", Prefix: "/prod/"}, uploader, objects)
	createdAt := time.Date(2026, time.June, 9, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return createdAt }
	retention := 24 * time.Hour
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "build",
			Namespace: "team-a",
			UID:       types.UID("run-uid"),
		},
		Spec: v1alpha1.RunSpec{Runtime: "bash"},
	}

	ref, err := store.Put(context.Background(), run, localPath, artifact.PutOptions{
		Name:        "report.txt",
		Type:        v1alpha1.ArtifactTypeFile,
		ContentType: "text/custom",
		Retention:   &retention,
	})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}

	wantKey := "prod/namespaces/team-a/runs/run-uid/report.txt"
	if got := aws.ToString(uploader.input.Bucket); got != "artifacts" {
		t.Errorf("bucket = %q, want artifacts", got)
	}
	if got := aws.ToString(uploader.input.Key); got != wantKey {
		t.Errorf("key = %q, want %q", got, wantKey)
	}
	if got := aws.ToString(uploader.input.ContentType); got != "text/custom" {
		t.Errorf("content type = %q, want text/custom", got)
	}
	if got := aws.ToInt64(uploader.input.ContentLength); got != int64(len(content)) {
		t.Errorf("content length = %d, want %d", got, len(content))
	}
	if string(uploader.body) != content {
		t.Errorf("uploaded body = %q, want %q", uploader.body, content)
	}

	wantDigest := "sha256:9451dbf6d6e0f04bef1872f32f245b4bfaa8bbbcbc08a73dcd3ddd334ae79759"
	if ref.Digest != wantDigest {
		t.Errorf("digest = %q, want %q", ref.Digest, wantDigest)
	}
	if ref.ContentType != "text/custom" || ref.SizeBytes != int64(len(content)) {
		t.Errorf("ref content metadata = (%q, %d)", ref.ContentType, ref.SizeBytes)
	}
	if !ref.CreatedAt.Time.Equal(createdAt) {
		t.Errorf("createdAt = %v, want %v", ref.CreatedAt.Time, createdAt)
	}
	if ref.Location.S3 == nil || ref.Location.S3.Key != wantKey {
		t.Fatalf("unexpected S3 location: %#v", ref.Location.S3)
	}

	wantMetadata := map[string]string{
		"kruntimes-run-name":          "build",
		"kruntimes-run-uid":           "run-uid",
		"kruntimes-namespace":         "team-a",
		"kruntimes-runtime":           "bash",
		"kruntimes-artifact-name":     "report.txt",
		"kruntimes-artifact-type":     "file",
		"kruntimes-digest":            wantDigest,
		"kruntimes-retention-seconds": "86400",
	}
	for key, want := range wantMetadata {
		if got := uploader.input.Metadata[key]; got != want {
			t.Errorf("metadata[%q] = %q, want %q", key, got, want)
		}
	}
}

func TestPutDetectsContentTypeAndRejectsInvalidInputs(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "artifact")
	if err := os.WriteFile(localPath, []byte("plain text"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newStore(Config{Bucket: "artifacts"}, &mockUploader{}, &mockObjectClient{})
	run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: "uid"}}

	ref, err := store.Put(context.Background(), run, localPath, artifact.PutOptions{Name: "artifact", Type: v1alpha1.ArtifactTypeBlob})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if ref.ContentType != "text/plain; charset=utf-8" {
		t.Errorf("detected content type = %q", ref.ContentType)
	}

	if _, err := store.Put(context.Background(), run, localPath, artifact.PutOptions{Name: "../bad", Type: v1alpha1.ArtifactTypeFile}); err == nil {
		t.Error("Put() accepted an unsafe name")
	}
	if _, err := store.Put(context.Background(), run, localPath, artifact.PutOptions{Name: "bad-type"}); err == nil {
		t.Error("Put() accepted an empty artifact type")
	}
	symlink := filepath.Join(dir, "link")
	if err := os.Symlink(localPath, symlink); err == nil {
		if _, err := store.Put(context.Background(), run, symlink, artifact.PutOptions{Name: "link", Type: v1alpha1.ArtifactTypeFile}); err == nil {
			t.Error("Put() accepted a symbolic link")
		}
	}
}

func TestPutArchivesDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "result.txt"), []byte("result"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploader := &mockUploader{}
	store := newStore(Config{Bucket: "artifacts"}, uploader, &mockObjectClient{})
	run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: "uid"}}

	ref, err := store.Put(t.Context(), run, dir, artifact.PutOptions{
		Name: "bundle",
		Type: v1alpha1.ArtifactTypeDirectory,
	})
	if err != nil {
		t.Fatalf("Put() directory: %v", err)
	}
	if ref.Type != v1alpha1.ArtifactTypeDirectory || ref.ContentType != artifact.DirectoryArchiveContentType {
		t.Fatalf("directory ref = %#v", ref)
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(uploader.body))
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	tarReader := tar.NewReader(gzipReader)
	found := false
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		if header.Name == "nested/result.txt" {
			content, err := io.ReadAll(tarReader)
			if err != nil {
				t.Fatal(err)
			}
			found = string(content) == "result"
		}
	}
	if !found {
		t.Fatal("directory archive missing nested/result.txt")
	}
}

func TestPutRejectsFinalRepresentationAboveLimitBeforeUpload(t *testing.T) {
	tests := []struct {
		name         string
		artifactType v1alpha1.ArtifactType
		prepare      func(*testing.T) string
	}{
		{
			name:         "file",
			artifactType: v1alpha1.ArtifactTypeFile,
			prepare: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "artifact")
				if err := os.WriteFile(path, []byte("too large"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
		{
			name:         "directory archive",
			artifactType: v1alpha1.ArtifactTypeDirectory,
			prepare: func(t *testing.T) string {
				path := t.TempDir()
				if err := os.WriteFile(filepath.Join(path, "artifact"), []byte("content"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uploader := &mockUploader{}
			store := newStore(Config{Bucket: "artifacts"}, uploader, &mockObjectClient{})
			run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: "uid"}}

			_, err := store.Put(t.Context(), run, tt.prepare(t), artifact.PutOptions{
				Name:         "artifact",
				Type:         tt.artifactType,
				MaxSizeBytes: 1,
			})
			if err == nil {
				t.Fatal("Put() accepted a final representation above the size limit")
			}
			if !errors.Is(err, artifact.ErrSizeLimitExceeded) {
				t.Fatalf("Put() error = %v, want size limit error", err)
			}
			if uploader.input != nil {
				t.Fatal("oversized artifact reached the S3 uploader")
			}
		})
	}
}

func TestOpenAndDelete(t *testing.T) {
	objects := &mockObjectClient{body: "stored artifact"}
	store := newStore(Config{Bucket: "artifacts", Prefix: "prod"}, &mockUploader{}, objects)
	ref := s3Ref("artifacts", "prod/namespaces/default/runs/uid/report")

	reader, err := store.Open(context.Background(), ref)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	body, _ := io.ReadAll(reader)
	_ = reader.Close()
	if string(body) != "stored artifact" {
		t.Errorf("Open() body = %q", body)
	}
	if aws.ToString(objects.getInput.Key) != ref.Location.S3.Key {
		t.Errorf("GetObject key = %q", aws.ToString(objects.getInput.Key))
	}

	if err := store.Delete(context.Background(), ref); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if aws.ToString(objects.deleteInput.Key) != ref.Location.S3.Key {
		t.Errorf("DeleteObject key = %q", aws.ToString(objects.deleteInput.Key))
	}
}

func TestOpenAndDeleteRejectForeignRefs(t *testing.T) {
	objects := &mockObjectClient{}
	store := newStore(Config{Bucket: "artifacts", Prefix: "prod"}, &mockUploader{}, objects)

	foreignBucket := s3Ref("other", "prod/namespaces/default/runs/uid/report")
	if _, err := store.Open(context.Background(), foreignBucket); err == nil {
		t.Error("Open() accepted a foreign bucket")
	}
	foreignPrefix := s3Ref("artifacts", "other/namespaces/default/runs/uid/report")
	if err := store.Delete(context.Background(), foreignPrefix); err == nil {
		t.Error("Delete() accepted a key outside the configured prefix")
	}
	if objects.getInput != nil || objects.deleteInput != nil {
		t.Error("foreign refs reached the S3 client")
	}
}

func TestStorePropagatesClientErrors(t *testing.T) {
	wantErr := errors.New("s3 unavailable")
	localPath := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(localPath, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: "uid"}}
	store := newStore(Config{Bucket: "artifacts"}, &mockUploader{err: wantErr}, &mockObjectClient{})
	if _, err := store.Put(context.Background(), run, localPath, artifact.PutOptions{Name: "artifact", Type: v1alpha1.ArtifactTypeFile}); !errors.Is(err, wantErr) {
		t.Fatalf("Put() error = %v, want wrapped client error", err)
	}

	objects := &mockObjectClient{getErr: wantErr, deleteErr: wantErr}
	store = newStore(Config{Bucket: "artifacts"}, &mockUploader{}, objects)
	ref := s3Ref("artifacts", "namespaces/default/runs/uid/artifact")
	if _, err := store.Open(context.Background(), ref); !errors.Is(err, wantErr) {
		t.Fatalf("Open() error = %v, want wrapped client error", err)
	}
	if err := store.Delete(context.Background(), ref); !errors.Is(err, wantErr) {
		t.Fatalf("Delete() error = %v, want wrapped client error", err)
	}
}

func TestDeleteRunDeletesObjectsUnderRunPrefix(t *testing.T) {
	objects := &mockObjectClient{
		listOutput: &awss3.ListObjectsV2Output{
			Contents: []s3types.Object{
				{Key: aws.String("prod/namespaces/default/runs/uid/a")},
				{Key: aws.String("prod/namespaces/default/runs/uid/b")},
			},
		},
	}
	store := newStore(Config{Bucket: "artifacts", Prefix: "prod"}, &mockUploader{}, objects)
	run := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Namespace: "default", UID: "uid"}}
	if err := store.DeleteRun(t.Context(), run); err != nil {
		t.Fatalf("DeleteRun: %v", err)
	}
	if got := aws.ToString(objects.listInput.Prefix); got != "prod/namespaces/default/runs/uid/" {
		t.Fatalf("prefix = %q", got)
	}
	if len(objects.deletedKeys) != 2 {
		t.Fatalf("deleted keys = %v", objects.deletedKeys)
	}
}

func s3Ref(bucket, key string) v1alpha1.ArtifactRef {
	return v1alpha1.ArtifactRef{
		Name:   "report",
		Driver: artifact.DriverS3,
		Type:   v1alpha1.ArtifactTypeFile,
		Location: v1alpha1.ArtifactLocation{
			S3: &v1alpha1.S3ArtifactLocation{Bucket: bucket, Key: key},
		},
	}
}
