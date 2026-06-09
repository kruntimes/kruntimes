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

// ArtifactType describes how an artifact is represented in storage.
// +kubebuilder:validation:Enum=file;directory;archive;blob
type ArtifactType string

const (
	ArtifactTypeFile      ArtifactType = "file"
	ArtifactTypeDirectory ArtifactType = "directory"
	ArtifactTypeArchive   ArtifactType = "archive"
	ArtifactTypeBlob      ArtifactType = "blob"
)

// ArtifactDriver identifies the storage backend for an artifact.
// +kubebuilder:validation:Enum=filesystem;s3
type ArtifactDriver string

const (
	ArtifactDriverFilesystem ArtifactDriver = "filesystem"
	ArtifactDriverS3         ArtifactDriver = "s3"
)

// +kubebuilder:object:generate=true
// FilesystemArtifactLocation identifies an artifact in a configured filesystem store.
type FilesystemArtifactLocation struct {
	// Path is relative to the artifact store root.
	Path string `json:"path"`

	// VolumeClaimName identifies the PVC backing the filesystem store.
	// +optional
	VolumeClaimName string `json:"volumeClaimName,omitempty"`
}

// +kubebuilder:object:generate=true
// S3ArtifactLocation identifies an artifact in an S3-compatible object store.
type S3ArtifactLocation struct {
	// Bucket is the object store bucket.
	Bucket string `json:"bucket"`

	// Key is the object key within the bucket.
	Key string `json:"key"`
}

// +kubebuilder:object:generate=true
// ArtifactLocation contains driver-specific artifact coordinates.
// Exactly one location must be populated for the selected driver.
// +kubebuilder:validation:XValidation:rule="has(self.filesystem) != has(self.s3)",message="exactly one artifact location must be set"
type ArtifactLocation struct {
	// Filesystem identifies an artifact in a filesystem store.
	// +optional
	Filesystem *FilesystemArtifactLocation `json:"filesystem,omitempty"`

	// S3 identifies an artifact in an S3-compatible object store.
	// +optional
	S3 *S3ArtifactLocation `json:"s3,omitempty"`
}

// +kubebuilder:object:generate=true
// ArtifactRef is compact metadata that points to artifact content stored outside etcd.
// +kubebuilder:validation:XValidation:rule="(self.driver == 'filesystem' && has(self.location.filesystem)) || (self.driver == 's3' && has(self.location.s3))",message="artifact location must match driver"
type ArtifactRef struct {
	// Name is the logical artifact name exposed by the Run.
	Name string `json:"name"`

	// Driver identifies the ArtifactStore implementation.
	Driver ArtifactDriver `json:"driver"`

	// Type describes how the artifact content is represented.
	Type ArtifactType `json:"type"`

	// Location contains driver-specific storage coordinates.
	Location ArtifactLocation `json:"location"`

	// SizeBytes is the stored artifact size.
	// +kubebuilder:validation:Minimum=0
	SizeBytes int64 `json:"sizeBytes"`

	// Digest is the content digest, including its algorithm prefix.
	// +optional
	Digest string `json:"digest,omitempty"`

	// ContentType is the detected media type.
	// +optional
	ContentType string `json:"contentType,omitempty"`

	// CreatedAt records when the artifact was stored.
	CreatedAt metav1.Time `json:"createdAt"`
}

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

	// TTLSecondsAfterFinished limits the lifetime of a finished Run.
	// If set to a positive value, the controller deletes the Run after this many seconds
	// from status.completionTime. If unset or zero, the Run is retained.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
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

	// ArtifactRefs point to artifacts stored outside etcd.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	ArtifactRefs []ArtifactRef `json:"artifactRefs,omitempty"`

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
