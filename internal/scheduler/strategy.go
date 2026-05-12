package scheduler

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/airconduct/kruntime/api/v1alpha1"
)

// Strategy picks the best pod from a list of candidates for a given task.
type Strategy interface {
	// Name returns the strategy name for configuration and metrics.
	Name() string

	// Select returns the most suitable pod for the task.
	// Returns nil and an error if no pod can be selected.
	Select(ctx context.Context, c client.Client, candidates []corev1.Pod, task *v1alpha1.Task) (*corev1.Pod, error)
}
