package runtimed

import (
	"strings"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
)

// classifyFailureReason determines the machine-readable failure reason
// from the gRPC StatusResponse or Execute error.
func classifyFailureReason(resp *pb.StatusResponse, executeErr error) string {
	if executeErr != nil {
		// gRPC Execute call itself failed (connection error, etc.).
		return runretry.ReasonRuntimeExecute
	}
	errMsg := resp.GetErrorMessage()
	exitCode := resp.GetExitCode()

	if errMsg == "timeout" && exitCode == -1 {
		return runretry.ReasonTimeout
	}
	if errMsg == "no args or script provided" || strings.HasPrefix(errMsg, "mkdir:") || strings.HasPrefix(errMsg, "git clone:") || strings.HasPrefix(errMsg, "git checkout:") || strings.HasPrefix(errMsg, "write inline:") {
		return runretry.ReasonPrepareSource
	}
	if exitCode != 0 {
		return runretry.ReasonRuntimeError
	}
	// Unknown error, default to RuntimeError.
	return runretry.ReasonRuntimeError
}
