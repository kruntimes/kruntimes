package bash

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
)

type functionInvocation struct {
	workingDir   string
	handlerFile  string
	handlerName  string
	env          map[string]string
	registration *pb.FunctionRegistration
	done         chan struct{}
}

type functionDrain struct {
	inFlight bool
	cancel   context.CancelFunc
	done     <-chan struct{}
}

// registrationAction returns an idempotent response for the current
// generation or the in-flight invocation that must finish before replacement.
func (e *functionEntry) registrationAction(attempt int32, digest string) (*pb.RegisterFunctionResponse, functionDrain, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch {
	case attempt < e.attempt:
		return nil, functionDrain{}, status.Error(codes.FailedPrecondition, "registration attempt is stale")
	case attempt == e.attempt:
		if digest != e.digest {
			return nil, functionDrain{}, status.Error(codes.AlreadyExists, "registration attempt already exists with a different digest")
		}
		return &pb.RegisterFunctionResponse{
			Registration: cloneFunctionRegistration(e.registration),
			State:        e.state,
		}, functionDrain{}, nil
	default:
		return nil, functionDrain{inFlight: e.inFlight, cancel: e.cancel, done: e.done}, nil
	}
}

func (e *functionEntry) registrationResponse() *pb.RegisterFunctionResponse {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return &pb.RegisterFunctionResponse{
		Registration: cloneFunctionRegistration(e.registration),
		State:        e.state,
	}
}

func (e *functionEntry) statusResponse() *pb.FunctionStatusResponse {
	e.mu.RLock()
	defer e.mu.RUnlock()

	inFlight := int32(0)
	if e.inFlight {
		inFlight = 1
	}
	return &pb.FunctionStatusResponse{
		Registration:         cloneFunctionRegistration(e.registration),
		State:                e.state,
		InFlight:             inFlight,
		LastActivityUnixNano: e.lastActivityUnixNano,
	}
}

func (e *functionEntry) startInvocation(cancel context.CancelFunc) (functionInvocation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.state != pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_READY {
		return functionInvocation{}, status.Error(codes.FailedPrecondition, "function registration is not ready")
	}
	if e.inFlight {
		return functionInvocation{}, status.Error(codes.ResourceExhausted, "function registration already has an in-flight invocation")
	}
	e.inFlight = true
	e.lastActivityUnixNano = time.Now().UnixNano()
	e.cancel = cancel
	e.done = make(chan struct{})
	return functionInvocation{
		workingDir:   e.workingDir,
		handlerFile:  e.handlerFile,
		handlerName:  e.handlerName,
		env:          cloneStringMap(e.env),
		registration: cloneFunctionRegistration(e.registration),
		done:         e.done,
	}, nil
}

func (e *functionEntry) finishInvocation(done chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done != done {
		return
	}
	e.inFlight = false
	e.cancel = nil
	e.lastActivityUnixNano = time.Now().UnixNano()
	close(done)
}

func (e *functionEntry) beginDrain(registrationID string) (functionDrain, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.registration.RegistrationId != registrationID {
		return functionDrain{}, status.Error(codes.FailedPrecondition, "function registration is stale")
	}
	e.state = pb.FunctionRegistrationState_FUNCTION_REGISTRATION_STATE_DRAINING
	return functionDrain{inFlight: e.inFlight, cancel: e.cancel, done: e.done}, nil
}

func (e *functionEntry) matchesRegistration(registrationID string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.registration.RegistrationId == registrationID
}
