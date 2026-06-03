package retry

import (
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestWithDefaultsNil(t *testing.T) {
	p := WithDefaults(nil)
	if p.MaxAttempts != 1 {
		t.Errorf("expected MaxAttempts=1, got %d", p.MaxAttempts)
	}
	if p.Backoff.Duration != time.Second {
		t.Errorf("expected Backoff=1s, got %v", p.Backoff.Duration)
	}
}

func TestWithDefaultsZeroValues(t *testing.T) {
	p := WithDefaults(&v1alpha1.RetryPolicy{})
	if p.MaxAttempts != 1 {
		t.Errorf("expected MaxAttempts=1, got %d", p.MaxAttempts)
	}
	if p.Backoff.Duration != time.Second {
		t.Errorf("expected Backoff=1s, got %v", p.Backoff.Duration)
	}
}

func TestWithDefaultsPreservesValues(t *testing.T) {
	p := WithDefaults(&v1alpha1.RetryPolicy{
		MaxAttempts: 5,
		Backoff:     metav1.Duration{Duration: 3 * time.Second},
	})
	if p.MaxAttempts != 5 {
		t.Errorf("expected MaxAttempts=5, got %d", p.MaxAttempts)
	}
	if p.Backoff.Duration != 3*time.Second {
		t.Errorf("expected Backoff=3s, got %v", p.Backoff.Duration)
	}
}

func TestWithDefaultsDoesNotMutateInput(t *testing.T) {
	p := &v1alpha1.RetryPolicy{}
	_ = WithDefaults(p)
	if p.MaxAttempts != 0 {
		t.Errorf("expected input MaxAttempts to remain 0, got %d", p.MaxAttempts)
	}
}

func TestBackoff(t *testing.T) {
	policy := &v1alpha1.RetryPolicy{
		Backoff: metav1.Duration{Duration: time.Second},
	}

	tests := []struct {
		attempt  int32
		expected time.Duration
	}{
		{2, time.Second},
		{3, 2 * time.Second},
		{4, 4 * time.Second},
		{5, 8 * time.Second},
		{10, time.Minute},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt=%d", tt.attempt), func(t *testing.T) {
			got := Backoff(policy, tt.attempt)
			if got != tt.expected {
				t.Errorf("Backoff(attempt=%d) = %v, want %v", tt.attempt, got, tt.expected)
			}
		})
	}
}

func TestCustomBackoff(t *testing.T) {
	policy := &v1alpha1.RetryPolicy{
		Backoff: metav1.Duration{Duration: 5 * time.Second},
	}
	if got := Backoff(policy, 2); got != 5*time.Second {
		t.Errorf("expected 5s, got %v", got)
	}
	if got := Backoff(policy, 3); got != 10*time.Second {
		t.Errorf("expected 10s, got %v", got)
	}
}

func TestShouldRetry(t *testing.T) {
	policy := &v1alpha1.RetryPolicy{MaxAttempts: 3}

	tests := []struct {
		attempt  int32
		reason   string
		expected bool
	}{
		{1, ReasonRuntimeError, true},
		{2, ReasonRuntimeError, true},
		{3, ReasonRuntimeError, false},
		{1, ReasonCancelled, false},
		{1, ReasonTimeout, true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_attempt=%d", tt.reason, tt.attempt), func(t *testing.T) {
			if got := ShouldRetry(policy, tt.attempt, tt.reason); got != tt.expected {
				t.Errorf("ShouldRetry(%d, %s) = %v, want %v", tt.attempt, tt.reason, got, tt.expected)
			}
		})
	}
}

func TestShouldRetryWithRetryableReasons(t *testing.T) {
	policy := &v1alpha1.RetryPolicy{
		MaxAttempts:      3,
		RetryableReasons: []string{ReasonRuntimeError},
	}

	if ShouldRetry(policy, 1, ReasonRuntimeError) != true {
		t.Error("RuntimeError should be retryable")
	}
	if ShouldRetry(policy, 1, ReasonTimeout) != false {
		t.Error("Timeout should NOT be retryable when not listed")
	}
}

func TestShouldRetrySingleAttempt(t *testing.T) {
	policy := &v1alpha1.RetryPolicy{MaxAttempts: 1}
	if ShouldRetry(policy, 1, ReasonRuntimeError) != false {
		t.Error("with MaxAttempts=1, should not retry")
	}
}

func TestAttemptHelpers(t *testing.T) {
	if got := CurrentAttempt(0); got != 1 {
		t.Errorf("CurrentAttempt(0) = %d, want 1", got)
	}
	if got := CurrentAttempt(3); got != 3 {
		t.Errorf("CurrentAttempt(3) = %d, want 3", got)
	}
	if got := NextAttempt(0); got != 2 {
		t.Errorf("NextAttempt(0) = %d, want 2", got)
	}
	if got := NextAttempt(3); got != 4 {
		t.Errorf("NextAttempt(3) = %d, want 4", got)
	}
}
