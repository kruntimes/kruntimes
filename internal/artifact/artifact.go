package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"time"

	"k8s.io/apimachinery/pkg/types"

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

	RunArtifactFinalizer = "kruntimes.io/artifact-cleanup"

	CleanupRunAnnotation    = "kruntimes.io/artifact-cleanup-run"
	CleanupRunUIDAnnotation = "kruntimes.io/artifact-cleanup-run-uid"
)

var ErrSizeLimitExceeded = errors.New("artifact size limit exceeded")

// CleanupJobName returns the stable name of the cleanup Job for a Run UID.
func CleanupJobName(uid types.UID) string {
	sum := sha256.Sum256([]byte(uid))
	return "artifact-cleanup-" + hex.EncodeToString(sum[:8])
}

// PutOptions describes metadata applied while storing an artifact.
type PutOptions struct {
	Name        string
	Type        v1alpha1.ArtifactType
	ContentType string
	Retention   *time.Duration
	// MaxSizeBytes bounds the final representation written to the store.
	// A non-positive value disables the store-level limit.
	MaxSizeBytes int64
}

// Store persists artifact content outside Kubernetes API storage.
type Store interface {
	Put(ctx context.Context, run *v1alpha1.Run, localPath string, opts PutOptions) (v1alpha1.ArtifactRef, error)
	Open(ctx context.Context, ref v1alpha1.ArtifactRef) (io.ReadCloser, error)
	Delete(ctx context.Context, ref v1alpha1.ArtifactRef) error
}

// RunStore can remove every object owned by a Run, including unpublished
// objects left behind by an interrupted multi-artifact upload.
type RunStore interface {
	Store
	DeleteRun(ctx context.Context, run *v1alpha1.Run) error
}
