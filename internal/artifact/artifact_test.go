package artifact

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestCleanupJobName(t *testing.T) {
	uid := types.UID("d6ecbc18-4411-4d54-9d8e-032687dc85e8")
	first := CleanupJobName(uid)
	if first != CleanupJobName(uid) {
		t.Fatal("cleanup Job name is not deterministic")
	}
	if len(first) > 63 || first == CleanupJobName(types.UID("different")) {
		t.Fatalf("invalid cleanup Job name %q", first)
	}
}
