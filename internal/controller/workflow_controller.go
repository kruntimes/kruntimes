package controller

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

// +kubebuilder:rbac:groups=kruntimes.kruntimes.com,resources=workflows,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kruntimes.kruntimes.com,resources=workflows/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kruntimes.kruntimes.com,resources=runs,verbs=get;list;watch;create;update;patch;delete
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

	completedOutputs := make(map[string]map[string]string)
	allDone := true
	anyFailed := false

	for i := range wf.Spec.Jobs {
		job := &wf.Spec.Jobs[i]
		js, exists := wf.Status.Jobs[job.Name]
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
			r.runJobSteps(ctx, &wf, job, &js, completedOutputs, log)
		}

		switch js.Phase {
		case v1alpha1.JobSucceeded:
			if job.Outputs != nil {
				jobOutputs := make(map[string]string)
				for k, expr := range job.Outputs {
					ectx := &resolveContext{steps: stepOutputs(&js), jobs: completedOutputs}
					val, err := resolveExpr(expr, ectx)
					if err != nil {
						log.Error(err, "resolve job output", "job", job.Name, "key", k)
						continue
					}
					jobOutputs[k] = val
				}
				completedOutputs[job.Name] = jobOutputs
			}
		case v1alpha1.JobFailed:
			anyFailed = true
			allDone = false
		default:
			allDone = false
		}

		if wf.Status.Jobs == nil {
				wf.Status.Jobs = make(map[string]v1alpha1.JobStatus)
			}
			wf.Status.Jobs[job.Name] = js
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

func (r *WorkflowReconciler) runJobSteps(ctx context.Context, wf *v1alpha1.Workflow, job *v1alpha1.JobSpec, js *v1alpha1.JobStatus, completedOutputs map[string]map[string]string, log logr.Logger) {
	if js.Steps == nil {
		js.Steps = make(map[string]v1alpha1.StepStatus)
	}
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
			run, err := r.buildRun(wf, job, step, exprCtx)
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
			if err := r.Create(ctx, run); err != nil {
				ss.Phase = v1alpha1.StepFailed
				js.Steps[step.Name] = ss
				js.Phase = v1alpha1.JobFailed
				log.Error(err, "create run", "step", step.Name)
				return
			}
			ss.Phase = v1alpha1.StepRunning
			ss.RunName = run.Name
			js.Steps[step.Name] = ss
			return // wait for Run to complete before next step

		case v1alpha1.StepRunning:
			var run v1alpha1.Run
			if err := r.Get(ctx, types.NamespacedName{Name: ss.RunName, Namespace: wf.Namespace}, &run); err != nil {
				log.Error(err, "get run", "run", ss.RunName)
				return
			}
			switch run.Status.Phase {
			case v1alpha1.RunSucceeded:
				ss.Phase = v1alpha1.StepSucceeded
				ss.Outputs = run.Status.Outputs
			case v1alpha1.RunFailed:
				ss.Phase = v1alpha1.StepFailed
			default:
				return
			}
			js.Steps[step.Name] = ss

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

func (r *WorkflowReconciler) buildRun(wf *v1alpha1.Workflow, job *v1alpha1.JobSpec, step *v1alpha1.StepSpec, ctx *resolveContext) (*v1alpha1.Run, error) {
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

	rt := step.Runtime
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
			Name:      fmt.Sprintf("wf-%s-%s-%s", wf.Name, job.Name, step.Name),
			Namespace: wf.Namespace,
			Labels: map[string]string{
				"workflow": wf.Name,
				"job":      job.Name,
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

// SetupWithManager registers the Workflow reconciler.
func (r *WorkflowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Workflow{}).
		Owns(&v1alpha1.Run{}).
		Complete(r)
}
