package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// WorkflowPhase is the lifecycle phase of a Workflow.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type WorkflowPhase string

const (
	WorkflowPending   WorkflowPhase = "Pending"
	WorkflowRunning   WorkflowPhase = "Running"
	WorkflowSucceeded WorkflowPhase = "Succeeded"
	WorkflowFailed    WorkflowPhase = "Failed"
)

// JobPhase is the lifecycle phase of a job within a workflow.
// +kubebuilder:validation:Enum=Pending;Waiting;Running;Succeeded;Failed
type JobPhase string

const (
	JobPending   JobPhase = "Pending"
	JobWaiting   JobPhase = "Waiting"
	JobRunning   JobPhase = "Running"
	JobSucceeded JobPhase = "Succeeded"
	JobFailed    JobPhase = "Failed"
)

// StepPhase is the lifecycle phase of a step within a job.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type StepPhase string

const (
	StepPending   StepPhase = "Pending"
	StepRunning   StepPhase = "Running"
	StepSucceeded StepPhase = "Succeeded"
	StepFailed    StepPhase = "Failed"
)

// +kubebuilder:object:generate=true
// WorkflowSpec defines the desired state of Workflow.
type WorkflowSpec struct {
	// Jobs is the list of jobs in the workflow.
	Jobs []JobSpec `json:"jobs"`
}

// +kubebuilder:object:generate=true
// JobSpec defines a single job within a workflow.
type JobSpec struct {
	// Name of the job.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Needs is the list of job names that must complete before this job starts.
	// +optional
	Needs []string `json:"needs,omitempty"`

	// Steps is the list of steps to run sequentially within the job.
	// +kubebuilder:validation:MinItems=1
	Steps []StepSpec `json:"steps"`

	// Outputs maps job-level output names to ${{ }} expressions.
	// +optional
	Outputs map[string]string `json:"outputs,omitempty"`
}

// +kubebuilder:object:generate=true
// StepSpec defines a single step within a job.
type StepSpec struct {
	// Name of the step.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Run is the inline command/script to execute.
	// +optional
	Run string `json:"run,omitempty"`

	// Args is positional arguments for the step.
	// +optional
	Args []string `json:"args,omitempty"`

	// Runtime is the runtime type to use for this step.
	// Defaults to the workflow-level runtime.
	// +optional
	Runtime string `json:"runtime,omitempty"`

	// Env is extra environment variables for the step.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// Uses references an Action or WorkflowTemplate (future).
	// +optional
	Uses string `json:"uses,omitempty"`

	// With passes parameters to an Action (future).
	// +optional
	With map[string]string `json:"with,omitempty"`
}

// +kubebuilder:object:generate=true
// WorkflowStatus defines the observed state of Workflow.
type WorkflowStatus struct {
	// Phase is the current lifecycle phase of the workflow.
	// +kubebuilder:default=Pending
	Phase WorkflowPhase `json:"phase"`

	// Jobs tracks the status of each job by name.
	// +optional
	Jobs map[string]JobStatus `json:"jobs,omitempty"`

	// Message is a human-readable status or error message.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:generate=true
// JobStatus tracks the status of a job.
type JobStatus struct {
	// Phase is the current phase of the job.
	Phase JobPhase `json:"phase"`

	// Steps tracks the status of each step by name.
	// +optional
	Steps map[string]StepStatus `json:"steps,omitempty"`
}

// +kubebuilder:object:generate=true
// StepStatus tracks the status of a step.
type StepStatus struct {
	// Phase is the current phase of the step.
	Phase StepPhase `json:"phase"`

	// RunName is the name of the Run CRD created for this step.
	// +optional
	RunName string `json:"runName,omitempty"`

	// Outputs is key-value pairs exposed by this step.
	// +optional
	Outputs map[string]string `json:"outputs,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:resource:path=workflows,scope=Namespaced,shortName=wf

// Workflow is the Schema for the workflows API.
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
