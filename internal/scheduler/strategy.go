package scheduler

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// Strategy picks the best pod from a list of candidates for a given run.
type Strategy interface {
	// Name returns the strategy name for configuration and metrics.
	Name() string

	// Select returns the most suitable pod for the run.
	// Returns nil and an error if no pod can be selected.
	Select(candidates []corev1.Pod, usageByPod map[string]int32, run *v1alpha1.Run) (*corev1.Pod, error)
}
