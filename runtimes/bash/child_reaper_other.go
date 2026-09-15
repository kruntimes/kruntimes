//go:build !linux

package bash

import (
	"os/exec"
	"syscall"
)

type commandWaitResult struct {
	status syscall.WaitStatus
	err    error
}

func enableChildSubreaper() {}

func startManagedCommand(command *exec.Cmd) (<-chan commandWaitResult, error) {
	if err := command.Start(); err != nil {
		return nil, err
	}
	result := make(chan commandWaitResult, 1)
	go func() {
		result <- commandWaitResult{err: command.Wait()}
		close(result)
	}()
	return result, nil
}
