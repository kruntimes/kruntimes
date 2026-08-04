package controller

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

var exprPattern = regexp.MustCompile(`\$\{\{\s*(.+?)\s*\}\}`)

// resolveContext holds the current state for expression resolution.
type resolveContext struct {
	inputs map[string]string            // resolved reusable Workflow or Action inputs
	steps  map[string]map[string]string // job's step outputs: stepName -> outputs
	jobs   map[string]map[string]string // completed job outputs: jobName -> outputs
}

// resolveExpr replaces ${{ }} expressions in s.
func resolveExpr(s string, ctx *resolveContext) (string, error) {
	var err error
	result := exprPattern.ReplaceAllStringFunc(s, func(match string) string {
		inner := exprPattern.FindStringSubmatch(match)[1]
		val, e := resolveRef(inner, ctx)
		if e != nil {
			err = e
		}
		return val
	})
	return result, err
}

// resolveRef resolves a single reference path like "steps.build.outputs.artifact"
func resolveRef(path string, ctx *resolveContext) (string, error) {
	parts := strings.SplitN(path, ".", 4)

	switch parts[0] {
	case "inputs":
		if len(parts) != 2 {
			return "", fmt.Errorf("invalid inputs ref: %s (expected inputs.<name>)", path)
		}
		if ctx.inputs == nil {
			return "", fmt.Errorf("no inputs available")
		}
		value, ok := ctx.inputs[parts[1]]
		if !ok {
			return "", fmt.Errorf("no input %q", parts[1])
		}
		return value, nil

	case "steps":
		if len(parts) != 4 || parts[2] != "outputs" {
			return "", fmt.Errorf("invalid steps ref: %s (expected steps.<name>.outputs.<key>)", path)
		}
		return resolveMap(ctx.steps, parts[1], parts[3])

	case "jobs":
		if len(parts) != 4 || parts[2] != "outputs" {
			return "", fmt.Errorf("invalid jobs ref: %s (expected jobs.<name>.outputs.<key>)", path)
		}
		return resolveMap(ctx.jobs, parts[1], parts[3])

	default:
		return "", fmt.Errorf("unknown ref prefix: %s (expected inputs, steps, or jobs)", parts[0])
	}
}

// resolveStepExecution resolves expressions in the fields carried into a
// child Run. It intentionally leaves Action-call fields for Action expansion.
func resolveStepExecution(step v1alpha1.StepSpec, ctx *resolveContext) (v1alpha1.StepSpec, error) {
	resolved := *step.DeepCopy()
	var err error
	if resolved.Run, err = resolveExpr(resolved.Run, ctx); err != nil {
		return v1alpha1.StepSpec{}, fmt.Errorf("run: %w", err)
	}
	if resolved.Args, err = resolveStepArgs(resolved.Args, ctx); err != nil {
		return v1alpha1.StepSpec{}, err
	}
	if resolved.Env, err = resolveEnv(resolved.Env, ctx); err != nil {
		return v1alpha1.StepSpec{}, err
	}
	return resolved, nil
}

func resolveMap(m map[string]map[string]string, name, key string) (string, error) {
	if m == nil {
		return "", fmt.Errorf("no outputs available")
	}
	outputs, ok := m[name]
	if !ok {
		return "", fmt.Errorf("no outputs for %s", name)
	}
	val, ok := outputs[key]
	if !ok {
		return "", fmt.Errorf("no output %q in %s", key, name)
	}
	return val, nil
}

// resolveStepArgs resolves all expressions in step args.
func resolveStepArgs(args []string, ctx *resolveContext) ([]string, error) {
	result := make([]string, len(args))
	for i, a := range args {
		resolved, err := resolveExpr(a, ctx)
		if err != nil {
			return nil, fmt.Errorf("arg[%d]: %w", i, err)
		}
		result[i] = resolved
	}
	return result, nil
}

// resolveEnv resolves all expressions in env values.
func resolveEnv(env map[string]string, ctx *resolveContext) (map[string]string, error) {
	result := make(map[string]string, len(env))
	for k, v := range env {
		resolved, err := resolveExpr(v, ctx)
		if err != nil {
			return nil, fmt.Errorf("env[%s]: %w", k, err)
		}
		result[k] = resolved
	}
	return result, nil
}
