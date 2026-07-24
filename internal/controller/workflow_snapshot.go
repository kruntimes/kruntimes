package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

const maxWorkflowSnapshotBytes = 1 << 20

// workflowExecutionSnapshot is controller-private storage for one WorkflowRun.
// It contains that WorkflowRun's accepted inline spec.
type workflowExecutionSnapshot struct {
	Spec v1alpha1.WorkflowRunSpec `json:"spec"`
}

type workflowSnapshotError struct {
	err error
}

func (e *workflowSnapshotError) Error() string { return e.err.Error() }
func (e *workflowSnapshotError) Unwrap() error { return e.err }

func workflowSnapshotForRun(workflowRun *v1alpha1.WorkflowRun) *workflowExecutionSnapshot {
	return &workflowExecutionSnapshot{Spec: *workflowRun.Spec.DeepCopy()}
}

func (r *WorkflowRunReconciler) ensureWorkflowSnapshot(ctx context.Context, workflowRun *v1alpha1.WorkflowRun, snapshot *workflowExecutionSnapshot) (string, *workflowExecutionSnapshot, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", nil, fmt.Errorf("serialize workflow snapshot: %w", err)
	}
	if len(raw) > maxWorkflowSnapshotBytes {
		return "", nil, &workflowSnapshotError{err: fmt.Errorf("workflow snapshot is %d bytes, exceeds %d byte limit", len(raw), maxWorkflowSnapshotBytes)}
	}

	name := workflowSnapshotName(workflowRun)
	revision := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: workflowRun.Namespace,
			Labels: map[string]string{
				v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID),
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
	if existing.Labels[v1alpha1.WorkflowRunUIDLabel] != string(workflowRun.UID) {
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
	if len(snapshot.Spec.Jobs) == 0 {
		return nil, fmt.Errorf("workflow snapshot %s/%s has no jobs", revision.Namespace, revision.Name)
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
