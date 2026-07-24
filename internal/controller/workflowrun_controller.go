package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/workflowtemplate"
)

type workflowRunState string

const (
	workflowRunStateEmpty      workflowRunState = "Empty"
	workflowRunStatePending    workflowRunState = "Pending"
	workflowRunStateRunning    workflowRunState = "Running"
	workflowRunStateCancelling workflowRunState = "Cancelling"
	workflowRunStateTerminal   workflowRunState = "Terminal"
)

const maxWorkflowOutputContractAnnotationBytes = 240 * 1024

type workflowRunAction string

const (
	workflowRunActionNone                     workflowRunAction = "None"
	workflowRunActionInitialize               workflowRunAction = "Initialize"
	workflowRunActionStartRunnableTargets     workflowRunAction = "StartRunnableTargets"
	workflowRunActionRequestChildCancellation workflowRunAction = "RequestChildCancellation"
)

type workflowRunResources struct {
	workflowRun    *v1alpha1.WorkflowRun
	childRuns      map[string]*v1alpha1.Run
	childWorkflows map[string]*v1alpha1.WorkflowRun
	snapshot       *workflowExecutionSnapshot
}

type workflowRunPlan struct {
	state   workflowRunState
	action  workflowRunAction
	targets []workflowRunTarget
}

type workflowRunTargetKind string

const (
	workflowRunTargetStep              workflowRunTargetKind = "Step"
	workflowRunTargetWorkflowCall      workflowRunTargetKind = "WorkflowCall"
	workflowRunTargetCancelRun         workflowRunTargetKind = "CancelRun"
	workflowRunTargetCancelWorkflowRun workflowRunTargetKind = "CancelWorkflowRun"
)

// workflowRunTarget identifies one child resource operation within a plan.
type workflowRunTarget struct {
	kind      workflowRunTargetKind
	jobName   string
	stepIndex int
	name      string
}

type workflowCallValidationError struct {
	err error
}

func (e *workflowCallValidationError) Error() string { return e.err.Error() }
func (e *workflowCallValidationError) Unwrap() error { return e.err }

// WorkflowRunReconciler owns WorkflowRun execution-instance status.
type WorkflowRunReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=workflowruns,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=workflowruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runs,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups=apps,resources=controllerrevisions,verbs=get;list;watch;create;delete
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
	resources.workflowRun = workflowRun.DeepCopy()
	plan := calculateWorkflowRunPlan(resources)
	if plan.action != workflowRunActionNone {
		if err := r.applyWorkflowRunAction(ctx, resources, plan); err != nil {
			return ctrl.Result{}, err
		}
	}

	desired := resources.workflowRun
	if apiequality.Semantic.DeepEqual(base.Status, desired.Status) {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Patch(ctx, desired, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch workflowrun status: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *WorkflowRunReconciler) loadWorkflowRunResources(ctx context.Context, key client.ObjectKey) (*workflowRunResources, error) {
	workflowRun := &v1alpha1.WorkflowRun{}
	if err := r.Get(ctx, key, workflowRun); err != nil {
		return nil, err
	}

	resources := &workflowRunResources{workflowRun: workflowRun}
	snapshotName := workflowRun.Status.SnapshotName
	if snapshotName == "" {
		snapshotName = workflowSnapshotName(workflowRun)
	}
	revision := &appsv1.ControllerRevision{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: workflowRun.Namespace, Name: snapshotName}, revision); err == nil {
		if revision.Labels[v1alpha1.WorkflowRunUIDLabel] != string(workflowRun.UID) {
			return nil, fmt.Errorf("workflow snapshot %s/%s belongs to another workflowrun", revision.Namespace, revision.Name)
		}
		snapshot, err := loadWorkflowSnapshot(revision)
		if err != nil {
			return nil, err
		}
		resources.snapshot = snapshot
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get workflow snapshot %s/%s: %w", workflowRun.Namespace, snapshotName, err)
	} else if workflowRun.Status.SnapshotName != "" {
		return nil, fmt.Errorf("get workflow snapshot %s/%s: %w", workflowRun.Namespace, snapshotName, err)
	}
	if resources.snapshot == nil && workflowRun.Status.Jobs != nil {
		return nil, fmt.Errorf("workflowrun %s/%s has initialized jobs without an execution snapshot", workflowRun.Namespace, workflowRun.Name)
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

	resources.childRuns = childRuns
	var workflowRuns v1alpha1.WorkflowRunList
	childWorkflowLabels := client.MatchingLabels{v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID)}
	if err := r.List(ctx, &workflowRuns, client.InNamespace(workflowRun.Namespace), childWorkflowLabels); err != nil {
		return nil, fmt.Errorf("list child workflowruns for workflowrun %s/%s: %w", workflowRun.Namespace, workflowRun.Name, err)
	}
	resources.childWorkflows = make(map[string]*v1alpha1.WorkflowRun)
	for i := range workflowRuns.Items {
		child := &workflowRuns.Items[i]
		if metav1.IsControlledBy(child, workflowRun) {
			resources.childWorkflows[child.Name] = child.DeepCopy()
		}
	}
	return resources, nil
}

