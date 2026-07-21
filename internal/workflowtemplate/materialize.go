// Package workflowtemplate binds reusable Workflow inputs into a concrete
// WorkflowRun job definition.
package workflowtemplate

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

var inputExpression = regexp.MustCompile(`\$\{\{\s*inputs\.([A-Za-z0-9_.-]+)\s*\}\}`)

// Materialize validates input values and renders them into a copy of the
// Workflow jobs. Other expressions, such as jobs.* outputs, remain intact for
// the WorkflowRun controller to resolve when their dependencies complete.
func Materialize(spec v1alpha1.WorkflowSpec, values map[string]string) (map[string]v1alpha1.JobSpec, error) {
	inputs, err := BindInputs(spec.Inputs, values)
	if err != nil {
		return nil, err
	}

	jobs := make(map[string]v1alpha1.JobSpec, len(spec.Jobs))
	for name, job := range spec.Jobs {
		rendered := *job.DeepCopy()
		rendered.Uses, err = renderInputs(rendered.Uses, inputs)
		if err != nil {
			return nil, fmt.Errorf("job %q uses: %w", name, err)
		}
		if rendered.With, err = renderStringMap(rendered.With, inputs); err != nil {
			return nil, fmt.Errorf("job %q with: %w", name, err)
		}
		if rendered.Outputs, err = renderStringMap(rendered.Outputs, inputs); err != nil {
			return nil, fmt.Errorf("job %q outputs: %w", name, err)
		}
		for i := range rendered.Steps {
			step := &rendered.Steps[i]
			if step.Run, err = renderInputs(step.Run, inputs); err != nil {
				return nil, fmt.Errorf("job %q step %q run: %w", name, step.Name, err)
			}
			if step.Args, err = renderStrings(step.Args, inputs); err != nil {
				return nil, fmt.Errorf("job %q step %q args: %w", name, step.Name, err)
			}
			if step.Env, err = renderStringMap(step.Env, inputs); err != nil {
				return nil, fmt.Errorf("job %q step %q env: %w", name, step.Name, err)
			}
			if step.With, err = renderStringMap(step.With, inputs); err != nil {
				return nil, fmt.Errorf("job %q step %q with: %w", name, step.Name, err)
			}
		}
		jobs[name] = rendered
	}
	return jobs, nil
}

// BindInputs applies declared defaults, rejects unknown values, and verifies
// that every required input is present.
func BindInputs(inputs map[string]v1alpha1.WorkflowInputSpec, values map[string]string) (map[string]string, error) {
	for name := range values {
		if _, ok := inputs[name]; !ok {
			return nil, fmt.Errorf("unknown input %q", name)
		}
	}

	bound := make(map[string]string, len(inputs))
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		input := inputs[name]
		value, set := values[name]
		if !set {
			value = input.Default
		}
		if value == "" && input.Required && !set && input.Default == "" {
			return nil, fmt.Errorf("missing required input %q", name)
		}
		bound[name] = value
	}
	return bound, nil
}

func renderInputs(value string, inputs map[string]string) (string, error) {
	var renderErr error
	rendered := inputExpression.ReplaceAllStringFunc(value, func(match string) string {
		name := inputExpression.FindStringSubmatch(match)[1]
		input, ok := inputs[name]
		if !ok {
			renderErr = fmt.Errorf("unknown input %q", name)
			return ""
		}
		return input
	})
	if renderErr != nil {
		return "", renderErr
	}
	return rendered, nil
}

func renderStrings(values []string, inputs map[string]string) ([]string, error) {
	if values == nil {
		return nil, nil
	}
	rendered := make([]string, len(values))
	for i, value := range values {
		var err error
		rendered[i], err = renderInputs(value, inputs)
		if err != nil {
			return nil, err
		}
	}
	return rendered, nil
}

func renderStringMap(values map[string]string, inputs map[string]string) (map[string]string, error) {
	if values == nil {
		return nil, nil
	}
	rendered := make(map[string]string, len(values))
	for name, value := range values {
		resolved, err := renderInputs(value, inputs)
		if err != nil {
			return nil, err
		}
		rendered[name] = resolved
	}
	return rendered, nil
}
