package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestReconcileKeepsRunPendingWhenNoRuntimePodAvailable(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "run-a",
			Namespace: "default",
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
		},
	}

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Run{}).
		WithObjects(run).
		Build()

	reconciler := &RunReconciler{
		Client:   client,
		Log:      logr.Discard(),
		Strategy: &LeastLoaded{},
	}

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{
			Name:      run.Name,
			Namespace: run.Namespace,
		},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if result.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want 30s", result.RequeueAfter)
	}

	var updated v1alpha1.Run
	if err := client.Get(context.Background(), types.NamespacedName{Name: run.Name, Namespace: run.Namespace}, &updated); err != nil {
		t.Fatalf("get updated run: %v", err)
	}
	if updated.Status.Phase != v1alpha1.RunPending {
		t.Fatalf("phase = %s, want %s", updated.Status.Phase, v1alpha1.RunPending)
	}
	if updated.Status.AssignedPod != "" {
		t.Fatalf("assignedPod = %q, want empty", updated.Status.AssignedPod)
	}
	if !strings.Contains(updated.Status.Message, "waiting for available runtime pods") {
		t.Fatalf("message = %q, want waiting message", updated.Status.Message)
	}
}
