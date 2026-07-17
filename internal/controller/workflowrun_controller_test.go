package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func workflowRunTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add kruntimes scheme: %v", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apps scheme: %v", err)
	}
	return scheme
}

func TestWorkflowRunReconcilerAcceptsWorkflowRun(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", UID: "workflowrun-uid", Generation: 3},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{
						{Name: "checkout", Run: "git status"},
						{Name: "package", Run: "make package"},
					},
				},
				"test": {
					RunsOn: "bash",
					Needs:  []string{"build"},
					Steps:  []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}},
				},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	reconcileWorkflowRun(t, reconciler, req, 2)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowRunning {
		t.Fatalf("phase = %q, want %q", updated.Status.Phase, v1alpha1.WorkflowRunning)
	}
	build := updated.Status.Jobs["build"]
	if build.Phase != v1alpha1.JobRunning {
		t.Fatalf("build phase = %q, want %q", build.Phase, v1alpha1.JobRunning)
	}
	if len(build.Pre) != 0 {
		t.Fatalf("build pre = %v, want empty", build.Pre)
	}
	if len(build.Steps) != 2 || build.Steps[0].Name != "checkout" || build.Steps[1].Name != "package" {
		t.Fatalf("build steps = %#v, want checkout, package", build.Steps)
	}
	for _, step := range build.Steps {
		if step.Name == "checkout" {
			if step.Phase != v1alpha1.StepRunning || step.RunName == "" {
				t.Fatalf("step %s = %#v, want running with runName", step.Name, step)
			}
			continue
		}
		if step.Phase != v1alpha1.StepPending || step.RunName != "" {
			t.Fatalf("step %s = %#v, want pending without runName", step.Name, step)
		}
	}
	var childRuns v1alpha1.RunList
	if err := c.List(context.Background(), &childRuns, client.InNamespace(workflowRun.Namespace)); err != nil {
		t.Fatalf("list child runs: %v", err)
	}
	if len(childRuns.Items) != 1 {
		t.Fatalf("child runs = %#v, want one first-step run", childRuns.Items)
	}
	childRun := childRuns.Items[0]
	if childRun.Spec.Runtime != "bash" || childRun.Spec.Source == nil || childRun.Spec.Source.Inline == nil || *childRun.Spec.Source.Inline != "git status" {
		t.Fatalf("child run spec = %#v, want bash inline git status", childRun.Spec)
	}
	if childRun.Labels[v1alpha1.WorkflowRunUIDLabel] != string(workflowRun.UID) ||
		childRun.Labels[v1alpha1.WorkflowJobLabel] != "build" ||
		childRun.Labels[v1alpha1.WorkflowStepLabel] != "checkout" {
		t.Fatalf("child run labels = %v, want workflow/job/step labels", childRun.Labels)
	}
	if len(childRun.OwnerReferences) != 1 || childRun.OwnerReferences[0].Name != workflowRun.Name {
		t.Fatalf("owner references = %#v, want WorkflowRun owner", childRun.OwnerReferences)
	}
	testJob := updated.Status.Jobs["test"]
	if testJob.Phase != v1alpha1.JobWaiting {
		t.Fatalf("test phase = %q, want %q", testJob.Phase, v1alpha1.JobWaiting)
	}
	if len(testJob.Pre) != 1 || testJob.Pre[0] != "build" {
		t.Fatalf("test pre = %v, want [build]", testJob.Pre)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha1.WorkflowRunAcceptedCondition)
	if cond == nil {
		t.Fatalf("missing %s condition", v1alpha1.WorkflowRunAcceptedCondition)
	}
	if cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != workflowRun.Generation {
		t.Fatalf("condition = %#v, want true observed generation %d", cond, workflowRun.Generation)
	}
}

func TestWorkflowRunReconcilerStartsAllIndependentReadyJobs(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "parallel", Namespace: "default", UID: "workflowrun-uid"},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"alpha": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "first", Run: "echo alpha"}}},
			"beta":  {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "first", Run: "echo beta"}}},
		}},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}

	// The first reconcile only persists the resolved graph.
	reconcileWorkflowRun(t, reconciler, req, 1)
	assertChildRunCount(t, c, workflowRun.Namespace, 0)

	// A single StartRunnableSteps transition creates all independent ready jobs.
	reconcileWorkflowRun(t, reconciler, req, 1)
	assertChildRunCount(t, c, workflowRun.Namespace, 2)
	// A subsequent reconcile sees the next step as running and creates nothing.
	reconcileWorkflowRun(t, reconciler, req, 1)
	assertChildRunCount(t, c, workflowRun.Namespace, 2)
}

func TestCalculateWorkflowRunPlanSeparatesCurrentStateFromAction(t *testing.T) {
	empty := &v1alpha1.WorkflowRun{}
	plan := calculateWorkflowRunPlan(&workflowRunResources{workflowRun: empty})
	if plan.state != workflowRunStateEmpty || plan.action != workflowRunActionInitialize {
		t.Fatalf("empty plan = %#v, want Empty + Initialize", plan)
	}

	pending := &v1alpha1.WorkflowRun{
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
		}},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowPending,
			Jobs:  resolvedJobStatuses(map[string]v1alpha1.JobSpec{"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}}}),
		},
	}
	plan = calculateWorkflowRunPlan(&workflowRunResources{workflowRun: pending})
	if plan.state != workflowRunStatePending || plan.action != workflowRunActionStartRunnableSteps || len(plan.targets) != 1 || plan.targets[0] != (workflowRunStepTarget{jobName: "build", stepIndex: 0}) {
		t.Fatalf("pending plan = %#v, want Pending + StartRunnableSteps(build[0])", plan)
	}

	cancelled := pending.DeepCopy()
	cancelled.Status.Phase = v1alpha1.WorkflowCancelled
	plan = calculateWorkflowRunPlan(&workflowRunResources{workflowRun: cancelled})
	if plan.state != workflowRunStateTerminal || plan.action != workflowRunActionNone {
		t.Fatalf("cancelled plan = %#v, want Terminal + None", plan)
	}

	cancelling := pending.DeepCopy()
	cancelling.Spec.CancelRequested = true
	activeRun := workflowChildRun(cancelling, "build", "compile", "build-run", v1alpha1.RunRunning)
	plan = calculateWorkflowRunPlan(&workflowRunResources{
		workflowRun: cancelling,
		childRuns:   map[string]*v1alpha1.Run{workflowStepKey("build", "compile"): activeRun},
	})
	if plan.state != workflowRunStateCancelling || plan.action != workflowRunActionRequestChildRunCancellation || !slices.Equal(plan.runNames, []string{"build-run"}) {
		t.Fatalf("cancelling plan = %#v, want Cancelling + RequestChildRunCancellation(build-run)", plan)
	}

	// A late child Run watch must repair an early Cancelled projection caused by
	// the create-before-cache-observation window.
	cancelling.Status.Phase = v1alpha1.WorkflowCancelled
	plan = calculateWorkflowRunPlan(&workflowRunResources{
		workflowRun: cancelling,
		childRuns:   map[string]*v1alpha1.Run{workflowStepKey("build", "compile"): activeRun},
	})
	if plan.state != workflowRunStateCancelling || plan.action != workflowRunActionRequestChildRunCancellation || !slices.Equal(plan.runNames, []string{"build-run"}) {
		t.Fatalf("late child plan = %#v, want cancellation repair", plan)
	}
}

