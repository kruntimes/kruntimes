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
// +kubebuilder:validation:XValidation:rule="self.template.spec.containers.size() > 0 && self.template.spec.containers[0].name == 'runtime'",message="template.spec.containers[0] must be named runtime"
// +kubebuilder:validation:XValidation:rule="self.template.spec.containers.size() == 0 || (self.template.spec.containers[0].image.size() > 0 && self.template.spec.containers[0].image.size() <= 2048)",message="the runtime container image must contain between 1 and 2048 characters"
// +kubebuilder:validation:XValidation:rule="!has(self.template.spec.restartPolicy) || self.template.spec.restartPolicy == 'Always'",message="template.spec.restartPolicy must be Always"
// +kubebuilder:validation:XValidation:rule="!has(self.template.spec.ephemeralContainers) || self.template.spec.ephemeralContainers.size() == 0",message="template.spec.ephemeralContainers is not supported in a Deployment"
// +kubebuilder:validation:XValidation:rule="!has(self.template.spec.serviceAccountName) || self.template.spec.serviceAccountName.matches('^[a-z0-9]([-a-z0-9]*[a-z0-9])?([.][a-z0-9]([-a-z0-9]*[a-z0-9])?)*$')",message="template.spec.serviceAccountName must be a valid DNS subdomain"
type RuntimeSpec struct {
	// Template defines the Runtime Pod. The first container must be named
	// "runtime". The controller injects the "runtimed" sidecar and the
	// "workspace" and "artifact-store" volumes.
	// +kubebuilder:validation:Required
	Template corev1.PodTemplateSpec `json:"template"`

	// Port is the gRPC port the runtime server listens on.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=9091
	Port int32 `json:"port,omitempty"`

	// Replicas is the number of runtime pods to run.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`

	// DaemonImage overrides the runtimed daemon image.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	DaemonImage string `json:"daemonImage,omitempty"`

	// Capacity declares the per-pod capacity for this runtime.
	// +optional
	Capacity *RuntimeCapacity `json:"capacity,omitempty"`

	// ArtifactStore configures durable artifact storage for Runs executed by this Runtime.
	// +optional
	ArtifactStore *RuntimeArtifactStoreSpec `json:"artifactStore,omitempty"`

	// Workspace configures the shared workspace volume mounted into the runtime pod.
	// +optional
	Workspace *RuntimeWorkspaceSpec `json:"workspace,omitempty"`
}

// +kubebuilder:object:generate=true
// RuntimeArtifactStoreSpec configures the artifact store available to runtimed.
// +kubebuilder:validation:XValidation:rule="(self.driver == 'filesystem' && has(self.filesystem) && !has(self.s3)) || (self.driver == 's3' && has(self.s3) && !has(self.filesystem))",message="exactly one artifact store configuration matching driver must be set"
type RuntimeArtifactStoreSpec struct {
	// Driver identifies the configured artifact storage backend.
	Driver ArtifactDriver `json:"driver"`

	// Filesystem configures a PVC-backed filesystem artifact store.
	// +optional
	Filesystem *FilesystemArtifactStoreSpec `json:"filesystem,omitempty"`

	// S3 configures an S3-compatible object artifact store.
	// +optional
	S3 *S3ArtifactStoreSpec `json:"s3,omitempty"`
}

// +kubebuilder:object:generate=true
// FilesystemArtifactStoreSpec configures a PVC-backed artifact store.
type FilesystemArtifactStoreSpec struct {
	// VolumeClaimName is the PVC mounted into runtimed for durable artifact storage.
	// +kubebuilder:validation:MinLength=1
	VolumeClaimName string `json:"volumeClaimName"`
}

// +kubebuilder:object:generate=true
// S3ArtifactStoreSpec configures an S3-compatible artifact store.
type S3ArtifactStoreSpec struct {
	// Bucket is the S3 bucket used to store artifacts.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Pattern=`^[^/]+$`
	Bucket string `json:"bucket"`

	// Prefix is prepended to generated artifact object keys.
	// +optional
	// +kubebuilder:validation:MaxLength=1024
	Prefix string `json:"prefix,omitempty"`

	// Region overrides the AWS SDK region.
	// +optional
	// +kubebuilder:validation:MaxLength=128
	Region string `json:"region,omitempty"`

	// Endpoint configures a custom S3-compatible service endpoint.
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Endpoint string `json:"endpoint,omitempty"`

	// ForcePathStyle enables path-style S3 addressing.
	// +optional
	ForcePathStyle bool `json:"forcePathStyle,omitempty"`

	// CredentialsSecretName names a Secret whose keys are exposed to runtimed
	// as environment variables for the AWS SDK credential chain.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +optional
	CredentialsSecretName string `json:"credentialsSecretName,omitempty"`

	// UploadPartSize overrides the multipart upload part size in bytes.
	// +kubebuilder:validation:Minimum=5242880
	// +optional
	UploadPartSize int64 `json:"uploadPartSize,omitempty"`

	// UploadConcurrency overrides the number of concurrent multipart upload workers.
	// +kubebuilder:validation:Minimum=1
	// +optional
	UploadConcurrency int32 `json:"uploadConcurrency,omitempty"`
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
// RuntimeWorkspaceSpec configures the shared workspace volume used by Runs.
// +kubebuilder:validation:XValidation:rule="(has(self.hostPath)?1:0) + (has(self.emptyDir)?1:0) + (has(self.gcePersistentDisk)?1:0) + (has(self.awsElasticBlockStore)?1:0) + (has(self.gitRepo)?1:0) + (has(self.secret)?1:0) + (has(self.nfs)?1:0) + (has(self.iscsi)?1:0) + (has(self.glusterfs)?1:0) + (has(self.persistentVolumeClaim)?1:0) + (has(self.rbd)?1:0) + (has(self.flexVolume)?1:0) + (has(self.cinder)?1:0) + (has(self.cephfs)?1:0) + (has(self.flocker)?1:0) + (has(self.downwardAPI)?1:0) + (has(self.fc)?1:0) + (has(self.azureFile)?1:0) + (has(self.configMap)?1:0) + (has(self.vsphereVolume)?1:0) + (has(self.quobyte)?1:0) + (has(self.azureDisk)?1:0) + (has(self.photonPersistentDisk)?1:0) + (has(self.projected)?1:0) + (has(self.portworxVolume)?1:0) + (has(self.scaleIO)?1:0) + (has(self.storageos)?1:0) + (has(self.csi)?1:0) + (has(self.ephemeral)?1:0) + (has(self.image)?1:0) <= 1",message="at most one workspace volume source may be set"
type RuntimeWorkspaceSpec struct {
	// VolumeSource configures the Kubernetes volume backing the shared workspace.
	// The fields are inlined so Runtime specs can use the same shape as
	// corev1.VolumeSource, for example workspace.persistentVolumeClaim.
	corev1.VolumeSource `json:",inline"`
}

// +kubebuilder:object:generate=true
// RuntimeStatus defines the observed state of Runtime.
type RuntimeStatus struct {
	// ReadyReplicas is the number of pods that are ready.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Conditions represent the latest available observations of the Runtime's state.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Image",type="string",JSONPath=".spec.template.spec.containers[0].image"
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
