package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RunPhase is the lifecycle phase of a Run.
// +kubebuilder:validation:Enum=Pending;Scheduled;Running;Ready;Finalizing;Succeeded;Failed;Timeout;Cancelled
type RunPhase string

const (
	RunPending   RunPhase = "Pending"
	RunScheduled RunPhase = "Scheduled"
	RunRunning   RunPhase = "Running"
	// RunReady is an active, non-terminal function or session Run that accepts
	// data-plane requests.
	RunReady RunPhase = "Ready"
	// RunFinalizing is an active Session Run that has stopped accepting new
	// operations while it drains and exports final artifacts.
	RunFinalizing RunPhase = "Finalizing"
	RunSucceeded  RunPhase = "Succeeded"
	RunFailed     RunPhase = "Failed"
	RunTimeout    RunPhase = "Timeout"
	RunCancelled  RunPhase = "Cancelled"

	// RunRegistrationCleanupFinalizer ensures that a long-lived Runtime Server
	// registration is released before its Run is deleted.
	RunRegistrationCleanupFinalizer = "kruntimes.io/registration-cleanup"
)

// RunEndpointProtocol identifies the public protocol for invoking a function Run.
// +kubebuilder:validation:Enum=HTTP;HTTPS
type RunEndpointProtocol string

const (
	// RunEndpointProtocolHTTP identifies a plain HTTP Runtime access endpoint,
	// normally the cluster-local Console Service.
	RunEndpointProtocolHTTP RunEndpointProtocol = "HTTP"
	// RunEndpointProtocolHTTPS identifies a TLS-terminated public endpoint.
	RunEndpointProtocolHTTPS RunEndpointProtocol = "HTTPS"
)

// +kubebuilder:object:generate=true
// RunEndpoint is a stable, bounded reference to a function or session Run
// endpoint.
type RunEndpoint struct {
	// Protocol identifies the endpoint protocol.
	// +kubebuilder:validation:Required
	Protocol RunEndpointProtocol `json:"protocol"`

	// URL is the invoke endpoint URL. The path includes the immutable Run UID.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MaxLength=2048
	URL string `json:"url"`

	// CABundle is the PEM trust bundle for the endpoint when it is not rooted in
	// a client-configured trust store.
	// +optional
	// +kubebuilder:validation:MaxLength=16384
	CABundle []byte `json:"caBundle,omitempty"`
}

// RunTerminationMode describes how a Run is asked to terminate.
// +kubebuilder:validation:Enum=Immediate;Drain
type RunTerminationMode string

const (
	// RunTerminationImmediate stops the Run as soon as the runtime can apply
	// cancellation. It is valid for every Run mode.
	RunTerminationImmediate RunTerminationMode = "Immediate"
	// RunTerminationDrain is valid only for Session Runs. It prevents new
	// operations, drains accepted operations, and finalizes exported artifacts.
	RunTerminationDrain RunTerminationMode = "Drain"
)

