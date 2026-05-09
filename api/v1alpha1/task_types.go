package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TaskPhase is the lifecycle phase of a Task.
// +kubebuilder:validation:Enum=Pending;Scheduled;Running;Succeeded;Failed
type TaskPhase string

const (
	TaskPending   TaskPhase = "Pending"
	TaskScheduled TaskPhase = "Scheduled"
	TaskRunning   TaskPhase = "Running"
	TaskSucceeded TaskPhase = "Succeeded"
	TaskFailed    TaskPhase = "Failed"
)

// +kubebuilder:object:generate=true
// TaskSpec defines the desired state of Task.
type TaskSpec struct {
	// Runtime is the execution environment type (e.g., "golang-1.22-test").
	// It maps to the "runtime" label on Runtime Pods.
	// +kubebuilder:validation:Required
	Runtime string `json:"runtime"`

	// Commands is the list of commands to execute sequentially.
	// +kubebuilder:validation:MinItems=1
	Commands []string `json:"commands"`

	// Env is the list of environment variables to set for execution.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Timeout is the maximum duration the task is allowed to run.
	// If not set, the agent applies a default timeout.
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
// TaskStatus defines the observed state of Task.
type TaskStatus struct {
	// Phase is the current lifecycle phase of the task.
	// +kubebuilder:default=Pending
	Phase TaskPhase `json:"phase"`

	// AssignedPod is the name of the Runtime Pod assigned by the scheduler.
	// +optional
	AssignedPod string `json:"assignedPod,omitempty"`

	// Message is a human-readable status or error message.
	// +optional
	Message string `json:"message,omitempty"`

	// StartTime is when the task began executing.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the task finished (success or failure).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Assigned Pod",type="string",JSONPath=".status.assignedPod"
// +kubebuilder:printcolumn:name="Runtime",type="string",JSONPath=".spec.runtime"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=tasks,scope=Namespaced,shortName="tk"

// Task is the Schema for the tasks API.
type Task struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TaskSpec   `json:"spec,omitempty"`
	Status TaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TaskList contains a list of Task.
type TaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Task `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Task{}, &TaskList{})
}
