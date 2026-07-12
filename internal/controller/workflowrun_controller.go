package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

type workflowInputBindingError struct {
	err error
}

func (e *workflowInputBindingError) Error() string {
	return e.err.Error()
}

func (e *workflowInputBindingError) Unwrap() error {
	return e.err
}

type workflowRunReconcileState string

const (
	workflowRunStateResolveJobs    workflowRunReconcileState = "ResolveJobs"
	workflowRunStateStartReadyJobs workflowRunReconcileState = "StartReadyJobs"
	workflowRunStateFailReadyJob   workflowRunReconcileState = "FailReadyJob"
	workflowRunStatePatchStatus    workflowRunReconcileState = "PatchStatus"
)

type workflowRunResources struct {
	workflowRun *v1alpha1.WorkflowRun
	childRuns   map[string]*v1alpha1.Run
}

type workflowRunPlan struct {
	state workflowRunReconcileState
	jobs  []string
}

// WorkflowRunReconciler owns WorkflowRun execution-instance status.
type WorkflowRunReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=workflowruns,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=workflowruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=workflows,verbs=get;list;watch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runs,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *WorkflowRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	resources, err := r.loadWorkflowRunResources(ctx, req.NamespacedName)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	workflowRun := resources.workflowRun
	if !workflowRun.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	base := workflowRun.DeepCopy()
	if workflowRun.Status.Phase == "" {
		workflowRun.Status.Phase = v1alpha1.WorkflowPending
	}

	condition := acceptedWorkflowRunCondition(workflowRun)
	plan := planWorkflowRun(resources)
	if err := r.applyWorkflowRunPlan(ctx, resources, plan, &condition); err != nil {
		return ctrl.Result{}, err
	}

	apimeta.SetStatusCondition(&workflowRun.Status.Conditions, condition)
	if err := r.Status().Patch(ctx, workflowRun, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch workflowrun status: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *WorkflowRunReconciler) loadWorkflowRunResources(ctx context.Context, key client.ObjectKey) (*workflowRunResources, error) {
	workflowRun := &v1alpha1.WorkflowRun{}
	if err := r.Get(ctx, key, workflowRun); err != nil {
		return nil, err
	}

	var runs v1alpha1.RunList
	labels := client.MatchingLabels{v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID)}
	if err := r.List(ctx, &runs, client.InNamespace(workflowRun.Namespace), labels); err != nil {
		return nil, fmt.Errorf("list child runs for workflowrun %s/%s: %w", workflowRun.Namespace, workflowRun.Name, err)
	}
	childRuns := make(map[string]*v1alpha1.Run, len(runs.Items))
	for i := range runs.Items {
		run := &runs.Items[i]
		key := workflowStepKey(run.Labels[v1alpha1.WorkflowJobLabel], run.Labels[v1alpha1.WorkflowStepLabel])
		if existing, ok := childRuns[key]; !ok || run.Name < existing.Name {
			childRuns[key] = run.DeepCopy()
		}
	}

	return &workflowRunResources{workflowRun: workflowRun, childRuns: childRuns}, nil
}

func acceptedWorkflowRunCondition(workflowRun *v1alpha1.WorkflowRun) metav1.Condition {
	if existing := apimeta.FindStatusCondition(workflowRun.Status.Conditions, v1alpha1.WorkflowRunAcceptedCondition); existing != nil && existing.ObservedGeneration == workflowRun.Generation {
		return *existing
	}
	return metav1.Condition{
		Type:               v1alpha1.WorkflowRunAcceptedCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "WorkflowRun accepted; execution is a follow-up implementation step",
		ObservedGeneration: workflowRun.Generation,
	}
}

