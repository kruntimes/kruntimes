package krt

import (
	"bytes"
	"context"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

func TestShowLogsOnceWritesStdoutAndStderr(t *testing.T) {
	cli := &statusSequenceClient{
		responses: []*pb.StatusResponse{{
			Stdout: "stdout",
			Stderr: "stderr",
		}},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if err := showLogsOnce(context.Background(), cli, "run-uid", 0, &stdout, &stderr); err != nil {
		t.Fatalf("showLogsOnce() error = %v", err)
	}
	if got := stdout.String(); got != "stdout\n" {
		t.Fatalf("stdout = %q, want %q", got, "stdout\n")
	}
	if got := stderr.String(); got != "stderr\n" {
		t.Fatalf("stderr = %q, want %q", got, "stderr\n")
	}
}

func TestFollowLogsTracksStdoutAndStderrOffsets(t *testing.T) {
	cli := &statusSequenceClient{
		responses: []*pb.StatusResponse{
			{
				Stdout: "old stdout\nstdout one\n",
				Stderr: "old stderr\nstderr one\n",
				State:  pb.ExecutionState_EXECUTION_STATE_RUNNING,
			},
			{
				Stdout: "old stdout\nstdout one\nstdout two\n",
				Stderr: "old stderr\nstderr one\nstderr two\n",
				State:  pb.ExecutionState_EXECUTION_STATE_SUCCEEDED,
			},
		},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if err := followLogs(context.Background(), cli, "run-uid", 1, &stdout, &stderr); err != nil {
		t.Fatalf("followLogs() error = %v", err)
	}
	if got := stdout.String(); got != "stdout one\nstdout two\n" {
		t.Fatalf("stdout = %q, want each line once", got)
	}
	if got := stderr.String(); got != "stderr one\nstderr two\n" {
		t.Fatalf("stderr = %q, want each line once", got)
	}
}

func TestLogOutputSinceResetsInvalidOffset(t *testing.T) {
	got, offset := logOutputSince("short", 10)
	if got != "short" || offset != len("short") {
		t.Fatalf("logOutputSince() = %q, %d; want %q, %d", got, offset, "short", len("short"))
	}
}

type statusSequenceClient struct {
	pb.RuntimeClient
	responses []*pb.StatusResponse
	index     int
}

func (c *statusSequenceClient) Status(
	context.Context,
	*pb.StatusRequest,
	...grpc.CallOption,
) (*pb.StatusResponse, error) {
	response := c.responses[c.index]
	if c.index < len(c.responses)-1 {
		c.index++
	}
	return response, nil
}
