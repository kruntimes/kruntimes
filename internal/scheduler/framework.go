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
	run              *v1alpha1.Run
	request          corev1.ResourceList
	pods             []corev1.Pod
	actualUsageByPod map[string]corev1.ResourceList
	usageByPod       map[string]corev1.ResourceList
	runs             []v1alpha1.Run
	affinityTargets  []affinityTarget
	now              time.Time
}

type schedulingPlan struct {
	action   schedulingPlanAction
	selected *corev1.Pod
	message  string
}

type schedulingPlanAction string

const (
	schedulingPlanWait schedulingPlanAction = "Wait"
	schedulingPlanBind schedulingPlanAction = "Bind"
)

func waitingMessageForFilterRejections(runtime string, rejections map[filterReason]int) string {
	message := fmt.Sprintf("waiting for available runtime pods for runtime %q", runtime)
	if rejections[filterReasonRunAffinity] > 0 {
		return fmt.Sprintf("waiting for available runtime pods satisfying required Run affinity for runtime %q", runtime)
	}
	if rejections[filterReasonRunAntiAffinity] > 0 {
		return fmt.Sprintf("waiting for available runtime pods satisfying required Run anti-affinity for runtime %q", runtime)
	}
	return message
}

// filterReason is a bounded reason returned when a candidate Runtime Pod is
// rejected. It is safe to use in status messages and metric labels.
type filterReason string

const (
	filterReasonRuntimePodUnavailable filterReason = "RuntimePodUnavailable"
	filterReasonRunAffinity           filterReason = "UnsatisfiedRunAffinity"
	filterReasonRunAntiAffinity       filterReason = "UnsatisfiedRunAntiAffinity"
)

type filterResult struct {
	feasible bool
	reason   filterReason
}

// schedulingPreFilterState contains Run-specific state prepared once before
// candidate Pods are evaluated. Filter and Score use the same immutable state.
type schedulingPreFilterState struct {
	affinity runAffinity
}

// filterPlugin evaluates one hard scheduling constraint. Plugins receive an
// immutable snapshot and must not mutate Kubernetes objects, reservations, or
// placement state.
type filterPlugin interface {
	Name() string
	Filter(*schedulingSnapshot, *corev1.Pod) filterResult
}

// filterPluginFactory prepares a plugin once per scheduling cycle. It keeps
// selector compilation and other Run-specific work out of the Pod loop.
type filterPluginFactory func(*RunReconciler, *schedulingSnapshot, *schedulingPreFilterState) (filterPlugin, error)

var defaultFilterPluginFactories = []filterPluginFactory{
	newRuntimePodAvailabilityFilter,
	newRunAffinityFilter,
}

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

	runs, actualUsageByPod, err := r.assignedRunUsage(ctx, run.Namespace)
	if err != nil {
		return nil, fmt.Errorf("snapshot assigned run usage: %w", err)
	}
	usageByPod, affinityTargets := r.assumptions.snapshot(actualUsageByPod, runs)

	return &schedulingSnapshot{
		run:              run,
		request:          request,
		pods:             podList.Items,
		actualUsageByPod: actualUsageByPod,
		usageByPod:       usageByPod,
		runs:             runs,
		affinityTargets:  affinityTargets,
		now:              time.Now(),
	}, nil
}

// planSchedulingCycle applies the Filter and Score phases to one snapshot.
func (r *RunReconciler) planSchedulingCycle(snapshot *schedulingSnapshot) (schedulingPlan, error) {
	preFilter, err := r.preFilter(snapshot)
	if err != nil {
		return schedulingPlan{}, err
	}
	filters, err := r.registeredFilterPlugins(snapshot, preFilter)
	if err != nil {
		return schedulingPlan{}, err
	}
	candidates := make([]corev1.Pod, 0, len(snapshot.pods))
	rejections := make(map[filterReason]int)
	for i := range snapshot.pods {
		pod := &snapshot.pods[i]
		feasible := true
		for _, filter := range filters {
			result := filter.Filter(snapshot, pod)
			if result.feasible {
				continue
			}
			rejections[result.reason]++
			feasible = false
			break
		}
		if feasible {
			candidates = append(candidates, *pod)
		}
	}
	if len(candidates) == 0 {
		return schedulingPlan{
			action:  schedulingPlanWait,
			message: waitingMessageForFilterRejections(snapshot.run.Spec.Runtime, rejections),
		}, nil
	}

	candidates = preFilter.affinity.preferredCandidates(candidates)
	selected, err := r.Strategy.Select(candidates, snapshot.usageByPod, snapshot.run)
	if err != nil {
		return schedulingPlan{}, fmt.Errorf("score runtime pods: %w", err)
	}
	return schedulingPlan{action: schedulingPlanBind, selected: selected}, nil
}

func (r *RunReconciler) preFilter(snapshot *schedulingSnapshot) (*schedulingPreFilterState, error) {
	affinity, err := newRunAffinity(snapshot.run, snapshot.affinityTargets)
	if err != nil {
		return nil, fmt.Errorf("pre-filter run affinity: %w", err)
	}
	return &schedulingPreFilterState{affinity: affinity}, nil
}

func (r *RunReconciler) registeredFilterPlugins(snapshot *schedulingSnapshot, preFilter *schedulingPreFilterState) ([]filterPlugin, error) {
	plugins := make([]filterPlugin, 0, len(defaultFilterPluginFactories))
	for _, factory := range defaultFilterPluginFactories {
		plugin, err := factory(r, snapshot, preFilter)
		if err != nil {
			return nil, err
		}
		plugins = append(plugins, plugin)
	}
	return plugins, nil
}
