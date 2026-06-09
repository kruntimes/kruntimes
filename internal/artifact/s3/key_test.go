package s3

import (
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func TestObjectKey(t *testing.T) {
	tests := []struct {
		name      string
		prefix    string
		namespace string
		runUID    types.UID
		artifact  string
		want      string
		wantErr   bool
	}{
		{
			name:      "without prefix",
			namespace: "default",
			runUID:    "run-uid",
			artifact:  "report.json",
			want:      "namespaces/default/runs/run-uid/report.json",
		},
		{
			name:      "normalized prefix",
			prefix:    "/prod/artifacts/",
			namespace: "team-a",
			runUID:    "run-uid",
			artifact:  "dist.tar.gz",
			want:      "prod/artifacts/namespaces/team-a/runs/run-uid/dist.tar.gz",
		},
		{name: "missing namespace", runUID: "uid", artifact: "report", wantErr: true},
		{name: "missing UID", namespace: "default", artifact: "report", wantErr: true},
		{name: "unsafe name", namespace: "default", runUID: "uid", artifact: "../report", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := objectKey(tt.prefix, tt.namespace, tt.runUID, tt.artifact)
			if (err != nil) != tt.wantErr {
				t.Fatalf("objectKey() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("objectKey() = %q, want %q", got, tt.want)
			}
		})
	}
}
