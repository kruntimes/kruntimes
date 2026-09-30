package e2e

// Workflow scenarios: templates, actions, reusable workflows, cancellation, and environment sharing.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/krt"
)

func TestWorkflowTriggerMaterializesAndExecutesTemplate(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-template-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Inputs: map[string]v1alpha1.WorkflowInputSpec{
				"message": {Required: true},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "render",
						Run:  "test \"$MESSAGE\" = \"${{ inputs.message }}\"",
						Env:  map[string]string{"MESSAGE": "${{ inputs.message }}"},
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRunName := "e2e-trigger-" + nameSuffix
	cmd := krt.NewRootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"wf", "trigger", workflow.Name, "--name", workflowRunName, "--set", "message=rendered-by-e2e", "--namespace", testNamespace})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("trigger workflow: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), workflowRunName) {
		t.Fatalf("trigger output = %q, want workflowrun name %q", stdout.String(), workflowRunName)
	}

	workflowRun := &v1alpha1.WorkflowRun{ObjectMeta: metav1.ObjectMeta{Name: workflowRunName, Namespace: testNamespace}}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
		t.Fatalf("get materialized workflowrun: %v", err)
	}
	step := workflowRun.Spec.Jobs["build"].Steps[0]
	if step.Run != "test \"$MESSAGE\" = \"rendered-by-e2e\"" || step.Env["MESSAGE"] != "rendered-by-e2e" {
		t.Fatalf("materialized step = %#v, want rendered inputs", step)
	}

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowSucceeded)
	if workflowRun.Status.Jobs["build"].Phase != v1alpha1.JobSucceeded {
		t.Fatalf("build job status = %#v, want Succeeded", workflowRun.Status.Jobs["build"])
	}
}

