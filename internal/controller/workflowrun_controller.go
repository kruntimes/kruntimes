package controller

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
// +kubebuilder:rbac:groups=kruntimes.io,resources=workflows,verbs=get;list;watch
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
	condition := metav1.Condition{
		Type:               workflowRunAcceptedCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "WorkflowRun accepted; execution is a follow-up implementation step",
		ObservedGeneration: workflowRun.Generation,
	}
	if workflowRun.Status.Phase != v1alpha1.WorkflowFailed && workflowRun.Status.Jobs == nil {
		jobs, err := r.resolveWorkflowRunJobs(ctx, &workflowRun)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			workflowRun.Status.Phase = v1alpha1.WorkflowFailed
			workflowRun.Status.Message = err.Error()
			condition.Status = metav1.ConditionFalse
			condition.Reason = "WorkflowResolutionFailed"
			condition.Message = err.Error()
		} else if len(jobs) > 0 {
			workflowRun.Status.Jobs = resolvedJobStatuses(jobs)
		}
	}
	apimeta.SetStatusCondition(&workflowRun.Status.Conditions, condition)
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

func (r *WorkflowRunReconciler) resolveWorkflowRunJobs(ctx context.Context, workflowRun *v1alpha1.WorkflowRun) (map[string]v1alpha1.JobSpec, error) {
	if len(workflowRun.Spec.Jobs) > 0 {
		return workflowRun.Spec.Jobs, nil
	}
	if workflowRun.Spec.Uses == "" {
		return nil, nil
	}

	var workflow v1alpha1.Workflow
	key := client.ObjectKey{Namespace: workflowRun.Namespace, Name: workflowRun.Spec.Uses}
	if err := r.Get(ctx, key, &workflow); err != nil {
		return nil, fmt.Errorf("get workflow %s/%s: %w", workflowRun.Namespace, workflowRun.Spec.Uses, err)
	}
	return workflow.Spec.Jobs, nil
}

func resolvedJobStatuses(jobs map[string]v1alpha1.JobSpec) map[string]v1alpha1.JobStatus {
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
