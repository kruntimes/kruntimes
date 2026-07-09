package controller

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const workflowRunAcceptedCondition = "Accepted"

// WorkflowRunReconciler owns WorkflowRun execution-instance status.
type WorkflowRunReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=workflowruns,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=workflowruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *WorkflowRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var workflowRun v1alpha1.WorkflowRun
	if err := r.Get(ctx, req.NamespacedName, &workflowRun); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !workflowRun.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	base := workflowRun.DeepCopy()
	if workflowRun.Status.Phase == "" {
		workflowRun.Status.Phase = v1alpha1.WorkflowPending
	}
	if workflowRun.Status.Jobs == nil && len(workflowRun.Spec.Jobs) > 0 {
		workflowRun.Status.Jobs = resolvedInlineJobStatuses(workflowRun.Spec.Jobs)
	}
	apimeta.SetStatusCondition(&workflowRun.Status.Conditions, metav1.Condition{
		Type:               workflowRunAcceptedCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "WorkflowRun accepted; execution is a follow-up implementation step",
		ObservedGeneration: workflowRun.Generation,
	})
	if err := r.Status().Patch(ctx, &workflowRun, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch workflowrun status: %w", err)
	}
	return ctrl.Result{}, nil
}

// SetupWithManager registers the WorkflowRun reconciler.
func (r *WorkflowRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WorkflowRun{}).
		Complete(r)
}

func resolvedInlineJobStatuses(jobs map[string]v1alpha1.JobSpec) map[string]v1alpha1.JobStatus {
	statuses := make(map[string]v1alpha1.JobStatus, len(jobs))
	for jobName, job := range jobs {
		pre := append([]string(nil), job.Needs...)
		sort.Strings(pre)
		phase := v1alpha1.JobPending
		if len(pre) > 0 {
			phase = v1alpha1.JobWaiting
		}
		steps := make([]v1alpha1.StepStatus, 0, len(job.Steps))
		for _, step := range job.Steps {
			steps = append(steps, v1alpha1.StepStatus{
				Name:  step.Name,
				Phase: v1alpha1.StepPending,
			})
		}
		statuses[jobName] = v1alpha1.JobStatus{
			Phase: phase,
			Pre:   pre,
			Steps: steps,
		}
	}
	return statuses
}
