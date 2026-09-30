package e2e

// PersistentWorkspace scenarios: binding fences, admission authorization, and retained data cleanup.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestPersistentWorkspaceBindingFencesRuntimePodReplacement(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	runtimeName := "workspace-binding-" + nameSuffix
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-" + nameSuffix, Namespace: testNamespace},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: runtimeName},
	}
	if err := k8sClient.Create(context.Background(), workspace); err != nil {
		t.Fatalf("create PersistentWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workspace) })

	waitForPersistentWorkspacePhase(t, workspace, 45*time.Second, v1alpha1.PersistentWorkspaceBound)
	if workspace.Status.BoundPod == "" || workspace.Status.BoundPodUID == "" || workspace.Status.Path == "" {
		t.Fatalf("bound workspace status = %#v, want fenced Pod and path", workspace.Status)
	}
	boundPod := workspace.Status.BoundPod
	writerRun := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-write-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode("echo workspace-data > shared.txt"),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), writerRun); err != nil {
		t.Fatalf("create workspace writer Run: %v", err)
	}
	waitForRun(t, writerRun, 30*time.Second)
	if writerRun.Status.AssignedPod != boundPod {
		t.Fatalf("workspace writer assignedPod = %q, want bound Pod %q", writerRun.Status.AssignedPod, boundPod)
	}
	readerRun := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-read-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode(`test "$(cat shared.txt)" = workspace-data`),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), readerRun); err != nil {
		t.Fatalf("create workspace reader Run: %v", err)
	}
	waitForRun(t, readerRun, 30*time.Second)
	if readerRun.Status.AssignedPod != boundPod {
		t.Fatalf("workspace reader assignedPod = %q, want bound Pod %q", readerRun.Status.AssignedPod, boundPod)
	}
	if err := coreClientset.CoreV1().Pods(testNamespace).Delete(context.Background(), boundPod, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete bound Runtime Pod %s: %v", boundPod, err)
	}

	waitForPersistentWorkspacePhase(t, workspace, 60*time.Second, v1alpha1.PersistentWorkspaceLost)
	if workspace.Status.BoundPod != boundPod {
		t.Fatalf("lost workspace boundPod = %q, want original %q", workspace.Status.BoundPod, boundPod)
	}
	lostRun := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-lost-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode("echo workspace-lost"),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), lostRun); err != nil {
		t.Fatalf("create lost workspace run: %v", err)
	}
	waitForPendingRunMessage(t, lostRun, 30*time.Second, "was lost")
}