func planWorkflowRun(resources *workflowRunResources) workflowRunPlan {
	workflowRun := resources.workflowRun
	if workflowRun.Status.Phase != v1alpha1.WorkflowFailed && workflowRun.Status.Jobs == nil {
		return workflowRunPlan{state: workflowRunStateResolveJobs}
	}
	if workflowRun.Status.Phase == v1alpha1.WorkflowFailed || workflowRun.Status.Phase == v1alpha1.WorkflowSucceeded || len(workflowRun.Spec.Jobs) == 0 || len(workflowRun.Status.Jobs) == 0 {
		return workflowRunPlan{state: workflowRunStatePatchStatus}
	}

	jobNames := make([]string, 0, len(workflowRun.Spec.Jobs))
	for jobName := range workflowRun.Spec.Jobs {
		jobNames = append(jobNames, jobName)
	}
	sort.Strings(jobNames)
	readyJobs := make([]string, 0, len(jobNames))
	for _, jobName := range jobNames {
		job := workflowRun.Spec.Jobs[jobName]
		status := workflowRun.Status.Jobs[jobName]
		if !jobReadyToStart(status, workflowRun.Status.Jobs) {
			continue
		}
		if len(job.Steps) == 0 || len(status.Steps) == 0 || job.RunsOn == "" {
			return workflowRunPlan{state: workflowRunStateFailReadyJob, jobs: []string{jobName}}
		}
		if status.Steps[0].RunName != "" {
			continue
		}
		readyJobs = append(readyJobs, jobName)
	}
	if len(readyJobs) > 0 {
		return workflowRunPlan{state: workflowRunStateStartReadyJobs, jobs: readyJobs}
	}

	return workflowRunPlan{state: workflowRunStatePatchStatus}
}

func (r *WorkflowRunReconciler) applyResolveWorkflowRunJobs(ctx context.Context, workflowRun *v1alpha1.WorkflowRun, condition *metav1.Condition) error {
	jobs, err := r.resolveWorkflowRunJobs(ctx, workflowRun)
	if err != nil {
		var inputErr *workflowInputBindingError
		switch {
		case apierrors.IsNotFound(err):
			condition.Reason = "WorkflowResolutionFailed"
		case errors.As(err, &inputErr):
			condition.Reason = "WorkflowInputBindingFailed"
		default:
			return err
		}
		workflowRun.Status.Phase = v1alpha1.WorkflowFailed
		workflowRun.Status.Message = err.Error()
		condition.Status = metav1.ConditionFalse
		condition.Message = err.Error()
		return nil
	}
	if len(jobs) > 0 {
		workflowRun.Status.Jobs = resolvedJobStatuses(jobs)
	}
	return nil
}

func (r *WorkflowRunReconciler) applyWorkflowRunPlan(ctx context.Context, resources *workflowRunResources, plan workflowRunPlan, condition *metav1.Condition) error {
	workflowRun := resources.workflowRun
	switch plan.state {
	case workflowRunStateResolveJobs:
		return r.applyResolveWorkflowRunJobs(ctx, workflowRun, condition)
	case workflowRunStateStartReadyJobs:
		return r.applyStartReadyJobs(ctx, resources, plan.jobs)
	case workflowRunStateFailReadyJob:
		applyFailReadyJob(workflowRun, plan.jobs[0])
		condition.Status = metav1.ConditionFalse
		condition.Reason = "WorkflowExecutionFailed"
		condition.Message = workflowRun.Status.Message
	}
	return nil
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
	if _, err := bindWorkflowInputs(workflow.Spec.Inputs, workflowRun.Spec.With); err != nil {
		return nil, &workflowInputBindingError{err: fmt.Errorf("bind workflow inputs for %s/%s: %w", workflowRun.Namespace, workflowRun.Spec.Uses, err)}
	}
	return workflow.Spec.Jobs, nil
}

func bindWorkflowInputs(inputs map[string]v1alpha1.WorkflowInputSpec, with map[string]string) (map[string]string, error) {
	for name := range with {
		if _, ok := inputs[name]; !ok {
			return nil, fmt.Errorf("unknown input %q", name)
		}
	}

	bound := make(map[string]string, len(inputs))
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		input := inputs[name]
		value, ok := with[name]
		if !ok {
			value = input.Default
		}
		if value == "" && input.Required && !ok && input.Default == "" {
			return nil, fmt.Errorf("missing required input %q", name)
		}
		bound[name] = value
	}
	return bound, nil
}

