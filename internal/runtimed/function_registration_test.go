package runtimed

import (
	"testing"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestFunctionRegistrationRequest(t *testing.T) {
	inline := "def handler(event): return event"
	idleTimeout := int32(300)
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("run-uid")},
		Spec: v1alpha1.RunSpec{
			Source: &v1alpha1.CodeSource{Inline: &inline},
			Mode: v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{
				Handler:            "app.handler",
				IdleTimeoutSeconds: &idleTimeout,
			}},
			Env: []corev1.EnvVar{
				{Name: "SECOND", Value: "two"},
				{Name: "FIRST", Value: "one"},
			},
		},
	}

	request, err := functionRegistrationRequest(run, "/workspace/run-uid")
	if err != nil {
		t.Fatal(err)
	}
	if request.RunUid != "run-uid" {
		t.Fatalf("RunUid = %q, want run-uid", request.RunUid)
	}
	if request.RegistrationAttempt != 1 {
		t.Fatalf("RegistrationAttempt = %d, want 1", request.RegistrationAttempt)
	}
	if request.WorkingDir != "/workspace/run-uid" {
		t.Fatalf("WorkingDir = %q, want /workspace/run-uid", request.WorkingDir)
	}
	if request.Handler != "app.handler" {
		t.Fatalf("Handler = %q, want app.handler", request.Handler)
	}
	if request.IdleTimeoutSeconds != 300 {
		t.Fatalf("IdleTimeoutSeconds = %d, want 300", request.IdleTimeoutSeconds)
	}
	if request.Env["FIRST"] != "one" || request.Env["SECOND"] != "two" {
		t.Fatalf("Env = %#v, want both values", request.Env)
	}
	if request.RegistrationDigest == "" {
		t.Fatal("RegistrationDigest is empty")
	}
}

func TestFunctionRegistrationRequestUsesCurrentAttempt(t *testing.T) {
	run := functionRegistrationTestRun()
	run.Status.Attempt = 3

	request, err := functionRegistrationRequest(run, "/workspace/run-uid")
	if err != nil {
		t.Fatal(err)
	}
	if request.RegistrationAttempt != 3 {
		t.Fatalf("RegistrationAttempt = %d, want 3", request.RegistrationAttempt)
	}
}

func TestFunctionRegistrationDigestIsCanonicalAndExcludesWorkingDirectory(t *testing.T) {
	run := functionRegistrationTestRun()
	first, err := functionRegistrationRequest(run, "/workspace/first")
	if err != nil {
		t.Fatal(err)
	}

	run.Spec.Env = []corev1.EnvVar{
		{Name: "SECOND", Value: "two"},
		{Name: "FIRST", Value: "one"},
	}
	second, err := functionRegistrationRequest(run, "/workspace/second")
	if err != nil {
		t.Fatal(err)
	}
	if first.RegistrationDigest != second.RegistrationDigest {
		t.Fatalf("digest changed for equivalent registration inputs: %q != %q", first.RegistrationDigest, second.RegistrationDigest)
	}

	run.Spec.Mode.Function.Handler = "app.other"
	changedHandler, err := functionRegistrationRequest(run, "/workspace/second")
	if err != nil {
		t.Fatal(err)
	}
	if first.RegistrationDigest == changedHandler.RegistrationDigest {
		t.Fatal("digest did not change after handler changed")
	}
}

func TestFunctionRegistrationRequestRejectsIncompleteFunctionRun(t *testing.T) {
	missingUID := functionRegistrationTestRun()
	missingUID.UID = ""
	missingHandler := functionRegistrationTestRun()
	missingHandler.Spec.Mode.Function.Handler = ""
	tests := []struct {
		name       string
		run        *v1alpha1.Run
		workingDir string
	}{
		{name: "missing-run-uid", run: missingUID, workingDir: "/workspace/run"},
		{name: "missing-working-directory", run: functionRegistrationTestRun()},
		{name: "missing-handler", run: missingHandler, workingDir: "/workspace/run"},
		{
			name: "task-mode",
			run: &v1alpha1.Run{
				ObjectMeta: metav1.ObjectMeta{UID: types.UID("run-uid")},
				Spec:       v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{}}},
			},
			workingDir: "/workspace/run",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := functionRegistrationRequest(tt.run, tt.workingDir); err == nil {
				t.Fatal("functionRegistrationRequest() error = nil")
			}
		})
	}
}

func functionRegistrationTestRun() *v1alpha1.Run {
	inline := "def handler(event): return event"
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("run-uid")},
		Spec: v1alpha1.RunSpec{
			Source: &v1alpha1.CodeSource{Inline: &inline},
			Mode: v1alpha1.RunMode{Function: &v1alpha1.RunFunctionMode{
				Handler: "app.handler",
			}},
			Env: []corev1.EnvVar{
				{Name: "FIRST", Value: "one"},
				{Name: "SECOND", Value: "two"},
			},
		},
	}
}
