package rleg

import (
	"context"
	"testing"
	"time"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type mockStatusProvider struct {
	states map[string]*pb.StatusResponse
	errors map[string]error
}

func (m *mockStatusProvider) Status(ctx context.Context, uid string) (*pb.StatusResponse, error) {
	if err, ok := m.errors[uid]; ok {
		return nil, err
	}
	if resp, ok := m.states[uid]; ok {
		return resp, nil
	}
	return &pb.StatusResponse{State: pb.ExecutionState_EXECUTION_STATE_RUNNING}, nil
}

func newRun(name, uid string, timeout time.Duration) *v1alpha1.Run {
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, StartTime: &metav1.Time{Time: time.Now()}},
	}
	if timeout > 0 {
		run.Spec.Timeout = &metav1.Duration{Duration: timeout}
	}
	return run
}

func TestAddAndRemoveRun(t *testing.T) {
	g := NewGenericRLEG(&mockStatusProvider{}, 0)
	run := newRun("test-run", "uid-1", 0)

	g.AddRun(run)
	g.mu.Lock()
	if _, ok := g.records["uid-1"]; !ok {
		t.Error("expected run to be added")
	}
	g.mu.Unlock()

	g.RemoveRun("uid-1")
	g.mu.Lock()
	if _, ok := g.records["uid-1"]; ok {
		t.Error("expected run to be removed")
	}
	g.mu.Unlock()
}

func TestUpdateRun_RecalculatesDeadline(t *testing.T) {
	g := NewGenericRLEG(&mockStatusProvider{}, 0)
	run := newRun("test-run", "uid-1", 0)
	g.AddRun(run)

	// Update with timeout.
	startTime := &metav1.Time{Time: time.Now()}
	run.Status.StartTime = startTime
	run.Spec.Timeout = &metav1.Duration{Duration: 30 * time.Second}
	g.UpdateRun(run)

	g.mu.Lock()
	rec := g.records["uid-1"]
	expected := startTime.Add(30 * time.Second)
	if rec.deadline.IsZero() || rec.deadline.Sub(expected) > time.Second {
		t.Errorf("expected deadline ~%v, got %v", expected, rec.deadline)
	}
	g.mu.Unlock()
}

func TestUpdateRun_NoTimeout(t *testing.T) {
	g := NewGenericRLEG(&mockStatusProvider{}, 0)
	run := newRun("test-run", "uid-1", 30*time.Second)
	g.AddRun(run)

	// Clear timeout.
	run.Spec.Timeout = nil
	g.UpdateRun(run)

	g.mu.Lock()
	if !g.records["uid-1"].deadline.IsZero() {
		t.Error("expected zero deadline after clearing timeout")
	}
	g.mu.Unlock()
}

func TestUpdateRun_UnknownRun(t *testing.T) {
	g := NewGenericRLEG(&mockStatusProvider{}, 0)
	run := newRun("test-run", "unknown", 0)
	// Should not panic.
	g.UpdateRun(run)
}

func TestRelist_StateTransition(t *testing.T) {
	provider := &mockStatusProvider{
		states: map[string]*pb.StatusResponse{
			"uid-1": {State: pb.ExecutionState_EXECUTION_STATE_SUCCEEDED},
		},
	}
	g := NewGenericRLEG(provider, 0)
	run := newRun("test-run", "uid-1", 0)
	g.AddRun(run)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	g.relist(ctx)

	select {
	case ev := <-g.Events():
		if ev.EventType != RunStateTransition {
			t.Errorf("expected StateTransition, got %s", ev.EventType)
		}
		if ev.NewState != pb.ExecutionState_EXECUTION_STATE_SUCCEEDED {
			t.Errorf("expected SUCCEEDED, got %v", ev.NewState)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected event on state transition")
	}
}

func TestRelist_Timeout(t *testing.T) {
	provider := &mockStatusProvider{}
	g := NewGenericRLEG(provider, 0)

	// Create a run that started 1 second ago with a 500ms timeout.
	pastStart := &metav1.Time{Time: time.Now().Add(-time.Second)}
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "test-run", UID: types.UID("uid-1")},
		Spec:       v1alpha1.RunSpec{Timeout: &metav1.Duration{Duration: 500 * time.Millisecond}},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunRunning, StartTime: pastStart},
	}
	g.AddRun(run)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	g.relist(ctx)

	select {
	case ev := <-g.Events():
		if ev.EventType != RunTimeout {
			t.Errorf("expected RunTimeout, got %s", ev.EventType)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("expected timeout event")
	}
}

