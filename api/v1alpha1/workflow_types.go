package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:generate=true
// WorkflowInputSpec defines one reusable Workflow input.
type WorkflowInputSpec struct {
	// Type is the input type.
	// +kubebuilder:default=string
	// +kubebuilder:validation:Enum=string
	// +optional
	Type string `json:"type,omitempty"`

	// Required marks the input as required.
	// +optional
	Required bool `json:"required,omitempty"`

	// Default is the default string value used when the caller does not pass this input.
	// +optional
	// +kubebuilder:validation:MaxLength=8192
	Default string `json:"default,omitempty"`
}

// +kubebuilder:object:generate=true
// WorkflowOutputSpec defines one reusable Workflow output.
type WorkflowOutputSpec struct {
	// Value is a ${{ }} expression evaluated after the Workflow jobs run.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=8192
	Value string `json:"value"`
}

// +kubebuilder:object:generate=true
// WorkflowSpec defines a reusable Workflow definition.
// +kubebuilder:validation:XValidation:rule="self.jobs.all(name, !has(self.jobs[name].needs) || self.jobs[name].needs.all(need, need in self.jobs && need != name))",message="each dependency must name another job in this workflow"
type WorkflowSpec struct {
	// Inputs defines parameters accepted by this Workflow.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Inputs map[string]WorkflowInputSpec `json:"inputs,omitempty"`

	// Outputs defines values exposed by this Workflow after its jobs complete.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Outputs map[string]WorkflowOutputSpec `json:"outputs,omitempty"`

	// Jobs is a map of reusable job names to job specs. Jobs run in parallel
	// unless constrained by needs; the map order is not significant.
	// Job names become Kubernetes label values on child Runs when a WorkflowRun
	// executes this definition.
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(name, size(name) <= 63 && name.matches('^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$'))",message="job names must be valid Kubernetes label values with at most 63 characters"
	Jobs map[string]JobSpec `json:"jobs"`
}

// +kubebuilder:object:generate=true
// JobSpec defines a single job within a workflow.
// +kubebuilder:validation:XValidation:rule="has(self.steps) != has(self.uses)",message="exactly one of steps or uses must be set"
// +kubebuilder:validation:XValidation:rule="!has(self.with) || has(self.uses)",message="with can only be set when uses is set"
// +kubebuilder:validation:XValidation:rule="!has(self.uses) || !has(self.outputs)",message="outputs may not be set when uses is set"
type JobSpec struct {
	// RunsOn is the runtime to use for all steps in this job (e.g., "bash", "python").
	// Runtime names are propagated to Kubernetes label values.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`
	RunsOn string `json:"runs-on,omitempty"`

	// Needs is the list of job names that must complete before this job starts.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=63
	Needs []string `json:"needs,omitempty"`

	// Steps is the list of steps to run sequentially within the job.
	// +optional
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	Steps []StepSpec `json:"steps,omitempty"`

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

	// Outputs maps job-level output names to ${{ }} expressions.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Outputs map[string]string `json:"outputs,omitempty"`
}

// +kubebuilder:object:generate=true
// StepSpec defines a single step within a job.
// +kubebuilder:validation:XValidation:rule="has(self.run) && !has(self.uses)",message="run must be set and uses is not supported yet"
type StepSpec struct {
	// Name of the step.
	// +kubebuilder:validation:Required
	// Step names become Kubernetes label values on child Runs.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`
	Name string `json:"name"`

	// Run is the inline command/script to execute.
	// +optional
	// Match the Run inline-source limit.
	// +kubebuilder:validation:MaxLength=262144
	Run string `json:"run,omitempty"`

	// Args is positional arguments for the step.
	// +optional
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MaxLength=8192
	Args []string `json:"args,omitempty"`

	// Env is extra environment variables for the step.
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	Env map[string]string `json:"env,omitempty"`

	// Uses references an Action or WorkflowTemplate (future).
	// +optional
	// +kubebuilder:validation:MaxLength=2048
	Uses string `json:"uses,omitempty"`

	// With passes parameters to an Action (future).
	// +optional
	// +kubebuilder:validation:MaxProperties=256
	With map[string]string `json:"with,omitempty"`
}

// +kubebuilder:object:generate=true
// WorkflowStatus defines the observed state of a reusable Workflow definition.
type WorkflowStatus struct {
	// Conditions represent definition-level validation and readiness.
	// +optional
	// +kubebuilder:validation:MaxItems=16
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=workflows,scope=Namespaced,shortName=wf

// Workflow is the Schema for reusable workflow definitions.
type Workflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WorkflowSpec   `json:"spec,omitempty"`
	Status WorkflowStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// WorkflowList contains a list of Workflow.
type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workflow `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workflow{}, &WorkflowList{})
}