func TestWorkflowRunExecutesActionAndProjectsOutputs(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.ActionSpec{
			Inputs: map[string]v1alpha1.ActionInputSpec{
				"message": {Required: true},
			},
			Outputs: map[string]v1alpha1.ActionOutputSpec{
				"endpoint": {Value: "${{ steps.emit.outputs.endpoint }}"},
			},
			Steps: []v1alpha1.StepSpec{
				{
					Name: "validate",
					Run:  `test "$MESSAGE" = "action-input"`,
					Env:  map[string]string{"MESSAGE": "${{ inputs.message }}"},
				},
				{
					Name: "emit",
					Run:  `printf 'endpoint=https://action.e2e.example.com\n' > "$KRUNTIME_OUTPUTS"`,
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), action); err != nil {
		t.Fatalf("create Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), action) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: "bash",
				Steps: []v1alpha1.StepSpec{
					{Name: "setup", Uses: action.Name, With: map[string]string{"message": "action-input"}},
					{
						Name: "consume",
						Run:  `test "$ENDPOINT" = "https://action.e2e.example.com"`,
						Env:  map[string]string{"ENDPOINT": "${{ steps.setup.outputs.endpoint }}"},
					},
				},
			},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with Action call: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	setup := workflowRun.Status.Jobs["build"].Steps[0]
	if setup.Phase != v1alpha1.StepSucceeded || setup.Outputs["endpoint"] != "https://action.e2e.example.com" {
		t.Fatalf("Action caller step = %#v, want projected endpoint", setup)
	}
	if len(setup.ActionSteps) != 2 || setup.ActionSteps[0].Phase != v1alpha1.StepSucceeded || setup.ActionSteps[1].Phase != v1alpha1.StepSucceeded {
		t.Fatalf("Action internal steps = %#v, want both succeeded", setup.ActionSteps)
	}
	if consume := workflowRun.Status.Jobs["build"].Steps[1]; consume.Phase != v1alpha1.StepSucceeded {
		t.Fatalf("consumer step = %#v, want succeeded after Action output projection", consume)
	}
}

func TestWorkflowRunSharesToolCacheAndEnvironmentBetweenSteps(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-tool-cache-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: "bash",
				Steps: []v1alpha1.StepSpec{
					{
						Name: "install-tool",
						Run:  "\"$KRUNTIME_TOOL_CACHE/bin/kruntime-cache\" ensure e2e/tool -- sh -ceu 'printf cache-hit > \"$KRUNTIME_CACHE_STAGING/tool\"'\nprintf 'kruntimes.io/env/E2E_TOOL=%s\\n' \"$KRUNTIME_TOOL_CACHE/e2e/tool/tool\" >> \"$KRUNTIME_OUTPUTS\"",
					},
					{
						Name: "use-tool",
						Run:  `test "$(cat "$E2E_TOOL")" = cache-hit`,
					},
				},
			},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with tool cache: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	steps := workflowRun.Status.Jobs["build"].Steps
	if len(steps) != 2 || steps[0].Phase != v1alpha1.StepSucceeded || steps[1].Phase != v1alpha1.StepSucceeded {
		t.Fatalf("tool-cache workflow steps = %#v, want two succeeded steps", steps)
	}
	if _, found := steps[0].Outputs[v1alpha1.WorkflowEnvironmentOutputPrefix+"E2E_TOOL"]; !found {
		t.Fatalf("install-tool outputs = %#v, want reserved E2E_TOOL output", steps[0].Outputs)
	}
}

func TestWorkflowRunFailsWhenActionChildRunFails(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-fail-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{{Name: "fail", Run: "exit 17"}},
		},
	}
	if err := k8sClient.Create(context.Background(), action); err != nil {
		t.Fatalf("create failing Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), action) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-fail-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: action.Name}}},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with failing Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowFailed)
	step := workflowRun.Status.Jobs["build"].Steps[0]
	if step.Phase != v1alpha1.StepFailed || len(step.ActionSteps) != 1 || step.ActionSteps[0].Phase != v1alpha1.StepFailed {
		t.Fatalf("failing Action status = %#v, want failed caller and child step", step)
	}
}

func TestWorkflowRunCancellationPropagatesToActionChildRun(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-cancel-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{{Name: "wait", Run: "sleep 30"}},
		},
	}
	if err := k8sClient.Create(context.Background(), action); err != nil {
		t.Fatalf("create cancellable Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), action) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-cancel-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: action.Name}}},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with cancellable Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	actionRunName := waitForActionChildRun(t, workflowRun, "build", "wait", 20*time.Second)
	workflowRun.Spec.CancelRequested = true
	if err := k8sClient.Update(context.Background(), workflowRun); err != nil {
		t.Fatalf("request WorkflowRun cancellation: %v", err)
	}

	actionRun := &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: actionRunName, Namespace: testNamespace}}
	waitForRunPhase(t, actionRun, 30*time.Second, v1alpha1.RunCancelled)
	if !actionRun.Spec.HasImmediateTermination() {
		t.Fatalf("Action child Run %s does not request immediate termination", actionRun.Name)
	}
	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowCancelled)
}

func TestWorkflowRunRejectsMissingActionWithoutChildRun(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-missing-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: "does-not-exist-" + nameSuffix}}},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with missing Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowFailed)
	if !strings.Contains(workflowRun.Status.Message, "does not exist") {
		t.Fatalf("missing Action message = %q, want missing definition", workflowRun.Status.Message)
	}
	var runs v1alpha1.RunList
	if err := k8sClient.List(context.Background(), &runs,
		client.InNamespace(testNamespace),
		client.MatchingLabels{v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID)}); err != nil {
		t.Fatalf("list child Runs for rejected WorkflowRun: %v", err)
	}
	if len(runs.Items) != 0 {
		t.Fatalf("child Runs = %#v, want none for missing Action", runs.Items)
	}
}

