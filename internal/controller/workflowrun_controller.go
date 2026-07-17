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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
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

type workflowRunState string

const (
	workflowRunStateEmpty      workflowRunState = "Empty"
	workflowRunStatePending    workflowRunState = "Pending"
	workflowRunStateRunning    workflowRunState = "Running"
	workflowRunStateCancelling workflowRunState = "Cancelling"
	workflowRunStateTerminal   workflowRunState = "Terminal"
)

type workflowRunAction string

const (
	workflowRunActionNone                        workflowRunAction = "None"
	workflowRunActionInitialize                  workflowRunAction = "Initialize"
	workflowRunActionStartRunnableSteps          workflowRunAction = "StartRunnableSteps"
	workflowRunActionRequestChildRunCancellation workflowRunAction = "RequestChildRunCancellation"
)

type workflowRunResources struct {
	workflowRun *v1alpha1.WorkflowRun
	childRuns   map[string]*v1alpha1.Run
	snapshot    *workflowExecutionSnapshot
}

type workflowRunPlan struct {
	state    workflowRunState
	action   workflowRunAction
	targets  []workflowRunStepTarget
	runNames []string
}

type workflowRunStepTarget struct {
	jobName   string
	stepIndex int
}

func workflowRunJobs(resources *workflowRunResources) map[string]v1alpha1.JobSpec {
	return resources.snapshot.rootJobs()
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
		if revision.Labels[v1alpha1.WorkflowRootRunUIDLabel] != string(workflowRun.UID) {
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
		if runNames := childRunsToCancel(resources.childRuns); len(runNames) > 0 {
			plan.state = workflowRunStateCancelling
			plan.action = workflowRunActionRequestChildRunCancellation
			plan.runNames = runNames
			return plan
		}
	}
	if state == workflowRunStateTerminal {
		return plan
	}
	if state == workflowRunStateCancelling {
		return plan
	}
	jobs := workflowRunJobs(resources)
	if len(jobs) == 0 || len(workflowRun.Status.Jobs) == 0 {
		return plan
	}
	if targets := runnableStepTargets(workflowRun.Status.Jobs, jobs); len(targets) > 0 {
		plan.action = workflowRunActionStartRunnableSteps
		plan.targets = targets
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
		snapshot, err = r.resolveWorkflowRunSnapshot(ctx, workflowRun)
		if err != nil {
			if reason, rejected := workflowRunRejectionReason(err); rejected {
				return rejectWorkflowRun(workflowRun, reason, err.Error())
			}
			return err
		}
		if err := validateResolvedWorkflowJobs(snapshot.rootJobs()); err != nil {
			return rejectWorkflowRun(workflowRun, "WorkflowValidationFailed", err.Error())
		}
		persistedName, persistedSnapshot, err := r.ensureWorkflowSnapshot(ctx, workflowRun, snapshot)
		if err != nil {
			if reason, rejected := workflowRunRejectionReason(err); rejected {
				return rejectWorkflowRun(workflowRun, reason, err.Error())
			}
			return err
		}
		snapshotName = persistedName
		snapshot = persistedSnapshot
		resources.snapshot = snapshot
	}
	workflowRun.Status.Phase = v1alpha1.WorkflowPending
	workflowRun.Status.Message = ""
	workflowRun.Status.Jobs = resolvedJobStatuses(snapshot.rootJobs())
	workflowRun.Status.SnapshotName = snapshotName
	setWorkflowRunAcceptedCondition(workflowRun, metav1.ConditionTrue, "Accepted", "WorkflowRun accepted and initialized")
	return nil
}

func (r *WorkflowRunReconciler) applyWorkflowRunAction(ctx context.Context, resources *workflowRunResources, plan workflowRunPlan) error {
	switch plan.action {
	case workflowRunActionInitialize:
		return r.applyInitializeWorkflowRun(ctx, resources)
	case workflowRunActionStartRunnableSteps:
		return r.applyStartRunnableSteps(ctx, resources, plan.targets)
	case workflowRunActionRequestChildRunCancellation:
		return r.applyRequestChildRunCancellation(ctx, resources, plan.runNames)
	}
	return nil
}

// SetupWithManager registers the WorkflowRun reconciler.
func (r *WorkflowRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.WorkflowRun{}).
		Owns(&v1alpha1.Run{}).
		Owns(&appsv1.ControllerRevision{}).
		Complete(r)
}

