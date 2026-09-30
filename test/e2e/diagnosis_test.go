package e2e

// Demo diagnosis runtime scenario verifying namespace read access from a Run.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kruntimes/kruntimes/api/v1alpha1"
)

func TestKubernetesDiagnosisRuntimeCanReadNamespace(t *testing.T) {
	t.Parallel()
	name := fmt.Sprintf("diagnosis-runtime-%d", time.Now().UnixNano())
	serviceAccountName := "diagnosis-reader-" + fmt.Sprintf("%d", time.Now().UnixNano())
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName, Namespace: testNamespace}}
	if err := k8sClient.Create(t.Context(), serviceAccount); err != nil {
		t.Fatalf("create diagnosis Runtime ServiceAccount: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), serviceAccount) })
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: serviceAccountName, Namespace: testNamespace},
		Rules:      []rbacv1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}},
	}
	if err := k8sClient.Create(t.Context(), role); err != nil {
		t.Fatalf("create diagnosis Runtime Role: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), role) })
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: testNamespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccountName, Namespace: testNamespace}},
	}
	if err := k8sClient.Create(t.Context(), binding); err != nil {
		t.Fatalf("create diagnosis Runtime RoleBinding: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), binding) })

	runtimeObject := &v1alpha1.Runtime{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: v1alpha1.RuntimeSpec{
			Template: runtimePodTemplate(diagnosisRuntimeImage(), 9092),
			Port:     9092,
			Replicas: 1,
			Capacity: &v1alpha1.RuntimeCapacity{Resources: corev1.ResourceList{
				corev1.ResourceName(v1alpha1.RuntimeResourceRuns): *resource.NewQuantity(1, resource.DecimalSI),
			}},
		},
	}
	runtimeObject.Spec.Template.Spec.ServiceAccountName = serviceAccountName
	if err := k8sClient.Create(t.Context(), runtimeObject); err != nil {
		t.Fatalf("create diagnosis Runtime: %v", err)
	}
	cleanupRuntime(t, name)
	waitForRuntimePod(t, name, diagnosisRuntimeImage(), runtimedImage(), 1, "diagnosis Runtime Pod")

	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "e2e-diagnosis-", Namespace: testNamespace},
		Spec:       v1alpha1.RunSpec{Runtime: name, Mode: v1alpha1.RunMode{Session: &v1alpha1.RunSessionMode{}}},
	}
	if err := k8sClient.Create(t.Context(), run); err != nil {
		t.Fatalf("create diagnosis Session Run: %v", err)
	}
	t.Cleanup(func() { _ = k8sClient.Delete(context.Background(), run) })
	waitForRunPhase(t, run, 45*time.Second, v1alpha1.RunReady)

	baseURL := gatewayEndpointURL(t, waitForGatewayPod(t), run.Status.Endpoint.URL)
	token := sessionGatewayToken(t, run)
	response := waitForGatewayResponse(t, http.MethodPost, baseURL+"/operations:execute", token,
		[]byte(`{"command":{"argv":["kubectl","get","pods","--namespace","default","--output","json"]}}`), http.StatusOK)
	var operation struct {
		Command struct {
			ExitCode int32  `json:"exitCode"`
			Stdout   []byte `json:"stdout"`
		} `json:"command"`
	}
	if err := json.Unmarshal(response, &operation); err != nil {
		t.Fatalf("decode diagnosis command response: %v", err)
	}
	if operation.Command.ExitCode != 0 {
		t.Fatalf("diagnosis kubectl command failed: %s", operation.Command.Stdout)
	}
	var pods corev1.PodList
	if err := json.Unmarshal(operation.Command.Stdout, &pods); err != nil || len(pods.Items) == 0 {
		t.Fatalf("diagnosis kubectl output = %s, unmarshal error = %v", operation.Command.Stdout, err)
	}
}
