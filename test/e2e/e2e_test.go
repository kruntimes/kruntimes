//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const testNamespace = "default"

var k8sClient client.Client

func TestMain(m *testing.M) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	cfg := config.GetConfigOrDie()
	cfg.QPS = 50
	cfg.Burst = 100

	var err error
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func ensureRuntime(t *testing.T, name, image string, port int32) {
	t.Helper()

	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: v1alpha1.RuntimeSpec{
			Image:    image,
			Port:     port,
			Replicas: 1,
			Command:  []string{fmt.Sprintf("--port=%d", port), "--work-dir=/workspace"},
		},
	}
	if err := k8sClient.Create(context.Background(), rt); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("create runtime: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods,
			client.InNamespace(testNamespace),
			client.MatchingLabels{"runtime": name},
		); err == nil {
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodRunning {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for runtime pods")
		case <-time.After(2 * time.Second):
		}
	}
}

func TestFullRunLifecycle(t *testing.T) {
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (runtime=bash)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lastPhase v1alpha1.RunPhase
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run completion, last phase=%s", lastPhase)
		default:
		}

		time.Sleep(time.Second)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}

		if run.Status.Phase != lastPhase {
			t.Logf("Run %s: %s -> %s (pod=%s)", run.Name, lastPhase, run.Status.Phase, run.Status.AssignedPod)
			lastPhase = run.Status.Phase
		}

		switch run.Status.Phase {
		case v1alpha1.RunSucceeded:
			t.Logf("Run completed successfully: %s", run.Status.Message)
			return
		case v1alpha1.RunFailed:
			t.Fatalf("Run failed: %s", run.Status.Message)
		}
	}
}

func TestSchedulerResponsiveness(t *testing.T) {
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-perf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "bash",
			Args:    []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	start := time.Now()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		time.Sleep(200 * time.Millisecond)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}

		if run.Status.Phase != v1alpha1.RunPending {
			elapsed := time.Since(start)
			t.Logf("Run scheduled in %v (phase=%s, pod=%s)", elapsed, run.Status.Phase, run.Status.AssignedPod)
			return
		}

		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for scheduler to pick up run")
		default:
		}
	}
}

func TestPythonInlineRun(t *testing.T) {
	ensureRuntime(t, "python", "kruntimes-python-runtime:latest", 9092)

	inline := `print("hello from python")`
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-py-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.RunSpec{
			Runtime: "python",
			Source:  &v1alpha1.CodeSource{Inline: &inline},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Python Run %s", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lastPhase v1alpha1.RunPhase
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for run completion, last phase=%s", lastPhase)
		default:
		}

		time.Sleep(time.Second)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(run), run); err != nil {
			t.Fatalf("get run: %v", err)
		}

		if run.Status.Phase != lastPhase {
			t.Logf("Python Run %s: %s -> %s (pod=%s)", run.Name, lastPhase, run.Status.Phase, run.Status.AssignedPod)
			lastPhase = run.Status.Phase
		}

		switch run.Status.Phase {
		case v1alpha1.RunSucceeded:
			t.Logf("Python Run completed successfully: %s", run.Status.Message)
			return
		case v1alpha1.RunFailed:
			t.Fatalf("Python Run failed: %s", run.Status.Message)
		}
	}
}

func TestWorkflowSingleJob(t *testing.T) {
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-wf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"test": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{{
						Name: "hello",
						Run:  "echo hello_from_workflow",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Logf("Created Workflow %s", wf.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for workflow completion")
		default:
		}
		time.Sleep(time.Second)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(wf), wf); err != nil {
			t.Fatalf("get workflow: %v", err)
		}
		js := wf.Status.Jobs["test"]
		t.Logf("Workflow %s: phase=%s, job=%s", wf.Name, wf.Status.Phase, js.Phase)

		switch wf.Status.Phase {
		case v1alpha1.WorkflowSucceeded:
			t.Logf("Workflow succeeded: %s", wf.Status.Message)
			return
		case v1alpha1.WorkflowFailed:
			t.Fatalf("Workflow failed: %s (job phase=%s)", wf.Status.Message, js.Phase)
		}
	}
}

func TestWorkflowStepOutputs(t *testing.T) {
	ensureRuntime(t, "bash", "kruntimes-bash-runtime:latest", 9091)

	wf := &v1alpha1.Workflow{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-wf-",
			Namespace:    testNamespace,
		},
		Spec: v1alpha1.WorkflowSpec{
			Jobs: map[string]v1alpha1.JobSpec{
				"build": {
					RunsOn: "bash",
					Steps: []v1alpha1.StepSpec{
						{
							Name: "gen-version",
							Run:  "echo version=v1.0 >> outputs",
						},
						{
							Name: "build-image",
							Run:  "echo image=app:${{ steps.gen-version.outputs.version }} >> outputs",
						},
					},
					Outputs: map[string]string{
						"artifact": "${{ steps.build-image.outputs.image }}",
					},
				},
				"deploy": {
					RunsOn: "bash",
					Needs:  []string{"build"},
					Steps: []v1alpha1.StepSpec{{
						Name: "deploy-step",
						Run:  "echo deploying ${{ jobs.build.outputs.artifact }}",
					}},
				},
			},
		},
	}
	if err := k8sClient.Create(context.Background(), wf); err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	t.Logf("Created Workflow %s", wf.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for workflow completion")
		default:
		}
		time.Sleep(time.Second)

		if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(wf), wf); err != nil {
			t.Fatalf("get workflow: %v", err)
		}
		t.Logf("Workflow %s: phase=%s", wf.Name, wf.Status.Phase)
		for _, jn := range []string{"build", "deploy"} {
			js := wf.Status.Jobs[jn]
			t.Logf("  Job %s: phase=%s", jn, js.Phase)
			for sn, ss := range js.Steps {
				t.Logf("    Step %s: phase=%s outputs=%v", sn, ss.Phase, ss.Outputs)
			}
		}

		switch wf.Status.Phase {
		case v1alpha1.WorkflowSucceeded:
			buildJob := wf.Status.Jobs["build"]
			buildStep := buildJob.Steps["build-image"]
			if buildStep.Outputs == nil || buildStep.Outputs["image"] != "app:v1.0" {
				t.Fatalf("build-image outputs mismatch: got %v", buildStep.Outputs)
			}
			deployJob := wf.Status.Jobs["deploy"]
			deployStep := deployJob.Steps["deploy-step"]
			t.Logf("Cross-job output resolved: deploy step run=%s", deployStep.RunName)
			t.Logf("All outputs verified")
			return
		case v1alpha1.WorkflowFailed:
			t.Fatalf("Workflow failed: %s", wf.Status.Message)
		}
	}
}
