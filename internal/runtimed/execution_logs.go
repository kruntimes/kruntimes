package runtimed

import (
	"encoding/json"
	"io"
	"os"
	"strings"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

type executionOutput struct {
	stdout string
	stderr string
}

type executionLogLine struct {
	RunUID    string `json:"run_uid"`
	RunName   string `json:"run_name"`
	Namespace string `json:"namespace"`
	Runtime   string `json:"runtime"`
	Pod       string `json:"pod"`
	Stream    string `json:"stream"`
	Message   string `json:"message"`
}

func outputFromStatus(resp *pb.StatusResponse) executionOutput {
	if resp == nil {
		return executionOutput{}
	}
	return executionOutput{stdout: resp.Stdout, stderr: resp.Stderr}
}

func (c *Controller) emitExecutionOutput(run *v1alpha1.Run, output executionOutput) {
	if run == nil {
		return
	}
	writer := c.ExecutionLogWriter
	if writer == nil {
		writer = os.Stdout
	}

	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.emitStream(writer, run, "stdout", output.stdout)
	c.emitStream(writer, run, "stderr", output.stderr)
}

func (c *Controller) emitStream(writer io.Writer, run *v1alpha1.Run, stream, content string) {
	for _, message := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		if message == "" {
			continue
		}
		line := executionLogLine{
			RunUID:    string(run.UID),
			RunName:   run.Name,
			Namespace: run.Namespace,
			Runtime:   run.Spec.Runtime,
			Pod:       c.Hostname,
			Stream:    stream,
			Message:   strings.TrimSuffix(message, "\r"),
		}
		encoded, err := json.Marshal(line)
		if err != nil {
			continue
		}
		_, _ = writer.Write(append(encoded, '\n'))
	}
}
