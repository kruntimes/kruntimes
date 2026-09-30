package sandbox

import (
	"context"
	"errors"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// Runtime is one reusable Session Runtime pool. Acquiring a Sandbox from it is
// the capacity-acquisition operation: it creates one Session-mode Run and
// returns only after the Run is registered and ready.
type Runtime struct {
	client    *Client
	namespace string
	name      string
}

// Runtime returns the named Runtime pool in namespace.
func (c *Client) Runtime(namespace, name string) *Runtime {
	return &Runtime{client: c, namespace: namespace, name: name}
}

// AcquireOptions selects the Session Run created for an acquired Sandbox.
// Runtime and Namespace are supplied by Runtime.AcquireSandbox.
type AcquireOptions struct {
	Name           string
	GenerateName   string
	Source         *v1alpha1.CodeSource
	ArtifactInputs []v1alpha1.ArtifactInput
	Env            map[string]string
	Timeout        *metav1.Duration
	Session        *v1alpha1.RunSessionMode
}

// AcquireSandbox creates and waits for one ready Session Run in this Runtime
// pool. It never returns a Sandbox that has not acquired Runtime capacity.
func (r *Runtime) AcquireSandbox(ctx context.Context, options AcquireOptions) (*Sandbox, error) {
	if r == nil || r.client == nil {
		return nil, errors.New("Sandbox Runtime is not configured")
	}
	if r.namespace == "" || r.name == "" {
		return nil, errors.New("Sandbox Runtime namespace and name are required")
	}
	sandbox, err := r.client.create(ctx, r.namespace, r.name, options)
	if err != nil {
		return nil, err
	}
	if err := sandbox.Wait(ctx); err != nil {
		return nil, fmt.Errorf("acquire Sandbox: %w", err)
	}
	return sandbox, nil
}

// Release terminates the Session gracefully, waits for cleanup, then deletes
// the backing Session Run. It is the capacity-release operation.
func (s *Sandbox) Release(ctx context.Context) error {
	if s == nil || s.client == nil || s.run == nil {
		return &StateError{Message: "Sandbox is not available"}
	}
	if err := s.Close(ctx); err != nil {
		return err
	}
	if err := s.client.runs.Delete(ctx, s.run); err != nil && client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("delete Session Run: %w", err)
	}
	return nil
}
