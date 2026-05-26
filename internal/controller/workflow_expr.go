package controller

import (
	"fmt"
	"regexp"
	"strings"
)

var exprPattern = regexp.MustCompile(`\$\{\{\s*(.+?)\s*\}\}`)

// resolveContext holds the current state for expression resolution.
type resolveContext struct {
	steps map[string]map[string]string // job's step outputs: stepName -> outputs
	jobs  map[string]map[string]string // completed job outputs: jobName -> outputs
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
		return "", fmt.Errorf("unknown ref prefix: %s (expected steps or jobs)", parts[0])
	}
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