func TestCalculateWorkflowRunPlanProjectsStatusBeforeStartingReadyJobs(t *testing.T) {
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{UID: "workflowrun-uid"},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
			"lint":  {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "check", Run: "make lint"}}},
		}},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowRunning,
			Jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobRunning, Steps: []v1alpha1.StepStatus{{Name: "compile", Phase: v1alpha1.StepRunning, RunName: "build-run"}}},
				"lint":  {Phase: v1alpha1.JobPending, Steps: []v1alpha1.StepStatus{{Name: "check", Phase: v1alpha1.StepPending}}},
			},
		},
	}
	buildRun := workflowChildRun(workflowRun, "build", "compile", "build-run", v1alpha1.RunSucceeded)
	plan := calculateWorkflowRunPlan(&workflowRunResources{
		workflowRun: workflowRun,
		childRuns:   map[string]*v1alpha1.Run{workflowStepKey("build", "compile"): buildRun},
	})
	want := []workflowRunStepTarget{{jobName: "lint", stepIndex: 0}}
	if plan.state != workflowRunStateRunning || plan.action != workflowRunActionStartRunnableSteps || !slices.Equal(plan.targets, want) {
		t.Fatalf("plan = %#v, want Running + StartRunnableSteps(%#v)", plan, want)
	}
	if workflowRun.Status.Jobs["build"].Phase != v1alpha1.JobSucceeded {
		t.Fatalf("build status = %#v, want derived succeeded job", workflowRun.Status.Jobs["build"])
	}
}

func TestPlanWorkflowRunStartsAllRunnableSteps(t *testing.T) {
	workflowRun := &v1alpha1.WorkflowRun{
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}, {Name: "package", Run: "make package"}}},
			"lint":  {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "check", Run: "make lint"}}},
		}},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowRunning,
			Jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobRunning, Steps: []v1alpha1.StepStatus{{Name: "compile", Phase: v1alpha1.StepSucceeded, RunName: "compile-run"}, {Name: "package", Phase: v1alpha1.StepPending}}},
				"lint":  {Phase: v1alpha1.JobPending, Steps: []v1alpha1.StepStatus{{Name: "check", Phase: v1alpha1.StepPending}}},
			},
		},
	}

	plan := calculateWorkflowRunPlan(&workflowRunResources{workflowRun: workflowRun})
	want := []workflowRunStepTarget{{jobName: "build", stepIndex: 1}, {jobName: "lint", stepIndex: 0}}
	if plan.state != workflowRunStateRunning || plan.action != workflowRunActionStartRunnableSteps || !slices.Equal(plan.targets, want) {
		t.Fatalf("plan = %#v, want Running + StartRunnableSteps(%#v)", plan, want)
	}
}

func TestCalculateWorkflowRunPlanFinalizesJobsBeforeStartingReadyJobs(t *testing.T) {
	workflowRun := &v1alpha1.WorkflowRun{
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
			"lint":  {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "check", Run: "make lint"}}},
		}},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowRunning,
			Jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobRunning, Steps: []v1alpha1.StepStatus{{Name: "compile", Phase: v1alpha1.StepSucceeded, RunName: "compile-run"}}},
				"lint":  {Phase: v1alpha1.JobPending, Steps: []v1alpha1.StepStatus{{Name: "check", Phase: v1alpha1.StepPending}}},
			},
		},
	}

	plan := calculateWorkflowRunPlan(&workflowRunResources{workflowRun: workflowRun})
	want := []workflowRunStepTarget{{jobName: "lint", stepIndex: 0}}
	if plan.state != workflowRunStateRunning || plan.action != workflowRunActionStartRunnableSteps || !slices.Equal(plan.targets, want) {
		t.Fatalf("plan = %#v, want Running + StartRunnableSteps(%#v)", plan, want)
	}
	if workflowRun.Status.Jobs["build"].Phase != v1alpha1.JobSucceeded {
		t.Fatalf("build status = %#v, want derived succeeded job", workflowRun.Status.Jobs["build"])
	}
}

func TestJobReadyToStartChecksDependencyStatus(t *testing.T) {
	status := v1alpha1.JobStatus{
		Phase: v1alpha1.JobWaiting,
		Pre:   []string{"build"},
	}
	jobs := map[string]v1alpha1.JobStatus{
		"build": {Phase: v1alpha1.JobRunning},
	}
	if jobReadyToStart(status, jobs) {
		t.Fatal("job with running dependency is ready, want not ready")
	}

	jobs["build"] = v1alpha1.JobStatus{Phase: v1alpha1.JobSucceeded}
	if !jobReadyToStart(status, jobs) {
		t.Fatal("job with succeeded dependency is not ready, want ready")
	}
}

