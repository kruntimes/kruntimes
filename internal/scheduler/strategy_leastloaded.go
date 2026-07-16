package scheduler

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

// LeastLoaded selects the pod with the fewest Running tasks.
type LeastLoaded struct{}

func (s *LeastLoaded) Name() string { return "least-loaded" }

func (s *LeastLoaded) Select(ctx context.Context, c client.Client, candidates []corev1.Pod, run *v1alpha1.Run) (*corev1.Pod, error) {
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no candidate pods")
	}

	type podLoad struct {
		pod       *corev1.Pod
		load      int
		available int32
	}

	pods := make([]podLoad, 0, len(candidates))
	for i := range candidates {
		pod := &candidates[i]
		if pod.DeletionTimestamp != nil {
			continue
		}

		var tasks v1alpha1.RunList
		if err := c.List(ctx, &tasks,
			client.InNamespace(pod.Namespace),
		); err != nil {
			continue
		}

		count := 0
		for _, t := range tasks.Items {
			if t.Status.AssignedPod != pod.Name {
				continue
			}
			if t.Status.Phase == v1alpha1.RunScheduled || t.Status.Phase == v1alpha1.RunRunning || t.Status.Phase == v1alpha1.RunReady {
				count++
			}
		}
		capacity := runtimepod.RunsCapacity(pod, v1alpha1.RuntimeDefaultRunsCapacity)
		pods = append(pods, podLoad{pod: pod, load: count, available: capacity - int32(count)})
	}

	if len(pods) == 0 {
		return nil, fmt.Errorf("no available pods")
	}

	sort.Slice(pods, func(i, j int) bool {
		if pods[i].available != pods[j].available {
			return pods[i].available > pods[j].available
		}
		if pods[i].load != pods[j].load {
			return pods[i].load < pods[j].load
		}
		return pods[i].pod.Name < pods[j].pod.Name
	})

	return pods[0].pod, nil
}