func calculateWorkflowRunPlan(resources *workflowRunResources) workflowRunPlan {
	deriveWorkflowRunStatus(resources)
	workflowRun := resources.workflowRun
	state := workflowRunStateFor(workflowRun)
	plan := workflowRunPlan{state: state, action: workflowRunActionNone}
	if state == workflowRunStateEmpty {
		plan.action = workflowRunActionInitialize
		return plan
	}
	if workflowRun.Spec.CancelRequested {
		plan.targets = append(childRunCancellationTargets(resources.childRuns), childWorkflowRunCancellationTargets(resources.childWorkflows)...)
		if len(plan.targets) > 0 {
			plan.state = workflowRunStateCancelling
			plan.action = workflowRunActionRequestChildCancellation
			return plan
		}
	}
	if state == workflowRunStateTerminal {
		return plan
	}
	if state == workflowRunStateCancelling {
		return plan
	}
	if resources.snapshot == nil {
		return plan
	}
	jobs := resources.snapshot.Spec.Jobs
	if len(jobs) == 0 || len(workflowRun.Status.Jobs) == 0 {
		return plan
	}
	plan.targets = append(runnableStepTargets(workflowRun.Status.Jobs, jobs), runnableWorkflowCallTargets(workflowRun.Status.Jobs, jobs)...)
	if len(plan.targets) > 0 {
		plan.action = workflowRunActionStartRunnableTargets
		return plan
	}
	return plan
}

func workflowRunStateFor(workflowRun *v1alpha1.WorkflowRun) workflowRunState {
	if isTerminalWorkflowPhase(workflowRun.Status.Phase) {
		return workflowRunStateTerminal
	}
	if workflowRun.Spec.CancelRequested {
		return workflowRunStateCancelling
	}
	if workflowRun.Status.Jobs == nil {
		return workflowRunStateEmpty
	}
	if workflowRun.Status.Phase == v1alpha1.WorkflowRunning {
		return workflowRunStateRunning
	}
	return workflowRunStatePending
}

func (r *WorkflowRunReconciler) applyInitializeWorkflowRun(ctx context.Context, resources *workflowRunResources) error {
	workflowRun := resources.workflowRun
	snapshot := resources.snapshot
	snapshotName := workflowRun.Status.SnapshotName
	if snapshot == nil {
		var err error
		snapshot = workflowSnapshotForRun(workflowRun)
		if err := validateWorkflowRunJobs(snapshot.Spec.Jobs); err != nil {
			return rejectWorkflowRun(workflowRun, "WorkflowValidationFailed", err.Error())
		}
		persistedName, persistedSnapshot, err := r.ensureWorkflowSnapshot(ctx, workflowRun, snapshot)
		if err != nil {
			var snapshotErr *workflowSnapshotError
			if errors.As(err, &snapshotErr) {
				return rejectWorkflowRun(workflowRun, "WorkflowValidationFailed", err.Error())
			}
			return err
		}
		snapshotName = persistedName
		snapshot = persistedSnapshot
		resources.snapshot = snapshot
	}
	workflowRun.Status.Phase = v1alpha1.WorkflowPending
	workflowRun.Status.Message = ""
	workflowRun.Status.Jobs = resolvedJobStatuses(snapshot.Spec.Jobs)
	workflowRun.Status.SnapshotName = snapshotName
	setWorkflowRunAcceptedCondition(workflowRun, metav1.ConditionTrue, "Accepted", "WorkflowRun accepted and initialized")
	return nil
}