func TestWorkflowRunRecoversActionAfterControllerRestart(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	action := &v1alpha1.Action{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-recovery-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.ActionSpec{
			Steps: []v1alpha1.StepSpec{
				{Name: "first", Run: "sleep 5"},
				{Name: "second", Run: "true"},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), action); err != nil {
		t.Fatalf("create recovery Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), action) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-action-recovery-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "setup", Uses: action.Name}}},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun with recovery Action: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	firstRunName := waitForActionChildRun(t, workflowRun, "build", "first", 20*time.Second)
	restartController(t)
	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	setup := workflowRun.Status.Jobs["build"].Steps[0]
	if len(setup.ActionSteps) != 2 || setup.ActionSteps[0].RunName != firstRunName || setup.ActionSteps[0].Phase != v1alpha1.StepSucceeded || setup.ActionSteps[1].Phase != v1alpha1.StepSucceeded {
		t.Fatalf("recovered Action status = %#v, want both persisted Action steps succeeded", setup)
	}
}

func TestWorkflowRunExecutesReusableWorkflowAndProjectsOutputs(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-deploy-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"endpoint": {Value: "${{ jobs.apply.outputs.endpoint }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"apply": {
					RunsOn: "bash",
					Outputs: map[string]string{
						"endpoint": "${{ steps.deploy.outputs.endpoint }}",
					},
					Steps: []v1alpha1.StepSpec{{
						Name: "deploy",
						Run:  `printf 'endpoint=https://e2e.example.com\n' > "$KRUNTIME_OUTPUTS"`,
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-reuse-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"deploy": {Uses: workflow.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	deploy := workflowRun.Status.Jobs["deploy"]
	if deploy.WorkflowRunName == "" || deploy.Phase != v1alpha1.JobSucceeded || deploy.Outputs["endpoint"] != "https://e2e.example.com" {
		t.Fatalf("deploy job status = %#v, want succeeded reusable call with projected endpoint", deploy)
	}
}

func TestWorkflowRunExecutesNestedReusableWorkflows(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	leaf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-leaf-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"result": {Value: "${{ jobs.test.outputs.result }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Outputs: map[string]string{
						"result": "${{ steps.emit.outputs.result }}",
					},
					Steps: []v1alpha1.StepSpec{{
						Name: "emit",
						Run:  `printf 'result=nested-ok\n' > "$KRUNTIME_OUTPUTS"`,
					}},
				},
			},
		},
	}
	middle := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-middle-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"result": {Value: "${{ jobs.call-leaf.outputs.result }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"call-leaf": {Uses: leaf.Name},
			},
		},
	}
	for _, workflow := range []*v1alpha1.Workflow{leaf, middle} {
		if err := k8sClient.Create(context.Background(), workflow); err != nil {
			t.Fatalf("create reusable workflow %s: %v", workflow.Name, err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })
	}

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-nested-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call-middle": {Uses: middle.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	call := workflowRun.Status.Jobs["call-middle"]
	if call.WorkflowRunName == "" || call.Phase != v1alpha1.JobSucceeded || call.Outputs["result"] != "nested-ok" {
		t.Fatalf("call-middle job status = %#v, want succeeded nested reusable call with projected result", call)
	}
}

func TestWorkflowRunCancellationPropagatesToReusableWorkflow(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cancel-reuse-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"apply": {
				RunsOn: "bash",
				Steps:  []v1alpha1.StepSpec{{Name: "wait", Run: "sleep 300"}},
			},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cancel-reuse-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"deploy": {Uses: workflow.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			t.Fatalf("get workflowrun: %v", err)
		}
		if workflowRun.Status.Jobs["deploy"].WorkflowRunName != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for reusable call to start: %#v", workflowRun.Status)
		default:
		}
	}
	workflowRun.Spec.CancelRequested = true
	if err := k8sClient.Update(context.Background(), workflowRun); err != nil {
		t.Fatalf("request workflowrun cancellation: %v", err)
	}

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowCancelled)
}

func TestWorkflowRunRejectsReusableWorkflowCycle(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflowA := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cycle-a-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call-b": {Uses: "e2e-cycle-b-" + nameSuffix},
		}},
	}
	workflowB := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cycle-b-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call-a": {Uses: workflowA.Name},
		}},
	}
	for _, workflow := range []*v1alpha1.Workflow{workflowA, workflowB} {
		if err := k8sClient.Create(context.Background(), workflow); err != nil {
			t.Fatalf("create reusable workflow %s: %v", workflow.Name, err)
		}
		t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })
	}

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-cycle-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call-a": {Uses: workflowA.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 20*time.Second, v1alpha1.WorkflowFailed)
	status := workflowRun.Status.Jobs["call-a"]
	if status.Phase != v1alpha1.JobFailed || status.WorkflowRunName != "" {
		t.Fatalf("call-a status = %#v, want failed call without child workflowrun", status)
	}
	if !strings.Contains(workflowRun.Status.Message, "workflow call cycle:") {
		t.Fatalf("message = %q, want reusable workflow cycle", workflowRun.Status.Message)
	}
}