func TestWorkflowRunReconcilerSkipsBlockedJobsAndStartsIndependentJobs(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "failure", Namespace: "default", UID: "workflowrun-uid"},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build":  {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
			"test":   {RunsOn: "bash", Needs: []string{"build"}, Steps: []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}}},
			"deploy": {RunsOn: "bash", Needs: []string{"test"}, Steps: []v1alpha1.StepSpec{{Name: "apply", Run: "make deploy"}}},
			"lint":   {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "check", Run: "make lint"}}},
		}},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowRunning,
			Jobs: map[string]v1alpha1.JobStatus{
				"build":  {Phase: v1alpha1.JobFailed, Steps: []v1alpha1.StepStatus{{Name: "compile", Phase: v1alpha1.StepFailed, RunName: "build-run"}}},
				"test":   {Phase: v1alpha1.JobWaiting, Pre: []string{"build"}, Steps: []v1alpha1.StepStatus{{Name: "unit", Phase: v1alpha1.StepPending}}},
				"deploy": {Phase: v1alpha1.JobWaiting, Pre: []string{"test"}, Steps: []v1alpha1.StepStatus{{Name: "apply", Phase: v1alpha1.StepPending}}},
				"lint":   {Phase: v1alpha1.JobPending, Steps: []v1alpha1.StepStatus{{Name: "check", Phase: v1alpha1.StepPending}}},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	reconcileWorkflowRun(t, reconciler, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}, 1)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Jobs["test"].Phase != v1alpha1.JobSkipped || updated.Status.Jobs["deploy"].Phase != v1alpha1.JobSkipped {
		t.Fatalf("jobs = %#v, want direct and transitive dependents skipped", updated.Status.Jobs)
	}
	if updated.Status.Jobs["lint"].Phase != v1alpha1.JobRunning {
		t.Fatalf("lint = %#v, want independent job running", updated.Status.Jobs["lint"])
	}

	var runs v1alpha1.RunList
	if err := c.List(context.Background(), &runs, client.InNamespace(workflowRun.Namespace)); err != nil {
		t.Fatalf("list child runs: %v", err)
	}
	if len(runs.Items) != 1 || runs.Items[0].Labels[v1alpha1.WorkflowJobLabel] != "lint" {
		t.Fatalf("child runs = %#v, want only independent lint run", runs.Items)
	}
}

func TestWorkflowRunReconcilerRejectsUnsupportedJobLevelUsesDuringInitialization(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: "default", UID: "workflowrun-uid", Generation: 3},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"release": {
					Uses: "build-and-test",
				},
			},
		},
	}
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "build-and-test", Namespace: workflowRun.Namespace},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
		}},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun, workflow).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	reconcileWorkflowRun(t, reconciler, req, 2)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowFailed {
		t.Fatalf("phase = %q, want %q", updated.Status.Phase, v1alpha1.WorkflowFailed)
	}
	if !strings.Contains(updated.Status.Message, "job-level uses is not implemented yet") {
		t.Fatalf("message = %q, want job-level uses not implemented", updated.Status.Message)
	}
	if updated.Status.Jobs != nil {
		t.Fatalf("jobs = %#v, want nil for rejected workflowrun", updated.Status.Jobs)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha1.WorkflowRunAcceptedCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WorkflowValidationFailed" {
		t.Fatalf("condition = %#v, want validation rejection", cond)
	}
}

func TestWorkflowRunReconcilerRejectsCyclicJobDAG(t *testing.T) {
	cyclicJobs := func() map[string]v1alpha1.JobSpec {
		return map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Needs: []string{"test"}, Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
			"test":  {RunsOn: "bash", Needs: []string{"build"}, Steps: []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}}},
		}
	}

	for _, test := range []struct {
		name     string
		reusable bool
	}{
		{name: "inline"},
		{name: "reusable", reusable: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := workflowRunTestScheme(t)

			workflowRun := &v1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: test.name, Namespace: "default", UID: "workflowrun-uid", Generation: 3},
				Spec:       v1alpha1.WorkflowRunSpec{Jobs: cyclicJobs()},
			}
			objects := []client.Object{workflowRun}
			if test.reusable {
				workflowRun.Spec = v1alpha1.WorkflowRunSpec{Uses: "cyclic"}
				objects = append(objects, &v1alpha1.Workflow{
					ObjectMeta: metav1.ObjectMeta{Name: "cyclic", Namespace: workflowRun.Namespace},
					Spec:       v1alpha1.WorkflowSpec{Jobs: cyclicJobs()},
				})
			}

			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(objects...).
				WithStatusSubresource(&v1alpha1.WorkflowRun{}).
				Build()
			reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
			reconcileWorkflowRun(t, reconciler, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}, 1)

			var updated v1alpha1.WorkflowRun
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
				t.Fatalf("get workflowrun: %v", err)
			}
			if updated.Status.Phase != v1alpha1.WorkflowFailed || updated.Status.Jobs != nil {
				t.Fatalf("status = %#v, want failed before graph initialization", updated.Status)
			}
			if !strings.Contains(updated.Status.Message, "workflow job dependency cycle: build -> test -> build") {
				t.Fatalf("message = %q, want deterministic cycle path", updated.Status.Message)
			}
			condition := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha1.WorkflowRunAcceptedCondition)
			if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != "WorkflowValidationFailed" {
				t.Fatalf("condition = %#v, want WorkflowValidationFailed", condition)
			}
			assertChildRunCount(t, c, workflowRun.Namespace, 0)
		})
	}
}

func TestValidateResolvedWorkflowJobsRejectsUnknownDependency(t *testing.T) {
	err := validateResolvedWorkflowJobs(map[string]v1alpha1.JobSpec{
		"build": {
			RunsOn: "bash",
			Needs:  []string{"missing"},
			Steps:  []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), `job "build" needs unknown job "missing"`) {
		t.Fatalf("validateResolvedWorkflowJobs() error = %v, want unknown dependency", err)
	}
}

func TestWorkflowRunReconcilerReusesExistingFirstStepRun(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", UID: "workflowrun-uid", Generation: 3},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "checkout", Run: "git status"}},
				},
			},
		},
	}
	existingRun := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "existing-first-step",
			Namespace: workflowRun.Namespace,
			Labels:    workflowStepLabels(workflowRun, "build", "checkout"),
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Source:  &v1alpha1.CodeSource{Inline: ptrTo("git status")},
			Mode:    v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun, existingRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	reconcileWorkflowRun(t, reconciler, req, 2)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	step := updated.Status.Jobs["build"].Steps[0]
	if step.RunName != existingRun.Name || step.Phase != v1alpha1.StepRunning {
		t.Fatalf("step = %#v, want existing run marked running", step)
	}
	var childRuns v1alpha1.RunList
	if err := c.List(context.Background(), &childRuns, client.InNamespace(workflowRun.Namespace)); err != nil {
		t.Fatalf("list child runs: %v", err)
	}
	if len(childRuns.Items) != 1 {
		t.Fatalf("child runs = %#v, want existing run only", childRuns.Items)
	}
}