func resolvedJobStatuses(jobs map[string]v1alpha1.JobSpec) map[string]v1alpha1.JobStatus {
	statuses := make(map[string]v1alpha1.JobStatus, len(jobs))
	for jobName, job := range jobs {
		pre := slices.Clone(job.Needs)
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

func (r *WorkflowRunReconciler) applyStartReadyJobs(ctx context.Context, resources *workflowRunResources, jobNames []string) error {
	workflowRun := resources.workflowRun
	for _, jobName := range jobNames {
		job := workflowRun.Spec.Jobs[jobName]
		step := job.Steps[0]
		run := resources.childRuns[workflowStepKey(jobName, step.Name)]
		if run == nil {
			run = buildStepRun(workflowRun, jobName, job, step, workflowStepLabels(workflowRun, jobName, step.Name))
			if err := controllerutil.SetControllerReference(workflowRun, run, r.Scheme); err != nil {
				return fmt.Errorf("set workflowrun owner reference on run %s/%s: %w", run.Namespace, run.Name, err)
			}
			if err := r.Create(ctx, run); err != nil {
				if !apierrors.IsAlreadyExists(err) {
					return fmt.Errorf("create child run %s/%s: %w", run.Namespace, run.Name, err)
				}
				var existing v1alpha1.Run
				if getErr := r.Get(ctx, client.ObjectKeyFromObject(run), &existing); getErr != nil {
					return fmt.Errorf("get existing child run %s/%s after create conflict: %w", run.Namespace, run.Name, getErr)
				}
				run = &existing
			}
		}
		recordFirstStepRun(workflowRun, jobName, run.Name)
	}
	return nil
}

func recordFirstStepRun(workflowRun *v1alpha1.WorkflowRun, jobName string, runName string) {
	status := workflowRun.Status.Jobs[jobName]
	status.Phase = v1alpha1.JobRunning
	status.Steps[0].Phase = v1alpha1.StepRunning
	status.Steps[0].RunName = runName
	workflowRun.Status.Jobs[jobName] = status
	workflowRun.Status.Phase = v1alpha1.WorkflowRunning
}

func applyFailReadyJob(workflowRun *v1alpha1.WorkflowRun, jobName string) {
	job := workflowRun.Spec.Jobs[jobName]
	status := workflowRun.Status.Jobs[jobName]
	workflowRun.Status.Phase = v1alpha1.WorkflowFailed
	if len(job.Steps) == 0 || len(status.Steps) == 0 {
		if job.Uses != "" {
			workflowRun.Status.Message = fmt.Sprintf("job %q uses reusable workflow %q, but job-level uses is not implemented yet", jobName, job.Uses)
		} else {
			workflowRun.Status.Message = fmt.Sprintf("job %q must contain at least one step before creating child Runs", jobName)
		}
	} else {
		workflowRun.Status.Message = fmt.Sprintf("job %q must set runs-on before creating child Runs", jobName)
	}
	status.Phase = v1alpha1.JobFailed
	workflowRun.Status.Jobs[jobName] = status
}

func jobReadyToStart(status v1alpha1.JobStatus, jobs map[string]v1alpha1.JobStatus) bool {
	if status.Phase != v1alpha1.JobPending && status.Phase != v1alpha1.JobWaiting {
		return false
	}
	for _, pre := range status.Pre {
		if jobs[pre].Phase != v1alpha1.JobSucceeded {
			return false
		}
	}
	return true
}

func buildStepRun(workflowRun *v1alpha1.WorkflowRun, jobName string, job v1alpha1.JobSpec, step v1alpha1.StepSpec, labels map[string]string) *v1alpha1.Run {
	inline := step.Run
	env := make([]corev1.EnvVar, 0, len(step.Env))
	envNames := make([]string, 0, len(step.Env))
	for name := range step.Env {
		envNames = append(envNames, name)
	}
	sort.Strings(envNames)
	for _, name := range envNames {
		env = append(env, corev1.EnvVar{Name: name, Value: step.Env[name]})
	}
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workflowStepRunName(workflowRun.Name, jobName, step.Name),
			Namespace: workflowRun.Namespace,
			Labels:    labels,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: job.RunsOn,
			Source:  &v1alpha1.CodeSource{Inline: &inline},
			Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{
				Args: slices.Clone(step.Args),
			}},
			Env: env,
		},
	}
}

func workflowStepLabels(workflowRun *v1alpha1.WorkflowRun, jobName string, stepName string) map[string]string {
	return map[string]string{
		v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID),
		v1alpha1.WorkflowJobLabel:    jobName,
		v1alpha1.WorkflowStepLabel:   stepName,
	}
}

func workflowStepKey(jobName string, stepName string) string {
	return jobName + "\x00" + stepName
}

func workflowStepRunName(workflowRunName string, jobName string, stepName string) string {
	sum := sha256.Sum256([]byte(jobName + "/" + stepName))
	suffix := hex.EncodeToString(sum[:])[:10]
	const maxNameLength = 253
	maxPrefixLength := maxNameLength - len(suffix) - 1
	prefix := workflowRunName
	if len(prefix) > maxPrefixLength {
		prefix = prefix[:maxPrefixLength]
		prefix = strings.TrimRight(prefix, "-.")
	}
	if prefix == "" {
		return suffix
	}
	return prefix + "-" + suffix
}
