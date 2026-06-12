package retry

import (
	"math"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	MaxBackoff = 60 * time.Second

	ReasonTimeout        = "Timeout"
	ReasonPrepareSource  = "PrepareSource"
	ReasonRuntimeExecute = "RuntimeExecute"
	ReasonRuntimeError   = "RuntimeError"
	ReasonExecutionLost  = "ExecutionLost"
	ReasonCancelled      = "Cancelled"
	ReasonPodGone        = "PodGone"
	ReasonPodTerminating = "PodTerminating"
	ReasonPodUnhealthy   = "PodUnhealthy"
)

// WithDefaults fills in default values for a nil or zero RetryPolicy.
func WithDefaults(p *v1alpha1.RetryPolicy) *v1alpha1.RetryPolicy {
	if p == nil {
		return &v1alpha1.RetryPolicy{
			MaxAttempts: 1,
			Backoff:     metav1.Duration{Duration: time.Second},
		}
	}
	policy := p.DeepCopy()
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 1
	}
	if policy.Backoff.Duration <= 0 {
		policy.Backoff = metav1.Duration{Duration: time.Second}
	}
	return policy
}

// Backoff returns the retry delay for the given attempt number.
// attempt=2 is the first retry, attempt=3 is the second retry, and so on.
func Backoff(p *v1alpha1.RetryPolicy, attempt int32) time.Duration {
	retries := max(attempt-2, 0)
	d := time.Duration(float64(p.Backoff.Duration) * math.Pow(2, float64(retries)))
	return time.Duration(min(int64(d), int64(MaxBackoff)))
}

// ShouldRetry reports whether a failed attempt should be retried.
func ShouldRetry(p *v1alpha1.RetryPolicy, attempt int32, reason string) bool {
	if attempt >= p.MaxAttempts {
		return false
	}
	if reason == ReasonCancelled {
		return false
	}
	if len(p.RetryableReasons) == 0 {
		return true
	}
	return slices.Contains(p.RetryableReasons, reason)
}

func CurrentAttempt(statusAttempt int32) int32 {
	if statusAttempt == 0 {
		return 1
	}
	return statusAttempt
}

func NextAttempt(statusAttempt int32) int32 {
	return CurrentAttempt(statusAttempt) + 1
}
