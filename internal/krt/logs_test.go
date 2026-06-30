package krt

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

func TestWriteStructuredRunLogsFiltersRunUIDAndStreams(t *testing.T) {
	input := strings.Join([]string{
		`{"run_uid":"other","stream":"stdout","message":"ignore"}`,
		`{"run_uid":"run-uid","stream":"stdout","message":"stdout one"}`,
		`{"run_uid":"run-uid","stream":"stderr","message":"stderr one"}`,
		`not-json`,
		`{"run_uid":"run-uid","stream":"stdout","message":"stdout two"}`,
		`{"run_uid":"run-uid","stream":"stderr","message":"stderr two"}`,
	}, "\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if err := writeStructuredRunLogs(strings.NewReader(input), "run-uid", 0, &stdout, &stderr); err != nil {
		t.Fatalf("writeStructuredRunLogs() error = %v", err)
	}
	if got := stdout.String(); got != "stdout one\nstdout two\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := stderr.String(); got != "stderr one\nstderr two\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestWriteStructuredRunLogsAppliesTailPerStream(t *testing.T) {
	input := strings.Join([]string{
		`{"run_uid":"run-uid","stream":"stdout","message":"stdout one"}`,
		`{"run_uid":"run-uid","stream":"stdout","message":"stdout two"}`,
		`{"run_uid":"run-uid","stream":"stderr","message":"stderr one"}`,
		`{"run_uid":"run-uid","stream":"stderr","message":"stderr two"}`,
	}, "\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	if err := writeStructuredRunLogs(strings.NewReader(input), "run-uid", 1, &stdout, &stderr); err != nil {
		t.Fatalf("writeStructuredRunLogs() error = %v", err)
	}
	if got := stdout.String(); got != "stdout two\n" {
		t.Fatalf("stdout = %q", got)
	}
	if got := stderr.String(); got != "stderr two\n" {
		t.Fatalf("stderr = %q", got)
	}
}

func TestIsGRPCCodeUnwrapsStatusErrors(t *testing.T) {
	err := fmt.Errorf("status: %w", status.Error(codes.NotFound, "request not found"))

	if !isGRPCCode(err, codes.NotFound) {
		t.Fatal("isGRPCCode() = false, want true")
	}
	if isGRPCCode(err, codes.Internal) {
		t.Fatal("isGRPCCode() matched wrong code")
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
