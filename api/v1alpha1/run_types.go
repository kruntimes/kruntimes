package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunPhase is the lifecycle phase of a Run.
// +kubebuilder:validation:Enum=Pending;Scheduled;Running;Succeeded;Failed;Timeout;Cancelled
type RunPhase string

const (
	RunPending   RunPhase = "Pending"
	RunScheduled RunPhase = "Scheduled"
	RunRunning   RunPhase = "Running"
	RunSucceeded RunPhase = "Succeeded"
	RunFailed    RunPhase = "Failed"
	RunTimeout   RunPhase = "Timeout"
	RunCancelled RunPhase = "Cancelled"
)

// +kubebuilder:object:generate=true
// RetryPolicy specifies the retry strategy for a Run.
type RetryPolicy struct {
	// MaxAttempts is the maximum number of execution attempts (including the initial attempt).
	// Default: 1 (no retries).
	// +optional
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// Backoff is the initial backoff duration between retries.
	// The backoff doubles after each retry (exponential backoff with 2x multiplier),
	// capped at 60 seconds.
	// +optional
	Backoff metav1.Duration `json:"backoff,omitempty"`

	// RetryableReasons lists the failure reasons that are eligible for retry.
	// If empty, all reasons except "Cancelled" are retryable.
	// +optional
	RetryableReasons []string `json:"retryableReasons,omitempty"`
}

// +kubebuilder:object:generate=true
// CodeSource specifies where the code to run comes from.
type CodeSource struct {
	// Inline is the source code as a string (for simple scripts).
	// Mutually exclusive with RepoURL.
	// +optional
	Inline *string `json:"inline,omitempty"`

	// RepoURL is the Git repository URL to clone before execution.
	// +optional
	RepoURL string `json:"repoURL,omitempty"`

	// CommitSHA is the specific commit to check out.
	// +optional
	CommitSHA string `json:"commitSHA,omitempty"`
}

// +kubebuilder:object:generate=true
// RunSpec defines the desired state of Run.
type RunSpec struct {
	// Runtime is the execution environment type (e.g., "python").
	// It maps to the "runtime" label on Runtime Pods.
	// +kubebuilder:validation:Required
	Runtime string `json:"runtime"`

	// Source specifies where the code to run comes from.
	// +optional
	Source *CodeSource `json:"source,omitempty"`

	// Entrypoint is the script file to execute (default "script" for inline source).
	// +optional
	Entrypoint string `json:"entrypoint,omitempty"`

	// Handler is the module.function to call (FaaS mode).
	// When set, the runtime imports and calls the function instead of running a script.
	// +optional
	Handler string `json:"handler,omitempty"`

	// Args is the list of arguments passed to the runtime.
	// +optional
	Args []string `json:"args,omitempty"`

	// Env is the list of environment variables to set for execution.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Timeout is the maximum duration the run is allowed to run.
	// If not set, the run runs with no time limit.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// CancelRequested is set to true to request cancellation of a running Run.
	// +optional
	CancelRequested bool `json:"cancelRequested,omitempty"`

	// RetryPolicy is the retry strategy for the Run. If nil, no retries are attempted.
	// +optional
	RetryPolicy *RetryPolicy `json:"retryPolicy,omitempty"`
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

	// Outputs is the key-value pairs exposed by this Run (from $OUTPUTS file).
	// +optional
	Outputs map[string]string `json:"outputs,omitempty"`

	// Attempt is the current execution attempt number (1-based).
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// Conditions represent the current state of the Run's lifecycle conditions.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
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
