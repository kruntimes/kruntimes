package krt

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriteSnapshotRunLogsWritesStdoutAndStderr(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := writeSnapshotRunLogs(strings.NewReader(`{"items":[
		{"stream":"stdout","message":"stdout one"},
		{"stream":"audit","message":"ignore"},
		{"stream":"stderr","message":"stderr one"},
		{"stream":"stdout","message":"stdout two\n"}
	]}`), &stdout, &stderr)
	if err != nil {
		t.Fatalf("writeSnapshotRunLogs() error = %v", err)
	}
	if got := stdout.String(); got != "stdout one\nstdout two\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := stderr.String(); got != "stderr one\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestWriteFollowRunLogsWritesEachRecord(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	input := strings.Join([]string{
		`{"stream":"stdout","message":"stdout one"}`,
		`{"stream":"stderr","message":"stderr one"}`,
		`{"stream":"audit","message":"ignore"}`,
	}, "\n")

	if err := writeFollowRunLogs(strings.NewReader(input), &stdout, &stderr); err != nil {
		t.Fatalf("writeFollowRunLogs() error = %v", err)
	}
	if got := stdout.String(); got != "stdout one\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := stderr.String(); got != "stderr one\n" {
		t.Fatalf("stderr = %q", got)
	}
}
