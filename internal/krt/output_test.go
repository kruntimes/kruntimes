package krt

import (
	"bytes"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestWriteStructuredOutputJSON(t *testing.T) {
	var output bytes.Buffer
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunSucceeded},
	}

	if err := writeStructuredOutput(&output, outputJSON, run); err != nil {
		t.Fatalf("writeStructuredOutput() error = %v", err)
	}

	got := output.String()
	if !strings.Contains(got, `"name": "run-1"`) {
		t.Fatalf("json output missing run name: %s", got)
	}
	if !strings.Contains(got, `"namespace": "team-a"`) {
		t.Fatalf("json output missing default namespace: %s", got)
	}
}

func TestWriteStructuredOutputYAML(t *testing.T) {
	var output bytes.Buffer
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a"},
		Spec:       v1alpha1.RunSpec{Runtime: "bash"},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunSucceeded},
	}

	if err := writeStructuredOutput(&output, outputYAML, run); err != nil {
		t.Fatalf("writeStructuredOutput() error = %v", err)
	}

	got := output.String()
	if !strings.Contains(got, "name: run-1") {
		t.Fatalf("yaml output missing run name: %s", got)
	}
}

func TestUnsupportedOutputFormat(t *testing.T) {
	err := writeStructuredOutput(&bytes.Buffer{}, "wide", &v1alpha1.Run{})
	if err == nil || !strings.Contains(err.Error(), `unsupported output format "wide"`) {
		t.Fatalf("writeStructuredOutput() error = %v", err)
	}
}

func TestFilterRuns(t *testing.T) {
	runs := []v1alpha1.Run{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "run-1"},
			Spec:       v1alpha1.RunSpec{Runtime: "bash"},
			Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunSucceeded},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "run-2"},
			Spec:       v1alpha1.RunSpec{Runtime: "python"},
			Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunFailed},
		},
	}

	filtered := filterRuns(runs, "bash", "Succeeded")
	if len(filtered) != 1 || filtered[0].Name != "run-1" {
		t.Fatalf("filterRuns() = %#v, want only run-1", filtered)
	}
}