func TestRelist_SkipsNonRunning(t *testing.T) {
	provider := &mockStatusProvider{
		states: map[string]*pb.StatusResponse{
			"uid-1": {State: pb.ExecutionState_EXECUTION_STATE_SUCCEEDED},
		},
	}
	g := NewGenericRLEG(provider, 0)

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "done-run", UID: types.UID("uid-1")},
		Status:     v1alpha1.RunStatus{Phase: v1alpha1.RunSucceeded},
	}
	g.AddRun(run)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	g.relist(ctx)

	// Should not emit any event for a non-Running run.
	select {
	case ev := <-g.Events():
		t.Errorf("unexpected event for non-Running run: %v", ev)
	default:
	}
}

func TestRelist_NoChange(t *testing.T) {
	provider := &mockStatusProvider{
		states: map[string]*pb.StatusResponse{
			"uid-1": {State: pb.ExecutionState_EXECUTION_STATE_RUNNING},
		},
	}
	g := NewGenericRLEG(provider, 0)
	run := newRun("test-run", "uid-1", 0)
	g.AddRun(run)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	g.relist(ctx)

	select {
	case ev := <-g.Events():
		t.Errorf("unexpected event when state unchanged: %v", ev)
	default:
	}
}

func TestRelist_StatusError(t *testing.T) {
	provider := &mockStatusProvider{
		errors: map[string]error{"uid-1": context.DeadlineExceeded},
	}
	g := NewGenericRLEG(provider, 0)
	run := newRun("test-run", "uid-1", 0)
	g.AddRun(run)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Should not panic on error.
	g.relist(ctx)
}

func TestHealthy(t *testing.T) {
	g := NewGenericRLEG(&mockStatusProvider{}, 0)

	// Initially unhealthy (never relisted).
	healthy, err := g.Healthy()
	if err != nil {
		t.Fatal(err)
	}
	if healthy {
		t.Error("should be unhealthy before first relist")
	}

	// Do a relist.
	ctx := context.Background()
	g.relist(ctx)

	// Now healthy.
	healthy, err = g.Healthy()
	if err != nil {
		t.Fatal(err)
	}
	if !healthy {
		t.Error("should be healthy after relist")
	}
}

func TestDefaultRelistInterval(t *testing.T) {
	g := NewGenericRLEG(&mockStatusProvider{}, 0)
	if g.relistInterval != DefaultRelistInterval {
		t.Errorf("expected default interval %v, got %v", DefaultRelistInterval, g.relistInterval)
	}
}

func TestChannelFullDropsEvent(t *testing.T) {
	provider := &mockStatusProvider{
		states: map[string]*pb.StatusResponse{
			"uid-1": {State: pb.ExecutionState_EXECUTION_STATE_FAILED},
		},
	}
	// Create with tiny channel.
	g := &GenericRLEG{
		statusProvider: provider,
		eventCh:        make(chan *RunEvent, 1),
		relistInterval: DefaultRelistInterval,
		records:        make(map[string]*runRecord),
	}
	run := newRun("test-run", "uid-1", 0)
	g.AddRun(run)

	// Fill the channel.
	g.eventCh <- &RunEvent{}

	ctx := context.Background()
	g.relist(ctx)

	// Event should have been dropped silently (not blocking).
	// Verify by draining the channel — we should only have the dummy event.
	select {
	case ev := <-g.eventCh:
		if ev.Run != nil {
			t.Error("expected dummy event, got real event")
		}
	default:
	}
}
