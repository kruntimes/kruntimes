package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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

	RunArtifactFinalizer = "kruntimes.io/artifact-cleanup"

	CleanupStoreHashLabel = "kruntimes.io/artifact-store-hash"
)

var ErrSizeLimitExceeded = errors.New("artifact size limit exceeded")

// RuntimeMaintainerName returns the stable name of the long-running runtime maintainer for a store hash.
func RuntimeMaintainerName(storeHash string) string {
	return "runtime-maintainer-" + storeHash
}

// StoreHash returns a stable short hash for an artifact store configuration.
func StoreHash(store *v1alpha1.RuntimeArtifactStoreSpec) (string, error) {
	if store == nil {
		return "", fmt.Errorf("artifact store is required")
	}
	payload, err := json.Marshal(store)
	if err != nil {
		return "", fmt.Errorf("marshal artifact store: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])[:16], nil
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
