package controller

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// WorkflowReconciler watches Workflow CRs and orchestrates Runs.
type WorkflowReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kruntimes.io,resources=workflows,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kruntimes.io,resources=runs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

func (r *WorkflowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("workflow", req.NamespacedName)

	var wf v1alpha1.Workflow
	if err := r.Get(ctx, req.NamespacedName, &wf); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get workflow: %w", err)
	}

	if wf.Status.Jobs == nil {
		wf.Status.Jobs = make(map[string]v1alpha1.JobStatus)
	}

	if wf.Status.Phase == v1alpha1.WorkflowSucceeded || wf.Status.Phase == v1alpha1.WorkflowFailed {
		return ctrl.Result{}, nil
	}

	if wf.Status.Phase == "" || wf.Status.Phase == v1alpha1.WorkflowPending {
		wf.Status.Phase = v1alpha1.WorkflowRunning
		if wf.Status.Jobs == nil {
			wf.Status.Jobs = make(map[string]v1alpha1.JobStatus)
		}
		if err := r.Status().Update(ctx, &wf); err != nil {
			return ctrl.Result{}, fmt.Errorf("update workflow to running: %w", err)
		}
	}

	// Detect implicit needs from cross-job output references.
	jobs := detectImplicitNeeds(wf.Spec.Jobs)

	sorted, err := topoSort(jobs)
	if err != nil {
		wf.Status.Phase = v1alpha1.WorkflowFailed
		wf.Status.Message = err.Error()
		if uerr := r.Status().Update(ctx, &wf); uerr != nil {
			return ctrl.Result{}, fmt.Errorf("update workflow failed: %w", uerr)
		}
		return ctrl.Result{}, nil
	}

	completedOutputs := make(map[string]map[string]string)
	allDone := true
	anyFailed := false

	for _, jobName := range sorted {
		jobSpec := jobs[jobName]
		job := jobSpec
		js, exists := wf.Status.Jobs[jobName]
		if !exists {
			js = v1alpha1.JobStatus{Phase: v1alpha1.JobPending, Steps: make(map[string]v1alpha1.StepStatus)}
		}

		if js.Phase == v1alpha1.JobPending && needsMet(job.Needs, wf.Status.Jobs) {
			js.Phase = v1alpha1.JobWaiting
		}
		if js.Phase == v1alpha1.JobWaiting {
			js.Phase = v1alpha1.JobRunning
		}
		if js.Phase == v1alpha1.JobRunning {
			r.runJobSteps(ctx, &wf, jobName, &job, &js, completedOutputs, log)
		}

		switch js.Phase {
		case v1alpha1.JobSucceeded:
			jobOutputs := make(map[string]string)
			// Expose step outputs with both "step.key" and bare "key" for cross-job refs.
			for name, ss := range js.Steps {
				for k, v := range ss.Outputs {
					jobOutputs[name+"."+k] = v
					jobOutputs[k] = v // ${{ jobs.X.outputs.key }} ref
				}
			}
			if job.Outputs != nil {
				ectx := &resolveContext{steps: stepOutputs(&js), jobs: completedOutputs}
				for k, expr := range job.Outputs {
					val, err := resolveExpr(expr, ectx)
					if err != nil {
						log.Error(err, "resolve job output", "job", jobName, "key", k)
						continue
					}
					jobOutputs[k] = val
				}
			}
			completedOutputs[jobName] = jobOutputs
		case v1alpha1.JobFailed:
			anyFailed = true
			allDone = false
		default:
			allDone = false
		}

		if wf.Status.Jobs == nil {
			wf.Status.Jobs = make(map[string]v1alpha1.JobStatus)
		}
		wf.Status.Jobs[jobName] = js
	}

	if anyFailed {
		wf.Status.Phase = v1alpha1.WorkflowFailed
		wf.Status.Message = "one or more jobs failed"
	} else if allDone {
		wf.Status.Phase = v1alpha1.WorkflowSucceeded
		wf.Status.Message = "all jobs completed"
	}

	if err := r.Status().Update(ctx, &wf); err != nil {
		return ctrl.Result{}, fmt.Errorf("update workflow status: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *WorkflowReconciler) runJobSteps(ctx context.Context, wf *v1alpha1.Workflow, jobName string, job *v1alpha1.JobSpec, js *v1alpha1.JobStatus, completedOutputs map[string]map[string]string, log logr.Logger) {
	if js.Steps == nil {
		js.Steps = make(map[string]v1alpha1.StepStatus)
	}

	// List existing Runs for this workflow+job and diff.
	var runs v1alpha1.RunList
	if err := r.List(ctx, &runs,
		client.InNamespace(wf.Namespace),
		client.MatchingLabels{"workflow": wf.Name, "job": jobName},
	); err != nil {
		log.Error(err, "list runs", "job", jobName)
		return
	}
	runsByStep := make(map[string]*v1alpha1.Run)
	for i := range runs.Items {
		run := &runs.Items[i]
		if sn := run.Labels["step"]; sn != "" {
			runsByStep[sn] = run
		}
	}

	// Sync step statuses from Run phases.
	for name, run := range runsByStep {
		ss, exists := js.Steps[name]
		if !exists {
			ss = v1alpha1.StepStatus{Phase: v1alpha1.StepPending}
		}
		switch run.Status.Phase {
		case v1alpha1.RunSucceeded:
			if ss.Phase == v1alpha1.StepRunning {
				ss.Phase = v1alpha1.StepSucceeded
				ss.Outputs = run.Status.Outputs
			}
		case v1alpha1.RunFailed:
			if ss.Phase == v1alpha1.StepRunning {
				ss.Phase = v1alpha1.StepFailed
			}
		}
		ss.RunName = run.Name
		js.Steps[name] = ss
	}

	// Process steps in order.
	for i := range job.Steps {
		step := &job.Steps[i]
		ss, exists := js.Steps[step.Name]
		if !exists {
			ss = v1alpha1.StepStatus{Phase: v1alpha1.StepPending}
			js.Steps[step.Name] = ss
		}

		switch ss.Phase {
		case v1alpha1.StepPending:
			exprCtx := &resolveContext{steps: stepOutputs(js), jobs: completedOutputs}
			run, err := r.buildRun(wf, jobName, job, step, exprCtx)
			if err != nil {
				ss.Phase = v1alpha1.StepFailed
				js.Steps[step.Name] = ss
				js.Phase = v1alpha1.JobFailed
				log.Error(err, "build run", "step", step.Name)
				return
			}
			if err := controllerutil.SetControllerReference(wf, run, r.Scheme); err != nil {
				ss.Phase = v1alpha1.StepFailed
				js.Steps[step.Name] = ss
				js.Phase = v1alpha1.JobFailed
				log.Error(err, "set owner ref", "step", step.Name)
				return
			}
			if err := r.Create(ctx, run); err != nil && !apierrors.IsAlreadyExists(err) {
				ss.Phase = v1alpha1.StepFailed
				js.Steps[step.Name] = ss
				js.Phase = v1alpha1.JobFailed
				log.Error(err, "create run", "step", step.Name)
				return
			}
			ss.Phase = v1alpha1.StepRunning
			ss.RunName = run.Name
			js.Steps[step.Name] = ss
			return

		case v1alpha1.StepRunning:
			return

		case v1alpha1.StepFailed:
			js.Phase = v1alpha1.JobFailed
			return

		case v1alpha1.StepSucceeded:
		}
	}

	for _, ss := range js.Steps {
		if ss.Phase != v1alpha1.StepSucceeded {
			return
		}
	}
	js.Phase = v1alpha1.JobSucceeded
}

func (r *WorkflowReconciler) buildRun(wf *v1alpha1.Workflow, jobName string, job *v1alpha1.JobSpec, step *v1alpha1.StepSpec, ctx *resolveContext) (*v1alpha1.Run, error) {
	resolvedRun, err := resolveExpr(step.Run, ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve run: %w", err)
	}
	resolvedArgs, err := resolveStepArgs(step.Args, ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve args: %w", err)
	}
	resolvedEnv, err := resolveEnv(step.Env, ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve env: %w", err)
	}

	rt := job.RunsOn
	if rt == "" {
		rt = "bash"
	}

	runSpec := v1alpha1.RunSpec{
		Runtime: rt,
		Args:    resolvedArgs,
	}
	if resolvedRun != "" {
		runSpec.Source = &v1alpha1.CodeSource{Inline: &resolvedRun}
	}

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("wf-%s-%s-%s", wf.Name, jobName, step.Name),
			Namespace: wf.Namespace,
			Labels: map[string]string{
				"workflow": wf.Name,
				"job":      jobName,
				"step":     step.Name,
			},
		},
		Spec: runSpec,
	}

	if len(resolvedEnv) > 0 {
		envVars := make([]corev1.EnvVar, 0, len(resolvedEnv))
		for k, v := range resolvedEnv {
			envVars = append(envVars, corev1.EnvVar{Name: k, Value: v})
		}
		run.Spec.Env = envVars
	}

	return run, nil
}

