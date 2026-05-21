//go:build e2e
// +build e2e

package e2e

import (
	"context"
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

	"github.com/airconduct/kruntime/api/v1alpha1"
)

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

	// Ensure Runtime CR exists so controller creates runtime pods.
	ensureRuntime(context.Background())

	os.Exit(m.Run())
}

func ensureRuntime(ctx context.Context) {
	rt := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golang-1.26",
			Namespace: "default",
		},
		Spec: v1alpha1.RuntimeSpec{
			Image:    "kruntime-bash-runtime:latest",
			Port:     9091,
			Replicas: 2,
			Command:  []string{"--port=9091", "--work-dir=/workspace"},
		},
	}
	if err := k8sClient.Create(ctx, rt); err != nil && !apierrors.IsAlreadyExists(err) {
		panic("create runtime: " + err.Error())
	}

	// Wait for at least one runtime pod to be ready.
	for i := 0; i < 60; i++ {
		var pods corev1.PodList
		if err := k8sClient.List(ctx, &pods,
			client.MatchingLabels{"runtime": "golang-1.26"},
		); err == nil {
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodRunning {
					return
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	panic("timed out waiting for runtime pods")
}

func initNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{}
	ns.GenerateName = "e2e-"
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	t.Cleanup(func() {
		k8sClient.Delete(context.Background(), ns)
	})
	return ns.Name
}

func TestFullRunLifecycle(t *testing.T) {
	ns := initNamespace(t)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-",
			Namespace:    ns,
		},
		Spec: v1alpha1.RunSpec{
			Runtime:  "golang-1.26",
			Commands: []string{"echo hello"},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Logf("Created Run %s (runtime=golang-1.26)", run.Name)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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
			t.Fatalf("Task failed: %s", run.Status.Message)
		}
	}
}

func TestSchedulerResponsiveness(t *testing.T) {
	ns := initNamespace(t)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "e2e-perf-",
			Namespace:    ns,
		},
		Spec: v1alpha1.RunSpec{
			Runtime:  "golang-1.26",
			Commands: []string{"echo hello"},
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

			if run.Status.AssignedPod == "" && run.Status.Phase != v1alpha1.RunFailed {
				t.Error("expected assignedPod to be set after scheduling")
			}
			return
		}

		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for scheduler to pick up run")
		default:
		}
	}
}
