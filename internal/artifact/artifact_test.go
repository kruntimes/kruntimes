package artifact

import (
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestStoreHash(t *testing.T) {
	store := &v1alpha1.RuntimeArtifactStoreSpec{
		Driver: v1alpha1.ArtifactDriverFilesystem,
		Filesystem: &v1alpha1.FilesystemArtifactStoreSpec{
			VolumeClaimName: "artifacts",
		},
	}
	first, err := StoreHash(store)
	if err != nil {
		t.Fatal(err)
	}
	second, err := StoreHash(store.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("StoreHash is not deterministic: %s != %s", first, second)
	}
	other, err := StoreHash(&v1alpha1.RuntimeArtifactStoreSpec{
		Driver: v1alpha1.ArtifactDriverFilesystem,
		Filesystem: &v1alpha1.FilesystemArtifactStoreSpec{
			VolumeClaimName: "other",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == other || len(first) != 16 {
		t.Fatalf("unexpected store hashes: first=%q other=%q", first, other)
	}
}
