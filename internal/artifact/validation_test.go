package artifact

import (
	"strings"
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "dist.tar.gz"},
		{name: "coverage.xml"},
		{name: "", wantErr: true},
		{name: "../secret", wantErr: true},
		{name: "nested/file", wantErr: true},
		{name: strings.Repeat("a", MaxArtifactNameBytes+1), wantErr: true},
	}

	for _, tt := range tests {
		err := ValidateName(tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateName(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
		}
	}
}

func TestValidateRef(t *testing.T) {
	tests := []struct {
		name    string
		ref     v1alpha1.ArtifactRef
		wantErr bool
	}{
		{
			name: "filesystem",
			ref: v1alpha1.ArtifactRef{
				Name:   "report",
				Driver: DriverFilesystem,
				Type:   v1alpha1.ArtifactTypeFile,
				Location: v1alpha1.ArtifactLocation{
					Filesystem: &v1alpha1.FilesystemArtifactLocation{Path: "namespaces/default/runs/uid/report"},
				},
			},
		},
		{
			name: "s3",
			ref: v1alpha1.ArtifactRef{
				Name:   "report",
				Driver: DriverS3,
				Type:   v1alpha1.ArtifactTypeFile,
				Location: v1alpha1.ArtifactLocation{
					S3: &v1alpha1.S3ArtifactLocation{Bucket: "artifacts", Key: "namespaces/default/runs/uid/report"},
				},
			},
		},
		{
			name: "absolute filesystem path",
			ref: v1alpha1.ArtifactRef{
				Name:   "report",
				Driver: DriverFilesystem,
				Type:   v1alpha1.ArtifactTypeFile,
				Location: v1alpha1.ArtifactLocation{
					Filesystem: &v1alpha1.FilesystemArtifactLocation{Path: "/tmp/report"},
				},
			},
			wantErr: true,
		},
		{
			name: "mixed locations",
			ref: v1alpha1.ArtifactRef{
				Name:   "report",
				Driver: DriverS3,
				Type:   v1alpha1.ArtifactTypeFile,
				Location: v1alpha1.ArtifactLocation{
					Filesystem: &v1alpha1.FilesystemArtifactLocation{Path: "report"},
					S3:         &v1alpha1.S3ArtifactLocation{Bucket: "artifacts", Key: "report"},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateRef(tt.ref)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateRef() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
