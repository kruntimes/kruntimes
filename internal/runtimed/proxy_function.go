package runtimed

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const defaultFunctionInvocationTimeout = 30 * time.Second

// functionRuntimeProxy implements the Console-facing form of FunctionRuntime.
// Console requests identify a Run UID but intentionally omit the opaque
// registration ID; only the assigned owner resolves that local reference.
type functionRuntimeProxy struct {
	pb.UnimplementedFunctionRuntimeServer

	reader      client.Reader
	podReader   client.Reader
	local       pb.FunctionRuntimeClient
	controller  *Controller
	namespace   string
	runtimeName string
	podName     string
	statusPort  string
	dialPeer    func(context.Context, string) (pb.FunctionRuntimeClient, io.Closer, error)
}

func newFunctionRuntimeProxy(reader, podReader client.Reader, local pb.FunctionRuntimeClient, controller *Controller, namespace, runtimeName, podName, statusPort string) *functionRuntimeProxy {
	return &functionRuntimeProxy{
		reader: reader, podReader: podReader, local: local, controller: controller, namespace: namespace, runtimeName: runtimeName,
		podName: podName, statusPort: statusPort, dialPeer: dialFunctionRuntimePeer,
	}
}

func (s *functionRuntimeProxy) InvokeFunction(ctx context.Context, request *pb.InvokeFunctionRequest) (*pb.InvokeFunctionResponse, error) {
	run, err := s.functionRun(ctx, request.GetRegistration())
	if err != nil {
		return nil, err
	}
	if run.Status.AssignedPod != s.podName {
		if forwarded(ctx) {
			return nil, status.Error(codes.FailedPrecondition, "forwarded function request did not reach its assigned Runtime Pod")
		}
		owner := &corev1.Pod{}
		if err := s.podReader.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: run.Status.AssignedPod}, owner); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, status.Error(codes.Unavailable, "assigned Runtime Pod is not available")
			}
			return nil, status.Errorf(codes.Internal, "get assigned Runtime Pod: %v", err)
		}
		if string(owner.UID) != run.Status.AssignedPodUID || owner.Labels["runtime"] != s.runtimeName || owner.Status.PodIP == "" {
			return nil, status.Error(codes.Unavailable, "assigned Runtime Pod is not available")
		}
		peer, closer, err := s.dialPeer(withForwardedMarker(ctx), net.JoinHostPort(owner.Status.PodIP, s.statusPort))
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "dial assigned Runtime Pod: %v", err)
		}
		defer closer.Close()
		return peer.InvokeFunction(withForwardedMarker(ctx), request)
	}

	value, ok := s.controller.activeRuns.Load(string(run.UID))
	if !ok {
		return nil, status.Error(codes.FailedPrecondition, "function registration is recovering")
	}
	registration := value.(*activeRun).functionRegistrationRef()
	if registration == nil {
		return nil, status.Error(codes.FailedPrecondition, "function registration is not ready")
	}
	timeout, err := functionInvocationTimeout(run, request.GetTimeoutMillis())
	if err != nil {
		return nil, err
	}
	ar := value.(*activeRun)
	started := time.Now()
	if err := resetFunctionInvocationOutputs(ar.outputPath); err != nil {
		invokeErr := status.Errorf(codes.Internal, "reset function invocation outputs: %v", err)
		s.controller.emitFunctionInvocationAudit(run, request.GetInvocationId(), invokeErr, time.Since(started))
		return nil, invokeErr
	}
	invokeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, invokeErr := s.local.InvokeFunction(invokeCtx, &pb.InvokeFunctionRequest{
		Registration: registration, InvocationId: request.GetInvocationId(), Input: request.GetInput(),
		ContentType: request.GetContentType(), TimeoutMillis: timeout.Milliseconds(),
	})
	if invokeErr == nil {
		fileOutputs, err := readOutputs(ar.outputPath)
		if err != nil {
			invokeErr = status.Errorf(codes.Internal, "read function invocation outputs: %v", err)
		} else if response.Outputs, err = mergeInvocationOutputs(response.GetOutputs(), fileOutputs); err != nil {
			invokeErr = status.Errorf(codes.Internal, "merge function invocation outputs: %v", err)
		}
	}
	invocationID := request.GetInvocationId()
	if response != nil && response.GetInvocationId() != "" {
		invocationID = response.GetInvocationId()
	}
	s.controller.emitFunctionInvocationAudit(run, invocationID, invokeErr, time.Since(started))
	if invokeErr != nil {
		return nil, invokeErr
	}
	return response, nil
}

func resetFunctionInvocationOutputs(path string) error {
	if path == "" {
		return fmt.Errorf("outputs path is empty")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s *functionRuntimeProxy) functionRun(ctx context.Context, registration *pb.FunctionRegistration) (*v1alpha1.Run, error) {
	if registration == nil || registration.GetRunUid() == "" || registration.GetRegistrationId() != "" {
		return nil, status.Error(codes.InvalidArgument, "Console function request requires a Run UID and no registration ID")
	}
	if s.reader == nil || s.podReader == nil || s.local == nil || s.controller == nil || s.namespace == "" || s.runtimeName == "" || s.podName == "" {
		return nil, status.Error(codes.FailedPrecondition, "FunctionRuntime proxy is not configured")
	}
	var runs v1alpha1.RunList
	if err := s.reader.List(ctx, &runs, client.InNamespace(s.namespace), client.MatchingFields{runtimeRunIndexField: s.runtimeName}); err != nil {
		return nil, status.Errorf(codes.Internal, "list Function Runs: %v", err)
	}
	for i := range runs.Items {
		run := &runs.Items[i]
		if string(run.UID) != registration.GetRunUid() {
			continue
		}
		if run.Spec.Runtime != s.runtimeName || run.Spec.Mode.Function == nil {
			return nil, status.Error(codes.NotFound, "function Run is not served by this Runtime")
		}
		if run.Status.Phase != v1alpha1.RunReady || run.Status.AssignedPod == "" || run.Status.AssignedPodUID == "" {
			return nil, status.Errorf(codes.FailedPrecondition, "function Run is %s, not Ready", run.Status.Phase)
		}
		return run, nil
	}
	return nil, status.Error(codes.NotFound, "function Run not found")
}

func functionInvocationTimeout(run *v1alpha1.Run, requestedMillis int64) (time.Duration, error) {
	timeout := defaultFunctionInvocationTimeout
	if requestedMillis > 0 {
		timeout = time.Duration(requestedMillis) * time.Millisecond
	}
	if run != nil && run.Spec.Timeout != nil && run.Status.StartTime != nil {
		remaining := time.Until(run.Status.StartTime.Add(run.Spec.Timeout.Duration))
		if remaining <= 0 {
			return 0, status.Error(codes.DeadlineExceeded, "function Run timeout has elapsed")
		}
		timeout = min(timeout, remaining)
	}
	if timeout <= 0 {
		return 0, status.Error(codes.InvalidArgument, "function invocation timeout must be positive")
	}
	return timeout, nil
}

func dialFunctionRuntimePeer(_ context.Context, address string) (pb.FunctionRuntimeClient, io.Closer, error) {
	connection, err := grpc.NewClient(runtimePeerTarget(address), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, nil, fmt.Errorf("create FunctionRuntime client: %w", err)
	}
	return pb.NewFunctionRuntimeClient(connection), connection, nil
}
