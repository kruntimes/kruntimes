package runtimed

import (
	"errors"
	"fmt"
	"testing"
	"time"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestWithRetryDefaults_Nil(t *testing.T) {
	p := withRetryDefaults(nil)
	if p.MaxAttempts != 1 {
		t.Errorf("expected MaxAttempts=1, got %d", p.MaxAttempts)
	}
	if p.Backoff.Duration != time.Second {
		t.Errorf("expected Backoff=1s, got %v", p.Backoff.Duration)
	}
}

func TestWithRetryDefaults_ZeroValues(t *testing.T) {
	p := withRetryDefaults(&v1alpha1.RetryPolicy{})
	if p.MaxAttempts != 1 {
		t.Errorf("expected MaxAttempts=1, got %d", p.MaxAttempts)
	}
	if p.Backoff.Duration != time.Second {
		t.Errorf("expected Backoff=1s, got %v", p.Backoff.Duration)
	}
}

func TestWithRetryDefaults_PreservesValues(t *testing.T) {
	p := withRetryDefaults(&v1alpha1.RetryPolicy{
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

func TestCalcBackoff(t *testing.T) {
	policy := &v1alpha1.RetryPolicy{
		Backoff: metav1.Duration{Duration: time.Second},
	}

	tests := []struct {
		attempt  int32
		expected time.Duration
	}{
		{2, time.Second},      // first retry: 1s * 2^0
		{3, 2 * time.Second},  // second retry: 1s * 2^1
		{4, 4 * time.Second},  // third retry: 1s * 2^2
		{5, 8 * time.Second},  // fourth retry: 1s * 2^3
		{10, 1 * time.Minute}, // capped at maxBackoff (60s), since 256s > 60s
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt=%d", tt.attempt), func(t *testing.T) {
			got := calcBackoff(policy, tt.attempt)
			if got != tt.expected {
				t.Errorf("calcBackoff(attempt=%d) = %v, want %v", tt.attempt, got, tt.expected)
			}
		})
	}
}

func TestCustomBackoff(t *testing.T) {
	policy := &v1alpha1.RetryPolicy{
		Backoff: metav1.Duration{Duration: 5 * time.Second},
	}
	if got := calcBackoff(policy, 2); got != 5*time.Second {
		t.Errorf("expected 5s, got %v", got)
	}
	if got := calcBackoff(policy, 3); got != 10*time.Second {
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
		{1, reasonRuntimeError, true},  // attempt 1 < 3, retryable
		{2, reasonRuntimeError, true},  // attempt 2 < 3, retryable
		{3, reasonRuntimeError, false}, // attempt 3 >= 3, exhausted
		{1, reasonCancelled, false},    // never retry cancelled
		{1, reasonTimeout, true},       // timeout is retryable
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_attempt=%d", tt.reason, tt.attempt), func(t *testing.T) {
			if got := shouldRetry(policy, tt.attempt, tt.reason); got != tt.expected {
				t.Errorf("shouldRetry(%d, %s) = %v, want %v", tt.attempt, tt.reason, got, tt.expected)
			}
		})
	}
}

func TestShouldRetry_WithRetryableReasons(t *testing.T) {
	policy := &v1alpha1.RetryPolicy{
		MaxAttempts:      3,
		RetryableReasons: []string{reasonRuntimeError},
	}

	if shouldRetry(policy, 1, reasonRuntimeError) != true {
		t.Error("RuntimeError should be retryable")
	}
	if shouldRetry(policy, 1, reasonTimeout) != false {
		t.Error("Timeout should NOT be retryable when not listed")
	}
}

func TestShouldRetry_SingleAttempt(t *testing.T) {
	policy := &v1alpha1.RetryPolicy{MaxAttempts: 1}
	if shouldRetry(policy, 1, reasonRuntimeError) != false {
		t.Error("with MaxAttempts=1, should not retry")
	}
}

func TestClassifyFailureReason_FromStatus(t *testing.T) {
	tests := []struct {
		name     string
		resp     *pb.StatusResponse
		expected string
	}{
		{"timeout", &pb.StatusResponse{ErrorMessage: "timeout", ExitCode: -1}, reasonTimeout},
		{"mkdir", &pb.StatusResponse{ErrorMessage: "mkdir: permission denied", ExitCode: 0}, reasonPrepareSource},
		{"git_clone", &pb.StatusResponse{ErrorMessage: "git clone: connection refused", ExitCode: 0}, reasonPrepareSource},
		{"git_checkout", &pb.StatusResponse{ErrorMessage: "git checkout: ref not found", ExitCode: 0}, reasonPrepareSource},
		{"write_inline", &pb.StatusResponse{ErrorMessage: "write inline: disk full", ExitCode: 0}, reasonPrepareSource},
		{"no_args", &pb.StatusResponse{ErrorMessage: "no args or script provided", ExitCode: 0}, reasonPrepareSource},
		{"runtime_error", &pb.StatusResponse{ErrorMessage: "exit status 1", ExitCode: 1, Stderr: "oops"}, reasonRuntimeError},
		{"unknown", &pb.StatusResponse{ErrorMessage: "something unknown", ExitCode: 0}, reasonRuntimeError},
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
	if got != reasonRuntimeExecute {
		t.Errorf("expected RuntimeExecute, got %s", got)
	}
}