func needsMet(needs []string, jobs map[string]v1alpha1.JobStatus) bool {
	if len(needs) == 0 {
		return true
	}
	for _, n := range needs {
		js, ok := jobs[n]
		if !ok || js.Phase != v1alpha1.JobSucceeded {
			return false
		}
	}
	return true
}

func stepOutputs(js *v1alpha1.JobStatus) map[string]map[string]string {
	m := make(map[string]map[string]string)
	for name, ss := range js.Steps {
		if ss.Outputs != nil {
			m[name] = ss.Outputs
		}
	}
	return m
}

// detectImplicitNeeds adds implicit needs based on ${{ jobs.X.outputs.* }} references.
func detectImplicitNeeds(jobs map[string]v1alpha1.JobSpec) map[string]v1alpha1.JobSpec {
	result := make(map[string]v1alpha1.JobSpec, len(jobs))
	for name, job := range jobs {
		needs := make(map[string]bool)
		for _, n := range job.Needs {
			needs[n] = true
		}
		for _, step := range job.Steps {
			for _, ref := range exprPattern.FindAllStringSubmatch(step.Run, -1) {
				if parts := strings.SplitN(ref[1], ".", 4); len(parts) == 4 && parts[0] == "jobs" && parts[1] != name && parts[2] == "outputs" {
					needs[parts[1]] = true
				}
			}
			for _, arg := range step.Args {
				for _, ref := range exprPattern.FindAllStringSubmatch(arg, -1) {
					if parts := strings.SplitN(ref[1], ".", 4); len(parts) == 4 && parts[0] == "jobs" && parts[1] != name && parts[2] == "outputs" {
						needs[parts[1]] = true
					}
				}
			}
		}
		// Build the merged job spec.
		merged := v1alpha1.JobSpec{
			RunsOn:  job.RunsOn,
			Steps:   job.Steps,
			Outputs: job.Outputs,
		}
		for n := range needs {
			merged.Needs = append(merged.Needs, n)
		}
		result[name] = merged
	}
	return result
}

// topoSort returns job names in topological order. Returns error on cycle.
func topoSort(jobs map[string]v1alpha1.JobSpec) ([]string, error) {
	inDegree := make(map[string]int)
	deps := make(map[string][]string)
	for name := range jobs {
		inDegree[name] = 0
	}
	for name, job := range jobs {
		for _, n := range job.Needs {
			if _, ok := jobs[n]; !ok {
				continue // reference to unknown job
			}
			deps[n] = append(deps[n], name)
			inDegree[name]++
		}
	}

	var names []string
	queue := make([]string, 0)
	for name, d := range inDegree {
		if d == 0 {
			queue = append(queue, name)
		}
	}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		names = append(names, name)
		for _, dep := range deps[name] {
			inDegree[dep]--
			if inDegree[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if len(names) != len(jobs) {
		return nil, fmt.Errorf("cycle detected in job dependencies")
	}
	return names, nil
}

// SetupWithManager registers the Workflow reconciler.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Workflow{}).
		Owns(&v1alpha1.Run{}).
		Complete(r)
}
