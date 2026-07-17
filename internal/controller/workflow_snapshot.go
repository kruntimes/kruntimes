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
	maxWorkflowSnapshotBytes = 1 << 20
	maxWorkflowCallDepth     = 8
	maxWorkflowCallNodes     = 64
)

// workflowExecutionSnapshot is controller-private storage. It retains the
// accepted WorkflowRun spec and every resolved reusable Workflow spec.
type workflowExecutionSnapshot struct {
	Root      workflowSnapshotRoot             `json:"root"`
	Workflows map[string]v1alpha1.WorkflowSpec `json:"workflows,omitempty"`
}

type workflowSnapshotRoot struct {
	// Spec is the exact accepted WorkflowRun spec for this root execution.
	Spec v1alpha1.WorkflowRunSpec `json:"spec"`
	// Workflow is the resolved reusable definition when Spec.Uses is set.
	// It is omitted for inline WorkflowRuns.
	Workflow *v1alpha1.WorkflowSpec `json:"workflow,omitempty"`
}

func (s *workflowExecutionSnapshot) rootJobs() map[string]v1alpha1.JobSpec {
	if s.Root.Workflow != nil {
		return s.Root.Workflow.Jobs
	}
	return s.Root.Spec.Jobs
}

type workflowSnapshotResolutionError struct {
	err error
}

func (e *workflowSnapshotResolutionError) Error() string { return e.err.Error() }
func (e *workflowSnapshotResolutionError) Unwrap() error { return e.err }

func (r *WorkflowRunReconciler) resolveWorkflowRunSnapshot(ctx context.Context, workflowRun *v1alpha1.WorkflowRun) (*workflowExecutionSnapshot, error) {
	snapshot := &workflowExecutionSnapshot{
		Root:      workflowSnapshotRoot{Spec: *workflowRun.Spec.DeepCopy()},
		Workflows: map[string]v1alpha1.WorkflowSpec{},
	}
	var stack []string

	if len(workflowRun.Spec.Jobs) > 0 {
	} else {
		workflow, err := r.getWorkflow(ctx, workflowRun.Namespace, workflowRun.Spec.Uses)
		if err != nil {
			return nil, err
		}
		if _, err := bindWorkflowInputs(workflow.Spec.Inputs, workflowRun.Spec.With); err != nil {
			return nil, &workflowInputBindingError{err: fmt.Errorf("bind workflow inputs for %s/%s: %w", workflowRun.Namespace, workflowRun.Spec.Uses, err)}
		}
		snapshot.Root.Workflow = workflow.Spec.DeepCopy()
		stack = []string{workflow.Name}
	}

	if err := r.resolveWorkflowCalls(ctx, workflowRun.Namespace, snapshot, snapshot.rootJobs(), "root", stack, 0); err != nil {
		return nil, err
	}
	if _, err := json.Marshal(snapshot); err != nil {
		return nil, fmt.Errorf("serialize workflow snapshot: %w", err)
	}
	return snapshot, nil
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
	return strings.Contains("\x00"+strings.Join(values, "\x00")+"\x00", "\x00"+value+"\x00")
}

func (r *WorkflowRunReconciler) ensureWorkflowSnapshot(ctx context.Context, workflowRun *v1alpha1.WorkflowRun, snapshot *workflowExecutionSnapshot) (string, *workflowExecutionSnapshot, error) {
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
		},
		Revision: 1,
		Data:     runtime.RawExtension{Raw: raw},
	}
	if err := controllerutil.SetControllerReference(workflowRun, revision, r.Scheme); err != nil {
		return "", nil, fmt.Errorf("set workflowrun owner reference on snapshot %s/%s: %w", revision.Namespace, revision.Name, err)
	}
	if err := r.Create(ctx, revision); err == nil {
		loaded, loadErr := loadWorkflowSnapshot(revision)
		if loadErr != nil {
			return "", nil, loadErr
		}
		return name, loaded, nil
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
	loaded, err := loadWorkflowSnapshot(existing)
	if err != nil {
		return "", nil, err
	}
	return name, loaded, nil
}

func loadWorkflowSnapshot(revision *appsv1.ControllerRevision) (*workflowExecutionSnapshot, error) {
	snapshot := &workflowExecutionSnapshot{}
	if err := json.Unmarshal(revision.Data.Raw, snapshot); err != nil {
		return nil, fmt.Errorf("decode workflow snapshot %s/%s: %w", revision.Namespace, revision.Name, err)
	}
	switch {
	case len(snapshot.Root.Spec.Jobs) > 0 && snapshot.Root.Workflow != nil:
		return nil, fmt.Errorf("workflow snapshot %s/%s has both inline jobs and a root workflow", revision.Namespace, revision.Name)
	case len(snapshot.Root.Spec.Jobs) > 0:
	case snapshot.Root.Spec.Uses != "" && snapshot.Root.Workflow == nil:
		return nil, fmt.Errorf("workflow snapshot %s/%s is missing the resolved root workflow", revision.Namespace, revision.Name)
	case snapshot.Root.Spec.Uses != "":
	default:
		return nil, fmt.Errorf("workflow snapshot %s/%s has no root execution definition", revision.Namespace, revision.Name)
	}
	return snapshot, nil
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
