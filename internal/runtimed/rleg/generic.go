package rleg

import (
	"context"
	"sync"
	"time"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"k8s.io/klog/v2"
)

const (
	// DefaultRelistInterval is the default polling interval.
	DefaultRelistInterval = 500 * time.Millisecond
	// EventChannelSize is the buffer size for the event channel.
	EventChannelSize = 256
	// StatusTimeout is the per-run gRPC Status call timeout.
	StatusTimeout = 5 * time.Second
	// HealthThreshold is the multiplier of relistInterval after which
	// the generator is considered unhealthy.
	HealthThreshold = 3
)

// runRecord holds the cached state for a single monitored run.
type runRecord struct {
	run       *v1alpha1.Run
	lastState pb.ExecutionState
	deadline  time.Time
}

// GenericRLEG is the default implementation of RunLifecycleEventGenerator.
// It periodically polls the runtime server via StatusProvider, compares
// current state against cached state, and emits events on transitions.
type GenericRLEG struct {
	statusProvider StatusProvider
	eventCh        chan *RunEvent
	relistInterval time.Duration

	mu         sync.Mutex
	records    map[string]*runRecord
	lastRelist time.Time
}

// NewGenericRLEG creates a new GenericRLEG.
func NewGenericRLEG(provider StatusProvider, relistInterval time.Duration) *GenericRLEG {
	if relistInterval <= 0 {
		relistInterval = DefaultRelistInterval
	}
	return &GenericRLEG{
		statusProvider: provider,
		eventCh:        make(chan *RunEvent, EventChannelSize),
		relistInterval: relistInterval,
		records:        make(map[string]*runRecord),
	}
}

// Start begins the periodic relist loop. Blocks until ctx is cancelled.
func (g *GenericRLEG) Start(ctx context.Context) {
	ticker := time.NewTicker(g.relistInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.relist(ctx)
		}
	}
}

// Events returns the event channel. Consumers should range over this channel.
func (g *GenericRLEG) Events() <-chan *RunEvent {
	return g.eventCh
}

// Healthy reports whether relists are happening on schedule.
func (g *GenericRLEG) Healthy() (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return time.Since(g.lastRelist) < g.relistInterval*HealthThreshold, nil
}

// AddRun adds a run to the monitored set.
func (g *GenericRLEG) AddRun(run *v1alpha1.Run) {
	g.mu.Lock()
	defer g.mu.Unlock()

	uid := string(run.UID)
	var deadline time.Time
	if run.Spec.Timeout != nil && run.Status.StartTime != nil {
		deadline = run.Status.StartTime.Add(run.Spec.Timeout.Duration)
	}
	g.records[uid] = &runRecord{
		run:       run,
		lastState: pb.ExecutionState_EXECUTION_STATE_RUNNING,
		deadline:  deadline,
	}
}

// RemoveRun stops monitoring a run.
func (g *GenericRLEG) RemoveRun(uid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.records, uid)
}

// UpdateRun refreshes the cached run object and recalculates its deadline.
func (g *GenericRLEG) UpdateRun(run *v1alpha1.Run) {
	g.mu.Lock()
	defer g.mu.Unlock()

	uid := string(run.UID)
	rec, ok := g.records[uid]
	if !ok {
		return
	}
	rec.run = run
	if run.Spec.Timeout != nil && run.Status.StartTime != nil {
		rec.deadline = run.Status.StartTime.Add(run.Spec.Timeout.Duration)
	} else {
		rec.deadline = time.Time{}
	}
}

// relist iterates all monitored runs, polls status, and emits events.
func (g *GenericRLEG) relist(ctx context.Context) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.lastRelist = time.Now()

	for uid, rec := range g.records {
		if rec.run.Status.Phase != v1alpha1.RunRunning {
			continue
		}

		// Check deadline.
		if !rec.deadline.IsZero() && time.Now().After(rec.deadline) {
			g.emit(&RunEvent{
				Run:       rec.run,
				EventType: RunTimeout,
				OldState:  rec.lastState,
			})
			continue
		}

		// Poll runtime status.
		rctx, cancel := context.WithTimeout(ctx, StatusTimeout)
		resp, err := g.statusProvider.Status(rctx, uid)
		cancel()
		if err != nil {
			klog.V(4).Infof("RLEG Status(%s): %v", uid, err)
			continue
		}

		if resp.State != rec.lastState {
			g.emit(&RunEvent{
				Run:       rec.run,
				EventType: RunStateTransition,
				OldState:  rec.lastState,
				NewState:  resp.State,
			})
			rec.lastState = resp.State
		}
	}
}

// emit sends an event to the channel, dropping it if the channel is full.
func (g *GenericRLEG) emit(ev *RunEvent) {
	select {
	case g.eventCh <- ev:
	default:
		klog.V(2).Infof("RLEG event channel full, dropping event for %s", ev.Run.Name)
	}
}
