//go:build linux

package bash

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func TestNewServerEnablesChildSubreaper(t *testing.T) {
	_ = NewServer(t.TempDir())

	var enabled int32
	_, _, errno := unix.Syscall6(
		unix.SYS_PRCTL,
		uintptr(unix.PR_GET_CHILD_SUBREAPER),
		uintptr(unsafe.Pointer(&enabled)),
		0,
		0,
		0,
		0,
	)
	if errno != 0 {
		t.Fatalf("read child subreaper setting: %v", errno)
	}
	if enabled != 1 {
		t.Fatalf("child subreaper = %d, want 1", enabled)
	}
}

func TestChildReaperReapsOrphanedDescendant(t *testing.T) {
	_ = NewServer(t.TempDir())

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	command := exec.Command("bash", "-c", "sleep 30 & echo $! > \"$1\"", "bash", pidFile)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	result, err := startManagedCommand(command)
	if err != nil {
		t.Fatalf("start shell: %v", err)
	}
	if result := <-result; result.err != nil || !result.status.Exited() || result.status.ExitStatus() != 0 {
		t.Fatalf("wait shell: %+v", result)
	}

	content, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	if err := syscall.Kill(childPID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill orphaned child: %v", err)
	}

	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if err := syscall.Kill(childPID, 0); err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("orphaned child %d was not reaped", childPID)
}