func TestWorkflowRunReconcilerRejectsJobWithoutRuntimeDuringInitialization(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", UID: "workflowrun-uid", Generation: 3},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					Steps: []v1alpha1.StepSpec{{Name: "checkout", Run: "git status"}},
				},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	reconcileWorkflowRun(t, reconciler, req, 2)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowFailed {
		t.Fatalf("phase = %q, want %q", updated.Status.Phase, v1alpha1.WorkflowFailed)
	}
	if updated.Status.Jobs != nil {
		t.Fatalf("jobs = %#v, want nil for rejected workflowrun", updated.Status.Jobs)
	}
	if !strings.Contains(updated.Status.Message, `job "build" must set runs-on`) {
		t.Fatalf("message = %q, want missing runs-on", updated.Status.Message)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha1.WorkflowRunAcceptedCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WorkflowValidationFailed" {
		t.Fatalf("condition = %#v, want validation rejection", cond)
	}
	var childRuns v1alpha1.RunList
	if err := c.List(context.Background(), &childRuns, client.InNamespace(workflowRun.Namespace)); err != nil {
		t.Fatalf("list child runs: %v", err)
	}
	if len(childRuns.Items) != 0 {
		t.Fatalf("child runs = %#v, want none", childRuns.Items)
	}
}

func TestWorkflowRunReconcilerObservesTerminalChildRuns(t *testing.T) {
	for _, test := range []struct {
		runPhase      v1alpha1.RunPhase
		stepPhase     v1alpha1.StepPhase
		workflowPhase v1alpha1.WorkflowPhase
	}{
		{runPhase: v1alpha1.RunSucceeded, stepPhase: v1alpha1.StepSucceeded, workflowPhase: v1alpha1.WorkflowSucceeded},
		{runPhase: v1alpha1.RunFailed, stepPhase: v1alpha1.StepFailed, workflowPhase: v1alpha1.WorkflowFailed},
		{runPhase: v1alpha1.RunTimeout, stepPhase: v1alpha1.StepFailed, workflowPhase: v1alpha1.WorkflowFailed},
		{runPhase: v1alpha1.RunCancelled, stepPhase: v1alpha1.StepFailed, workflowPhase: v1alpha1.WorkflowFailed},
	} {
		t.Run(string(test.runPhase), func(t *testing.T) {
			scheme := workflowRunTestScheme(t)
			workflowRun := &v1alpha1.WorkflowRun{
				ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", UID: "workflowrun-uid"},
				Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
					"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
				}},
				Status: v1alpha1.WorkflowRunStatus{
					Phase: v1alpha1.WorkflowRunning,
					Jobs: map[string]v1alpha1.JobStatus{
						"build": {Phase: v1alpha1.JobRunning, Steps: []v1alpha1.StepStatus{{Name: "compile", Phase: v1alpha1.StepRunning, RunName: "build-run"}}},
					},
				},
			}
			run := workflowChildRun(workflowRun, "build", "compile", "build-run", test.runPhase)
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(workflowRun, run).
				WithStatusSubresource(&v1alpha1.WorkflowRun{}).
				Build()
			reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
			reconcileWorkflowRun(t, reconciler, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}, 1)

			var updated v1alpha1.WorkflowRun
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
				t.Fatalf("get workflowrun: %v", err)
			}
			step := updated.Status.Jobs["build"].Steps[0]
			if step.Phase != test.stepPhase || step.RunName != run.Name {
				t.Fatalf("step = %#v, want %s %s", step, test.stepPhase, run.Name)
			}
			wantJobPhase := v1alpha1.JobFailed
			if test.stepPhase == v1alpha1.StepSucceeded {
				wantJobPhase = v1alpha1.JobSucceeded
			}
			if updated.Status.Jobs["build"].Phase != wantJobPhase || updated.Status.Phase != test.workflowPhase {
				t.Fatalf("status = %#v, want derived terminal job and workflow %s", updated.Status, test.workflowPhase)
			}
		})
	}
}

func TestWorkflowRunReconcilerCreatesNextStepAfterObservedSuccess(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", UID: "workflowrun-uid"},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: "bash",
				Steps: []v1alpha1.StepSpec{
					{Name: "compile", Run: "make build"},
					{Name: "package", Run: "make package"},
				},
			},
			"lint": {
				RunsOn: "bash",
				Steps:  []v1alpha1.StepSpec{{Name: "check", Run: "make lint"}},
			},
		}},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowRunning,
			Jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobRunning, Steps: []v1alpha1.StepStatus{
					{Name: "compile", Phase: v1alpha1.StepRunning, RunName: "compile-run"},
					{Name: "package", Phase: v1alpha1.StepPending},
				}},
				"lint": {Phase: v1alpha1.JobPending, Steps: []v1alpha1.StepStatus{{Name: "check", Phase: v1alpha1.StepPending}}},
			},
		},
	}
	compileRun := workflowChildRun(workflowRun, "build", "compile", "compile-run", v1alpha1.RunSucceeded)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun, compileRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}

	// Status projection and action planning happen in the same reconciliation,
	// so the next build step and independent lint job start immediately.
	reconcileWorkflowRun(t, reconciler, req, 1)
	assertChildRunCount(t, c, workflowRun.Namespace, 3)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	steps := updated.Status.Jobs["build"].Steps
	if steps[0].Phase != v1alpha1.StepSucceeded || steps[0].RunName != compileRun.Name {
		t.Fatalf("first step = %#v, want succeeded compile run", steps[0])
	}
	if steps[1].Phase != v1alpha1.StepRunning || steps[1].RunName == "" {
		t.Fatalf("second step = %#v, want running next-step run", steps[1])
	}
	lintStep := updated.Status.Jobs["lint"].Steps[0]
	if lintStep.Phase != v1alpha1.StepRunning || lintStep.RunName == "" {
		t.Fatalf("lint step = %#v, want running first-step run", lintStep)
	}

	var runs v1alpha1.RunList
	if err := c.List(context.Background(), &runs, client.InNamespace(workflowRun.Namespace)); err != nil {
		t.Fatalf("list child runs: %v", err)
	}
	byName := make(map[string]v1alpha1.Run, len(runs.Items))
	for _, run := range runs.Items {
		byName[run.Name] = run
	}
	packageRun, ok := byName[steps[1].RunName]
	if !ok {
		t.Fatalf("missing next-step run %q", steps[1].RunName)
	}
	if packageRun.Spec.Source == nil || packageRun.Spec.Source.Inline == nil || *packageRun.Spec.Source.Inline != "make package" {
		t.Fatalf("next-step run spec = %#v, want package command", packageRun.Spec)
	}
	if packageRun.Labels[v1alpha1.WorkflowStepLabel] != "package" {
		t.Fatalf("next-step run labels = %v, want package step label", packageRun.Labels)
	}
	lintRun, ok := byName[lintStep.RunName]
	if !ok {
		t.Fatalf("missing lint run %q", lintStep.RunName)
	}
	if lintRun.Spec.Source == nil || lintRun.Spec.Source.Inline == nil || *lintRun.Spec.Source.Inline != "make lint" {
		t.Fatalf("lint run spec = %#v, want lint command", lintRun.Spec)
	}
}