func (r *WorkflowRunReconciler) applyWorkflowRunAction(ctx context.Context, resources *workflowRunResources, plan workflowRunPlan) error {
	switch plan.action {
	case workflowRunActionInitialize:
		return r.applyInitializeWorkflowRun(ctx, resources)
	case workflowRunActionStartRunnableTargets:
		return r.applyStartRunnableTargets(ctx, resources, plan.targets)
	case workflowRunActionRequestChildCancellation:
		return r.applyRequestChildCancellation(ctx, resources, plan.targets)
	}
	return nil
}

// SetupWithManager registers the WorkflowRun reconciler.
func (r *WorkflowRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WorkflowRun{}).
		Owns(&v1alpha1.Run{}).
		Owns(&v1alpha1.WorkflowRun{}).
		Owns(&appsv1.ControllerRevision{}).
		Complete(r)
}

func validateWorkflowRunJobs(jobs map[string]v1alpha1.JobSpec) error {
	jobNames := make([]string, 0, len(jobs))
	for jobName := range jobs {
		jobNames = append(jobNames, jobName)
	}
	sort.Strings(jobNames)
	for _, jobName := range jobNames {
		job := jobs[jobName]
		if job.Uses != "" {
			if job.RunsOn != "" || len(job.Steps) != 0 {
				return fmt.Errorf("job %q uses reusable workflow %q and may not set runs-on or steps", jobName, job.Uses)
			}
			continue
		}
		if job.RunsOn == "" {
			return fmt.Errorf("job %q must set runs-on before creating child Runs", jobName)
		}
		if len(job.Steps) == 0 {
			return fmt.Errorf("job %q must contain at least one step before creating child Runs", jobName)
		}
	}
	return validateWorkflowJobDAG(jobs, jobNames)
}

