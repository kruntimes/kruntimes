package runtimepod

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func CapacityAnnotation(resourceName string) string {
	return v1alpha1.RuntimePodCapacityAnnotationPrefix + resourceName
}

func CapacityAnnotations(rt *v1alpha1.Runtime) map[string]string {
	if rt.Spec.Capacity == nil || len(rt.Spec.Capacity.Resources) == 0 {
		return nil
	}

	annotations := make(map[string]string, len(rt.Spec.Capacity.Resources))
	for name, qty := range rt.Spec.Capacity.Resources {
		if qty.Sign() <= 0 {
			continue
		}
		annotations[CapacityAnnotation(string(name))] = qty.String()
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

func RunsCapacityFromRuntime(rt *v1alpha1.Runtime, fallback int32) int32 {
	if rt.Spec.Capacity == nil {
		return fallback
	}
	qty, ok := rt.Spec.Capacity.Resources[corev1.ResourceName(v1alpha1.RuntimeResourceRuns)]
	if !ok {
		return fallback
	}
	return quantityToPositiveInt32(qty, fallback)
}

func RunsCapacity(pod *corev1.Pod, fallback int32) int32 {
	runs := corev1.ResourceName(v1alpha1.RuntimeResourceRuns)
	quantity := Capacity(pod, corev1.ResourceList{
		runs: *resource.NewQuantity(int64(fallback), resource.DecimalSI),
	})[runs]
	return quantityToPositiveInt32(quantity, fallback)
}

// Capacity returns the complete logical capacity advertised by a Runtime Pod.
// Fallback capacity is used for resources not advertised by the Pod, which
// preserves compatibility with Pods created by an older Runtime controller.
// Callers should only include resources that are safe to assume without a Pod
// capacity annotation.
func Capacity(pod *corev1.Pod, fallback corev1.ResourceList) corev1.ResourceList {
	capacity := fallback.DeepCopy()
	if capacity == nil {
		capacity = corev1.ResourceList{}
	}
	if pod != nil {
		for key, raw := range pod.Annotations {
			if !strings.HasPrefix(key, v1alpha1.RuntimePodCapacityAnnotationPrefix) {
				continue
			}
			name := strings.TrimPrefix(key, v1alpha1.RuntimePodCapacityAnnotationPrefix)
			if name == "" {
				continue
			}
			quantity, err := resource.ParseQuantity(raw)
			if err != nil || quantity.Sign() <= 0 {
				continue
			}
			capacity[corev1.ResourceName(name)] = quantity
		}
	}
	return capacity
}

// Fits reports whether the complete request can be added to current usage
// without exceeding the complete capacity vector. A requested resource without
// a Pod capacity annotation is infeasible.
func Fits(capacity, usage, request corev1.ResourceList) bool {
	for name, requested := range request {
		if requested.Sign() <= 0 {
			continue
		}
		available, ok := capacity[name]
		if !ok {
			return false
		}
		used := usage[name].DeepCopy()
		used.Add(requested)
		if used.Cmp(available) > 0 {
			return false
		}
	}
	return true
}

func IsRuntimedReady(pod *corev1.Pod) bool {
	cond := FindRuntimedReadyCondition(pod)
	return cond != nil && cond.Status == corev1.ConditionTrue
}

func FindRuntimedReadyCondition(pod *corev1.Pod) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type == v1alpha1.RuntimePodRuntimedReadyCondition {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

func SetRuntimedReadyCondition(pod *corev1.Pod, status corev1.ConditionStatus, reason, message string, now metav1.Time) {
	cond := corev1.PodCondition{
		Type:               v1alpha1.RuntimePodRuntimedReadyCondition,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastProbeTime:      now,
		LastTransitionTime: now,
	}

	for i := range pod.Status.Conditions {
		if pod.Status.Conditions[i].Type != v1alpha1.RuntimePodRuntimedReadyCondition {
			continue
		}
		if pod.Status.Conditions[i].Status == status {
			cond.LastTransitionTime = pod.Status.Conditions[i].LastTransitionTime
		}
		pod.Status.Conditions[i] = cond
		return
	}
	pod.Status.Conditions = append(pod.Status.Conditions, cond)
}

func FormatCapacityAnnotations(annotations map[string]string) string {
	if len(annotations) == 0 {
		return ""
	}
	keys := make([]string, 0, len(annotations))
	for key := range annotations {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", key, annotations[key]))
	}
	return strings.Join(parts, ",")
}

func quantityToPositiveInt32(qty resource.Quantity, fallback int32) int32 {
	if qty.Sign() <= 0 {
		return fallback
	}
	value, ok := qty.AsInt64()
	if !ok || value <= 0 {
		return fallback
	}
	if value > int64(^uint32(0)>>1) {
		return fallback
	}
	return int32(value)
}

func ParsePositiveInt(raw string, fallback int32) int32 {
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value <= 0 {
		return fallback
	}
	return int32(value)
}

func FreshRuntimedReady(pod *corev1.Pod, now time.Time, staleAfter time.Duration) bool {
	cond := FindRuntimedReadyCondition(pod)
	if cond == nil || cond.Status != corev1.ConditionTrue {
		return false
	}
	if staleAfter <= 0 {
		return true
	}
	return now.Sub(cond.LastProbeTime.Time) <= staleAfter
}
