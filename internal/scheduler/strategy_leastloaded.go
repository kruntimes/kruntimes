package scheduler

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

// LeastLoaded selects the pod with the fewest Running tasks.
type LeastLoaded struct{}

func (s *LeastLoaded) Name() string { return "least-loaded" }

func (s *LeastLoaded) Select(candidates []corev1.Pod, usageByPod map[string]int32, run *v1alpha1.Run) (*corev1.Pod, error) {
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

		count := usageByPod[pod.Name]
		capacity := runtimepod.RunsCapacity(pod, v1alpha1.RuntimeDefaultRunsCapacity)
		pods = append(pods, podLoad{pod: pod, load: int(count), available: capacity - count})
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
