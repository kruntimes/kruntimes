package artifact

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// ValidateName rejects artifact names that are unsafe or unsuitable as storage keys.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("artifact name is required")
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("artifact name must be valid UTF-8")
	}
	if len(name) > MaxArtifactNameBytes {
		return fmt.Errorf("artifact name exceeds %d bytes", MaxArtifactNameBytes)
	}
	if name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("artifact name %q must not contain path separators", name)
	}
	return nil
}

// ValidateRef checks the common and driver-specific fields of an artifact reference.
func ValidateRef(ref v1alpha1.ArtifactRef) error {
	if err := ValidateName(ref.Name); err != nil {
		return err
	}
	if ref.SizeBytes < 0 {
		return fmt.Errorf("artifact size must not be negative")
	}
	switch ref.Type {
	case v1alpha1.ArtifactTypeFile, v1alpha1.ArtifactTypeDirectory, v1alpha1.ArtifactTypeArchive, v1alpha1.ArtifactTypeBlob:
	default:
		return fmt.Errorf("unsupported artifact type %q", ref.Type)
	}
	switch ref.Driver {
	case DriverFilesystem:
		if ref.Location.Filesystem == nil || ref.Location.S3 != nil {
			return fmt.Errorf("filesystem artifact must contain only a filesystem location")
		}
		if ref.Location.Filesystem.Path == "" || filepath.IsAbs(ref.Location.Filesystem.Path) {
			return fmt.Errorf("filesystem artifact path must be relative")
		}
		clean := filepath.Clean(ref.Location.Filesystem.Path)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("filesystem artifact path escapes the store root")
		}
	case DriverS3:
		if ref.Location.S3 == nil || ref.Location.Filesystem != nil {
			return fmt.Errorf("s3 artifact must contain only an s3 location")
		}
		if ref.Location.S3.Bucket == "" || ref.Location.S3.Key == "" {
			return fmt.Errorf("s3 artifact bucket and key are required")
		}
	default:
		return fmt.Errorf("unsupported artifact driver %q", ref.Driver)
	}
	return nil
}