func validateWorkflowJobDAG(jobs map[string]v1alpha1.JobSpec, jobNames []string) error {
	const (
		jobVisiting = iota + 1
		jobVisited
	)
	states := make(map[string]int, len(jobs))
	stack := make([]string, 0, len(jobs))

	var visit func(string) error
	visit = func(jobName string) error {
		switch states[jobName] {
		case jobVisited:
			return nil
		case jobVisiting:
			cycleStart := slices.Index(stack, jobName)
			cycle := append(slices.Clone(stack[cycleStart:]), jobName)
			return fmt.Errorf("workflow job dependency cycle: %s", strings.Join(cycle, " -> "))
		}

		states[jobName] = jobVisiting
		stack = append(stack, jobName)
		dependencies := slices.Clone(jobs[jobName].Needs)
		sort.Strings(dependencies)
		for _, dependency := range dependencies {
			if _, ok := jobs[dependency]; !ok {
				return fmt.Errorf("job %q needs unknown job %q", jobName, dependency)
			}
			if err := visit(dependency); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		states[jobName] = jobVisited
		return nil
	}

	for _, jobName := range jobNames {
		if err := visit(jobName); err != nil {
			return err
		}
	}
	return nil
}

func rejectWorkflowRun(workflowRun *v1alpha1.WorkflowRun, reason string, message string) error {
	workflowRun.Status.Phase = v1alpha1.WorkflowFailed
	workflowRun.Status.Message = message
	setWorkflowRunAcceptedCondition(workflowRun, metav1.ConditionFalse, reason, message)
	return nil
}

func setWorkflowRunAcceptedCondition(workflowRun *v1alpha1.WorkflowRun, status metav1.ConditionStatus, reason string, message string) {
	apimeta.SetStatusCondition(&workflowRun.Status.Conditions, metav1.Condition{
		Type:               v1alpha1.WorkflowRunAcceptedCondition,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: workflowRun.Generation,
	})
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

func (r *WorkflowRunReconciler) applyStartRunnableTargets(ctx context.Context, resources *workflowRunResources, targets []workflowRunTarget) error {
	workflowRun := resources.workflowRun
	for _, target := range targets {
		switch target.kind {
		case workflowRunTargetStep:
			job := resources.snapshot.Spec.Jobs[target.jobName]
			run, err := r.createOrReuseStepRun(ctx, resources, target.jobName, job, target.stepIndex)
			if err != nil {
				return err
			}
			recordStepRun(workflowRun, target.jobName, target.stepIndex, run.Name)
		case workflowRunTargetWorkflowCall:
			job := resources.snapshot.Spec.Jobs[target.jobName]
			child, err := r.createWorkflowCall(ctx, workflowRun, target.jobName, job)
			if err != nil {
				var validationErr *workflowCallValidationError
				if errors.As(err, &validationErr) {
					recordWorkflowCallFailure(workflowRun, target.jobName, validationErr.Error())
					continue
				}
				return err
			}
			recordWorkflowCall(workflowRun, target.jobName, child.Name)
		default:
			return fmt.Errorf("start workflowrun target has unsupported kind %q", target.kind)
		}
	}
	return nil
}

func (r *WorkflowRunReconciler) createWorkflowCall(ctx context.Context, parent *v1alpha1.WorkflowRun, jobName string, job v1alpha1.JobSpec) (*v1alpha1.WorkflowRun, error) {
	workflow := &v1alpha1.Workflow{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: parent.Namespace, Name: job.Uses}, workflow); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &workflowCallValidationError{err: fmt.Errorf("reusable workflow %q for job %q does not exist", job.Uses, jobName)}
		}
		return nil, fmt.Errorf("get reusable workflow %q for job %q: %w", job.Uses, jobName, err)
	}
	if err := workflowtemplate.ValidateCallGraph(ctx, workflow.Name, func(ctx context.Context, name string) (*v1alpha1.Workflow, error) {
		candidate := &v1alpha1.Workflow{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: parent.Namespace, Name: name}, candidate); err != nil {
			return nil, err
		}
		return candidate, nil
	}); err != nil {
		return nil, &workflowCallValidationError{err: fmt.Errorf("validate reusable workflow %q for job %q: %w", job.Uses, jobName, err)}
	}
	inputs, err := resolveWorkflowCallInputs(job.With, workflowRunJobOutputContext(parent.Status.Jobs))
	if err != nil {
		return nil, &workflowCallValidationError{err: fmt.Errorf("resolve inputs for job %q: %w", jobName, err)}
	}
	jobs, err := workflowtemplate.Materialize(workflow.Spec, inputs)
	if err != nil {
		return nil, &workflowCallValidationError{err: fmt.Errorf("materialize reusable workflow %q for job %q: %w", workflow.Name, jobName, err)}
	}
	outputAnnotations, err := workflowOutputContractAnnotations(workflow.Spec.Outputs)
	if err != nil {
		return nil, &workflowCallValidationError{err: fmt.Errorf("materialize output contract for reusable workflow %q and job %q: %w", workflow.Name, jobName, err)}
	}

	child := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workflowCallRunName(parent.Name, jobName),
			Namespace: parent.Namespace,
			Labels: map[string]string{
				v1alpha1.WorkflowRunUIDLabel: string(parent.UID),
			},
			Annotations: outputAnnotations,
		},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: jobs},
	}
	if err := controllerutil.SetControllerReference(parent, child, r.Scheme); err != nil {
		return nil, fmt.Errorf("set parent workflowrun owner reference on child %s/%s: %w", child.Namespace, child.Name, err)
	}
	if err := r.Create(ctx, child); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create child workflowrun %s/%s: %w", child.Namespace, child.Name, err)
		}
		if err := r.Get(ctx, client.ObjectKeyFromObject(child), child); err != nil {
			return nil, fmt.Errorf("get existing child workflowrun %s/%s: %w", child.Namespace, child.Name, err)
		}
	}
	return child, nil
}

func recordWorkflowCall(workflowRun *v1alpha1.WorkflowRun, jobName string, childName string) {
	status := workflowRun.Status.Jobs[jobName]
	status.Phase = v1alpha1.JobRunning
	status.WorkflowRunName = childName
	workflowRun.Status.Jobs[jobName] = status
	workflowRun.Status.Phase = v1alpha1.WorkflowRunning
}

func recordWorkflowCallFailure(workflowRun *v1alpha1.WorkflowRun, jobName string, message string) {
	status := workflowRun.Status.Jobs[jobName]
	status.Phase = v1alpha1.JobFailed
	workflowRun.Status.Jobs[jobName] = status
	workflowRun.Status.Phase = v1alpha1.WorkflowRunning
	workflowRun.Status.Message = message
}

