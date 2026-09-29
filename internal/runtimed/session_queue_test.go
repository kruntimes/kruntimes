package runtimed

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestSessionOperationQueueExecutesMutationsInFIFOOrder(t *testing.T) {
	queue := NewSessionOperationQueue(2, time.Minute)
	run := queuedSessionRun("session")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)

	go func() {
		_, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			close(firstStarted)
			<-releaseFirst
			return &pb.ExecuteSessionOperationResponse{}, nil
		})
		firstDone <- err
	}()
	<-firstStarted
	go func() {
		_, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			close(secondStarted)
			return &pb.ExecuteSessionOperationResponse{}, nil
		})
		secondDone <- err
	}()

	assertChannelOpen(t, secondStarted, "second mutation started before the first completed")
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second mutation did not start after the first completed")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
}

func TestSessionOperationQueueWaitsForAdmissionCallback(t *testing.T) {
	queue := NewSessionOperationQueue(1, time.Minute)
	run := queuedSessionRun("session-run")
	admissionStarted := make(chan struct{})
	releaseAdmission := make(chan struct{})
	executionStarted := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		_, err := queue.ExecuteWithAdmission(t.Context(), run, func() error {
			close(admissionStarted)
			<-releaseAdmission
			return nil
		}, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			close(executionStarted)
			return &pb.ExecuteSessionOperationResponse{}, nil
		})
		done <- err
	}()
	<-admissionStarted
	assertChannelOpen(t, executionStarted, "operation started before admission callback completed")
	close(releaseAdmission)
	select {
	case <-executionStarted:
	case <-time.After(time.Second):
		t.Fatal("operation did not start after admission callback completed")
	}
	if err := <-done; err != nil {
		t.Fatalf("ExecuteWithAdmission() error = %v", err)
	}
}

func TestSessionOperationQueueIdleDeadlineTracksAcceptedOperations(t *testing.T) {
	queue := NewSessionOperationQueue(1, time.Minute)
	run := queuedSessionRun("session")
	start := time.Now().Add(-time.Second)
	if err := queue.Ensure(run, start); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	timeout := time.Second
	if deadline, ok := queue.IdleDeadline(string(run.UID), timeout, time.Now()); !ok || deadline.Before(start.Add(timeout)) {
		t.Fatalf("initial idle deadline = %s, tracked=%t", deadline, ok)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			close(started)
			<-release
			return &pb.ExecuteSessionOperationResponse{}, nil
		})
		done <- err
	}()
	<-started
	if deadline, ok := queue.IdleDeadline(string(run.UID), timeout, time.Now()); !ok || deadline.Before(time.Now().Add(timeout-time.Millisecond)) {
		t.Fatalf("active idle deadline = %s, tracked=%t", deadline, ok)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Execute: %v", err)
	}
}

func TestSessionOperationQueueRejectsWhenQueuedMutationLimitIsReached(t *testing.T) {
	queue := NewSessionOperationQueue(8, time.Minute)
	run := queuedSessionRun("session")
	limit := int32(1)
	run.Spec.Mode.Session.QueueSize = &limit
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)

	go func() {
		_, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			close(started)
			<-release
			return &pb.ExecuteSessionOperationResponse{}, nil
		})
		firstDone <- err
	}()
	<-started
	go func() {
		_, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			return &pb.ExecuteSessionOperationResponse{}, nil
		})
		secondDone <- err
	}()
	waitForQueueDepth(t, queue, string(run.UID), 1)

	_, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
		return &pb.ExecuteSessionOperationResponse{}, nil
	})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("third Execute() code = %s, want %s", status.Code(err), codes.ResourceExhausted)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
}

func TestSessionOperationQueueCloseCancelsActiveAndQueuedMutations(t *testing.T) {
	queue := NewSessionOperationQueue(2, time.Minute)
	run := queuedSessionRun("session")
	started := make(chan struct{})
	queuedExecuted := make(chan struct{}, 1)
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)

	go func() {
		_, err := queue.Execute(t.Context(), run, func(ctx context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		firstDone <- err
	}()
	<-started
	go func() {
		_, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			queuedExecuted <- struct{}{}
			return nil, nil
		})
		secondDone <- err
	}()
	waitForQueueDepth(t, queue, string(run.UID), 1)

	queue.Close(string(run.UID))
	firstErr := <-firstDone
	if status.Code(firstErr) != codes.Canceled {
		t.Fatalf("active Execute() code = %s, want %s", status.Code(firstErr), codes.Canceled)
	}
	secondErr := <-secondDone
	if status.Code(secondErr) != codes.Canceled {
		t.Fatalf("queued Execute() code = %s, want %s", status.Code(secondErr), codes.Canceled)
	}
	select {
	case <-queuedExecuted:
		t.Fatal("queued mutation executed after session close")
	default:
	}
}

func TestSessionOperationQueueDrainCompletesAcceptedMutations(t *testing.T) {
	queue := NewSessionOperationQueue(2, time.Minute)
	run := queuedSessionRun("session")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)

	go func() {
		_, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			close(firstStarted)
			<-releaseFirst
			return &pb.ExecuteSessionOperationResponse{}, nil
		})
		firstDone <- err
	}()
	<-firstStarted
	go func() {
		_, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
			close(secondStarted)
			return &pb.ExecuteSessionOperationResponse{}, nil
		})
		secondDone <- err
	}()
	waitForQueueDepth(t, queue, string(run.UID), 1)

	if queue.Drain(string(run.UID)) {
		t.Fatal("Drain() = true with accepted operations, want false")
	}
	if _, err := queue.Execute(t.Context(), run, func(context.Context) (*pb.ExecuteSessionOperationResponse, error) {
		return &pb.ExecuteSessionOperationResponse{}, nil
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Execute() after Drain code = %s, want %s", status.Code(err), codes.FailedPrecondition)
	}

	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Execute() error = %v", err)
	}
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("queued mutation did not execute while draining")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	if !queue.Drain(string(run.UID)) {
		t.Fatal("Drain() = false after accepted operations completed, want true")
	}
}

func TestSessionOperationQueueAppliesSessionOperationTimeout(t *testing.T) {
	queue := NewSessionOperationQueue(1, time.Minute)
	run := queuedSessionRun("session")
	run.Spec.Mode.Session.OperationTimeout = &metav1.Duration{Duration: 10 * time.Millisecond}

	_, err := queue.Execute(t.Context(), run, func(ctx context.Context) (*pb.ExecuteSessionOperationResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Execute() code = %s, want %s", status.Code(err), codes.DeadlineExceeded)
	}
}

func queuedSessionRun(uid string) *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID(uid)},
		Spec:       v1alpha1.RunSpec{Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
}

func waitForQueueDepth(t *testing.T, queue *SessionOperationQueue, uid string, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		queue.mu.Lock()
		entry := queue.sessions[uid]
		depth := 0
		if entry != nil {
			depth = len(entry.jobs)
		}
		queue.mu.Unlock()
		if depth == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue depth did not become %d", want)
}

func assertChannelOpen(t *testing.T, ch <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(message)
	case <-time.After(20 * time.Millisecond):
	}
}
