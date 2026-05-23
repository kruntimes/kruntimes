package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunPhase is the lifecycle phase of a Run.
// +kubebuilder:validation:Enum=Pending;Scheduled;Running;Succeeded;Failed
type RunPhase string

const (
	RunPending   RunPhase = "Pending"
	RunScheduled RunPhase = "Scheduled"
	RunRunning   RunPhase = "Running"
	RunSucceeded RunPhase = "Succeeded"
	RunFailed    RunPhase = "Failed"
)

// +kubebuilder:object:generate=true
// RunSpec defines the desired state of Run.
type RunSpec struct {
	// Runtime is the execution environment type (e.g., "golang-1.22-test").
	// It maps to the "runtime" label on Runtime Pods.
	// +kubebuilder:validation:Required
	Runtime string `json:"runtime"`

	// Args is the list of arguments passed to the runtime.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env is the list of environment variables to set for execution.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Timeout is the maximum duration the run is allowed to run.
	// If not set, the runtimed applies a default timeout.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// RepoURL is the Git repository URL to clone before execution.
	// +optional
	RepoURL string `json:"repoURL,omitempty"`

	// CommitSHA is the specific commit to check out.
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`
}

// +kubebuilder:object:generate=true
// RunStatus defines the observed state of Run.
type RunStatus struct {
	// Phase is the current lifecycle phase of the run.
	// +kubebuilder:default=Pending
	Phase RunPhase `json:"phase"`

	// AssignedPod is the name of the Runtime Pod assigned by the scheduler.
	// +optional
	AssignedPod string `json:"assignedPod,omitempty"`

	// Message is a human-readable status or error message.
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is when the run began executing.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run finished (success or failure).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Assigned Pod",type="string",JSONPath=".status.assignedPod"
// +kubebuilder:printcolumn:name="Runtime",type="string",JSONPath=".spec.runtime"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=runs,scope=Namespaced,shortName=rn

// Run is the Schema for the runs API.
type Run struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RunSpec   `json:"spec,omitempty"`
	Status RunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RunList contains a list of Run.
type RunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Run `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Run{}, &RunList{})
}
