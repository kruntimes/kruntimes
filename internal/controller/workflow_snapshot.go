package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const (
	workflowSnapshotRootKindWorkflowRun = "WorkflowRun"
	workflowSnapshotRootKindWorkflow    = "Workflow"
	maxWorkflowSnapshotBytes            = 1 << 20
	maxWorkflowCallDepth                = 8
	maxWorkflowCallNodes                = 64
)

// workflowExecutionSnapshot is controller-private storage. Its payloads are
// direct WorkflowRunSpec and WorkflowSpec values rather than copied API types.
type workflowExecutionSnapshot struct {
	Root      workflowSnapshotRoot             `json:"root"`
	Workflows map[string]v1alpha1.WorkflowSpec `json:"workflows,omitempty"`
}

type workflowSnapshotRoot struct {
	Kind string          `json:"kind"`
	Spec json.RawMessage `json:"spec"`
}

type workflowSnapshotResolutionError struct {
	err error
}

func (e *workflowSnapshotResolutionError) Error() string { return e.err.Error() }
func (e *workflowSnapshotResolutionError) Unwrap() error { return e.err }

func (r *WorkflowRunReconciler) resolveWorkflowRunSnapshot(ctx context.Context, workflowRun *v1alpha1.WorkflowRun) (*workflowExecutionSnapshot, map[string]v1alpha1.JobSpec, error) {
	snapshot := &workflowExecutionSnapshot{Workflows: map[string]v1alpha1.WorkflowSpec{}}
	var jobs map[string]v1alpha1.JobSpec
	var stack []string

	if len(workflowRun.Spec.Jobs) > 0 {
		raw, err := json.Marshal(workflowRun.Spec)
		if err != nil {
			return nil, nil, fmt.Errorf("serialize workflowrun spec: %w", err)
		}
		snapshot.Root = workflowSnapshotRoot{Kind: workflowSnapshotRootKindWorkflowRun, Spec: raw}
		jobs = workflowRun.Spec.Jobs
	} else {
		workflow, err := r.getWorkflow(ctx, workflowRun.Namespace, workflowRun.Spec.Uses)
		if err != nil {
			return nil, nil, err
		}
		if _, err := bindWorkflowInputs(workflow.Spec.Inputs, workflowRun.Spec.With); err != nil {
			return nil, nil, &workflowInputBindingError{err: fmt.Errorf("bind workflow inputs for %s/%s: %w", workflowRun.Namespace, workflowRun.Spec.Uses, err)}
		}
		raw, err := json.Marshal(workflow.Spec)
		if err != nil {
			return nil, nil, fmt.Errorf("serialize workflow spec %s/%s: %w", workflowRun.Namespace, workflow.Name, err)
		}
		snapshot.Root = workflowSnapshotRoot{Kind: workflowSnapshotRootKindWorkflow, Spec: raw}
		jobs = workflow.Spec.Jobs
		stack = []string{workflow.Name}
	}

	if err := r.resolveWorkflowCalls(ctx, workflowRun.Namespace, snapshot, jobs, "root", stack, 0); err != nil {
		return nil, nil, err
	}
	if _, err := json.Marshal(snapshot); err != nil {
		return nil, nil, fmt.Errorf("serialize workflow snapshot: %w", err)
	}
	return snapshot, jobs, nil
}

