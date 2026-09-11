package runtimed

import (
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type executionOutput struct {
	stdout string
	stderr string
}

type executionLogLine struct {
	RunUID               string `json:"run_uid"`
	AssignedPodUID       string `json:"assigned_pod_uid,omitempty"`
	RunName              string `json:"run_name"`
	Namespace            string `json:"namespace"`
	Runtime              string `json:"runtime"`
	Pod                  string `json:"pod"`
	Stream               string `json:"stream"`
	Message              string `json:"message"`
	InvocationID         string `json:"invocation_id,omitempty"`
	Operation            string `json:"operation,omitempty"`
	Outcome              string `json:"outcome,omitempty"`
	StatusCode           string `json:"status_code,omitempty"`
	ExitCode             *int32 `json:"exit_code,omitempty"`
	TimedOut             bool   `json:"timed_out,omitempty"`
	DurationMilliseconds int64  `json:"duration_milliseconds,omitempty"`
}

func outputFromStatus(resp *pb.StatusResponse) executionOutput {
	if resp == nil {
		return executionOutput{}
	}
	return executionOutput{stdout: resp.Stdout, stderr: resp.Stderr}
}

// emitExecutionOutputDelta writes only complete lines newly observed through
// Runtime Status. A Runtime restart loses the local cursor, so the retained
// Runtime buffer may be written again; container logs deliberately provide
// at-least-once, rather than exactly-once, delivery.
//
// terminal flushes a final unterminated line. It must only be true after the
// Run terminal status update succeeds, so callers never observe a terminal
// fragment before the Run itself is terminal.
func (c *Controller) emitExecutionOutputDelta(ar *activeRun, output executionOutput, terminal bool) {
	if ar == nil || ar.run == nil {
		return
	}
	writer := c.ExecutionLogWriter
	if writer == nil {
		writer = os.Stdout
	}

	ar.outputMu.Lock()
	defer ar.outputMu.Unlock()
	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.emitOutputDelta(writer, ar.run, "stdout", output.stdout, &ar.stdoutCursor, terminal)
	c.emitOutputDelta(writer, ar.run, "stderr", output.stderr, &ar.stderrCursor, terminal)
}

func (c *Controller) emitOutputDelta(writer io.Writer, run *v1alpha1.Run, stream, content string, cursor *outputCursor, terminal bool) {
	if cursor == nil {
		return
	}
	if cursor.seen > len(content) || (cursor.hasHash && sha256.Sum256([]byte(content[:cursor.seen])) != cursor.prefixHash) {
		// Runtime Status no longer contains the previous prefix. This can happen
		// after a Runtime Server restart or a bounded-buffer implementation
		// change. Restart from the retained content; duplicates are preferable to
		// silently losing logs.
		cursor.seen = 0
		cursor.pending = ""
	}

	newContent := content[cursor.seen:]
	cursor.seen = len(content)
	cursor.prefixHash = sha256.Sum256([]byte(content))
	cursor.hasHash = true
	combined := cursor.pending + newContent
	lastNewline := strings.LastIndexByte(combined, '\n')
	if lastNewline >= 0 {
		for _, message := range strings.Split(combined[:lastNewline], "\n") {
			if message != "" {
				writeExecutionLogLine(writer, executionLogLineFor(run, c.PodName, stream, strings.TrimSuffix(message, "\r")))
			}
		}
		cursor.pending = combined[lastNewline+1:]
	} else {
		cursor.pending = combined
	}

	if terminal && cursor.pending != "" {
		writeExecutionLogLine(writer, executionLogLineFor(run, c.PodName, stream, strings.TrimSuffix(cursor.pending, "\r")))
		cursor.pending = ""
	}
}

func executionLogLineFor(run *v1alpha1.Run, pod, stream, message string) executionLogLine {
	return executionLogLine{
		RunUID:         string(run.UID),
		AssignedPodUID: run.Status.AssignedPodUID,
		RunName:        run.Name,
		Namespace:      run.Namespace,
		Runtime:        run.Spec.Runtime,
		Pod:            pod,
		Stream:         stream,
		Message:        message,
	}
}

func writeExecutionLogLine(writer io.Writer, line executionLogLine) {
	encoded, err := json.Marshal(line)
	if err != nil {
		return
	}
	_, _ = writer.Write(append(encoded, '\n'))
}

func (c *Controller) emitFunctionInvocationAudit(run *v1alpha1.Run, invocationID string, invokeErr error, duration time.Duration) {
	if run == nil {
		return
	}
	writer := c.ExecutionLogWriter
	if writer == nil {
		writer = os.Stdout
	}
	line := executionLogLineFor(run, c.PodName, "audit", "function invocation completed")
	line.InvocationID = invocationID
	line.Operation = "function_invoke"
	line.DurationMilliseconds = duration.Milliseconds()
	if invokeErr == nil {
		line.Outcome = "succeeded"
	} else {
		line.StatusCode = status.Code(invokeErr).String()
		switch status.Code(invokeErr) {
		case codes.Canceled:
			line.Outcome = "cancelled"
		case codes.DeadlineExceeded:
			line.Outcome = "timed_out"
		default:
			line.Outcome = "failed"
		}
	}
	c.logMu.Lock()
	defer c.logMu.Unlock()
	writeExecutionLogLine(writer, line)
}
