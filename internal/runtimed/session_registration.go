package runtimed

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	"github.com/kruntimes/kruntimes/internal/artifact"
)

// sessionRegistrationRequest builds the Pod-local registration request for a
// Session Run. The assigned Pod UID fences stale runtimed instances after a
// Run has been reassigned.
func sessionRegistrationRequest(run *v1alpha1.Run, workingDir, artifactsDir string) (*pb.RegisterSessionRequest, error) {
	if run == nil || run.UID == "" {
		return nil, fmt.Errorf("session registration requires a Run UID")
	}
	if run.Status.AssignedPodUID == "" {
		return nil, fmt.Errorf("session registration requires an assigned Pod UID")
	}
	if workingDir == "" {
		return nil, fmt.Errorf("session registration requires a working directory")
	}
	session := run.Spec.Mode.Session
	if session == nil {
		return nil, fmt.Errorf("session registration requires session mode")
	}
	identity, err := sessionIdentityForRun(run)
	if err != nil {
		return nil, err
	}

	env := sessionRegistrationEnv(run.Spec.Env)
	if artifactsDir != "" {
		env[artifact.ArtifactsDirEnv] = artifactsDir
	}
	return &pb.RegisterSessionRequest{
		Identity:   identity,
		WorkingDir: workingDir,
		Env:        env,
	}, nil
}

func sessionIdentityForRun(run *v1alpha1.Run) (*pb.SessionIdentity, error) {
	if run == nil || run.UID == "" {
		return nil, fmt.Errorf("session identity requires a Run UID")
	}
	if run.Status.AssignedPodUID == "" {
		return nil, fmt.Errorf("session identity requires an assigned Pod UID")
	}
	if run.Spec.Mode.Session == nil {
		return nil, fmt.Errorf("session identity requires session mode")
	}
	return &pb.SessionIdentity{RunUid: string(run.UID), AssignedPodUid: run.Status.AssignedPodUID}, nil
}

func sessionRegistrationEnv(env []corev1.EnvVar) map[string]string {
	values := make(map[string]string, len(env))
	for _, variable := range env {
		values[variable.Name] = variable.Value
	}
	return values
}
