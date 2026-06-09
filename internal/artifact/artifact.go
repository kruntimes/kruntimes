package artifact

import (
	"context"
	"io"
	"time"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	DriverFilesystem = v1alpha1.ArtifactDriverFilesystem
	DriverS3         = v1alpha1.ArtifactDriverS3

	OutputsEnv      = "KRUNTIME_OUTPUTS"
	ArtifactsDirEnv = "KRUNTIME_ARTIFACTS_DIR"

	MaxOutputKeys       = 64
	MaxOutputKeyBytes   = 128
	MaxOutputValueBytes = 8 * 1024
	MaxOutputsBytes     = 64 * 1024

	MaxArtifactRefs      = 32
	MaxArtifactNameBytes = 255

	DefaultMaxArtifactBytes  int64 = 1 << 30
	DefaultMaxArtifactsBytes int64 = 10 << 30
)

// PutOptions describes metadata applied while storing an artifact.
type PutOptions struct {
	Name        string
	Type        v1alpha1.ArtifactType
	ContentType string
	Retention   *time.Duration
}

// Store persists artifact content outside Kubernetes API storage.
type Store interface {
	Put(ctx context.Context, run *v1alpha1.Run, localPath string, opts PutOptions) (v1alpha1.ArtifactRef, error)
	Open(ctx context.Context, ref v1alpha1.ArtifactRef) (io.ReadCloser, error)
	Delete(ctx context.Context, ref v1alpha1.ArtifactRef) error
}
