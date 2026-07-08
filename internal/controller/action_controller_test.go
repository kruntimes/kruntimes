package controller

import (
	"context"
	"testing"

	"github.com/go-logr/logr/testr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestActionReconcileSetsReadyCondition(t *testing.T) {
	ctx := context.Background()
	scheme := actionTestScheme(t)
	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "setup", Namespace: "default"},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{{Name: "install", Run: "echo installing"}},
		},
	}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.Action{}).
		WithObjects(action).
		Build()
	reconciler := &ActionReconciler{Client: client, Log: testr.New(t), Scheme: scheme}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "setup", Namespace: "default"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got v1alpha1.Action
	if err := client.Get(ctx, types.NamespacedName{Name: "setup", Namespace: "default"}, &got); err != nil {
		t.Fatalf("get action: %v", err)
	}
	condition := findCondition(got.Status.Conditions, actionReadyCondition)
	if condition == nil {
		t.Fatalf("Ready condition not found in %#v", got.Status.Conditions)
	}
	if condition.Status != metav1.ConditionTrue || condition.Reason != "Accepted" {
		t.Fatalf("Ready condition = (%s, %s), want (True, Accepted)", condition.Status, condition.Reason)
	}
}

func actionTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return scheme
}
