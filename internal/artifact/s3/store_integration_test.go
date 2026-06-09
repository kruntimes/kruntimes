//go:build integration

package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

func TestMinIOStoreLifecycle(t *testing.T) {
	endpoint := os.Getenv("KRUNTIMES_S3_ENDPOINT")
	bucket := os.Getenv("KRUNTIMES_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("KRUNTIMES_S3_ENDPOINT and KRUNTIMES_S3_BUCKET are required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cfg := Config{
		Bucket:            bucket,
		Prefix:            "/integration/",
		Region:            "us-east-1",
		Endpoint:          endpoint,
		ForcePathStyle:    true,
		UploadPartSize:    5 * 1024 * 1024,
		UploadConcurrency: 2,
	}
	store, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	client := integrationClient(t, ctx, cfg)
	if _, err := client.CreateBucket(ctx, &awss3.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_, _ = client.DeleteBucket(cleanupCtx, &awss3.DeleteBucketInput{Bucket: aws.String(bucket)})
	})

	content := bytes.Repeat([]byte("kruntimes-artifact\n"), 400000)
	localPath := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "integration-run", Namespace: "default", UID: "integration-uid"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
	}

	ref, err := store.Put(ctx, run, localPath, artifact.PutOptions{
		Name:        "artifact.bin",
		Type:        v1alpha1.ArtifactTypeArchive,
		ContentType: "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	wantKey := "integration/namespaces/default/runs/integration-uid/artifact.bin"
	if ref.Location.S3 == nil || ref.Location.S3.Key != wantKey {
		t.Fatalf("S3 key = %#v, want %q", ref.Location.S3, wantKey)
	}
	if ref.SizeBytes != int64(len(content)) {
		t.Fatalf("size = %d, want %d", ref.SizeBytes, len(content))
	}

	head, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(wantKey),
	})
	if err != nil {
		t.Fatalf("head object: %v", err)
	}
	if aws.ToInt64(head.ContentLength) != int64(len(content)) {
		t.Errorf("stored size = %d, want %d", aws.ToInt64(head.ContentLength), len(content))
	}
	if aws.ToString(head.ContentType) != "application/octet-stream" {
		t.Errorf("content type = %q", aws.ToString(head.ContentType))
	}
	if head.Metadata["kruntimes-run-uid"] != "integration-uid" {
		t.Errorf("run UID metadata = %q", head.Metadata["kruntimes-run-uid"])
	}
	if head.Metadata["kruntimes-digest"] != ref.Digest {
		t.Errorf("digest metadata = %q, want %q", head.Metadata["kruntimes-digest"], ref.Digest)
	}

	reader, err := store.Open(ctx, ref)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	got, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read artifact: read=%v close=%v", err, closeErr)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("downloaded artifact content differs")
	}

	if err := store.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	_, err = client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(wantKey),
	})
	var apiErr smithy.APIError
	if err == nil || !errors.As(err, &apiErr) {
		t.Fatalf("HeadObject after Delete() error = %v, want S3 API error", err)
	}
}

func integrationClient(t *testing.T, ctx context.Context, cfg Config) *awss3.Client {
	t.Helper()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	return awss3.NewFromConfig(awsCfg, func(options *awss3.Options) {
		options.BaseEndpoint = aws.String(cfg.Endpoint)
		options.UsePathStyle = cfg.ForcePathStyle
	})
}