// +kubebuilder:object:generate=true
// RunTermination requests an irreversible termination transition. A Session
// Run may escalate Drain to Immediate, but cannot remove or downgrade a
// request.
type RunTermination struct {
	// Mode selects immediate cancellation or Session-specific graceful drain.
	// +kubebuilder:validation:Required
	Mode RunTerminationMode `json:"mode"`
}

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
// ArtifactInput materializes an artifact stored outside etcd into a Run working
// directory before execution.
// +kubebuilder:validation:XValidation:rule="!self.path.startsWith('/') && !self.path.split('/').exists(segment, size(segment) == 0 || segment == '.' || segment == '..')",message="path must be a relative path without empty, '.' or '..' segments"
type ArtifactInput struct {
	// Ref identifies immutable artifact content readable by the Runtime's
	// configured ArtifactStore.
	// +kubebuilder:validation:Required
	Ref ArtifactRef `json:"ref"`

	// Path is the relative destination below the Run working directory. File
	// artifacts are written to this path; directory artifacts are extracted into
	// this directory.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	Path string `json:"path"`
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
// RunResourceRequirements declares the logical Runtime resources consumed by a Run.
type RunResourceRequirements struct {
	// Requests declares logical Runtime resources held from assignment until the
	// Run reaches a terminal state or a function registration is released.
	// These are independent from the Kubernetes container resources on a Runtime
	// Pod template.
	// +optional
	Requests corev1.ResourceList `json:"requests,omitempty"`
}

// +kubebuilder:object:generate=true
// CodeSource specifies where the code to run comes from.
// +kubebuilder:validation:XValidation:rule="!(has(self.inline) && has(self.repoURL))",message="inline and repoURL are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!has(self.commitSHA) || has(self.repoURL)",message="commitSHA requires repoURL"
// +kubebuilder:validation:XValidation:rule="!has(self.inlinePath) || (!self.inlinePath.startsWith('/') && !self.inlinePath.split('/').exists(segment, size(segment) == 0 || segment == '.' || segment == '..'))",message="inlinePath must be a relative file path without empty, '.' or '..' segments"
type CodeSource struct {
	// Inline is a standalone script. In task and session modes, runtimed writes
	// it to the default script file. In function mode, InlinePath identifies the
	// file to materialize.
	// Mutually exclusive with RepoURL.
	// +optional
	// 256 KiB keeps simple scripts well below the Kubernetes object size limit.
	// +kubebuilder:validation:MaxLength=262144
	Inline *string `json:"inline,omitempty"`

	// InlinePath is the relative file path below the prepared working directory
	// where runtimed materializes inline function source. It is only valid when
	// inline source is used by function mode.
	// +optional
	// Linux PATH_MAX is 4096 bytes.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=4096
	InlinePath string `json:"inlinePath,omitempty"`

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
// RunMode contains mutually exclusive execution-mode-specific configuration.
// +kubebuilder:validation:XValidation:rule="(has(self.task) && !has(self.function) && !has(self.session)) || (!has(self.task) && has(self.function) && !has(self.session)) || (!has(self.task) && !has(self.function) && has(self.session))",message="exactly one of task, function, or session must be set"
type RunMode struct {
	// Task configures one-shot process execution.
	// +optional
	Task *RunTaskMode `json:"task,omitempty"`

	// Function configures callable function execution.
	// +optional
	Function *RunFunctionMode `json:"function,omitempty"`

	// Session configures a stateful agent sandbox session.
	// +optional
	Session *RunSessionMode `json:"session,omitempty"`
}

// +kubebuilder:object:generate=true
// RunTaskMode configures one-shot process execution.
type RunTaskMode struct {
	// Entrypoint is the relative script file to execute for non-inline sources.
	// It is ignored when Source.Inline is set.
	// +optional
	// Linux PATH_MAX is 4096 bytes.
	// +kubebuilder:validation:MaxLength=4096
	Entrypoint string `json:"entrypoint,omitempty"`

	// Args is the list of arguments passed to the runtime for non-inline sources.
	// It is ignored when Source.Inline is set.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=8192
	Args []string `json:"args,omitempty"`
}

// +kubebuilder:object:generate=true
// RunFunctionMode configures callable function execution.
type RunFunctionMode struct {
	// Handler is the module.function to call.
	// The runtime imports and calls the function instead of running a script.
	// +optional
	// +kubebuilder:validation:MaxLength=512
	Handler string `json:"handler,omitempty"`

	// IdleTimeoutSeconds is the duration after the last invocation before the
	// function reservation can be released. This field is reserved for the
	// function-mode lifecycle implementation.
	// +optional
	// +kubebuilder:validation:Minimum=1
	IdleTimeoutSeconds *int32 `json:"idleTimeoutSeconds,omitempty"`
}

// +kubebuilder:object:generate=true
// RunSessionMode configures a stateful, mutable workspace reserved by one Run.
// It is a trusted-workload preview; a Session Run holds exclusive v0 Runtime
// capacity until it terminates.
type RunSessionMode struct {
	// LeaseTimeoutSeconds is the duration after the last server-observed
	// connection heartbeat before the session is closed. It is independent of
	// idle operation expiry so a connected agent can retain its sandbox without
	// issuing artificial commands.
	// +optional
	// +kubebuilder:validation:Minimum=1
	LeaseTimeoutSeconds *int32 `json:"leaseTimeoutSeconds,omitempty"`

	// IdleTimeoutSeconds is the duration after the last accepted command or file
	// mutation before the session is closed.
	// +optional
	// +kubebuilder:validation:Minimum=1
	IdleTimeoutSeconds *int32 `json:"idleTimeoutSeconds,omitempty"`

	// QueueSize limits queued command and file-mutation operations for this
	// session. runtimed also applies its administrator-configured global limit.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	QueueSize *int32 `json:"queueSize,omitempty"`

	// OperationTimeout limits one command or file-mutation operation. runtimed
	// rejects values above its administrator-configured maximum.
	// +optional
	OperationTimeout *metav1.Duration `json:"operationTimeout,omitempty"`
}

const (
	// RunWorkspaceReferenceKindPersistentWorkspace is the initial workspace
	// provider accepted by Run workspace references.
	RunWorkspaceReferenceKindPersistentWorkspace = "PersistentWorkspace"
	// RunWorkspaceReferenceAPIGroup is the initial served workspace API group.
	RunWorkspaceReferenceAPIGroup = "kruntimes.io/v1alpha1"
	// RunAffinityTopologyRuntimePod co-locates Runs on the same Runtime Pod.
	RunAffinityTopologyRuntimePod = "kruntimes.io/runtime-pod"
)

// +kubebuilder:object:generate=true
// RunWorkspaceReference identifies a namespace-local PersistentWorkspace.
type RunWorkspaceReference struct {
	// Name is the PersistentWorkspace name in the Run namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`

	// Kind identifies the workspace provider kind.
	// +optional
	// +kubebuilder:default=PersistentWorkspace
	// +kubebuilder:validation:Enum=PersistentWorkspace
	Kind string `json:"kind,omitempty"`

	// APIGroup identifies the served workspace API group and version.
	// +optional
	// +kubebuilder:default=kruntimes.io/v1alpha1
	// +kubebuilder:validation:Enum=kruntimes.io/v1alpha1
	APIGroup string `json:"apiGroup,omitempty"`
}

// +kubebuilder:object:generate=true
// RunAffinity is a Run-to-Run placement constraint evaluated by the scheduler.
type RunAffinity struct {
	// RunAffinity requires or prefers sharing a topology with matching active Runs.
	// +optional
	RunAffinity *RunAffinityRules `json:"runAffinity,omitempty"`

	// RunAntiAffinity requires or prefers avoiding a topology with matching active Runs.
	// +optional
	RunAntiAffinity *RunAffinityRules `json:"runAntiAffinity,omitempty"`
}

// +kubebuilder:object:generate=true
// RunAffinityRules contains required and preferred Run affinity terms.
type RunAffinityRules struct {
	// RequiredDuringSchedulingIgnoredDuringExecution defines hard placement constraints.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	RequiredDuringSchedulingIgnoredDuringExecution []RunAffinityTerm `json:"requiredDuringSchedulingIgnoredDuringExecution,omitempty"`

	// PreferredDuringSchedulingIgnoredDuringExecution defines soft placement preferences.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	PreferredDuringSchedulingIgnoredDuringExecution []WeightedRunAffinityTerm `json:"preferredDuringSchedulingIgnoredDuringExecution,omitempty"`
}

// +kubebuilder:object:generate=true
// WeightedRunAffinityTerm applies a weight to a preferred Run affinity term.
type WeightedRunAffinityTerm struct {
	// Weight is the relative preference weight.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Weight int32 `json:"weight"`

	// RunAffinityTerm is the preferred placement term.
	RunAffinityTerm RunAffinityTerm `json:"runAffinityTerm"`
}

// +kubebuilder:object:generate=true
// RunAffinityTerm matches active namespace-local Runs for one topology.
// +kubebuilder:validation:XValidation:rule="has(self.labelSelector) && ((has(self.labelSelector.matchLabels) && size(self.labelSelector.matchLabels) > 0) || (has(self.labelSelector.matchExpressions) && size(self.labelSelector.matchExpressions) > 0))",message="labelSelector must not be empty"
type RunAffinityTerm struct {
	// LabelSelector selects active namespace-local Runs.
	// +kubebuilder:validation:Required
	LabelSelector *metav1.LabelSelector `json:"labelSelector"`

	// TopologyKey identifies the supported Runtime Pod topology.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=kruntimes.io/runtime-pod
	TopologyKey string `json:"topologyKey"`
}

// +kubebuilder:object:generate=true
// RunSpec defines the desired state of Run.
// +kubebuilder:validation:XValidation:rule="!has(self.mode.task) || !has(self.mode.task.entrypoint) || (has(self.source) && has(self.source.inline)) || (!self.mode.task.entrypoint.startsWith('/') && !self.mode.task.entrypoint.split('/').exists(segment, segment == '..'))",message="mode.task.entrypoint must be a relative path that does not contain '..'"
// +kubebuilder:validation:XValidation:rule="!has(self.source) || !has(self.source.inlinePath) || (has(self.mode.function) && has(self.source.inline))",message="source.inlinePath is only valid for function mode with inline source"
// +kubebuilder:validation:XValidation:rule="!has(self.mode.function) || !has(self.source) || !has(self.source.inline) || has(self.source.inlinePath)",message="function mode inline source requires source.inlinePath"
// +kubebuilder:validation:XValidation:rule="!has(self.mode.session) || !has(self.workspace)",message="session mode does not support spec.workspace"
// +kubebuilder:validation:XValidation:rule="!has(self.termination) || self.termination.mode != 'Drain' || has(self.mode.session)",message="termination.mode Drain is only valid for session mode"
// +kubebuilder:validation:XValidation:rule="self.runtime == oldSelf.runtime && has(self.source) == has(oldSelf.source) && (!has(self.source) || self.source == oldSelf.source) && self.mode == oldSelf.mode && has(self.artifactInputs) == has(oldSelf.artifactInputs) && (!has(self.artifactInputs) || self.artifactInputs == oldSelf.artifactInputs) && has(self.env) == has(oldSelf.env) && (!has(self.env) || self.env == oldSelf.env) && has(self.timeout) == has(oldSelf.timeout) && (!has(self.timeout) || self.timeout == oldSelf.timeout) && has(self.retryPolicy) == has(oldSelf.retryPolicy) && (!has(self.retryPolicy) || self.retryPolicy == oldSelf.retryPolicy) && has(self.resources) == has(oldSelf.resources) && (!has(self.resources) || self.resources == oldSelf.resources)",message="runtime, source, mode, artifactInputs, env, timeout, retryPolicy, and resources are immutable after Run creation"
// +kubebuilder:validation:XValidation:rule="has(self.workspace) == has(oldSelf.workspace) && (!has(self.workspace) || self.workspace == oldSelf.workspace) && has(self.affinity) == has(oldSelf.affinity) && (!has(self.affinity) || self.affinity == oldSelf.affinity)",message="workspace and affinity are immutable after Run creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.termination) || (has(self.termination) && (self.termination == oldSelf.termination || (oldSelf.termination.mode == 'Drain' && self.termination.mode == 'Immediate')))",message="termination may not be removed, downgraded, or changed except from Drain to Immediate"
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

	// Mode contains execution-mode-specific configuration.
	// +kubebuilder:validation:Required
	Mode RunMode `json:"mode"`

	// ArtifactInputs are materialized from the configured ArtifactStore into the
	// Run working directory before execution.
	// +optional
	// +kubebuilder:validation:MaxItems=32
	ArtifactInputs []ArtifactInput `json:"artifactInputs,omitempty"`

	// Workspace references a namespace-local PersistentWorkspace used by this Run.
	// +optional
	Workspace *RunWorkspaceReference `json:"workspace,omitempty"`

	// Affinity constrains or prefers placement relative to other active Runs.
	// +optional
	Affinity *RunAffinity `json:"affinity,omitempty"`

	// Resources declares logical Runtime capacity reserved by this Run.
	// When omitted, the Run requests one unit of the built-in "runs" resource.
	// +optional
	Resources *RunResourceRequirements `json:"resources,omitempty"`

	// Env is the list of environment variables to set for execution.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Timeout is the maximum duration the run is allowed to run.
	// If not set, the run runs with no time limit.
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// Termination requests an irreversible termination transition. Immediate is
	// valid for every Run mode; Drain is valid only for Session Runs and may be
	// escalated to Immediate.
	// +optional
	Termination *RunTermination `json:"termination,omitempty"`

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

// EffectiveEntrypoint returns the task entrypoint after applying the Run mode
// compatibility rules.
func (s RunSpec) EffectiveEntrypoint() string {
	if s.Mode.Task != nil {
		return s.Mode.Task.Entrypoint
	}
	return ""
}

// HasImmediateTermination reports whether the Run requested immediate
// cancellation.
func (s RunSpec) HasImmediateTermination() bool {
	return s.Termination != nil && s.Termination.Mode == RunTerminationImmediate
}

// HasDrainTermination reports whether the Session Run requested graceful
// completion.
func (s RunSpec) HasDrainTermination() bool {
	return s.Termination != nil && s.Termination.Mode == RunTerminationDrain
}

// EffectiveArgs returns the task args after applying the Run mode compatibility
// rules.
func (s RunSpec) EffectiveArgs() []string {
	if s.Mode.Task != nil {
		return s.Mode.Task.Args
	}
	return nil
}

// EffectiveHandler returns the function handler after applying the Run mode
// compatibility rules.
func (s RunSpec) EffectiveHandler() string {
	if s.Mode.Function != nil {
		return s.Mode.Function.Handler
	}
	return ""
}

// ResourceRequests returns the complete logical Runtime request for the Run.
// The built-in runs resource defaults to one so existing Runs retain their
// concurrent-capacity behavior.
func (s RunSpec) ResourceRequests() (corev1.ResourceList, error) {
	requests := corev1.ResourceList{}
	if s.Resources != nil {
		for name, quantity := range s.Resources.Requests {
			_, integer := quantity.AsInt64()
			if quantity.Sign() < 0 || !integer {
				return nil, fmt.Errorf("resource request %q must be a non-negative integer", name)
			}
			if quantity.Sign() > 0 {
				requests[name] = quantity.DeepCopy()
			}
		}
	}
	runs := corev1.ResourceName(RuntimeResourceRuns)
	if quantity, ok := requests[runs]; !ok || quantity.Sign() <= 0 {
		requests[runs] = *resource.NewQuantity(1, resource.DecimalSI)
	}
	return requests, nil
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

	// AssignedPodUID is the UID of AssignedPod. It distinguishes Pod-name reuse
	// and is set and cleared together with AssignedPod.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	AssignedPodUID string `json:"assignedPodUID,omitempty"`

	// Endpoint is the stable Console Runtime access endpoint for a ready function or session
	// Run. It is absent for one-shot task Runs.
	// +optional
	Endpoint *RunEndpoint `json:"endpoint,omitempty"`

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
