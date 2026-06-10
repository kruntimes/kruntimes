package krt

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"
)

type portForwarder interface {
	Forward(ctx context.Context, namespace, podName string, localPort, remotePort int) (io.Closer, error)
}

type kubectlPortForwarder struct{}

func (kubectlPortForwarder) Forward(
	ctx context.Context,
	namespace, podName string,
	localPort, remotePort int,
) (io.Closer, error) {
	forwardCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(
		forwardCtx,
		"kubectl",
		"port-forward",
		"pod/"+podName,
		fmt.Sprintf("%d:%d", localPort, remotePort),
		"--namespace",
		namespace,
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start port-forward: %w", err)
	}

	forward := &runningPortForward{cancel: cancel, cmd: cmd, done: make(chan error, 1)}
	go func() {
		forward.done <- cmd.Wait()
	}()

	if err := waitForLocalPort(ctx, localPort, forward.done); err != nil {
		_ = forward.Close()
		return nil, fmt.Errorf("wait for port-forward to pod %s: %w", podName, err)
	}
	return forward, nil
}

type runningPortForward struct {
	cancel context.CancelFunc
	cmd    *exec.Cmd
	done   chan error
}

func (f *runningPortForward) Close() error {
	f.cancel()
	select {
	case err := <-f.done:
		if err != nil && f.cmd.ProcessState != nil && !f.cmd.ProcessState.Success() {
			return nil
		}
		return err
	case <-time.After(2 * time.Second):
		if f.cmd.Process != nil {
			return f.cmd.Process.Kill()
		}
		return nil
	}
}

func waitForLocalPort(ctx context.Context, port int, processDone <-chan error) error {
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-processDone:
			if err == nil {
				return fmt.Errorf("kubectl port-forward exited")
			}
			return fmt.Errorf("kubectl port-forward exited: %w", err)
		case <-timeout.C:
			return fmt.Errorf("timed out connecting to %s", address)
		case <-ticker.C:
			connection, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
			if err == nil {
				_ = connection.Close()
				return nil
			}
		}
	}
}
