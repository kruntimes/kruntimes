package runtimed

import (
	"math"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	maxBackoff = 60 * time.Second

	reasonTimeout        = "Timeout"
	reasonPrepareSource  = "PrepareSource"
	reasonRuntimeExecute = "RuntimeExecute"
	reasonRuntimeError   = "RuntimeError"
	reasonCancelled      = "Cancelled"
)

// withRetryDefaults fills in default values for a nil or zero RetryPolicy.
func withRetryDefaults(p *v1alpha1.RetryPolicy) *v1alpha1.RetryPolicy {
	if p == nil {
		return &v1alpha1.RetryPolicy{
			MaxAttempts: 1,
			Backoff:     metav1.Duration{Duration: time.Second},
		}
	}
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 1
	}
	if p.Backoff.Duration <= 0 {
		p.Backoff = metav1.Duration{Duration: time.Second}
	}
	return p
}

// calcBackoff returns the backoff duration for a given attempt number.
// Uses exponential backoff with 2x multiplier, capped at maxBackoff.
// attempt=2 (first retry) → backoff, attempt=3 → 2*backoff, etc.
func calcBackoff(p *v1alpha1.RetryPolicy, attempt int32) time.Duration {
	// attempt=1 is the initial execution (no backoff).
	// attempt=2 is the first retry → 1 * backoff.
	// attempt=3 is the second retry → 2 * backoff.
	retries := max(attempt-2, 0)
	d := time.Duration(float64(p.Backoff.Duration) * math.Pow(2, float64(retries)))
	return time.Duration(min(int64(d), int64(maxBackoff)))
}

// shouldRetry reports whether a failed run should be retried.
func shouldRetry(p *v1alpha1.RetryPolicy, attempt int32, reason string) bool {
	if attempt >= p.MaxAttempts {
		return false
	}
	if reason == reasonCancelled {
		return false
	}
	if len(p.RetryableReasons) == 0 {
		return true
	}
	return slices.Contains(p.RetryableReasons, reason)
}

// classifyFailureReason determines the machine-readable failure reason
// from the gRPC StatusResponse or Execute error.
func classifyFailureReason(resp *pb.StatusResponse, executeErr error) string {
	if executeErr != nil {
		// gRPC Execute call itself failed (connection error, etc.).
		return reasonRuntimeExecute
	}
	errMsg := resp.GetErrorMessage()
	exitCode := resp.GetExitCode()

	if errMsg == "timeout" && exitCode == -1 {
		return reasonTimeout
	}
	if errMsg == "no args or script provided" || strings.HasPrefix(errMsg, "mkdir:") || strings.HasPrefix(errMsg, "git clone:") || strings.HasPrefix(errMsg, "git checkout:") || strings.HasPrefix(errMsg, "write inline:") {
		return reasonPrepareSource
	}
	if exitCode != 0 {
		return reasonRuntimeError
	}
	// Unknown error, default to RuntimeError.
	return reasonRuntimeError
}