func TestWorkflowRunReconcilerDoesNotPatchDerivedStatusWhenActionFails(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", UID: "workflowrun-uid"},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
			"lint":  {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "check", Run: "make lint"}}},
		}},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowRunning,
			Jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobRunning, Steps: []v1alpha1.StepStatus{{Name: "compile", Phase: v1alpha1.StepRunning, RunName: "compile-run"}}},
				"lint":  {Phase: v1alpha1.JobPending, Steps: []v1alpha1.StepStatus{{Name: "check", Phase: v1alpha1.StepPending}}},
			},
		},
	}
	compileRun := workflowChildRun(workflowRun, "build", "compile", "compile-run", v1alpha1.RunSucceeded)
	createErr := errors.New("create child run")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun, compileRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return createErr
			},
		}).
		Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)})
	if !errors.Is(err, createErr) {
		t.Fatalf("Reconcile error = %v, want %v", err, createErr)
	}

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	build := updated.Status.Jobs["build"]
	if build.Phase != v1alpha1.JobRunning || build.Steps[0].Phase != v1alpha1.StepRunning {
		t.Fatalf("build status = %#v, want persisted status unchanged", build)
	}
}

func TestWorkflowRunReconcilerRecoversAfterRestartAcrossStatusPatchFailure(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", UID: "workflowrun-uid"},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: "bash",
				Steps: []v1alpha1.StepSpec{
					{Name: "compile", Run: "make build"},
					{Name: "package", Run: "make package"},
				},
			},
		}},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowRunning,
			Jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobRunning, Steps: []v1alpha1.StepStatus{
					{Name: "compile", Phase: v1alpha1.StepSucceeded, RunName: "compile-run"},
					{Name: "package", Phase: v1alpha1.StepPending},
				}},
			},
		},
	}
	compileRun := workflowChildRun(workflowRun, "build", "compile", "compile-run", v1alpha1.RunSucceeded)
	statusErr := errors.New("patch workflowrun status")
	failStatusPatch := true
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun, compileRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}, &v1alpha1.Run{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, underlying client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if subResourceName == "status" && failStatusPatch {
					return statusErr
				}
				return underlying.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}

	firstController := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	if _, err := firstController.Reconcile(context.Background(), req); !errors.Is(err, statusErr) {
		t.Fatalf("first Reconcile error = %v, want %v", err, statusErr)
	}
	assertChildRunCount(t, c, workflowRun.Namespace, 2)

	var persisted v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &persisted); err != nil {
		t.Fatalf("get workflowrun after failed status patch: %v", err)
	}
	if step := persisted.Status.Jobs["build"].Steps[1]; step.RunName != "" || step.Phase != v1alpha1.StepPending {
		t.Fatalf("persisted package step = %#v, want pending without runName", step)
	}

	// A replacement controller discovers the already-created Run by labels and
	// repairs status instead of creating a duplicate.
	failStatusPatch = false
	restartedController := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	reconcileWorkflowRun(t, restartedController, req, 1)
	assertChildRunCount(t, c, workflowRun.Namespace, 2)

	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &persisted); err != nil {
		t.Fatalf("get recovered workflowrun: %v", err)
	}
	packageStep := persisted.Status.Jobs["build"].Steps[1]
	if packageStep.RunName == "" || packageStep.Phase != v1alpha1.StepRunning {
		t.Fatalf("recovered package step = %#v, want running with existing runName", packageStep)
	}

	var packageRun v1alpha1.Run
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: workflowRun.Namespace, Name: packageStep.RunName}, &packageRun); err != nil {
		t.Fatalf("get recovered package run: %v", err)
	}
	packageRun.Status.Phase = v1alpha1.RunSucceeded
	if err := c.Status().Update(context.Background(), &packageRun); err != nil {
		t.Fatalf("complete package run: %v", err)
	}

	// Another replacement controller derives terminal step and job state from
	// the durable child Run without relying on process-local memory.
	terminalController := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	reconcileWorkflowRun(t, terminalController, req, 1)
	assertChildRunCount(t, c, workflowRun.Namespace, 2)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &persisted); err != nil {
		t.Fatalf("get terminal workflowrun: %v", err)
	}
	build := persisted.Status.Jobs["build"]
	if build.Phase != v1alpha1.JobSucceeded || build.Steps[1].Phase != v1alpha1.StepSucceeded {
		t.Fatalf("recovered build status = %#v, want succeeded", build)
	}
}

func TestDeriveTerminalWorkflowRunStatus(t *testing.T) {
	for _, test := range []struct {
		name            string
		cancelRequested bool
		jobs            map[string]v1alpha1.JobStatus
		want            v1alpha1.WorkflowPhase
	}{
		{
			name: "all jobs succeeded",
			jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobSucceeded},
				"test":  {Phase: v1alpha1.JobSucceeded},
			},
			want: v1alpha1.WorkflowSucceeded,
		},
		{
			name: "failed job with skipped dependent",
			jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobFailed},
				"test":  {Phase: v1alpha1.JobSkipped},
			},
			want: v1alpha1.WorkflowFailed,
		},
		{
			name: "succeeded and skipped jobs",
			jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobSucceeded},
				"docs":  {Phase: v1alpha1.JobSkipped},
			},
			want: v1alpha1.WorkflowSucceeded,
		},
		{
			name: "independent job still running",
			jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobFailed},
				"lint":  {Phase: v1alpha1.JobRunning},
			},
			want: v1alpha1.WorkflowRunning,
		},
		{
			name:            "cancellation owns terminal phase",
			cancelRequested: true,
			jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobFailed},
			},
			want: v1alpha1.WorkflowRunning,
		},
		{
			name: "uninitialized status",
			want: v1alpha1.WorkflowRunning,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflowRun := &v1alpha1.WorkflowRun{
				Spec: v1alpha1.WorkflowRunSpec{CancelRequested: test.cancelRequested},
				Status: v1alpha1.WorkflowRunStatus{
					Phase: v1alpha1.WorkflowRunning,
					Jobs:  test.jobs,
				},
			}
			deriveTerminalWorkflowRunStatus(workflowRun)
			if workflowRun.Status.Phase != test.want {
				t.Fatalf("phase = %q, want %q", workflowRun.Status.Phase, test.want)
			}
		})
	}
}

