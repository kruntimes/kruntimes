package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// RuntimePodAnnotationPrefix is the annotation prefix used for Runtime Pod metadata.
	RuntimePodAnnotationPrefix = "kruntimes.io/"

	// RuntimePodCapacityAnnotationPrefix prefixes per-pod runtime capacity annotations.
	RuntimePodCapacityAnnotationPrefix = RuntimePodAnnotationPrefix + "capacity."

	// RuntimeResourceRuns is the built-in capacity resource for concurrent Run executions.
	RuntimeResourceRuns = "runs"

	// RuntimeDefaultRunsCapacity is the default concurrent Run capacity per Runtime Pod.
	RuntimeDefaultRunsCapacity int32 = 2

	// RuntimePodRuntimedReadyCondition reports whether the runtimed daemon is heartbeating.
	RuntimePodRuntimedReadyCondition corev1.PodConditionType = "kruntimes.io/RuntimedReady"
)

// +kubebuilder:object:generate=true
// RuntimeSpec defines the desired state of Runtime.
type RuntimeSpec struct {
	// Image is the container image for this runtime (e.g., "my-bash-runner:latest").
	// +kubebuilder:validation:Required
	Image string `json:"image"`

	// Port is the gRPC port the runtime server listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=9091
	Port int32 `json:"port,omitempty"`

	// Command is the entrypoint for the runtime container.
	// +optional
	Command []string `json:"command,omitempty"`

	// Env is extra environment variables for the runtime container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Resources for the runtime container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Replicas is the number of runtime pods to run.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`

	// DaemonImage overrides the runtimed daemon image.
	// +optional
	DaemonImage string `json:"daemonImage,omitempty"`

	// Capacity declares the per-pod capacity for this runtime.
	// +optional
	Capacity *RuntimeCapacity `json:"capacity,omitempty"`
}

// +kubebuilder:object:generate=true
// RuntimeCapacity declares resource capacities exposed by each Runtime Pod.
type RuntimeCapacity struct {
	// Resources maps logical resource names to their per-pod capacity.
	// The built-in "runs" resource limits concurrent Run executions per pod.
	// +optional
	Resources corev1.ResourceList `json:"resources,omitempty"`
}

// +kubebuilder:object:generate=true
// RuntimeStatus defines the observed state of Runtime.
type RuntimeStatus struct {
	// ReadyReplicas is the number of pods that are ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Conditions represent the latest available observations of the Runtime's state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.image"
// +kubebuilder:printcolumn:name="Replicas",type="integer",JSONPath=".spec.replicas"
// +kubebuilder:printcolumn:name="Ready",type="integer",JSONPath=".status.readyReplicas"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=runtimes,scope=Namespaced,shortName="rt"

// Runtime is the Schema for the runtimes API.
type Runtime struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RuntimeSpec   `json:"spec,omitempty"`
	Status RuntimeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RuntimeList contains a list of Runtime.
type RuntimeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Runtime `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Runtime{}, &RuntimeList{})
}
