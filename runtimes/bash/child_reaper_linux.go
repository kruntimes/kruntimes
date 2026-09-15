//go:build linux

package bash

import (
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
	"k8s.io/klog/v2"
)

type commandWaitResult struct {
	status syscall.WaitStatus
	err    error
}

var childSupervisor = struct {
	sync.Mutex
	waiters map[int]chan commandWaitResult
}{waiters: make(map[int]chan commandWaitResult)}

var childSupervisorOnce sync.Once

// enableChildSubreaper makes this runtime adopt orphaned descendants. A single
// supervisor owns wait4, like an init process such as tini; execution paths
// receive a per-command result channel rather than calling cmd.Wait.
func enableChildSubreaper() {
	childSupervisorOnce.Do(func() {
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			klog.V(2).Infof("Unable to enable Bash runtime child subreaper: %v", err)
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGCHLD)
		go func() {
			for range signals {
				reapChildren()
			}
		}()
	})
}

func startManagedCommand(command *exec.Cmd) (<-chan commandWaitResult, error) {
	childSupervisor.Lock()
	defer childSupervisor.Unlock()
	if err := command.Start(); err != nil {
		return nil, err
	}
	result := make(chan commandWaitResult, 1)
	childSupervisor.waiters[command.Process.Pid] = result
	return result, nil
}

func reapChildren() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
		childSupervisor.Lock()
		waiter := childSupervisor.waiters[pid]
		delete(childSupervisor.waiters, pid)
		childSupervisor.Unlock()
		if waiter != nil {
			waiter <- commandWaitResult{status: status}
			close(waiter)
		}
	}
}
