package controller

import (
	"fmt"
	"maps"
	"strings"

	v1alpha1 "github.com/kruntimes/kruntimes/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// workflowEnvironmentOutputs extracts the reserved output namespace used to
// carry environment changes between sequential steps. Run status is the
// durable transport; the controller must never read a Runtime Pod filesystem.
func workflowEnvironmentOutputs(outputs map[string]string) (map[string]string, error) {
	environment := make(map[string]string)
	for key, value := range outputs {
		name, ok := strings.CutPrefix(key, v1alpha1.WorkflowEnvironmentOutputPrefix)
		if !ok {
			continue
		}
		if errs := validation.IsCIdentifier(name); len(errs) > 0 {
			return nil, fmt.Errorf("reserved environment output %q has invalid variable name: %s", key, strings.Join(errs, ", "))
		}
		environment[name] = value
	}
	return environment, nil
}

func mergeWorkflowEnvironment(environment map[string]string, outputs map[string]string) error {
	updates, err := workflowEnvironmentOutputs(outputs)
	if err != nil {
		return err
	}
	for name, value := range updates {
		environment[name] = value
	}
	return nil
}

// workflowStepEnvironment returns the environment accumulated by completed
// steps before target. For an Action, completed inner steps also contribute to
// the next inner step. The source is WorkflowRun status so reconciliation is
// restart-safe and never depends on Runtime Pod-local files.
func workflowStepEnvironment(status v1alpha1.JobStatus, target jobStepRunTarget) (map[string]string, error) {
	environment := make(map[string]string)
	for index := 0; index < target.stepIndex && index < len(status.Steps); index++ {
		step := status.Steps[index]
		if step.Phase != v1alpha1.StepSucceeded {
			continue
		}
		if err := mergeWorkflowEnvironment(environment, step.Outputs); err != nil {
			return nil, err
		}
	}

	if target.actionStepIndex == 0 || target.stepIndex >= len(status.Steps) {
		return environment, nil
	}
	for index := 0; index < target.actionStepIndex-1 && index < len(status.Steps[target.stepIndex].ActionSteps); index++ {
		step := status.Steps[target.stepIndex].ActionSteps[index]
		if step.Phase != v1alpha1.StepSucceeded {
			continue
		}
		if err := mergeWorkflowEnvironment(environment, step.Outputs); err != nil {
			return nil, err
		}
	}
	return environment, nil
}

// publicWorkflowOutputs excludes controller-reserved state from the normal
// expression context, so ${{ steps.<step>.outputs.* }} remains an application
// output contract.
func publicWorkflowOutputs(outputs map[string]string) map[string]string {
	if len(outputs) == 0 {
		return nil
	}
	public := maps.Clone(outputs)
	for key := range public {
		if strings.HasPrefix(key, v1alpha1.WorkflowEnvironmentOutputPrefix) {
			delete(public, key)
		}
	}
	if len(public) == 0 {
		return nil
	}
	return public
}