func TestPersistentWorkspaceAdmissionAuthorization(t *testing.T) {
	ensureRuntime(t, "bash", bashRuntimeImage(), 9091)

	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-admission-" + suffix, Namespace: testNamespace},
		Spec:       v1alpha1.PersistentWorkspaceSpec{Runtime: "bash"},
	}
	if err := k8sClient.Create(ctx, workspace); err != nil {
		t.Fatalf("create authorization workspace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, workspace) })

	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "workspace-user-" + suffix, Namespace: testNamespace}}
	if err := k8sClient.Create(ctx, serviceAccount); err != nil {
		t.Fatalf("create workspace test ServiceAccount: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, serviceAccount) })

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-user-" + suffix, Namespace: testNamespace},
		Rules: []rbacv1.PolicyRule{{
			APIGroups: []string{v1alpha1.GroupVersion.Group},
			Resources: []string{"runs"},
			Verbs:     []string{"create"},
		}},
	}
	if err := k8sClient.Create(ctx, role); err != nil {
		t.Fatalf("create workspace test Role: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, role) })
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: testNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount.Name, Namespace: testNamespace}},
	}
	if err := k8sClient.Create(ctx, binding); err != nil {
		t.Fatalf("create workspace test RoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, binding) })

	workspaceUser := impersonatedClient(t, "system:serviceaccount:"+testNamespace+":"+serviceAccount.Name)
	deniedRun := workspaceAuthorizationRun("workspace-denied-"+suffix, workspace.Name)
	if err := workspaceUser.Create(ctx, deniedRun); !apierrors.IsForbidden(err) {
		t.Fatalf("create Run without workspace use permission = %v, want forbidden", err)
	}

	role.Rules = append(role.Rules, rbacv1.PolicyRule{
		APIGroups:     []string{v1alpha1.GroupVersion.Group},
		Resources:     []string{"persistentworkspaces/use"},
		ResourceNames: []string{workspace.Name},
		Verbs:         []string{"use"},
	})
	if err := k8sClient.Update(ctx, role); err != nil {
		t.Fatalf("grant named workspace use permission: %v", err)
	}
	allowedRun := workspaceAuthorizationRun("workspace-allowed-"+suffix, workspace.Name)
	if err := workspaceUser.Create(ctx, allowedRun); err != nil {
		t.Fatalf("create Run with named workspace use permission: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, allowedRun) })

	workflowRun := &v1alpha1.WorkflowRun{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-workflow-" + suffix, Namespace: testNamespace},
		Spec: v1alpha1.WorkflowRunSpec{Jobs: map[string]v1alpha1.JobSpec{
			"build": {RunsOn: "bash", Steps: []v1alpha1.StepSpec{{Name: "run", Run: "echo authorized"}}},
		}},
	}
	if err := k8sClient.Create(ctx, workflowRun); err != nil {
		t.Fatalf("create WorkflowRun for controller authorization: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(ctx, workflowRun) })
	waitForWorkflowRunPhase(t, workflowRun, 45*time.Second, v1alpha1.WorkflowSucceeded)

	workflowWorkspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "workspace-controller-" + suffix,
			Namespace:       testNamespace,
			Labels:          map[string]string{v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID), v1alpha1.WorkflowJobLabel: "build"},
			OwnerReferences: []metav1.OwnerReference{workflowRunOwnerReference(workflowRun)},
		},
		Spec: v1alpha1.PersistentWorkspaceSpec{Runtime: "bash"},
	}
	if err := k8sClient.Create(ctx, workflowWorkspace); err != nil {
		t.Fatalf("create WorkflowRun-owned workspace: %v", err)
	}
	controllerClient := impersonatedClient(t, "system:serviceaccount:"+testNamespace+":kruntimes-controller")
	crossJobRun := workspaceAuthorizationRun("workspace-controller-cross-job-"+suffix, workflowWorkspace.Name)
	crossJobRun.Labels = map[string]string{
		v1alpha1.WorkflowRunUIDLabel: string(workflowRun.UID),
		v1alpha1.WorkflowJobLabel:    "other",
	}
	crossJobRun.OwnerReferences = []metav1.OwnerReference{workflowRunOwnerReference(workflowRun)}
	if err := controllerClient.Create(ctx, crossJobRun); !apierrors.IsForbidden(err) {
		t.Fatalf("controller create cross-job workspace Run = %v, want forbidden", err)
	}
}

func workspaceAuthorizationRun(name, workspaceName string) *v1alpha1.Run {
	return &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   "bash",
			Mode:      v1alpha1.RunMode{Task: &v1alpha1.RunTaskMode{Args: []string{"echo authorization"}}},
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspaceName},
		},
	}
}

func impersonatedClient(t *testing.T, username string) client.Client {
	t.Helper()
	config := rest.CopyConfig(restConfig)
	config.Impersonate = rest.ImpersonationConfig{UserName: username}
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	impersonated, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create impersonated Kubernetes client: %v", err)
	}
	return impersonated
}

func workflowRunOwnerReference(workflowRun *v1alpha1.WorkflowRun) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(),
		Kind:       "WorkflowRun",
		Name:       workflowRun.Name,
		UID:        workflowRun.UID,
		Controller: &controller,
	}
}