func (r *WorkflowRunReconciler) createOrReuseStepRun(ctx context.Context, resources *workflowRunResources, jobName string, job v1alpha1.JobSpec, stepIndex int) (*v1alpha1.Run, error) {
	workflowRun := resources.workflowRun
	step := job.Steps[stepIndex]
	run := resources.childRuns[workflowStepKey(jobName, step.Name)]
	if run != nil {
		return run, nil
	}

	run = buildStepRun(workflowRun, jobName, job, step, workflowStepLabels(workflowRun, jobName, step.Name))
	if err := controllerutil.SetControllerReference(workflowRun, run, r.Scheme); err != nil {
		return nil, fmt.Errorf("set workflowrun owner reference on run %s/%s: %w", run.Namespace, run.Name, err)
	}
	if err := r.Create(ctx, run); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create child run %s/%s: %w", run.Namespace, run.Name, err)
		}
		var existing v1alpha1.Run
		if getErr := r.Get(ctx, client.ObjectKeyFromObject(run), &existing); getErr != nil {
			return nil, fmt.Errorf("get existing child run %s/%s after create conflict: %w", run.Namespace, run.Name, getErr)
		}
		run = &existing
	}
	return run, nil
}

func recordStepRun(workflowRun *v1alpha1.WorkflowRun, jobName string, stepIndex int, runName string) {
	status := workflowRun.Status.Jobs[jobName]
	status.Phase = v1alpha1.JobRunning
	status.Steps[stepIndex].Phase = v1alpha1.StepRunning
	status.Steps[stepIndex].RunName = runName
	workflowRun.Status.Jobs[jobName] = status
	workflowRun.Status.Phase = v1alpha1.WorkflowRunning
}

func runnableStepTargets(statuses map[string]v1alpha1.JobStatus, jobs map[string]v1alpha1.JobSpec) []workflowRunTarget {
	jobNames := make([]string, 0, len(jobs))
	for jobName := range jobs {
		jobNames = append(jobNames, jobName)
	}
	sort.Strings(jobNames)

	targets := make([]workflowRunTarget, 0, len(jobNames))
	for _, jobName := range jobNames {
		job := jobs[jobName]
		status, ok := statuses[jobName]
		if !ok || job.Uses != "" || len(status.Steps) != len(job.Steps) || len(job.Steps) == 0 {
			continue
		}
		if jobReadyToStart(status, statuses) && status.Steps[0].RunName == "" {
			targets = append(targets, workflowRunTarget{kind: workflowRunTargetStep, jobName: jobName, stepIndex: 0})
			continue
		}
		if stepIndex, ok := nextStepToStart(status); ok {
			targets = append(targets, workflowRunTarget{kind: workflowRunTargetStep, jobName: jobName, stepIndex: stepIndex})
		}
	}
	return targets
}

func runnableWorkflowCallTargets(statuses map[string]v1alpha1.JobStatus, jobs map[string]v1alpha1.JobSpec) []workflowRunTarget {
	names := make([]string, 0, len(jobs))
	for name := range jobs {
		names = append(names, name)
	}
	sort.Strings(names)
	targets := make([]workflowRunTarget, 0, len(names))
	for _, name := range names {
		job := jobs[name]
		status, ok := statuses[name]
		if !ok || job.Uses == "" || status.WorkflowRunName != "" || !jobReadyToStart(status, statuses) {
			continue
		}
		targets = append(targets, workflowRunTarget{kind: workflowRunTargetWorkflowCall, jobName: name})
	}
	return targets
}

func workflowRunJobOutputContext(statuses map[string]v1alpha1.JobStatus) *resolveContext {
	jobs := make(map[string]map[string]string, len(statuses))
	for name, status := range statuses {
		if len(status.Outputs) > 0 {
			jobs[name] = status.Outputs
		}
	}
	return &resolveContext{jobs: jobs}
}

func resolveWorkflowCallInputs(values map[string]string, ctx *resolveContext) (map[string]string, error) {
	if values == nil {
		return nil, nil
	}
	resolved := make(map[string]string, len(values))
	for name, value := range values {
		result, err := resolveExpr(value, ctx)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", name, err)
		}
		resolved[name] = result
	}
	return resolved, nil
}

func workflowOutputContractAnnotations(outputs map[string]v1alpha1.WorkflowOutputSpec) (map[string]string, error) {
	if len(outputs) == 0 {
		return nil, nil
	}
	annotations := make(map[string]string, len(outputs))
	annotationBytes := 0
	for name, output := range outputs {
		key := v1alpha1.WorkflowOutputAnnotationPrefix + name
		if len(validation.IsQualifiedName(key)) != 0 {
			return nil, fmt.Errorf("output name %q cannot be represented as a WorkflowRun annotation", name)
		}
		annotationBytes += len(key) + len(output.Value)
		if annotationBytes > maxWorkflowOutputContractAnnotationBytes {
			return nil, fmt.Errorf("output contract is %d bytes, exceeds %d byte annotation limit", annotationBytes, maxWorkflowOutputContractAnnotationBytes)
		}
		annotations[key] = output.Value
	}
	return annotations, nil
}