func TestWorkflowRunReconcilerRequestsCancellationWithoutStartingNewJobs(t *testing.T) {
	scheme := workflowRunTestScheme(t)
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cancel-build", Namespace: "default", UID: "workflowrun-uid"},
		Spec: v1alpha1.WorkflowRunSpec{
			CancelRequested: true,
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
				"lint":  {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "check", Run: "make lint"}}},
			},
		},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowRunning,
			Jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobRunning, Steps: []v1alpha1.StepStatus{{Name: "compile", Phase: v1alpha1.StepRunning, RunName: "build-run"}}},
				"lint":  {Phase: v1alpha1.JobPending, Steps: []v1alpha1.StepStatus{{Name: "check", Phase: v1alpha1.StepPending}}},
			},
		},
	}
	activeRun := workflowChildRun(workflowRun, "build", "compile", "build-run", v1alpha1.RunRunning)
	patches := 0
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun, activeRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, underlying client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*v1alpha1.Run); ok {
					patches++
				}
				return underlying.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}

	reconcileWorkflowRun(t, reconciler, req, 2)

	var updatedRun v1alpha1.Run
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(activeRun), &updatedRun); err != nil {
		t.Fatalf("get child run: %v", err)
	}
	if !updatedRun.Spec.CancelRequested {
		t.Fatal("child run cancelRequested = false, want true")
	}
	if patches != 1 {
		t.Fatalf("child run patches = %d, want one idempotent cancellation request", patches)
	}
	assertChildRunCount(t, c, workflowRun.Namespace, 1)

	var updatedWorkflowRun v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updatedWorkflowRun); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updatedWorkflowRun.Status.Phase != v1alpha1.WorkflowRunning || updatedWorkflowRun.Status.Jobs["lint"].Phase != v1alpha1.JobPending {
		t.Fatalf("status = %#v, want running cancellation with untouched pending job", updatedWorkflowRun.Status)
	}
}

func TestWorkflowRunReconcilerFinalizesCancellationAfterChildRunsSettle(t *testing.T) {
	scheme := workflowRunTestScheme(t)
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cancelled-build", Namespace: "default", UID: "workflowrun-uid"},
		Spec: v1alpha1.WorkflowRunSpec{
			CancelRequested: true,
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
				"lint":  {RunsOn: "bash", Needs: []string{"build"}, Steps: []v1alpha1.StepSpec{{Name: "check", Run: "make lint"}}},
			},
		},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowRunning,
			Jobs: map[string]v1alpha1.JobStatus{
				"build": {Phase: v1alpha1.JobRunning, Steps: []v1alpha1.StepStatus{{Name: "compile", Phase: v1alpha1.StepRunning, RunName: "build-run"}}},
				"lint":  {Phase: v1alpha1.JobWaiting, Pre: []string{"build"}, Steps: []v1alpha1.StepStatus{{Name: "check", Phase: v1alpha1.StepPending}}},
			},
		},
	}
	cancelledRun := workflowChildRun(workflowRun, "build", "compile", "build-run", v1alpha1.RunCancelled)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun, cancelledRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	reconcileWorkflowRun(t, reconciler, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}, 1)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowCancelled {
		t.Fatalf("phase = %q, want %q", updated.Status.Phase, v1alpha1.WorkflowCancelled)
	}
	if updated.Status.Jobs["build"].Phase != v1alpha1.JobFailed || updated.Status.Jobs["lint"].Phase != v1alpha1.JobWaiting {
		t.Fatalf("jobs = %#v, want failed active job and untouched waiting job", updated.Status.Jobs)
	}
	assertChildRunCount(t, c, workflowRun.Namespace, 1)
}

func TestWorkflowRunReconcilerCancelsBeforeCreatingAnyChildRun(t *testing.T) {
	scheme := workflowRunTestScheme(t)
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "cancel-pending", Namespace: "default", UID: "workflowrun-uid"},
		Spec: v1alpha1.WorkflowRunSpec{
			CancelRequested: true,
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}}},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	reconcileWorkflowRun(t, reconciler, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}, 1)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowCancelled || updated.Status.Jobs != nil {
		t.Fatalf("status = %#v, want cancellation before initialization", updated.Status)
	}
	assertChildRunCount(t, c, workflowRun.Namespace, 0)
}

func TestDeriveCancelledWorkflowRunStatusPreservesExistingTerminalPhase(t *testing.T) {
	for _, phase := range []v1alpha1.WorkflowPhase{
		v1alpha1.WorkflowSucceeded,
		v1alpha1.WorkflowFailed,
		v1alpha1.WorkflowCancelled,
	} {
		t.Run(string(phase), func(t *testing.T) {
			workflowRun := &v1alpha1.WorkflowRun{
				Spec:   v1alpha1.WorkflowRunSpec{CancelRequested: true},
				Status: v1alpha1.WorkflowRunStatus{Phase: phase},
			}
			deriveCancelledWorkflowRunStatus(&workflowRunResources{workflowRun: workflowRun})
			if workflowRun.Status.Phase != phase {
				t.Fatalf("phase = %q, want existing terminal phase %q", workflowRun.Status.Phase, phase)
			}
		})
	}
}

