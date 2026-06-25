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
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Path string `json:"path"`

	// VolumeClaimName identifies the PVC backing the filesystem store.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	VolumeClaimName string `json:"volumeClaimName,omitempty"`
}

// +kubebuilder:object:generate=true
// S3ArtifactLocation identifies an artifact in an S3-compatible object store.
type S3ArtifactLocation struct {
	// Bucket is the object store bucket.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
	Bucket string `json:"bucket"`

	// Key is the object key within the bucket.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
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
	// Artifact collection enforces the same 255-byte upper bound.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=255
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
	// +kubebuilder:validation:MaxLength=256
	Digest string `json:"digest,omitempty"`

	// ContentType is the detected media type.
	// +optional
	// +kubebuilder:validation:MaxLength=255
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
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// Backoff is the initial backoff duration between retries.
	// The backoff doubles after each retry (exponential backoff with 2x multiplier),
	// capped at 60 seconds.
	// +optional
	Backoff metav1.Duration `json:"backoff,omitempty"`

	// RetryableReasons lists the failure reasons that are eligible for retry.
	// If empty, all reasons except "Cancelled" are retryable.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MaxLength=128
	RetryableReasons []string `json:"retryableReasons,omitempty"`
}

// +kubebuilder:object:generate=true
// CodeSource specifies where the code to run comes from.
// +kubebuilder:validation:XValidation:rule="!(has(self.inline) && has(self.repoURL))",message="inline and repoURL are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!has(self.commitSHA) || has(self.repoURL)",message="commitSHA requires repoURL"
type CodeSource struct {
	// Inline is the source code as a string (for simple scripts).
	// Mutually exclusive with RepoURL.
	// +optional
	// 256 KiB keeps simple scripts well below the Kubernetes object size limit.
	// +kubebuilder:validation:MaxLength=262144
	Inline *string `json:"inline,omitempty"`

	// RepoURL is the Git repository URL to clone before execution.
	// +optional
	// 2048 characters accommodates conventional HTTPS and SSH Git URLs.
	// +kubebuilder:validation:MaxLength=2048
	RepoURL string `json:"repoURL,omitempty"`

	// CommitSHA is the specific commit to check out.
	// +optional
	// The limit also permits symbolic refs while bounding object growth.
	// +kubebuilder:validation:MaxLength=256
	CommitSHA string `json:"commitSHA,omitempty"`
}

// +kubebuilder:object:generate=true
// RunSpec defines the desired state of Run.
// +kubebuilder:validation:XValidation:rule="!has(self.entrypoint) || (!self.entrypoint.startsWith('/') && !self.entrypoint.split('/').exists(segment, segment == '..'))",message="entrypoint must be a relative path that does not contain '..'"
type RunSpec struct {
	// Runtime is the execution environment type (e.g., "python").
	// It maps to the "runtime" label on Runtime Pods.
	// +kubebuilder:validation:Required
	// Runtime names are propagated to Kubernetes label values.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`
	Runtime string `json:"runtime"`

	// Source specifies where the code to run comes from.
	// +optional
	Source *CodeSource `json:"source,omitempty"`

	// Entrypoint is the script file to execute (default "script" for inline source).
	// +optional
	// Linux PATH_MAX is 4096 bytes.
	// +kubebuilder:validation:MaxLength=4096
	Entrypoint string `json:"entrypoint,omitempty"`

	// Handler is the module.function to call (FaaS mode).
	// When set, the runtime imports and calls the function instead of running a script.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Handler string `json:"handler,omitempty"`

	// Args is the list of arguments passed to the runtime.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=8192
	Args []string `json:"args,omitempty"`

	// Env is the list of environment variables to set for execution.
	// +optional
	// +kubebuilder:validation:MaxItems=256
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
	// Status messages are diagnostic summaries, not execution logs.
	// +kubebuilder:validation:MaxLength=4096
	Message string `json:"message,omitempty"`

	// StartTime is when the run began executing.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run finished (success or failure).
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Outputs is the key-value pairs exposed by this Run (from $OUTPUTS file).
	// +optional
	// These limits mirror the runtimed output parser's per-key bounds.
	// +kubebuilder:validation:MaxProperties=64
	Outputs map[string]string `json:"outputs,omitempty"`

	// ArtifactRefs point to artifacts stored outside etcd.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	ArtifactRefs []ArtifactRef `json:"artifactRefs,omitempty"`

	// ArtifactStore is the immutable cleanup configuration captured before
	// artifacts are uploaded. It allows cleanup to continue if the Runtime is
	// later changed or deleted. Secret contents are never copied here.
	// +optional
	ArtifactStore *RuntimeArtifactStoreSpec `json:"artifactStore,omitempty"`

	// Attempt is the current execution attempt number (1-based).
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// Conditions represent the current state of the Run's lifecycle conditions.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
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
