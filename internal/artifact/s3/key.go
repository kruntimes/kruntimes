package s3

import (
	"fmt"
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/types"

	"github.com/kruntimes/kruntimes/internal/artifact"
)

func normalizePrefix(prefix string) string {
	return strings.Trim(strings.TrimSpace(prefix), "/")
}

func objectKey(prefix, namespace string, runUID types.UID, artifactName string) (string, error) {
	if namespace == "" {
		return "", fmt.Errorf("run namespace is required")
	}
	if runUID == "" {
		return "", fmt.Errorf("run UID is required")
	}
	if err := artifact.ValidateName(artifactName); err != nil {
		return "", err
	}

	parts := []string{"namespaces", namespace, "runs", string(runUID), artifactName}
	if normalized := normalizePrefix(prefix); normalized != "" {
		parts = append([]string{normalized}, parts...)
	}
	return path.Join(parts...), nil
}

func keyRoot(prefix string) string {
	if normalized := normalizePrefix(prefix); normalized != "" {
		return normalized + "/namespaces/"
	}
	return "namespaces/"
}
