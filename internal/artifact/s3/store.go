package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

type uploadClient interface {
	Upload(context.Context, *awss3.PutObjectInput, ...func(*manager.Uploader)) (*manager.UploadOutput, error)
}

type objectClient interface {
	GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	DeleteObject(context.Context, *awss3.DeleteObjectInput, ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error)
}

// Store persists artifacts in an S3-compatible object store.
type Store struct {
	bucket   string
	prefix   string
	uploader uploadClient
	objects  objectClient
	now      func() time.Time
}

var _ artifact.Store = (*Store)(nil)

func newStore(cfg Config, uploader uploadClient, objects objectClient) *Store {
	return &Store{
		bucket:   cfg.Bucket,
		prefix:   normalizePrefix(cfg.Prefix),
		uploader: uploader,
		objects:  objects,
		now:      time.Now,
	}
}

// Put uploads a local file and returns its compact S3 artifact reference.
func (s *Store) Put(ctx context.Context, run *v1alpha1.Run, localPath string, opts artifact.PutOptions) (v1alpha1.ArtifactRef, error) {
	if run == nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("run is required")
	}
	if err := artifact.ValidateName(opts.Name); err != nil {
		return v1alpha1.ArtifactRef{}, err
	}
	switch opts.Type {
	case v1alpha1.ArtifactTypeFile, v1alpha1.ArtifactTypeDirectory, v1alpha1.ArtifactTypeArchive, v1alpha1.ArtifactTypeBlob:
	default:
		return v1alpha1.ArtifactRef{}, fmt.Errorf("unsupported artifact type %q", opts.Type)
	}

	file, err := os.Open(localPath)
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("open artifact %q: %w", localPath, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("stat artifact %q: %w", localPath, err)
	}
	if info.IsDir() {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("artifact %q is a directory; the s3 backend requires directories to be archived before upload", localPath)
	}
	if !info.Mode().IsRegular() {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("artifact %q must be a regular file", localPath)
	}

	contentType, digest, err := inspectFile(file, localPath, opts.ContentType)
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("inspect artifact %q: %w", localPath, err)
	}
	key, err := objectKey(s.prefix, run.Namespace, run.UID, opts.Name)
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("build artifact object key: %w", err)
	}

	metadata := map[string]string{
		"kruntimes-run-name":      run.Name,
		"kruntimes-run-uid":       string(run.UID),
		"kruntimes-namespace":     run.Namespace,
		"kruntimes-runtime":       run.Spec.Runtime,
		"kruntimes-artifact-name": opts.Name,
		"kruntimes-artifact-type": string(opts.Type),
		"kruntimes-digest":        digest,
	}
	if opts.Retention != nil {
		metadata["kruntimes-retention-seconds"] = strconv.FormatInt(int64(opts.Retention.Seconds()), 10)
	}

	_, err = s.uploader.Upload(ctx, &awss3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          file,
		ContentLength: aws.Int64(info.Size()),
		ContentType:   aws.String(contentType),
		Metadata:      metadata,
	})
	if err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("upload artifact to s3://%s/%s: %w", s.bucket, key, err)
	}

	ref := v1alpha1.ArtifactRef{
		Name:   opts.Name,
		Driver: artifact.DriverS3,
		Type:   opts.Type,
		Location: v1alpha1.ArtifactLocation{
			S3: &v1alpha1.S3ArtifactLocation{Bucket: s.bucket, Key: key},
		},
		SizeBytes:   info.Size(),
		Digest:      digest,
		ContentType: contentType,
		CreatedAt:   metav1.NewTime(s.now().UTC()),
	}
	if err := artifact.ValidateRef(ref); err != nil {
		return v1alpha1.ArtifactRef{}, fmt.Errorf("validate uploaded artifact reference: %w", err)
	}
	return ref, nil
}

// Open returns a streaming reader for an artifact owned by this store.
func (s *Store) Open(ctx context.Context, ref v1alpha1.ArtifactRef) (io.ReadCloser, error) {
	location, err := s.validateRef(ref)
	if err != nil {
		return nil, err
	}
	output, err := s.objects.GetObject(ctx, &awss3.GetObjectInput{
		Bucket: aws.String(location.Bucket),
		Key:    aws.String(location.Key),
	})
	if err != nil {
		return nil, fmt.Errorf("open artifact s3://%s/%s: %w", location.Bucket, location.Key, err)
	}
	return output.Body, nil
}

// Delete removes an artifact owned by this store.
func (s *Store) Delete(ctx context.Context, ref v1alpha1.ArtifactRef) error {
	location, err := s.validateRef(ref)
	if err != nil {
		return err
	}
	_, err = s.objects.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(location.Bucket),
		Key:    aws.String(location.Key),
	})
	if err != nil {
		return fmt.Errorf("delete artifact s3://%s/%s: %w", location.Bucket, location.Key, err)
	}
	return nil
}

func (s *Store) validateRef(ref v1alpha1.ArtifactRef) (*v1alpha1.S3ArtifactLocation, error) {
	if err := artifact.ValidateRef(ref); err != nil {
		return nil, fmt.Errorf("invalid artifact reference: %w", err)
	}
	location := ref.Location.S3
	if ref.Driver != artifact.DriverS3 || location == nil {
		return nil, fmt.Errorf("artifact reference is not an s3 artifact")
	}
	if location.Bucket != s.bucket {
		return nil, fmt.Errorf("artifact bucket %q does not match configured bucket %q", location.Bucket, s.bucket)
	}
	if !strings.HasPrefix(location.Key, keyRoot(s.prefix)) {
		return nil, fmt.Errorf("artifact key %q is outside configured prefix", location.Key)
	}
	return location, nil
}

func inspectFile(file *os.File, localPath, configuredContentType string) (string, string, error) {
	hash := sha256.New()
	header := make([]byte, 512)
	n, readErr := io.ReadFull(file, header)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return "", "", readErr
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", err
	}
	if _, err := io.Copy(hash, file); err != nil {
		return "", "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", "", err
	}

	contentType := configuredContentType
	if contentType == "" {
		contentType = mime.TypeByExtension(filepath.Ext(localPath))
	}
	if contentType == "" {
		contentType = http.DetectContentType(header[:n])
	}
	return contentType, "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
