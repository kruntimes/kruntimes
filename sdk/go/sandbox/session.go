package sandbox

import (
	"context"
	"time"

	"github.com/kruntimes/kruntimes/api/runextensionapi"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// Session is one replaceable, persistent connection to a ready Sandbox. It is
// the reusable Run extension API Session type. Closing it does not terminate
// the Sandbox or release Runtime capacity.
type Session = runextensionapi.SessionExecutor

// OpenSession opens a persistent, authenticated connection to this ready
// Sandbox through the Kubernetes aggregated Session API. It does not create a
// Run or acquire capacity.
func (s *Sandbox) OpenSession(ctx context.Context) (*Session, error) {
	if s == nil || s.client == nil {
		return nil, &StateError{Message: "Sandbox is not available"}
	}
	if s.run == nil || s.run.Spec.Mode.Session == nil {
		return nil, &StateError{Message: "Session Run is not available"}
	}
	if s.run.Status.Phase != v1alpha1.RunReady {
		return nil, &StateError{Run: s.run.DeepCopy(), Message: "Session Run is not ready"}
	}
	return s.client.extension.Sessions().NewExecutor(ctx, s.run.Namespace, s.run.Name, &runextensionapi.SessionOptions{
		HeartbeatInterval: sessionHeartbeatInterval(s.run),
	})
}

func sessionHeartbeatInterval(run *v1alpha1.Run) time.Duration {
	if run == nil || run.Spec.Mode.Session == nil || run.Spec.Mode.Session.LeaseTimeoutSeconds == nil || *run.Spec.Mode.Session.LeaseTimeoutSeconds <= 0 {
		return 0
	}
	interval := time.Duration(*run.Spec.Mode.Session.LeaseTimeoutSeconds) * time.Second / 3
	return min(max(interval, 100*time.Millisecond), 30*time.Second)
}
