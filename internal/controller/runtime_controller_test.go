package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/runtimepod"
)

func TestBuildDeploymentAddsCapacityAnnotationsAndWorkers(t *testing.T) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: "bash", Namespace: "default"},
		Spec: v1alpha1.RuntimeSpec{
			Image: "bash-runtime:latest",
			Capacity: &v1alpha1.RuntimeCapacity{
				Resources: corev1.ResourceList{
					corev1.ResourceName(v1alpha1.RuntimeResourceRuns): resource.MustParse("3"),
					corev1.ResourceName("gpu"):                        resource.MustParse("1"),
				},
			},
		},
	}

	deploy := (&RuntimeReconciler{}).buildDeployment(rt)
	annotations := deploy.Spec.Template.Annotations
	if annotations[runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns)] != "3" {
		t.Fatalf("runs capacity annotation = %q, want 3", annotations[runtimepod.CapacityAnnotation(v1alpha1.RuntimeResourceRuns)])
	}
	if annotations[runtimepod.CapacityAnnotation("gpu")] != "1" {
		t.Fatalf("gpu capacity annotation = %q, want 1", annotations[runtimepod.CapacityAnnotation("gpu")])
	}

	daemon := deploy.Spec.Template.Spec.Containers[1]
	if !slices.Contains(daemon.Args, "--workers=3") {
		t.Fatalf("daemon args = %v, want --workers=3", daemon.Args)
	}
}
