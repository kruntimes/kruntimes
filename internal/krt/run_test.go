package krt

import (
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestRunTerminalResult(t *testing.T) {
	tests := []struct {
		name    string
		phase   v1alpha1.RunPhase
		done    bool
		wantErr string
	}{
		{name: "pending", phase: v1alpha1.RunPending},
		{name: "scheduled", phase: v1alpha1.RunScheduled},
		{name: "running", phase: v1alpha1.RunRunning},
		{name: "succeeded", phase: v1alpha1.RunSucceeded, done: true},
		{name: "failed", phase: v1alpha1.RunFailed, done: true, wantErr: "run failed"},
		{name: "timeout", phase: v1alpha1.RunTimeout, done: true, wantErr: "run timed out"},
		{name: "cancelled", phase: v1alpha1.RunCancelled, done: true, wantErr: "run cancelled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			done, err := runTerminalResult(tt.phase)
			if done != tt.done {
				t.Fatalf("done = %v, want %v", done, tt.done)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
