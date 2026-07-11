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
	workflowRunStateResolveJobs          workflowRunReconcileState = "ResolveJobs"
	workflowRunStateStartReadyInlineJobs workflowRunReconcileState = "StartReadyInlineJobs"
	workflowRunStatePatchStatus          workflowRunReconcileState = "PatchStatus"
)

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

	condition := acceptedWorkflowRunCondition(&workflowRun)
	for {
		switch workflowRunStateFor(&workflowRun) {
		case workflowRunStateResolveJobs:
			if err := r.applyResolveWorkflowRunJobs(ctx, &workflowRun, &condition); err != nil {
				return ctrl.Result{}, err
			}
			continue
		case workflowRunStateStartReadyInlineJobs:
			if err := r.applyStartReadyInlineJobs(ctx, &workflowRun, &condition); err != nil {
				return ctrl.Result{}, err
			}
		case workflowRunStatePatchStatus:
		}
		break
	}

	apimeta.SetStatusCondition(&workflowRun.Status.Conditions, condition)
	if err := r.Status().Patch(ctx, &workflowRun, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch workflowrun status: %w", err)
	}
	return ctrl.Result{}, nil
}

func acceptedWorkflowRunCondition(workflowRun *v1alpha1.WorkflowRun) metav1.Condition {
	return metav1.Condition{
		Type:               v1alpha1.WorkflowRunAcceptedCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "Accepted",
		Message:            "WorkflowRun accepted; execution is a follow-up implementation step",
		ObservedGeneration: workflowRun.Generation,
	}
}

func workflowRunStateFor(workflowRun *v1alpha1.WorkflowRun) workflowRunReconcileState {
	if workflowRun.Status.Phase != v1alpha1.WorkflowFailed && workflowRun.Status.Jobs == nil {
		return workflowRunStateResolveJobs
	}
	if workflowRun.Status.Phase == v1alpha1.WorkflowPending && len(workflowRun.Spec.Jobs) > 0 && len(workflowRun.Status.Jobs) > 0 {
		return workflowRunStateStartReadyInlineJobs
	}
	return workflowRunStatePatchStatus
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

func (r *WorkflowRunReconciler) applyStartReadyInlineJobs(ctx context.Context, workflowRun *v1alpha1.WorkflowRun, condition *metav1.Condition) error {
	if err := r.startReadyInlineJobs(ctx, workflowRun); err != nil {
		return err
	}
	if workflowRun.Status.Phase == v1alpha1.WorkflowFailed {
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

func (r *WorkflowRunReconciler) startReadyInlineJobs(ctx context.Context, workflowRun *v1alpha1.WorkflowRun) error {
	type readyJob struct {
		name   string
		spec   v1alpha1.JobSpec
		status v1alpha1.JobStatus
	}
	jobNames := make([]string, 0, len(workflowRun.Spec.Jobs))
	for jobName := range workflowRun.Spec.Jobs {
		jobNames = append(jobNames, jobName)
	}
	sort.Strings(jobNames)
	readyJobs := make([]readyJob, 0, len(jobNames))
	for _, jobName := range jobNames {
		job := workflowRun.Spec.Jobs[jobName]
		status := workflowRun.Status.Jobs[jobName]
		if !jobReadyToStart(status, workflowRun.Status.Jobs) {
			continue
		}
		if len(job.Steps) == 0 || len(status.Steps) == 0 {
			workflowRun.Status.Phase = v1alpha1.WorkflowFailed
			if job.Uses != "" {
				workflowRun.Status.Message = fmt.Sprintf("job %q uses reusable workflow %q, but job-level uses is not implemented yet", jobName, job.Uses)
			} else {
				workflowRun.Status.Message = fmt.Sprintf("job %q must contain at least one step before creating child Runs", jobName)
			}
			status.Phase = v1alpha1.JobFailed
			workflowRun.Status.Jobs[jobName] = status
			return nil
		}
		if status.Steps[0].RunName != "" {
			continue
		}
		if job.RunsOn == "" {
			workflowRun.Status.Phase = v1alpha1.WorkflowFailed
			workflowRun.Status.Message = fmt.Sprintf("job %q must set runs-on before creating child Runs", jobName)
			status.Phase = v1alpha1.JobFailed
			workflowRun.Status.Jobs[jobName] = status
			return nil
		}
		readyJobs = append(readyJobs, readyJob{name: jobName, spec: job, status: status})
	}
	for _, ready := range readyJobs {
		run, err := r.ensureStepRun(ctx, workflowRun, ready.name, ready.spec, ready.spec.Steps[0])
		if err != nil {
			return err
		}
		status := ready.status
		status.Phase = v1alpha1.JobRunning
		status.Steps[0].Phase = v1alpha1.StepRunning
		status.Steps[0].RunName = run.Name
		workflowRun.Status.Jobs[ready.name] = status
		workflowRun.Status.Phase = v1alpha1.WorkflowRunning
	}
	return nil
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

func (r *WorkflowRunReconciler) ensureStepRun(ctx context.Context, workflowRun *v1alpha1.WorkflowRun, jobName string, job v1alpha1.JobSpec, step v1alpha1.StepSpec) (*v1alpha1.Run, error) {
	labels := workflowStepLabels(workflowRun, jobName, step.Name)
	var existing v1alpha1.RunList
	if err := r.List(ctx, &existing, client.InNamespace(workflowRun.Namespace), client.MatchingLabels(labels)); err != nil {
		return nil, fmt.Errorf("list child runs for workflowrun %s/%s job %s step %s: %w", workflowRun.Namespace, workflowRun.Name, jobName, step.Name, err)
	}
	if len(existing.Items) > 0 {
		sort.Slice(existing.Items, func(i, j int) bool {
			return existing.Items[i].Name < existing.Items[j].Name
		})
		return &existing.Items[0], nil
	}

	run := buildStepRun(workflowRun, jobName, job, step, labels)
	if err := controllerutil.SetControllerReference(workflowRun, run, r.Scheme); err != nil {
		return nil, fmt.Errorf("set workflowrun owner reference on run %s/%s: %w", run.Namespace, run.Name, err)
	}
	if err := r.Create(ctx, run); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create child run %s/%s: %w", run.Namespace, run.Name, err)
		}
		var created v1alpha1.Run
		if getErr := r.Get(ctx, client.ObjectKeyFromObject(run), &created); getErr != nil {
			return nil, fmt.Errorf("get existing child run %s/%s after create conflict: %w", run.Namespace, run.Name, getErr)
		}
		return &created, nil
	}
	return run, nil
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
