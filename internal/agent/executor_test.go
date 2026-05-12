package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/airconduct/kruntime/api/v1alpha1"
)

func TestExecutor_Success(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{WorkspaceBase: dir}

	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{UID: "test-uid"},
		Spec: v1alpha1.TaskSpec{
			Commands: []string{"echo hello"},
		},
	}

	result := e.Execute(context.Background(), task)
	if result.Phase != v1alpha1.TaskSucceeded {
		t.Errorf("expected Succeeded, got %s: %s", result.Phase, result.Message)
	}
}

func TestExecutor_Failure(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{WorkspaceBase: dir}

	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{UID: "test-uid"},
		Spec: v1alpha1.TaskSpec{
			Commands: []string{"exit 1"},
		},
	}

	result := e.Execute(context.Background(), task)
	if result.Phase != v1alpha1.TaskFailed {
		t.Errorf("expected Failed, got %s", result.Phase)
	}
}

func TestExecutor_EnvironmentVariables(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{WorkspaceBase: dir}

	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{UID: "test-uid"},
		Spec: v1alpha1.TaskSpec{
			Commands: []string{"echo $MY_VAR"},
			Env: []corev1.EnvVar{
				{Name: "MY_VAR", Value: "test-value"},
			},
		},
	}

	result := e.Execute(context.Background(), task)
	if result.Phase != v1alpha1.TaskSucceeded {
		t.Fatalf("expected Succeeded, got %s", result.Phase)
	}
	if result.Message != "test-value\n" {
		t.Errorf("expected 'test-value\\n', got %q", result.Message)
	}
}

func TestExecutor_Cleanup(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{WorkspaceBase: dir}

	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{UID: "test-uid"},
		Spec: v1alpha1.TaskSpec{
			Commands: []string{"echo done"},
		},
	}

	workDir := filepath.Join(dir, "test-uid")
	_ = os.MkdirAll(workDir, 0o755)

	e.Execute(context.Background(), task)

	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Error("workspace directory should be cleaned up after execution")
	}
}

func TestExecutor_Timeout(t *testing.T) {
	dir := t.TempDir()
	e := &Executor{WorkspaceBase: dir}

	ctx, cancel := context.WithTimeout(context.Background(), 100_000_000) // 100ms
	defer cancel()

	task := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{UID: "test-uid"},
		Spec: v1alpha1.TaskSpec{
			Commands: []string{"sleep 10"},
		},
	}

	result := e.Execute(ctx, task)
	if result.Phase != v1alpha1.TaskFailed {
		t.Errorf("expected Failed due to timeout, got %s", result.Phase)
	}
}
