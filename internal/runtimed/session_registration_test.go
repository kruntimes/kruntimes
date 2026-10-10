package runtimed

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

func TestSessionRegistrationRequest(t *testing.T) {
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("session-run")},
		Spec: v1alpha1.RunSpec{
			Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}},
			Env:  []corev1.EnvVar{{Name: "TOKEN", Value: "value"}},
		},
		Status: v1alpha1.RunStatus{AssignedPodUID: "runtime-pod"},
	}

	request, err := sessionRegistrationRequest(run, "/workspace/session-run", "/workspace/session-run/artifacts")
	if err != nil {
		t.Fatal(err)
	}
	if request.Identity.GetRunUid() != "session-run" || request.Identity.GetAssignedPodUid() != "runtime-pod" {
		t.Fatalf("identity = %#v", request.Identity)
	}
	if request.WorkingDir != "/workspace/session-run" || request.Env["TOKEN"] != "value" {
		t.Fatalf("request = %#v", request)
	}
	if request.Env[artifact.ArtifactsDirEnv] != "/workspace/session-run/artifacts" {
		t.Fatalf("artifact directory = %q", request.Env[artifact.ArtifactsDirEnv])
	}
}

func TestSessionRegistrationRequestRejectsIncompleteSessionRun(t *testing.T) {
	valid := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("session-run")},
		Spec:       v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
		Status:     v1alpha1.RunStatus{AssignedPodUID: "runtime-pod"},
	}
	tests := []struct {
		name string
		run  *v1alpha1.Run
		dir  string
	}{
		{name: "missing-run-uid", run: valid.DeepCopy(), dir: "/workspace/run"},
		{name: "missing-pod-uid", run: valid.DeepCopy(), dir: "/workspace/run"},
		{name: "missing-working-directory", run: valid.DeepCopy()},
		{name: "task-mode", run: &v1alpha1.Run{ObjectMeta: metav1.ObjectMeta{UID: types.UID("session-run")}, Spec: v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}}}, Status: v1alpha1.RunStatus{AssignedPodUID: "runtime-pod"}}, dir: "/workspace/run"},
	}
	tests[0].run.UID = ""
	tests[1].run.Status.AssignedPodUID = ""
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := sessionRegistrationRequest(tt.run, tt.dir, ""); err == nil {
				t.Fatal("sessionRegistrationRequest() error = nil")
			}
		})
	}
}
