package runtimed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	pb "github.com/kruntimes/kruntimes/api/runtime/v1"
	"github.com/kruntimes/kruntimes/api/v1alpha1"
	runretry "github.com/kruntimes/kruntimes/internal/retry"
	corev1 "k8s.io/api/core/v1"
)

// functionRegistrationRequest builds the idempotent, Pod-local registration
// request for the current function Run attempt. The digest deliberately omits
// workingDir because that path may change when the same immutable Run input is
// assigned to another Runtime Pod.
func functionRegistrationRequest(run *v1alpha1.Run, workingDir string) (*pb.RegisterFunctionRequest, error) {
	if run == nil || run.UID == "" {
		return nil, fmt.Errorf("function registration requires a Run UID")
	}
	if workingDir == "" {
		return nil, fmt.Errorf("function registration requires a working directory")
	}
	function := run.Spec.Mode.Function
	if function == nil {
		return nil, fmt.Errorf("function registration requires function mode")
	}
	if function.Handler == "" {
		return nil, fmt.Errorf("function registration requires a handler")
	}

	digest, err := functionRegistrationDigest(run)
	if err != nil {
		return nil, err
	}

	var idleTimeoutSeconds int64
	if function.IdleTimeoutSeconds != nil {
		idleTimeoutSeconds = int64(*function.IdleTimeoutSeconds)
	}
	return &pb.RegisterFunctionRequest{
		RunUid:              string(run.UID),
		RegistrationAttempt: runretry.CurrentAttempt(run.Status.Attempt),
		WorkingDir:          workingDir,
		Handler:             function.Handler,
		Env:                 functionRegistrationEnv(run.Spec.Env),
		IdleTimeoutSeconds:  idleTimeoutSeconds,
		RegistrationDigest:  digest,
	}, nil
}

// functionRegistrationDigest is stable for retries of the same immutable Run
// input and changes when any input visible to FunctionRuntime changes.
func functionRegistrationDigest(run *v1alpha1.Run) (string, error) {
	function := run.Spec.Mode.Function
	if function == nil {
		return "", fmt.Errorf("function registration requires function mode")
	}

	identity := struct {
		Source             *v1alpha1.CodeSource `json:"source,omitempty"`
		Handler            string               `json:"handler"`
		Env                map[string]string    `json:"env,omitempty"`
		IdleTimeoutSeconds *int32               `json:"idleTimeoutSeconds,omitempty"`
	}{
		Source:             run.Spec.Source,
		Handler:            function.Handler,
		Env:                functionRegistrationEnv(run.Spec.Env),
		IdleTimeoutSeconds: function.IdleTimeoutSeconds,
	}
	canonical, err := json.Marshal(identity)
	if err != nil {
		return "", fmt.Errorf("marshal function registration inputs: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func functionRegistrationEnv(env []corev1.EnvVar) map[string]string {
	values := make(map[string]string, len(env))
	for _, variable := range env {
		values[variable.Name] = variable.Value
	}
	return values
}
