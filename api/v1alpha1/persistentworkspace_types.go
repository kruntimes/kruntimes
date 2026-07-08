package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PersistentWorkspaceMode defines how a PersistentWorkspace is bound.
// +kubebuilder:validation:Enum=RuntimePodLocal
type PersistentWorkspaceMode string

const (
	// PersistentWorkspaceRuntimePodLocal stores workspace data under one Runtime Pod workspace volume.
	PersistentWorkspaceRuntimePodLocal PersistentWorkspaceMode = "RuntimePodLocal"
)

// PersistentWorkspaceCleanupPolicy defines how an unused workspace is cleaned up.
// +kubebuilder:validation:Enum=DeleteAfterTTL;Retain
type PersistentWorkspaceCleanupPolicy string

const (
	// PersistentWorkspaceDeleteAfterTTL deletes workspace data after the unused TTL expires.
	PersistentWorkspaceDeleteAfterTTL PersistentWorkspaceCleanupPolicy = "DeleteAfterTTL"
	// PersistentWorkspaceRetain leaves workspace data for an external actor to clean up.
	PersistentWorkspaceRetain PersistentWorkspaceCleanupPolicy = "Retain"
)

// PersistentWorkspacePhase is the lifecycle phase of a PersistentWorkspace.
// +kubebuilder:validation:Enum=Pending;Bound;Lost;Released
type PersistentWorkspacePhase string

const (
	PersistentWorkspacePending  PersistentWorkspacePhase = "Pending"
	PersistentWorkspaceBound    PersistentWorkspacePhase = "Bound"
	PersistentWorkspaceLost     PersistentWorkspacePhase = "Lost"
	PersistentWorkspaceReleased PersistentWorkspacePhase = "Released"
)

// +kubebuilder:object:generate=true
// PersistentWorkspaceSpec defines the desired state of PersistentWorkspace.
type PersistentWorkspaceSpec struct {
	// Runtime is the Runtime whose workspace volume backs this workspace.
	// The initial RuntimePodLocal mode binds to one pod of this Runtime.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`
	Runtime string `json:"runtime"`

	// Mode controls how the workspace is bound.
	// +kubebuilder:default=RuntimePodLocal
	// +optional
	Mode PersistentWorkspaceMode `json:"mode,omitempty"`

	// TTLSecondsAfterUnused is the number of seconds to retain workspace data
	// after the workspace becomes unused. It is ignored when cleanupPolicy is Retain.
	// +optional
	// +kubebuilder:validation:Minimum=0
	TTLSecondsAfterUnused *int32 `json:"ttlSecondsAfterUnused,omitempty"`

	// CleanupPolicy controls how workspace data is cleaned up.
	// +kubebuilder:default=DeleteAfterTTL
	// +optional
	CleanupPolicy PersistentWorkspaceCleanupPolicy `json:"cleanupPolicy,omitempty"`
}

// +kubebuilder:object:generate=true
// PersistentWorkspaceStatus defines the observed state of PersistentWorkspace.
type PersistentWorkspaceStatus struct {
	// Phase is the current lifecycle phase.
	// +kubebuilder:default=Pending
	Phase PersistentWorkspacePhase `json:"phase,omitempty"`

	// Runtime is the observed Runtime backing this workspace.
	// +optional
	Runtime string `json:"runtime,omitempty"`

	// BoundPod is the Runtime Pod currently backing this workspace.
	// It remains empty until workspace binding is implemented.
	// +optional
	BoundPod string `json:"boundPod,omitempty"`

	// Path is the runtime-local workspace path.
	// It remains empty until workspace binding is implemented.
	// +optional
	Path string `json:"path,omitempty"`

	// LastUsedTime is the last time a Run used this workspace.
	// +optional
	LastUsedTime *metav1.Time `json:"lastUsedTime,omitempty"`

	// Conditions represent the current state of the workspace lifecycle.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Runtime",type="string",JSONPath=".spec.runtime"
// +kubebuilder:printcolumn:name="Bound Pod",type="string",JSONPath=".status.boundPod"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=persistentworkspaces,scope=Namespaced,shortName=pw

// PersistentWorkspace is the Schema for the persistentworkspaces API.
type PersistentWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PersistentWorkspaceSpec   `json:"spec,omitempty"`
	Status PersistentWorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PersistentWorkspaceList contains a list of PersistentWorkspace.
type PersistentWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PersistentWorkspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PersistentWorkspace{}, &PersistentWorkspaceList{})
}