func TestWorkflowRunFreezesReusableTemplateAfterChildCreation(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-freeze-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"result": {Value: "${{ jobs.apply.outputs.result }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"apply": {
					RunsOn: "bash",
					Outputs: map[string]string{
						"result": "${{ steps.emit.outputs.result }}",
					},
					Steps: []v1alpha1.StepSpec{{
						Name: "emit",
						Run:  `sleep 2; printf 'result=original\n' > "$KRUNTIME_OUTPUTS"`,
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-freeze-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"call": {Uses: workflow.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		time.Sleep(200 * time.Millisecond)
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflowRun), workflowRun); err != nil {
			t.Fatalf("get workflowrun: %v", err)
		}
		if workflowRun.Status.Jobs["call"].WorkflowRunName != "" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for reusable child creation: %#v", workflowRun.Status)
		default:
		}
	}
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflow), workflow); err != nil {
		t.Fatalf("get reusable workflow: %v", err)
	}
	workflow.Spec.Outputs["result"] = v1alpha1.WorkflowOutputSpec{Value: "changed"}
	if err := k8sClient.Update(context.Background(), workflow); err != nil {
		t.Fatalf("update reusable workflow after child creation: %v", err)
	}

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowSucceeded)
	if got := workflowRun.Status.Jobs["call"].Outputs["result"]; got != "original" {
		t.Fatalf("call output = %q, want original frozen output contract", got)
	}
}

func TestWorkflowRunBindsReusableTemplateWhenCallBecomesReady(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workflow := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-late-bind-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowSpec{
			Outputs: map[string]v1alpha1.WorkflowOutputSpec{
				"result": {Value: "${{ jobs.apply.outputs.result }}"},
			},
			Jobs: map[string]v1alpha1.JobSpec{
				"apply": reusableOutputJob(`printf 'result=original\n' > "$KRUNTIME_OUTPUTS"`),
			},
		},
	}
	if err := k8sClient.Create(context.Background(), workflow); err != nil {
		t.Fatalf("create reusable workflow: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflow) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-late-bind-run-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"gate": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "wait", Run: "sleep 2"}}},
			"call": {Needs: []string{"gate"}, Uses: workflow.Name},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create workflowrun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current v1alpha1.Workflow
		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(workflow), &current); err != nil {
			return err
		}
		current.Spec.Jobs["apply"] = reusableOutputJob(`printf 'result=updated\n' > "$KRUNTIME_OUTPUTS"`)
		return k8sClient.Update(context.Background(), &current)
	}); err != nil {
		t.Fatalf("update reusable workflow before call becomes ready: %v", err)
	}

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowSucceeded)
	if got := workflowRun.Status.Jobs["call"].Outputs["result"]; got != "updated" {
		t.Fatalf("call output = %q, want updated late-bound template output", got)
	}
}

func reusableOutputJob(run string) v1alpha1.JobSpec {
	return v1alpha1.JobSpec{
		RunsOn: "bash",
		Outputs: map[string]string{
			"result": "${{ steps.emit.outputs.result }}",
		},
		Steps: []v1alpha1.StepSpec{{Name: "emit", Run: run}},
	}
}

