package scheduler

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// schedulingSnapshot is the consistent scheduler input for one Run key. The
// controller-runtime workqueue owns key activation; a worker builds one
// snapshot and makes one placement decision for that key at a time.
type schedulingSnapshot struct {
	run        *v1alpha1.Run
	request    corev1.ResourceList
	pods       []corev1.Pod
	usageByPod map[string]corev1.ResourceList
	now        time.Time
}

type schedulingPlan struct {
	action   schedulingPlanAction
	selected *corev1.Pod
}

type schedulingPlanAction string

const (
	schedulingPlanWait schedulingPlanAction = "Wait"
	schedulingPlanBind schedulingPlanAction = "Bind"
)

// loadSchedulingSnapshot performs the Snapshot and PreFilter framework
// phases. Resource request validation and the Runtime selector are derived
// once, before Pod filtering starts.
func (r *RunReconciler) loadSchedulingSnapshot(ctx context.Context, run *v1alpha1.Run, request corev1.ResourceList) (*schedulingSnapshot, error) {
	requirement, err := labels.NewRequirement("runtime", selection.Equals, []string{run.Spec.Runtime})
	if err != nil {
		return nil, fmt.Errorf("pre-filter runtime selector: %w", err)
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList, &client.ListOptions{
		Namespace:     run.Namespace,
		LabelSelector: labels.NewSelector().Add(*requirement),
	}); err != nil {
		return nil, fmt.Errorf("snapshot runtime pods: %w", err)
	}

	usageByPod, err := r.assignedRunUsage(ctx, run.Namespace)
	if err != nil {
		return nil, fmt.Errorf("snapshot assigned run usage: %w", err)
	}

	return &schedulingSnapshot{
		run:        run,
		request:    request,
		pods:       podList.Items,
		usageByPod: usageByPod,
		now:        time.Now(),
	}, nil
}

// planSchedulingCycle applies the Filter and Score phases to one snapshot.
// Reserve/Assume is intentionally not part of this core refactor; it requires
// a separate, reviewed local reservation lifecycle.
func (r *RunReconciler) planSchedulingCycle(snapshot *schedulingSnapshot) (schedulingPlan, error) {
	candidates := make([]corev1.Pod, 0, len(snapshot.pods))
	for i := range snapshot.pods {
		pod := &snapshot.pods[i]
		if r.isRuntimePodAvailable(pod, snapshot.now, snapshot.usageByPod[pod.Name], snapshot.request) {
			candidates = append(candidates, *pod)
		}
	}
	if len(candidates) == 0 {
		return schedulingPlan{action: schedulingPlanWait}, nil
	}

	selected, err := r.Strategy.Select(candidates, snapshot.usageByPod, snapshot.run)
	if err != nil {
		return schedulingPlan{}, fmt.Errorf("score runtime pods: %w", err)
	}
	return schedulingPlan{action: schedulingPlanBind, selected: selected}, nil
}
