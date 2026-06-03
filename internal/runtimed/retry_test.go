package runtimed

import (
	"errors"
	"testing"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
)

func TestClassifyFailureReason_FromStatus(t *testing.T) {
	tests := []struct {
		name     string
		resp     *pb.StatusResponse
		expected string
	}{
		{"timeout", &pb.StatusResponse{ErrorMessage: "timeout", ExitCode: -1}, runretry.ReasonTimeout},
		{"mkdir", &pb.StatusResponse{ErrorMessage: "mkdir: permission denied", ExitCode: 0}, runretry.ReasonPrepareSource},
		{"git_clone", &pb.StatusResponse{ErrorMessage: "git clone: connection refused", ExitCode: 0}, runretry.ReasonPrepareSource},
		{"git_checkout", &pb.StatusResponse{ErrorMessage: "git checkout: ref not found", ExitCode: 0}, runretry.ReasonPrepareSource},
		{"write_inline", &pb.StatusResponse{ErrorMessage: "write inline: disk full", ExitCode: 0}, runretry.ReasonPrepareSource},
		{"no_args", &pb.StatusResponse{ErrorMessage: "no args or script provided", ExitCode: 0}, runretry.ReasonPrepareSource},
		{"runtime_error", &pb.StatusResponse{ErrorMessage: "exit status 1", ExitCode: 1, Stderr: "oops"}, runretry.ReasonRuntimeError},
		{"unknown", &pb.StatusResponse{ErrorMessage: "something unknown", ExitCode: 0}, runretry.ReasonRuntimeError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyFailureReason(tt.resp, nil); got != tt.expected {
				t.Errorf("classifyFailureReason() = %s, want %s", got, tt.expected)
			}
		})
	}
}

func TestClassifyFailureReason_ExecuteError(t *testing.T) {
	got := classifyFailureReason(nil, errors.New("connection refused"))
	if got != runretry.ReasonRuntimeExecute {
		t.Errorf("expected RuntimeExecute, got %s", got)
	}
}