func TestNextStepToStartRequiresPrecedingSuccess(t *testing.T) {
	status := v1alpha1.JobStatus{Steps: []v1alpha1.StepStatus{
		{Name: "compile", Phase: v1alpha1.StepRunning, RunName: "compile-run"},
		{Name: "package", Phase: v1alpha1.StepPending},
	}}
	if _, ok := nextStepToStart(status); ok {
		t.Fatal("nextStepToStart() selected a step before its predecessor succeeded")
	}

	status.Steps[0].Phase = v1alpha1.StepSucceeded
	index, ok := nextStepToStart(status)
	if !ok || index != 1 {
		t.Fatalf("nextStepToStart() = %d, %t, want 1, true", index, ok)
	}
}

func TestWorkflowRunReconcilerPreservesResolvedJobStatus(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default", Generation: 4},
		Spec: v1alpha1.WorkflowRunSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}},
				},
			},
		},
		Status: v1alpha1.WorkflowRunStatus{
			Phase: v1alpha1.WorkflowPending,
			Jobs: map[string]v1alpha1.JobStatus{
				"test": {
					Phase: v1alpha1.JobRunning,
					Pre:   []string{"prepare"},
					Steps: []v1alpha1.StepStatus{
						{Name: "unit", Phase: v1alpha1.StepRunning, RunName: "existing-run"},
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	reconcileWorkflowRun(t, reconciler, req, 2)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	status := updated.Status.Jobs["test"]
	if status.Phase != v1alpha1.JobRunning {
		t.Fatalf("job phase = %q, want %q", status.Phase, v1alpha1.JobRunning)
	}
	if len(status.Pre) != 1 || status.Pre[0] != "prepare" {
		t.Fatalf("job pre = %v, want [prepare]", status.Pre)
	}
	if len(status.Steps) != 1 || status.Steps[0].RunName != "existing-run" {
		t.Fatalf("steps = %#v, want existing run preserved", status.Steps)
	}
}

func TestWorkflowRunReconcilerResolvesReusableWorkflow(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "build-and-test", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{
			Inputs: map[string]v1alpha1.WorkflowInputSpec{
				"ref":    {Required: true},
				"target": {Default: "linux-amd64"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}},
				},
				"test": {
					RunsOn: "bash",
					Needs:  []string{"build"},
					Steps:  []v1alpha1.StepSpec{{Name: "unit", Run: "make test"}},
				},
			},
		},
	}
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: "default", Generation: 5},
		Spec: v1alpha1.WorkflowRunSpec{
			Uses: "build-and-test",
			With: map[string]string{"ref": "main"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflow, workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	reconcileWorkflowRun(t, reconciler, req, 2)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowRunning {
		t.Fatalf("phase = %q, want %q", updated.Status.Phase, v1alpha1.WorkflowRunning)
	}
	if len(updated.Status.Jobs) != 2 {
		t.Fatalf("jobs = %#v, want 2 resolved jobs", updated.Status.Jobs)
	}
	if got := updated.Status.Jobs["build"]; got.Phase != v1alpha1.JobRunning || len(got.Pre) != 0 || len(got.Steps) != 1 || got.Steps[0].Name != "compile" || got.Steps[0].RunName == "" {
		t.Fatalf("build status = %#v, want running compile step", got)
	}
	if got := updated.Status.Jobs["test"]; got.Phase != v1alpha1.JobWaiting || len(got.Pre) != 1 || got.Pre[0] != "build" {
		t.Fatalf("test status = %#v, want waiting on build", got)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha1.WorkflowRunAcceptedCondition)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("condition = %#v, want accepted true", cond)
	}
	if updated.Status.SnapshotName == "" {
		t.Fatal("snapshotName is empty")
	}
	var snapshot appsv1.ControllerRevision
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: updated.Namespace, Name: updated.Status.SnapshotName}, &snapshot); err != nil {
		t.Fatalf("get workflow snapshot: %v", err)
	}
}

func TestWorkflowRunReconcilerExecutesTopLevelUsesFromSnapshot(t *testing.T) {
	scheme := workflowRunTestScheme(t)
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "build", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"compile": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "run", Run: "echo snapshot"}}},
		}},
	}
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: "default", UID: "release-uid"},
		Spec:       v1alpha1.WorkflowRunSpec{Uses: workflow.Name},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflow, workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workflowRun)}

	// Initialization captures the reusable definition but creates no Run yet.
	reconcileWorkflowRun(t, reconciler, req, 1)
	resources, err := reconciler.loadWorkflowRunResources(context.Background(), client.ObjectKeyFromObject(workflowRun))
	if err != nil {
		t.Fatalf("load workflowrun resources: %v", err)
	}
	if resources.snapshot == nil || resources.snapshot.Root.Spec.Uses != workflow.Name || resources.snapshot.Root.Workflow == nil || resources.snapshot.rootJobs()["compile"].Steps[0].Run != "echo snapshot" {
		t.Fatalf("loaded snapshot = %#v, want immutable root execution definition", resources.snapshot)
	}
	workflow.Spec.Jobs["compile"] = v1alpha1.JobSpec{RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "run", Run: "echo mutable"}}}
	if err := c.Update(context.Background(), workflow); err != nil {
		t.Fatalf("update reusable workflow: %v", err)
	}

	reconcileWorkflowRun(t, reconciler, req, 1)
	var runs v1alpha1.RunList
	if err := c.List(context.Background(), &runs, client.InNamespace(workflowRun.Namespace)); err != nil {
		t.Fatalf("list child runs: %v", err)
	}
	if len(runs.Items) != 1 || runs.Items[0].Spec.Source == nil || runs.Items[0].Spec.Source.Inline == nil || *runs.Items[0].Spec.Source.Inline != "echo snapshot" {
		t.Fatalf("child runs = %#v, want execution from immutable snapshot", runs.Items)
	}
}

func TestResolveWorkflowRunSnapshotResolvesNestedCalls(t *testing.T) {
	scheme := workflowRunTestScheme(t)
	smoke := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"check": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "run", Run: "check"}}},
		}},
	}
	deploy := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "deploy", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"verify": {Uses: smoke.Name},
		}},
	}
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: "default"},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"deploy": {Uses: deploy.Name},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(smoke, deploy).Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	snapshot, err := reconciler.resolveWorkflowRunSnapshot(context.Background(), workflowRun)
	if err != nil {
		t.Fatalf("resolve workflow snapshot: %v", err)
	}
	if _, ok := snapshot.Workflows["root/jobs/deploy"]; !ok {
		t.Fatalf("snapshot workflows = %#v, want deploy call", snapshot.Workflows)
	}
	if _, ok := snapshot.Workflows["root/jobs/deploy/jobs/verify"]; !ok {
		t.Fatalf("snapshot workflows = %#v, want nested smoke call", snapshot.Workflows)
	}
}

