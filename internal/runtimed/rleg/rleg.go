// Package rleg implements the Run Lifecycle Event Generator, inspired by
// kubelet's Pod Lifecycle Event Generator (PLEG). It periodically polls the
// runtime server for execution status and emits events on state transitions.
package rleg

import (
	"context"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// RunEventType classifies the kind of lifecycle event.
type RunEventType string

const (
	// RunStateTransition indicates a run's execution state changed
	// (e.g. Running → Succeeded).
	RunStateTransition RunEventType = "StateTransition"
	// RunTimeout indicates a run exceeded its deadline.
	RunTimeout RunEventType = "RunTimeout"
	// RunExecutionLost indicates the runtime no longer knows the execution.
	RunExecutionLost RunEventType = "ExecutionLost"
)

// RunEvent is emitted when a run's lifecycle state changes.
type RunEvent struct {
	Run       *v1alpha1.Run
	EventType RunEventType
	OldState  pb.ExecutionState
	NewState  pb.ExecutionState
}

// StatusProvider abstracts the gRPC Status call to the runtime server.
type StatusProvider interface {
	Status(ctx context.Context, uid string) (*pb.StatusResponse, error)
}

// RunLifecycleEventGenerator is the interface for run lifecycle event generation.
type RunLifecycleEventGenerator interface {
	// Start begins the periodic relist loop. Blocks until ctx is cancelled.
	Start(ctx context.Context)

	// Events returns the channel on which lifecycle events are delivered.
	Events() <-chan *RunEvent

	// Healthy reports whether the generator is performing relists on schedule.
	Healthy() (bool, error)

	// AddRun adds a run to the set being monitored.
	AddRun(run *v1alpha1.Run)

	// RemoveRun stops monitoring a run.
	RemoveRun(uid string)

	// UpdateRun refreshes the cached run object (e.g. after spec changes).
	UpdateRun(run *v1alpha1.Run)
}