func workflowRunRejectionReason(err error) (string, bool) {
	var inputErr *workflowInputBindingError
	var snapshotErr *workflowSnapshotResolutionError
	switch {
	case apierrors.IsNotFound(err):
		return "WorkflowResolutionFailed", true
	case errors.As(err, &inputErr):
		return "WorkflowInputBindingFailed", true
	case errors.As(err, &snapshotErr):
		return "WorkflowResolutionFailed", true
	default:
		return "", false
	}
}

func validateResolvedWorkflowJobs(jobs map[string]v1alpha1.JobSpec) error {
	jobNames := make([]string, 0, len(jobs))
	for jobName := range jobs {
		jobNames = append(jobNames, jobName)
	}
	sort.Strings(jobNames)
	for _, jobName := range jobNames {
		job := jobs[jobName]
		if job.Uses != "" {
			return fmt.Errorf("job %q uses reusable workflow %q, but job-level uses is not implemented yet", jobName, job.Uses)
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

func (r *WorkflowRunReconciler) applyStartRunnableSteps(ctx context.Context, resources *workflowRunResources, targets []workflowRunStepTarget) error {
	workflowRun := resources.workflowRun
	for _, target := range targets {
		job := workflowRunJobs(resources)[target.jobName]
		run, err := r.createOrReuseStepRun(ctx, resources, target.jobName, job, target.stepIndex)
		if err != nil {
			return err
		}
		recordStepRun(workflowRun, target.jobName, target.stepIndex, run.Name)
	}
	return nil
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

func runnableStepTargets(statuses map[string]v1alpha1.JobStatus, jobs map[string]v1alpha1.JobSpec) []workflowRunStepTarget {
	jobNames := make([]string, 0, len(jobs))
	for jobName := range jobs {
		jobNames = append(jobNames, jobName)
	}
	sort.Strings(jobNames)

	targets := make([]workflowRunStepTarget, 0, len(jobNames))
	for _, jobName := range jobNames {
		job := jobs[jobName]
		status, ok := statuses[jobName]
		if !ok || len(status.Steps) != len(job.Steps) {
			continue
		}
		if jobReadyToStart(status, statuses) && status.Steps[0].RunName == "" {
			targets = append(targets, workflowRunStepTarget{jobName: jobName, stepIndex: 0})
			continue
		}
		if stepIndex, ok := nextStepToStart(status); ok {
			targets = append(targets, workflowRunStepTarget{jobName: jobName, stepIndex: stepIndex})
		}
	}
	return targets
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
	deriveJobStatuses(resources.workflowRun)
	if resources.workflowRun.Spec.CancelRequested {
		deriveCancelledWorkflowRunStatus(resources)
		return
	}
	deriveSkippedJobStatuses(resources.workflowRun.Status.Jobs)
	deriveTerminalWorkflowRunStatus(resources.workflowRun)
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

func childRunsToCancel(childRuns map[string]*v1alpha1.Run) []string {
	runNames := make([]string, 0, len(childRuns))
	for _, run := range childRuns {
		if !isTerminalRunPhase(run.Status.Phase) && !run.Spec.CancelRequested {
			runNames = append(runNames, run.Name)
		}
	}
	sort.Strings(runNames)
	return runNames
}

func (r *WorkflowRunReconciler) applyRequestChildRunCancellation(ctx context.Context, resources *workflowRunResources, runNames []string) error {
	childRuns := make(map[string]*v1alpha1.Run, len(resources.childRuns))
	for _, run := range resources.childRuns {
		childRuns[run.Name] = run
	}
	for _, runName := range runNames {
		run := childRuns[runName]
		if run == nil || run.Spec.CancelRequested || isTerminalRunPhase(run.Status.Phase) {
			continue
		}
		base := run.DeepCopy()
		run.Spec.CancelRequested = true
		if err := r.Patch(ctx, run, client.MergeFrom(base)); err != nil {
			return fmt.Errorf("request cancellation of child run %s/%s: %w", run.Namespace, run.Name, err)
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

func deriveJobStatuses(workflowRun *v1alpha1.WorkflowRun) {
	for jobName, status := range workflowRun.Status.Jobs {
		if status.Phase != v1alpha1.JobRunning {
			continue
		}
		phase, ok := terminalJobPhase(status)
		if !ok {
			continue
		}
		status.Phase = phase
		workflowRun.Status.Jobs[jobName] = status
	}
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