func workflowOutputContractFromAnnotations(annotations map[string]string) (map[string]v1alpha1.WorkflowOutputSpec, error) {
	outputs := make(map[string]v1alpha1.WorkflowOutputSpec)
	for key, value := range annotations {
		if !strings.HasPrefix(key, v1alpha1.WorkflowOutputAnnotationPrefix) {
			continue
		}
		name := strings.TrimPrefix(key, v1alpha1.WorkflowOutputAnnotationPrefix)
		if name == "" || len(validation.IsQualifiedName(key)) != 0 {
			return nil, fmt.Errorf("invalid workflow output annotation %q", key)
		}
		outputs[name] = v1alpha1.WorkflowOutputSpec{Value: value}
	}
	return outputs, nil
}

func nextStepToStart(status v1alpha1.JobStatus) (int, bool) {
	for i, step := range status.Steps {
		if step.Phase == v1alpha1.StepSucceeded {
			continue
		}
		if i > 0 && step.Phase == v1alpha1.StepPending && step.RunName == "" {
			return i, true
		}
		return 0, false
	}
	return 0, false
}

func terminalJobPhase(status v1alpha1.JobStatus) (v1alpha1.JobPhase, bool) {
	if len(status.Steps) == 0 {
		return "", false
	}
	allSucceeded := true
	for _, step := range status.Steps {
		switch step.Phase {
		case v1alpha1.StepFailed:
			return v1alpha1.JobFailed, true
		case v1alpha1.StepSucceeded:
		default:
			allSucceeded = false
		}
	}
	if allSucceeded {
		return v1alpha1.JobSucceeded, true
	}
	return "", false
}

func deriveWorkflowRunStatus(resources *workflowRunResources) {
	deriveStepStatusesFromChildRuns(resources)
	deriveWorkflowCallStatuses(resources)
	deriveJobStatuses(resources)
	if resources.workflowRun.Spec.CancelRequested {
		deriveCancelledWorkflowRunStatus(resources)
		return
	}
	deriveSkippedJobStatuses(resources.workflowRun.Status.Jobs)
	deriveTerminalWorkflowRunStatus(resources.workflowRun)
}

func deriveWorkflowCallStatuses(resources *workflowRunResources) {
	for jobName, status := range resources.workflowRun.Status.Jobs {
		if status.WorkflowRunName == "" || status.Phase != v1alpha1.JobRunning {
			continue
		}
		child := resources.childWorkflows[status.WorkflowRunName]
		if child == nil {
			continue
		}
		switch child.Status.Phase {
		case v1alpha1.WorkflowSucceeded:
			outputs, err := resolveWorkflowCallOutputs(child)
			if err != nil {
				status.Phase = v1alpha1.JobFailed
				resources.workflowRun.Status.Message = fmt.Sprintf("resolve outputs for job %q: %v", jobName, err)
			} else {
				status.Phase = v1alpha1.JobSucceeded
				status.Outputs = outputs
			}
		case v1alpha1.WorkflowFailed, v1alpha1.WorkflowCancelled:
			status.Phase = v1alpha1.JobFailed
		default:
			continue
		}
		resources.workflowRun.Status.Jobs[jobName] = status
	}
}

func resolveWorkflowCallOutputs(child *v1alpha1.WorkflowRun) (map[string]string, error) {
	contract, err := workflowOutputContractFromAnnotations(child.Annotations)
	if err != nil {
		return nil, fmt.Errorf("child workflowrun %s/%s: %w", child.Namespace, child.Name, err)
	}
	if len(contract) == 0 {
		return nil, nil
	}
	outputs := make(map[string]string, len(contract))
	outputNames := make([]string, 0, len(contract))
	for name := range contract {
		outputNames = append(outputNames, name)
	}
	sort.Strings(outputNames)
	ctx := workflowRunJobOutputContext(child.Status.Jobs)
	for _, name := range outputNames {
		value, err := resolveExpr(contract[name].Value, ctx)
		if err != nil {
			return nil, fmt.Errorf("output %q: %w", name, err)
		}
		outputs[name] = value
	}
	return outputs, nil
}