func (r *WorkflowRunReconciler) resolveWorkflowCalls(ctx context.Context, namespace string, snapshot *workflowExecutionSnapshot, jobs map[string]v1alpha1.JobSpec, parentPath string, stack []string, depth int) error {
	jobNames := make([]string, 0, len(jobs))
	for jobName := range jobs {
		jobNames = append(jobNames, jobName)
	}
	sort.Strings(jobNames)

	for _, jobName := range jobNames {
		job := jobs[jobName]
		if job.Uses == "" {
			continue
		}
		if depth >= maxWorkflowCallDepth {
			return &workflowSnapshotResolutionError{err: fmt.Errorf("workflow call depth exceeds %d at %s/jobs/%s", maxWorkflowCallDepth, parentPath, jobName)}
		}
		if len(snapshot.Workflows) >= maxWorkflowCallNodes {
			return &workflowSnapshotResolutionError{err: fmt.Errorf("workflow call count exceeds %d", maxWorkflowCallNodes)}
		}
		if containsString(stack, job.Uses) {
			cycle := append(append([]string(nil), stack...), job.Uses)
			return &workflowSnapshotResolutionError{err: fmt.Errorf("workflow reuse cycle: %s", strings.Join(cycle, " -> "))}
		}

		workflow, err := r.getWorkflow(ctx, namespace, job.Uses)
		if err != nil {
			return err
		}
		if _, err := bindWorkflowInputs(workflow.Spec.Inputs, job.With); err != nil {
			return &workflowInputBindingError{err: fmt.Errorf("bind workflow inputs for %s/%s: %w", namespace, workflow.Name, err)}
		}
		callPath := parentPath + "/jobs/" + jobName
		snapshot.Workflows[callPath] = *workflow.Spec.DeepCopy()
		if err := r.resolveWorkflowCalls(ctx, namespace, snapshot, workflow.Spec.Jobs, callPath, append(append([]string(nil), stack...), workflow.Name), depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (r *WorkflowRunReconciler) getWorkflow(ctx context.Context, namespace string, name string) (*v1alpha1.Workflow, error) {
	workflow := &v1alpha1.Workflow{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, workflow); err != nil {
		return nil, fmt.Errorf("get workflow %s/%s: %w", namespace, name, err)
	}
	return workflow, nil
}

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func (r *WorkflowRunReconciler) ensureWorkflowSnapshot(ctx context.Context, workflowRun *v1alpha1.WorkflowRun, snapshot *workflowExecutionSnapshot) (string, map[string]v1alpha1.JobSpec, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", nil, fmt.Errorf("serialize workflow snapshot: %w", err)
	}
	if len(raw) > maxWorkflowSnapshotBytes {
		return "", nil, &workflowSnapshotResolutionError{err: fmt.Errorf("workflow snapshot is %d bytes, exceeds %d byte limit", len(raw), maxWorkflowSnapshotBytes)}
	}

	name := workflowSnapshotName(workflowRun)
	revision := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: workflowRun.Namespace,
			Labels: map[string]string{
				v1alpha1.WorkflowRootRunUIDLabel: string(workflowRun.UID),
			},
			Annotations: map[string]string{
				v1alpha1.WorkflowCallPathAnnotation: "root",
			},
		},
		Revision: 1,
		Data:     runtime.RawExtension{Raw: raw},
	}
	if err := controllerutil.SetControllerReference(workflowRun, revision, r.Scheme); err != nil {
		return "", nil, fmt.Errorf("set workflowrun owner reference on snapshot %s/%s: %w", revision.Namespace, revision.Name, err)
	}
	if err := r.Create(ctx, revision); err == nil {
		_, jobs, loadErr := loadWorkflowSnapshot(revision)
		if loadErr != nil {
			return "", nil, loadErr
		}
		return name, jobs, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return "", nil, fmt.Errorf("create workflow snapshot %s/%s: %w", revision.Namespace, revision.Name, err)
	}

	existing := &appsv1.ControllerRevision{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(revision), existing); err != nil {
		return "", nil, fmt.Errorf("get workflow snapshot %s/%s after create conflict: %w", revision.Namespace, revision.Name, err)
	}
	if existing.Labels[v1alpha1.WorkflowRootRunUIDLabel] != string(workflowRun.UID) {
		return "", nil, fmt.Errorf("workflow snapshot %s/%s belongs to another workflowrun", existing.Namespace, existing.Name)
	}
	_, jobs, err := loadWorkflowSnapshot(existing)
	if err != nil {
		return "", nil, err
	}
	return name, jobs, nil
}

func loadWorkflowSnapshot(revision *appsv1.ControllerRevision) (*workflowExecutionSnapshot, map[string]v1alpha1.JobSpec, error) {
	snapshot := &workflowExecutionSnapshot{}
	if err := json.Unmarshal(revision.Data.Raw, snapshot); err != nil {
		return nil, nil, fmt.Errorf("decode workflow snapshot %s/%s: %w", revision.Namespace, revision.Name, err)
	}
	var jobs map[string]v1alpha1.JobSpec
	switch snapshot.Root.Kind {
	case workflowSnapshotRootKindWorkflowRun:
		var spec v1alpha1.WorkflowRunSpec
		if err := json.Unmarshal(snapshot.Root.Spec, &spec); err != nil {
			return nil, nil, fmt.Errorf("decode workflowrun root snapshot %s/%s: %w", revision.Namespace, revision.Name, err)
		}
		jobs = spec.Jobs
	case workflowSnapshotRootKindWorkflow:
		var spec v1alpha1.WorkflowSpec
		if err := json.Unmarshal(snapshot.Root.Spec, &spec); err != nil {
			return nil, nil, fmt.Errorf("decode workflow root snapshot %s/%s: %w", revision.Namespace, revision.Name, err)
		}
		jobs = spec.Jobs
	default:
		return nil, nil, fmt.Errorf("workflow snapshot %s/%s has unsupported root kind %q", revision.Namespace, revision.Name, snapshot.Root.Kind)
	}
	return snapshot, jobs, nil
}

func workflowSnapshotName(workflowRun *v1alpha1.WorkflowRun) string {
	sum := sha256.Sum256([]byte(workflowRun.UID))
	suffix := hex.EncodeToString(sum[:])[:10]
	const separator = "-snapshot-"
	const maxNameLength = 253
	maxPrefixLength := maxNameLength - len(separator) - len(suffix)
	prefix := workflowRun.Name
	if len(prefix) > maxPrefixLength {
		prefix = strings.TrimRight(prefix[:maxPrefixLength], "-.")
	}
	if prefix == "" {
		return "snapshot-" + suffix
	}
	return prefix + separator + suffix
}