func TestResolveWorkflowRunSnapshotRejectsReuseCycle(t *testing.T) {
	scheme := workflowRunTestScheme(t)
	workflowA := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "workflow-a", Namespace: "default"},
		Spec:       v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{"call-b": {Uses: "workflow-b"}}},
	}
	workflowB := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "workflow-b", Namespace: "default"},
		Spec:       v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{"call-a": {Uses: "workflow-a"}}},
	}
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: "default"},
		Spec:       v1alpha1.WorkflowRunSpec{Uses: workflowA.Name},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workflowA, workflowB).Build()
	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	_, err := reconciler.resolveWorkflowRunSnapshot(context.Background(), workflowRun)
	if err == nil || !strings.Contains(err.Error(), "workflow reuse cycle") {
		t.Fatalf("resolve workflow snapshot error = %v, want reuse cycle", err)
	}
}

func TestWorkflowRunReconcilerFailsWhenReusableWorkflowMissing(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: "default", Generation: 6},
		Spec: v1alpha1.WorkflowRunSpec{
			Uses: "missing-workflow",
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	reconcileWorkflowRun(t, reconciler, req, 2)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowFailed {
		t.Fatalf("phase = %q, want %q", updated.Status.Phase, v1alpha1.WorkflowFailed)
	}
	if updated.Status.Jobs != nil {
		t.Fatalf("jobs = %#v, want nil on failed resolution", updated.Status.Jobs)
	}
	if !strings.Contains(updated.Status.Message, "missing-workflow") {
		t.Fatalf("message = %q, want missing workflow name", updated.Status.Message)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha1.WorkflowRunAcceptedCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WorkflowResolutionFailed" {
		t.Fatalf("condition = %#v, want resolution failure", cond)
	}
}

func TestWorkflowRunReconcilerFailsWhenWorkflowInputUnknown(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "build-and-test", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{
			Inputs: map[string]v1alpha1.WorkflowInputSpec{
				"ref": {Required: true},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}},
				},
			},
		},
	}
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: "default", Generation: 7},
		Spec: v1alpha1.WorkflowRunSpec{
			Uses: "build-and-test",
			With: map[string]string{
				"ref":     "main",
				"unknown": "value",
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflow, workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	reconcileWorkflowRun(t, reconciler, req, 2)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowFailed {
		t.Fatalf("phase = %q, want %q", updated.Status.Phase, v1alpha1.WorkflowFailed)
	}
	if !strings.Contains(updated.Status.Message, `unknown input "unknown"`) {
		t.Fatalf("message = %q, want unknown input", updated.Status.Message)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha1.WorkflowRunAcceptedCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WorkflowInputBindingFailed" {
		t.Fatalf("condition = %#v, want input binding failure", cond)
	}
}

func TestWorkflowRunReconcilerFailsWhenWorkflowInputRequired(t *testing.T) {
	scheme := workflowRunTestScheme(t)

	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "build-and-test", Namespace: "default"},
		Spec: v1alpha1.WorkflowSpec{
			Inputs: map[string]v1alpha1.WorkflowInputSpec{
				"ref": {Required: true},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps:  []v1alpha1.StepSpec{{Name: "compile", Run: "make build"}},
				},
			},
		},
	}
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "release", Namespace: "default", Generation: 8},
		Spec: v1alpha1.WorkflowRunSpec{
			Uses: "build-and-test",
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workflow, workflowRun).
		WithStatusSubresource(&v1alpha1.WorkflowRun{}).
		Build()

	reconciler := &WorkflowRunReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: workflowRun.Namespace,
		Name:      workflowRun.Name,
	}}
	reconcileWorkflowRun(t, reconciler, req, 2)

	var updated v1alpha1.WorkflowRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), &updated); err != nil {
		t.Fatalf("get workflowrun: %v", err)
	}
	if updated.Status.Phase != v1alpha1.WorkflowFailed {
		t.Fatalf("phase = %q, want %q", updated.Status.Phase, v1alpha1.WorkflowFailed)
	}
	if !strings.Contains(updated.Status.Message, `missing required input "ref"`) {
		t.Fatalf("message = %q, want missing required input", updated.Status.Message)
	}
	cond := apimeta.FindStatusCondition(updated.Status.Conditions, v1alpha1.WorkflowRunAcceptedCondition)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "WorkflowInputBindingFailed" {
		t.Fatalf("condition = %#v, want input binding failure", cond)
	}
}

func TestBindWorkflowInputsAppliesDefaults(t *testing.T) {
	bound, err := bindWorkflowInputs(
		map[string]v1alpha1.WorkflowInputSpec{
			"ref":    {Required: true},
			"target": {Default: "linux-amd64"},
		},
		map[string]string{"ref": "main"},
	)
	if err != nil {
		t.Fatalf("bindWorkflowInputs() error = %v", err)
	}
	if bound["ref"] != "main" || bound["target"] != "linux-amd64" {
		t.Fatalf("bound inputs = %#v, want ref and default target", bound)
	}
}

func ptrTo(value string) *string {
	return &value
}

func reconcileWorkflowRun(t *testing.T, reconciler *WorkflowRunReconciler, req ctrl.Request, times int) {
	t.Helper()
	for range times {
		if _, err := reconciler.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile workflowrun: %v", err)
		}
	}
}

func assertChildRunCount(t *testing.T, c client.Client, namespace string, want int) {
	t.Helper()
	var runs v1alpha1.RunList
	if err := c.List(context.Background(), &runs, client.InNamespace(namespace)); err != nil {
		t.Fatalf("list child runs: %v", err)
	}
	if len(runs.Items) != want {
		t.Fatalf("child runs = %#v, want %d", runs.Items, want)
	}
}

func workflowChildRun(workflowRun *v1alpha1.WorkflowRun, jobName string, stepName string, runName string, phase v1alpha1.RunPhase) *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: workflowRun.Namespace,
			Labels:    workflowStepLabels(workflowRun, jobName, stepName),
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Source:  &v1alpha1.CodeSource{Inline: ptrTo("make build")},
			Mode:    v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}},
		},
		Status: v1alpha1.RunStatus{Phase: phase},
	}
}
