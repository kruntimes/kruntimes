package runtimed

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	defaultSessionQueueSize        = 32
	defaultSessionOperationTimeout = 5 * time.Minute
)

// SessionOperationQueue serializes mutating operations for each Session Run.
// The owning runtimed is its sole caller. A Run-specific limit can only reduce
// the process-wide queue and timeout limits supplied at construction.
type SessionOperationQueue struct {
	mu                  sync.Mutex
	maxQueueSize        int
	maxOperationTimeout time.Duration
	sessions            map[string]*sessionOperationQueueEntry
}

type sessionOperationQueueEntry struct {
	jobs     chan sessionOperationJob
	closed   bool
	draining bool

	mu           sync.Mutex
	activeCancel context.CancelFunc
	lastActivity time.Time
	pending      int
}

// Ensure starts tracking a ready Session Run before its first operation.
func (q *SessionOperationQueue) Ensure(run *v1alpha1.Run, activity time.Time) error {
	if q == nil {
		return fmt.Errorf("session operation queue is not configured")
	}
	uid, queueSize, timeout, err := q.limits(run)
	if err != nil {
		return err
	}
	if activity.IsZero() {
		activity = time.Now()
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if entry := q.sessions[uid]; entry != nil {
		entry.setActivity(activity)
		return nil
	}
	entry := &sessionOperationQueueEntry{jobs: make(chan sessionOperationJob, queueSize), lastActivity: activity}
	q.sessions[uid] = entry
	go entry.run(timeout)
	return nil
}

// IdleDeadline returns the next idle expiry for a tracked Session Run. Active
// or queued operations keep the session active until they finish.
func (q *SessionOperationQueue) IdleDeadline(runUID string, timeout time.Duration, now time.Time) (time.Time, bool) {
	if q == nil || runUID == "" || timeout <= 0 {
		return time.Time{}, false
	}
	q.mu.Lock()
	entry := q.sessions[runUID]
	q.mu.Unlock()
	if entry == nil {
		return time.Time{}, false
	}
	return entry.idleDeadline(timeout, now), true
}

type sessionOperationJob struct {
	ctx      context.Context
	admitted chan struct{}
	execute  func(context.Context) (*pb.ExecuteSessionOperationResponse, error)
	result   chan sessionOperationResult
}

type sessionOperationResult struct {
	response *pb.ExecuteSessionOperationResponse
	err      error
}

// NewSessionOperationQueue creates the per-runtimed operation queue manager.
// Non-positive limits use the v0 defaults.
func NewSessionOperationQueue(maxQueueSize int, maxOperationTimeout time.Duration) *SessionOperationQueue {
	if maxQueueSize <= 0 {
		maxQueueSize = defaultSessionQueueSize
	}
	if maxOperationTimeout <= 0 {
		maxOperationTimeout = defaultSessionOperationTimeout
	}
	return &SessionOperationQueue{
		maxQueueSize:        maxQueueSize,
		maxOperationTimeout: maxOperationTimeout,
		sessions:            make(map[string]*sessionOperationQueueEntry),
	}
}

// Execute waits for the Session Run's FIFO turn, then invokes execute with the
// effective per-operation deadline. The caller's context can cancel work while
// it is queued or running.
func (q *SessionOperationQueue) Execute(
	ctx context.Context,
	run *v1alpha1.Run,
	execute func(context.Context) (*pb.ExecuteSessionOperationResponse, error),
) (*pb.ExecuteSessionOperationResponse, error) {
	return q.ExecuteWithAdmission(ctx, run, nil, execute)
}

// ExecuteWithAdmission is Execute with a callback that runs immediately after
// the operation has been admitted to the Session FIFO queue. The callback runs
// outside the queue mutex, but before this call waits for the operation result.
func (q *SessionOperationQueue) ExecuteWithAdmission(
	ctx context.Context,
	run *v1alpha1.Run,
	onAdmitted func() error,
	execute func(context.Context) (*pb.ExecuteSessionOperationResponse, error),
) (*pb.ExecuteSessionOperationResponse, error) {
	if q == nil {
		return nil, status.Error(codes.FailedPrecondition, "Session operation queue is not configured")
	}
	if ctx == nil {
		return nil, status.Error(codes.InvalidArgument, "operation context is required")
	}
	if execute == nil {
		return nil, status.Error(codes.InvalidArgument, "session operation is required")
	}
	uid, queueSize, timeout, err := q.limits(run)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	q.mu.Lock()
	entry := q.sessions[uid]
	if entry == nil {
		entry = &sessionOperationQueueEntry{jobs: make(chan sessionOperationJob, queueSize), lastActivity: time.Now()}
		q.sessions[uid] = entry
		go entry.run(timeout)
	}
	if !entry.accepting() {
		q.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "session is finalizing")
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	job := sessionOperationJob{
		ctx:      jobCtx,
		admitted: make(chan struct{}),
		execute:  execute,
		result:   make(chan sessionOperationResult, 1),
	}
	entry.accept(time.Now())
	select {
	case entry.jobs <- job:
		q.mu.Unlock()
		var admissionErr error
		if onAdmitted != nil {
			admissionErr = onAdmitted()
		}
		if admissionErr != nil {
			cancel()
		}
		close(job.admitted)
		if admissionErr != nil {
			return nil, admissionErr
		}
	default:
		entry.complete(time.Now())
		q.mu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "session operation queue is full")
	}

	select {
	case result := <-job.result:
		return result.response, result.err
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

// Drain prevents new operations for a Session Run while allowing operations
// already accepted into its FIFO queue to finish. It reports whether there is
// no accepted work remaining. A missing queue entry is already drained.
func (q *SessionOperationQueue) Drain(runUID string) bool {
	if q == nil || runUID == "" {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	entry := q.sessions[runUID]
	if entry == nil {
		return true
	}
	return entry.beginDrain()
}

// Close prevents new work for a Session Run, cancels the active operation, and
// rejects each queued operation. It is idempotent and is called before runtimed
// closes the corresponding Runtime Server session.
func (q *SessionOperationQueue) Close(runUID string) {
	if q == nil || runUID == "" {
		return
	}
	q.mu.Lock()
	entry := q.sessions[runUID]
	if entry == nil || entry.isClosed() {
		q.mu.Unlock()
		return
	}
	entry.markClosed()
	delete(q.sessions, runUID)
	close(entry.jobs)
	q.mu.Unlock()
	entry.cancelActive()
}

func (q *SessionOperationQueue) limits(run *v1alpha1.Run) (string, int, time.Duration, error) {
	if run == nil || run.UID == "" || run.Spec.Mode.Session == nil {
		return "", 0, 0, fmt.Errorf("session Run is required")
	}
	queueSize := q.maxQueueSize
	if limit := run.Spec.Mode.Session.QueueSize; limit != nil {
		if *limit <= 0 {
			return "", 0, 0, fmt.Errorf("session queue size must be positive")
		}
		queueSize = min(queueSize, int(*limit))
	}
	timeout := q.maxOperationTimeout
	if limit := run.Spec.Mode.Session.OperationTimeout; limit != nil {
		if limit.Duration <= 0 {
			return "", 0, 0, fmt.Errorf("session operation timeout must be positive")
		}
		timeout = min(timeout, limit.Duration)
	}
	return string(run.UID), queueSize, timeout, nil
}

func (e *sessionOperationQueueEntry) run(timeout time.Duration) {
	for job := range e.jobs {
		select {
		case <-job.admitted:
		case <-job.ctx.Done():
			e.deliver(job, nil, status.FromContextError(job.ctx.Err()).Err())
			e.complete(time.Now())
			continue
		}
		if e.isClosed() {
			e.deliver(job, nil, status.Error(codes.Canceled, "session closed"))
			e.complete(time.Now())
			continue
		}
		select {
		case <-job.ctx.Done():
			e.deliver(job, nil, status.FromContextError(job.ctx.Err()).Err())
			e.complete(time.Now())
			continue
		default:
		}

		operationCtx, cancel := context.WithTimeout(job.ctx, timeout)
		if !e.setActive(cancel) {
			cancel()
			e.deliver(job, nil, status.Error(codes.Canceled, "session closed"))
			e.complete(time.Now())
			continue
		}
		response, err := job.execute(operationCtx)
		operationErr := operationCtx.Err()
		cancel()
		e.clearActive()
		if operationErr != nil && (err == nil || errors.Is(err, operationErr)) {
			if response == nil || response.GetCommand() == nil || !response.GetCommand().GetTimedOut() {
				err = status.FromContextError(operationErr).Err()
			}
		}
		e.deliver(job, response, err)
		e.complete(time.Now())
	}
}

func (e *sessionOperationQueueEntry) setActivity(activity time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lastActivity.Before(activity) {
		e.lastActivity = activity
	}
}

func (e *sessionOperationQueueEntry) accept(activity time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pending++
	e.lastActivity = activity
}

func (e *sessionOperationQueueEntry) complete(activity time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending > 0 {
		e.pending--
	}
	e.lastActivity = activity
}

func (e *sessionOperationQueueEntry) idleDeadline(timeout time.Duration, now time.Time) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending > 0 {
		return now.Add(timeout)
	}
	return e.lastActivity.Add(timeout)
}

func (e *sessionOperationQueueEntry) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

func (e *sessionOperationQueueEntry) accepting() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.closed && !e.draining
}

func (e *sessionOperationQueueEntry) beginDrain() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.draining = true
	return e.pending == 0
}

func (e *sessionOperationQueueEntry) markClosed() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
}

func (e *sessionOperationQueueEntry) setActive(cancel context.CancelFunc) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	e.activeCancel = cancel
	return true
}

func (e *sessionOperationQueueEntry) clearActive() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.activeCancel = nil
}

func (e *sessionOperationQueueEntry) cancelActive() {
	e.mu.Lock()
	cancel := e.activeCancel
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (e *sessionOperationQueueEntry) deliver(job sessionOperationJob, response *pb.ExecuteSessionOperationResponse, err error) {
	job.result <- sessionOperationResult{response: response, err: err}
}
