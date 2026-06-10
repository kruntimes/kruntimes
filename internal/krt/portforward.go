package krt

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

type portForwarder interface {
	Forward(ctx context.Context, namespace, podName string, localPort, remotePort int) (io.Closer, error)
}

type clientGoPortForwarder struct {
	restConfig *rest.Config
	pods       corev1client.CoreV1Interface
}

func newPortForwarder(restConfig *rest.Config) (portForwarder, error) {
	if restConfig == nil {
		return nil, fmt.Errorf("rest config is required")
	}
	coreClient, err := corev1client.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("create core client: %w", err)
	}
	return &clientGoPortForwarder{restConfig: restConfig, pods: coreClient}, nil
}

func (f *clientGoPortForwarder) Forward(
	ctx context.Context,
	namespace, podName string,
	localPort, remotePort int,
) (io.Closer, error) {
	transport, upgrader, err := spdy.RoundTripperFor(f.restConfig)
	if err != nil {
		return nil, fmt.Errorf("build port-forward transport: %w", err)
	}
	requestURL := f.pods.RESTClient().Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("portforward").
		URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", requestURL)

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(
		dialer,
		[]string{"127.0.0.1"},
		[]string{fmt.Sprintf("%d:%d", localPort, remotePort)},
		stopCh,
		readyCh,
		io.Discard,
		os.Stderr,
	)
	if err != nil {
		return nil, fmt.Errorf("start port-forward: %w", err)
	}

	forward := &runningPortForward{stopCh: stopCh, done: make(chan error, 1)}
	go func() {
		forward.done <- forwarder.ForwardPorts()
	}()

	if err := waitForPortForward(ctx, podName, localPort, readyCh, forward.done); err != nil {
		_ = forward.Close()
		return nil, err
	}
	return forward, nil
}

type runningPortForward struct {
	stopCh chan struct{}
	done   chan error
	once   sync.Once
}

func (f *runningPortForward) Close() error {
	f.once.Do(func() {
		close(f.stopCh)
	})
	select {
	case err := <-f.done:
		return err
	case <-time.After(2 * time.Second):
		return fmt.Errorf("timed out stopping port-forward")
	}
}

func waitForPortForward(
	ctx context.Context,
	podName string,
	port int,
	readyCh <-chan struct{},
	processDone <-chan error,
) error {
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-readyCh:
			return waitForLocalPort(ctx, port, processDone)
		case err := <-processDone:
			if err == nil {
				return fmt.Errorf("port-forward to pod %s exited", podName)
			}
			return fmt.Errorf("port-forward to pod %s exited: %w", podName, err)
		case <-timeout.C:
			return fmt.Errorf("timed out starting port-forward to pod %s on %s", podName, address)
		}
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
				return fmt.Errorf("port-forward exited")
			}
			return fmt.Errorf("port-forward exited: %w", err)
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
