package execpath

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ResolveEntrypoint normalizes an entrypoint and rejects any path that would
// escape the execution workspace.
func ResolveEntrypoint(entrypoint, fallback string) (string, error) {
	if entrypoint == "" {
		entrypoint = fallback
	}

	clean := filepath.Clean(entrypoint)
	if clean == "." {
		return "", fmt.Errorf("entrypoint must reference a file within the workspace")
	}
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("entrypoint must be a relative path within the workspace")
	}
	return clean, nil
}