func TestPersistentWorkspaceExplicitDeletionCleansRetainedData(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	runtimeName := "workspace-cleanup-" + nameSuffix
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-cleanup-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime:       runtimeName,
			CleanupPolicy: v1alpha1.PersistentWorkspaceRetain,
		},
	}
	if err := k8sClient.Create(context.Background(), workspace); err != nil {
		t.Fatalf("create PersistentWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workspace) })

	waitForPersistentWorkspacePhase(t, workspace, 45*time.Second, v1alpha1.PersistentWorkspaceBound)
	marker := "cleanup-marker"
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-cleanup-write-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode("echo retained-data > " + marker),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create workspace writer Run: %v", err)
	}
	waitForRun(t, run, 30*time.Second)

	markerPath := filepath.Join(workspace.Status.Path, marker)
	if _, stderr, err := execInPod(context.Background(), workspace.Status.BoundPod, "runtimed", []string{"/bin/sh", "-c", "test -f " + markerPath}); err != nil {
		t.Fatalf("verify workspace marker before deletion: %v: %s", err, stderr)
	}
	if err := k8sClient.Delete(context.Background(), workspace); err != nil {
		t.Fatalf("delete PersistentWorkspace: %v", err)
	}
	waitForPersistentWorkspaceDeleted(t, workspace, 45*time.Second)
	if _, stderr, err := execInPod(context.Background(), workspace.Status.BoundPod, "runtimed", []string{"/bin/sh", "-c", "test ! -e " + markerPath}); err != nil {
		t.Fatalf("verify workspace marker after deletion: %v: %s", err, stderr)
	}
}

func TestPersistentWorkspaceTTLDeletionCleansData(t *testing.T) {
	nameSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	runtimeName := "workspace-ttl-cleanup-" + nameSuffix
	ensureRuntime(t, runtimeName, bashRuntimeImage(), 9091)

	// A newly Bound workspace begins its unused interval immediately. Leave
	// enough time to create and complete the writer Run before exercising the
	// post-Run TTL cleanup path.
	ttlSeconds := int32(10)
	workspace := &v1alpha1.PersistentWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "workspace-ttl-cleanup-" + nameSuffix, Namespace: testNamespace},
		Spec: v1alpha1.PersistentWorkspaceSpec{
			Runtime:               runtimeName,
			CleanupPolicy:         v1alpha1.PersistentWorkspaceDeleteAfterTTL,
			TTLSecondsAfterUnused: &ttlSeconds,
		},
	}
	if err := k8sClient.Create(context.Background(), workspace); err != nil {
		t.Fatalf("create PersistentWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), workspace) })

	waitForPersistentWorkspacePhase(t, workspace, 45*time.Second, v1alpha1.PersistentWorkspaceBound)
	marker := "ttl-cleanup-marker"
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "workspace-ttl-cleanup-write-", Namespace: testNamespace},
		Spec: v1alpha1.RunSpec{
			Runtime:   runtimeName,
			Mode:      taskMode("echo ttl-data > " + marker),
			Workspace: &v1alpha1.RunWorkspaceReference{Name: workspace.Name},
		},
	}
	if err := k8sClient.Create(context.Background(), run); err != nil {
		t.Fatalf("create workspace writer Run: %v", err)
	}
	waitForRun(t, run, 30*time.Second)

	markerPath := filepath.Join(workspace.Status.Path, marker)
	if _, stderr, err := execInPod(context.Background(), workspace.Status.BoundPod, "runtimed", []string{"/bin/sh", "-c", "test -f " + markerPath}); err != nil {
		t.Fatalf("verify workspace marker before TTL cleanup: %v: %s", err, stderr)
	}
	waitForPersistentWorkspaceDeleted(t, workspace, 45*time.Second)
	if _, stderr, err := execInPod(context.Background(), workspace.Status.BoundPod, "runtimed", []string{"/bin/sh", "-c", "test ! -e " + markerPath}); err != nil {
		t.Fatalf("verify workspace marker after TTL cleanup: %v: %s", err, stderr)
	}
}
