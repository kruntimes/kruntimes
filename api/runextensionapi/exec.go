package runextensionapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// execCancelWriteTimeout bounds the cancel frame write after the operation
	// context is done.
	execCancelWriteTimeout = 3 * time.Second
	// execCancelWaitTimeout bounds how long Exec waits for the server's terminal
	// event after the cancel frame has been sent.
	execCancelWaitTimeout = 5 * time.Second
)

// ExecOptions configures one operation executed through SessionExecutor.Exec or
// SessionsInterface.Exec.
type ExecOptions struct {
	// OnEvent receives every event for the operation in order: the accepted
	// event followed by output, progress, completed, and failed events. Send
	// consumes the accepted frame, so Exec reports a synthesized accepted event
	// that carries the operation ID and sequence 1. Exec calls OnEvent from its
	// own goroutine; returning an error stops Exec and returns that error.
	OnEvent func(OperationEvent) error
	// HeartbeatInterval is used by SessionsInterface.Exec when it opens its own
	// Session connection. It has no effect on SessionExecutor.Exec.
	HeartbeatInterval time.Duration
}

// OperationResult is the terminal result of one Session operation.
type OperationResult struct {
	// OperationID is the server-generated operation ID returned by Send.
	OperationID string
	// Command is the terminal command result. It is nil for file operations and
	// for operations that ended with a failed event.
	Command *CommandResult
}

// OperationFailedError reports a terminal failed event sent by the Runtime
// Server, including a cancellation confirmed by the server.
type OperationFailedError struct {
	Code    int32
	Message string
}

func (e *OperationFailedError) Error() string {
	return fmt.Sprintf("Session operation failed (%d): %s", e.Code, e.Message)
}

// TransportError reports a Session connection failure outside a terminal
// operation event: the WebSocket closed early, a frame could not be written, or
// the stream failed. Callers can distinguish it from a failed operation with
// errors.As. Exec never replays an operation after a TransportError.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string {
	if e.Err == nil {
		return "session transport failed"
	}
	return e.Err.Error()
}

func (e *TransportError) Unwrap() error { return e.Err }

// Exec submits one operation and consumes its event stream until a terminal
// event, a transport failure, or context cancellation.
//
// Exec reports events incrementally through ExecOptions.OnEvent and returns the
// terminal result. A non-zero command exit code or a timed-out command is a
// normal terminal result (CommandResult.ExitCode, CommandResult.TimedOut), not
// an error; a failed event returns OperationFailedError and a broken stream
// returns TransportError. A canceled context returns an error that matches
// errors.Is(err, context.Canceled) or context.DeadlineExceeded.
//
// Exec does not close the Session connection, which the caller owns. On
// cancellation it sends the operation-ID cancel frame before returning and
// waits a bounded time for the server's terminal event, so the caller can
// reuse the connection or close it safely. Exec never replays an operation.
func (s *SessionExecutor) Exec(ctx context.Context, operation Operation, options *ExecOptions) (*OperationResult, error) {
	if s == nil || s.connection == nil {
		return nil, errors.New("session connection is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var opts ExecOptions
	if options != nil {
		opts = *options
	}
	operationID, err := s.Send(ctx, operation)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, sessionCanceledError(ctxErr)
		}
		return nil, &TransportError{Err: err}
	}
	result := &OperationResult{OperationID: operationID}
	if opts.OnEvent != nil {
		accepted := OperationEvent{Sequence: 1, Type: "accepted"}
		accepted.Accepted = &struct {
			OperationID string `json:"operationID"`
		}{OperationID: operationID}
		if err := opts.OnEvent(accepted); err != nil {
			return result, err
		}
	}
	var (
		stdout, stderr []byte
		cancelOnce     sync.Once
	)
	// On cancellation the cancel frame must be sent even when the executor's own
	// context also closes the event stream, so the write is attempted from both
	// the cancellation watcher and the receive loop; cancelOnce keeps it single.
	cancelOperation := func() {
		cancelOnce.Do(func() {
			cancelCtx, cancel := context.WithTimeout(context.Background(), execCancelWriteTimeout)
			defer cancel()
			_ = s.Cancel(cancelCtx, operationID)
		})
	}
	stopCancelWatcher := make(chan struct{})
	defer close(stopCancelWatcher)
	go func() {
		select {
		case <-ctx.Done():
			cancelOperation()
		case <-stopCancelWatcher:
		}
	}()
	var cancelTimer *time.Timer
	var cancelWait <-chan time.Time
	defer func() {
		if cancelTimer != nil {
			cancelTimer.Stop()
		}
	}()
	events := s.Receive()
	ctxDone := ctx.Done()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				if ctxErr := ctx.Err(); ctxErr != nil {
					cancelOperation()
					return result, sessionCanceledError(ctxErr)
				}
				if err := s.Err(); err != nil {
					return result, &TransportError{Err: err}
				}
				return result, &TransportError{Err: errors.New("session connection closed before the operation completed")}
			}
			if opts.OnEvent != nil {
				if err := opts.OnEvent(event); err != nil {
					return result, err
				}
			}
			if output := event.Output; output != nil {
				switch output.Stream {
				case "stdout":
					stdout = append(stdout, output.Data...)
				case "stderr":
					stderr = append(stderr, output.Data...)
				}
			}
			if failed := event.Failed; failed != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					cancelOperation()
					return result, sessionCanceledError(ctxErr)
				}
				return result, &OperationFailedError{Code: failed.Code, Message: failed.Message}
			}
			if completed := event.Completed; completed != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					cancelOperation()
					return result, sessionCanceledError(ctxErr)
				}
				if command := completed.Command; command != nil {
					result.Command = command
					// Runtime Servers stream command output so the terminal event
					// stays small; prefer the incrementally accumulated bytes when
					// they exist, mirroring the runtimed log projection.
					if len(stdout) > 0 {
						result.Command.Stdout = stdout
					}
					if len(stderr) > 0 {
						result.Command.Stderr = stderr
					}
				}
				return result, nil
			}
		case <-ctxDone:
			ctxDone = nil
			cancelOperation()
			if cancelWait == nil {
				cancelTimer = time.NewTimer(execCancelWaitTimeout)
				cancelWait = cancelTimer.C
			}
		case <-cancelWait:
			return result, sessionCanceledError(ctx.Err())
		}
	}
}

// Exec opens a Session connection to a ready Session Run, executes one
// operation, and closes the connection. It is the one-command entry point for
// callers that do not manage a persistent SessionExecutor; callers that submit
// several operations keep their executor and call SessionExecutor.Exec.
//
// ExecOptions.HeartbeatInterval sets the application heartbeat for the
// connection it opens. The remaining Exec behavior is identical.
func (s SessionsInterface) Exec(ctx context.Context, namespace, runName string, operation Operation, options *ExecOptions) (*OperationResult, error) {
	var heartbeat time.Duration
	if options != nil {
		heartbeat = options.HeartbeatInterval
	}
	executor, err := s.NewExecutor(ctx, namespace, runName, &SessionOptions{HeartbeatInterval: heartbeat})
	if err != nil {
		return nil, err
	}
	defer executor.Close()
	return executor.Exec(ctx, operation, options)
}

func sessionCanceledError(err error) error {
	if err == nil {
		err = context.Canceled
	}
	return fmt.Errorf("session operation canceled: %w", err)
}
