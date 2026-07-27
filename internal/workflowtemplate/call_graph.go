package workflowtemplate

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

// MaxCallDepth bounds a reusable Workflow root and its nested Workflow calls.
const MaxCallDepth = 8

// WorkflowLookup loads a namespace-local reusable Workflow by name.
type WorkflowLookup func(context.Context, string) (*v1alpha1.Workflow, error)

// ValidateCallGraph verifies that the reusable Workflows reachable from root
// exist, do not form a call cycle, and do not exceed MaxCallDepth. It visits
// referenced Workflow names in sorted order so callers receive stable errors.
func ValidateCallGraph(ctx context.Context, root string, lookup WorkflowLookup) error {
	if root == "" {
		return fmt.Errorf("workflow name is required")
	}
	if lookup == nil {
		return fmt.Errorf("workflow lookup is required")
	}

	const (
		stateVisiting = iota + 1
		stateVisited
	)
	states := map[string]int{}
	stack := make([]string, 0, MaxCallDepth)

	var visit func(string) error
	visit = func(name string) error {
		if states[name] == stateVisiting {
			cycleStart := slices.Index(stack, name)
			cycle := append(slices.Clone(stack[cycleStart:]), name)
			return fmt.Errorf("workflow call cycle: %s", strings.Join(cycle, " -> "))
		}
		if states[name] == stateVisited {
			return nil
		}
		if len(stack) >= MaxCallDepth {
			path := append(slices.Clone(stack), name)
			return fmt.Errorf("workflow call depth exceeds maximum %d: %s", MaxCallDepth, strings.Join(path, " -> "))
		}

		workflow, err := lookup(ctx, name)
		if err != nil {
			return fmt.Errorf("get workflow %q: %w", name, err)
		}
		if workflow == nil {
			return fmt.Errorf("get workflow %q: empty result", name)
		}

		states[name] = stateVisiting
		stack = append(stack, name)
		for _, callee := range calledWorkflowNames(workflow.Spec.Jobs) {
			if err := visit(callee); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		states[name] = stateVisited
		return nil
	}

	return visit(root)
}

func calledWorkflowNames(jobs map[string]v1alpha1.JobSpec) []string {
	names := make([]string, 0, len(jobs))
	seen := make(map[string]struct{}, len(jobs))
	for _, job := range jobs {
		if job.Uses == "" {
			continue
		}
		if _, ok := seen[job.Uses]; ok {
			continue
		}
		seen[job.Uses] = struct{}{}
		names = append(names, job.Uses)
	}
	sort.Strings(names)
	return names
}