func TestWorkflowRunSharesJobLocalWorkspace(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("e2e-workspace-%d", time.Now().UnixNano()),
			Namespace: testNamespace,
		},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: "bash",
				Steps: []v1alpha1.StepSpec{
					{Name: "write", Run: "echo workflow-data > shared.txt"},
					{Name: "read", Run: `test "$(cat shared.txt)" = workflow-data`},
				},
			},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create WorkflowRun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 30*time.Second, v1alpha1.WorkflowSucceeded)
	var workspaces v1alpha1.PersistentWorkspaceList
	if err := k8sClient.List(context.Background(), &workspaces, client.InNamespace(testNamespace), client.MatchingLabels{
		v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID),
		v1alpha1.WorkflowJobLabel:    "build",
	}); err != nil {
		t.Fatalf("list job workspaces: %v", err)
	}
	if len(workspaces.Items) != 1 {
		t.Fatalf("job workspaces = %#v, want one", workspaces.Items)
	}
	workspace := &workspaces.Items[0]
	if workspace.Spec.Runtime != "bash" || !metav1.IsControlledBy(workspace, workflowRun) {
		t.Fatalf("workspace = %#v, want WorkflowRun-owned bash workspace", workspace)
	}

	var runs v1alpha1.RunList
	if err := k8sClient.List(context.Background(), &runs, client.InNamespace(testNamespace), client.MatchingLabels{
		v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID),
		v1alpha1.WorkflowJobLabel:    "build",
	}); err != nil {
		t.Fatalf("list child Runs: %v", err)
	}
	if len(runs.Items) != 2 {
		t.Fatalf("child Runs = %#v, want write and read", runs.Items)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if run.Spec.Workspace == nil || run.Spec.Workspace.Name != workspace.Name {
			t.Fatalf("Run %s workspace = %#v, want %q", run.Name, run.Spec.Workspace, workspace.Name)
		}
	}
}

func TestWorkflowRunTransfersArtifactsBetweenJobs(t *testing.T) {
	runtimeName := fmt.Sprintf("workflow-artifacts-%d", time.Now().UnixNano())
	claimName := runtimeName + "-artifacts"
	ensureFilesystemRuntime(t, runtimeName, claimName)

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "e2e-workflow-artifacts-" + fmt.Sprint(time.Now().UnixNano()), Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {
				RunsOn: runtimeName,
				Steps: []v1alpha1.StepSpec{{
					Name: "package",
					Run:  `mkdir -p "$KRUNTIME_ARTIFACTS_DIR"; printf workflow-artifact > "$KRUNTIME_ARTIFACTS_DIR/dist.txt"`,
				}},
			},
			"verify": {
				RunsOn: runtimeName,
				Needs:  []string{"build"},
				Steps: []v1alpha1.StepSpec{{
					Name: "verify",
					Artifacts: []v1alpha1.WorkflowArtifactInput{{
						From: "jobs.build.artifacts.dist.txt",
						Path: "dist.txt",
					}},
					Run: `test "$(cat dist.txt)" = workflow-artifact`,
				}},
			},
		}},
	}
	if err := k8sClient.Create(context.Background(), workflowRun); err != nil {
		t.Fatalf("create artifact WorkflowRun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workflowRun) })

	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)
	artifact, found := workflowRun.Status.Jobs["build"].Artifacts["dist.txt"]
	if !found || artifact.Location.Filesystem == nil || artifact.Location.Filesystem.Path == "" {
		t.Fatalf("build artifacts = %#v, want dist.txt filesystem reference", workflowRun.Status.Jobs["build"].Artifacts)
	}

	missingArtifact := workflowRun.DeepCopy()
	missingArtifact.ResourceVersion = ""
	missingArtifact.UID = ""
	missingArtifact.Name = workflowRun.Name + "-missing"
	missingArtifact.CreationTimestamp = metav1.Time{}
	missingArtifact.Status = v1alpha1.WorkflowRunStatus{}
	missingArtifact.Spec.Jobs["verify"] = v1alpha1.JobSpec{
		RunsOn: runtimeName,
		Needs:  []string{"build"},
		Steps: []v1alpha1.StepSpec{{
			Name: "verify",
			Artifacts: []v1alpha1.WorkflowArtifactInput{{
				From: "jobs.build.artifacts.missing.txt",
				Path: "missing.txt",
			}},
			Run: "exit 0",
		}},
	}
	if err := k8sClient.Create(context.Background(), missingArtifact); err != nil {
		t.Fatalf("create missing-artifact WorkflowRun: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), missingArtifact) })
	waitForWorkflowRunPhase(t, missingArtifact, 45*time.Second, v1alpha1.WorkflowFailed)
	if status := missingArtifact.Status.Jobs["verify"]; status.Phase != v1alpha1.JobFailed {
		t.Fatalf("missing artifact verify job = %#v, want Failed", status)
	}
}
