package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:generate=true
// WorkflowRunSpec defines one workflow execution instance.
// +kubebuilder:validation:XValidation:rule="has(self.jobs) != has(self.uses)",message="exactly one of jobs or uses must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.with) || has(self.uses)",message="with can only be set when uses is set"
// +kubebuilder:validation:XValidation:rule="!has(self.jobs) || self.jobs.all(name, !has(self.jobs[name].needs) || self.jobs[name].needs.all(need, need in self.jobs && need != name))",message="each dependency must name another job in this WorkflowRun"
type WorkflowRunSpec struct {
	// Jobs is a map of inline job names to job specs. Jobs run in parallel
	// unless constrained by needs; the map order is not significant.
	// Job names become Kubernetes label values on child Runs.
	// +optional
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(name, size(name) <= 63 && name.matches('^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$'))",message="job names must be valid Kubernetes label values with at most 63 characters"
	Jobs map[string]JobSpec `json:"jobs,omitempty"`

	// Uses references a reusable Workflow in the same namespace.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Uses string `json:"uses,omitempty"`

	// With passes string inputs to the reusable Workflow referenced by Uses.
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	With map[string]string `json:"with,omitempty"`
}

// +kubebuilder:object:generate=true
// WorkflowRunStatus defines the observed state of a WorkflowRun.
type WorkflowRunStatus struct {
	// Phase is the current lifecycle phase of this workflow execution.
	// +kubebuilder:default=Pending
	Phase WorkflowPhase `json:"phase"`

	// Jobs tracks the status of each job by name.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Jobs map[string]JobStatus `json:"jobs,omitempty"`

	// Message is a human-readable status or error message.
	// +optional
	// +kubebuilder:validation:MaxLength=4096
	Message string `json:"message,omitempty"`

	// Conditions represent the current state of the WorkflowRun lifecycle.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=workflowruns,scope=Namespaced,shortName=wfr

// WorkflowRun is the Schema for workflow execution instances.
type WorkflowRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkflowRunSpec   `json:"spec,omitempty"`
	Status WorkflowRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkflowRunList contains a list of WorkflowRun.
type WorkflowRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkflowRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkflowRun{}, &WorkflowRunList{})
}
