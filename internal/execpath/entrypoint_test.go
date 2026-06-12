package execpath

import "testing"

func TestResolveEntrypoint(t *testing.T) {
	tests := []struct {
		name       string
		entrypoint string
		fallback   string
		want       string
		wantErr    bool
	}{
		{name: "default", fallback: "script", want: "script"},
		{name: "clean relative", entrypoint: "./run.sh", fallback: "script", want: "run.sh"},
		{name: "nested relative", entrypoint: "scripts/../run.sh", fallback: "script", want: "run.sh"},
		{name: "absolute", entrypoint: "/tmp/run.sh", fallback: "script", wantErr: true},
		{name: "parent traversal", entrypoint: "../run.sh", fallback: "script", wantErr: true},
		{name: "cleaned traversal", entrypoint: "scripts/../../run.sh", fallback: "script", wantErr: true},
		{name: "dot", entrypoint: ".", fallback: "script", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveEntrypoint(tt.entrypoint, tt.fallback)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveEntrypoint: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}