func deriveCancelledWorkflowRunStatus(resources *workflowRunResources) {
	workflowRun := resources.workflowRun
	if !workflowRun.Spec.CancelRequested || isTerminalWorkflowPhase(workflowRun.Status.Phase) {
		return
	}
	for _, run := range resources.childRuns {
		if !isTerminalRunPhase(run.Status.Phase) {
			return
		}
	}
	for _, child := range resources.childWorkflows {
		if !isTerminalWorkflowPhase(child.Status.Phase) {
			return
		}
	}
	workflowRun.Status.Phase = v1alpha1.WorkflowCancelled
}

func isTerminalWorkflowPhase(phase v1alpha1.WorkflowPhase) bool {
	switch phase {
	case v1alpha1.WorkflowSucceeded, v1alpha1.WorkflowFailed, v1alpha1.WorkflowCancelled:
		return true
	default:
		return false
	}
}

func childRunCancellationTargets(childRuns map[string]*v1alpha1.Run) []workflowRunTarget {
	targets := make([]workflowRunTarget, 0, len(childRuns))
	for _, run := range childRuns {
		if !isTerminalRunPhase(run.Status.Phase) && !run.Spec.CancelRequested {
			targets = append(targets, workflowRunTarget{kind: workflowRunTargetCancelRun, name: run.Name})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	return targets
}

func childWorkflowRunCancellationTargets(childWorkflows map[string]*v1alpha1.WorkflowRun) []workflowRunTarget {
	targets := make([]workflowRunTarget, 0, len(childWorkflows))
	for _, child := range childWorkflows {
		if !isTerminalWorkflowPhase(child.Status.Phase) && !child.Spec.CancelRequested {
			targets = append(targets, workflowRunTarget{kind: workflowRunTargetCancelWorkflowRun, name: child.Name})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].name < targets[j].name })
	return targets
}

func (r *WorkflowRunReconciler) applyRequestChildCancellation(ctx context.Context, resources *workflowRunResources, targets []workflowRunTarget) error {
	childRuns := make(map[string]*v1alpha1.Run, len(resources.childRuns))
	for _, run := range resources.childRuns {
		childRuns[run.Name] = run
	}
	for _, target := range targets {
		switch target.kind {
		case workflowRunTargetCancelRun:
			run := childRuns[target.name]
			if run == nil || run.Spec.CancelRequested || isTerminalRunPhase(run.Status.Phase) {
				continue
			}
			base := run.DeepCopy()
			run.Spec.CancelRequested = true
			if err := r.Patch(ctx, run, client.MergeFrom(base)); err != nil {
				return fmt.Errorf("request cancellation of child run %s/%s: %w", run.Namespace, run.Name, err)
			}
		case workflowRunTargetCancelWorkflowRun:
			child := resources.childWorkflows[target.name]
			if child == nil || child.Spec.CancelRequested || isTerminalWorkflowPhase(child.Status.Phase) {
				continue
			}
			base := child.DeepCopy()
			child.Spec.CancelRequested = true
			if err := r.Patch(ctx, child, client.MergeFrom(base)); err != nil {
				return fmt.Errorf("request cancellation of child workflowrun %s/%s: %w", child.Namespace, child.Name, err)
			}
		default:
			return fmt.Errorf("cancel workflowrun target has unsupported kind %q", target.kind)
		}
	}
	return nil
}

func deriveTerminalWorkflowRunStatus(workflowRun *v1alpha1.WorkflowRun) {
	if workflowRun.Spec.CancelRequested || len(workflowRun.Status.Jobs) == 0 {
		return
	}

	phase := v1alpha1.WorkflowSucceeded
	for _, status := range workflowRun.Status.Jobs {
		switch status.Phase {
		case v1alpha1.JobFailed:
			phase = v1alpha1.WorkflowFailed
		case v1alpha1.JobSucceeded, v1alpha1.JobSkipped:
		default:
			return
		}
	}
	workflowRun.Status.Phase = phase
}

func deriveJobStatuses(resources *workflowRunResources) {
	workflowRun := resources.workflowRun
	for jobName, status := range workflowRun.Status.Jobs {
		if status.Phase != v1alpha1.JobRunning {
			continue
		}
		phase, ok := terminalJobPhase(status)
		if !ok {
			continue
		}
		status.Phase = phase
		if phase == v1alpha1.JobSucceeded {
			outputs, err := resolveJobOutputs(resources.snapshot.Spec.Jobs[jobName], status)
			if err != nil {
				status.Phase = v1alpha1.JobFailed
				workflowRun.Status.Message = fmt.Sprintf("resolve outputs for job %q: %v", jobName, err)
			} else {
				status.Outputs = outputs
			}
		}
		workflowRun.Status.Jobs[jobName] = status
	}
}

func resolveJobOutputs(job v1alpha1.JobSpec, status v1alpha1.JobStatus) (map[string]string, error) {
	if len(job.Outputs) == 0 {
		return nil, nil
	}
	steps := make(map[string]map[string]string, len(status.Steps))
	for _, step := range status.Steps {
		if len(step.Outputs) > 0 {
			steps[step.Name] = step.Outputs
		}
	}
	outputNames := make([]string, 0, len(job.Outputs))
	for name := range job.Outputs {
		outputNames = append(outputNames, name)
	}
	sort.Strings(outputNames)
	outputs := make(map[string]string, len(job.Outputs))
	for _, name := range outputNames {
		value, err := resolveExpr(job.Outputs[name], &resolveContext{steps: steps})
		if err != nil {
			return nil, fmt.Errorf("output %q: %w", name, err)
		}
		outputs[name] = value
	}
	return outputs, nil
}

func deriveSkippedJobStatuses(jobs map[string]v1alpha1.JobStatus) {
	jobNames := make([]string, 0, len(jobs))
	for jobName := range jobs {
		jobNames = append(jobNames, jobName)
	}
	sort.Strings(jobNames)

	// A newly skipped job can transitively block another job later or earlier
	// in lexical order, so derive until the bounded job graph reaches a fixed point.
	for range len(jobNames) {
		changed := false
		for _, jobName := range jobNames {
			status := jobs[jobName]
			if status.Phase != v1alpha1.JobPending && status.Phase != v1alpha1.JobWaiting {
				continue
			}
			if !hasFailedOrSkippedDependency(status, jobs) {
				continue
			}
			status.Phase = v1alpha1.JobSkipped
			jobs[jobName] = status
			changed = true
		}
		if !changed {
			return
		}
	}
}

func hasFailedOrSkippedDependency(status v1alpha1.JobStatus, jobs map[string]v1alpha1.JobStatus) bool {
	for _, pre := range status.Pre {
		switch jobs[pre].Phase {
		case v1alpha1.JobFailed, v1alpha1.JobSkipped:
			return true
		}
	}
	return false
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

func deriveStepStatusesFromChildRuns(resources *workflowRunResources) {
	workflowRun := resources.workflowRun
	keys := make([]string, 0, len(resources.childRuns))
	for key := range resources.childRuns {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		run := resources.childRuns[key]
		stepPhase, terminal := terminalRunStepPhase(run.Status.Phase)
		if !terminal {
			continue
		}
		jobName := run.Labels[v1alpha1.WorkflowJobLabel]
		stepName := run.Labels[v1alpha1.WorkflowStepLabel]
		jobStatus, ok := workflowRun.Status.Jobs[jobName]
		if !ok {
			continue
		}
		for i := range jobStatus.Steps {
			step := &jobStatus.Steps[i]
			if step.Name != stepName {
				continue
			}
			if step.RunName != "" && step.RunName != run.Name {
				break
			}
			step.RunName = run.Name
			step.Phase = stepPhase
			step.Outputs = maps.Clone(run.Status.Outputs)
			workflowRun.Status.Jobs[jobName] = jobStatus
			break
		}
	}
}

func terminalRunStepPhase(phase v1alpha1.RunPhase) (v1alpha1.StepPhase, bool) {
	switch phase {
	case v1alpha1.RunSucceeded:
		return v1alpha1.StepSucceeded, true
	case v1alpha1.RunFailed, v1alpha1.RunTimeout, v1alpha1.RunCancelled:
		return v1alpha1.StepFailed, true
	default:
		return "", false
	}
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

func workflowCallRunName(parentName string, jobName string) string {
	sum := sha256.Sum256([]byte(jobName))
	suffix := hex.EncodeToString(sum[:])[:10]
	const maxNameLength = 253
	maxPrefixLength := maxNameLength - len(suffix) - 1
	prefix := parentName
	if len(prefix) > maxPrefixLength {
		prefix = strings.TrimRight(prefix[:maxPrefixLength], "-.")
	}
	if prefix == "" {
		return suffix
	}
	return prefix + "-" + suffix
}
