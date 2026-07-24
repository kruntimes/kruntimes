package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// WorkflowPhase is the lifecycle phase of a WorkflowRun execution.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Cancelled
type WorkflowPhase string

const (
	WorkflowPending   WorkflowPhase = "Pending"
	WorkflowRunning   WorkflowPhase = "Running"
	WorkflowSucceeded WorkflowPhase = "Succeeded"
	WorkflowFailed    WorkflowPhase = "Failed"
	WorkflowCancelled WorkflowPhase = "Cancelled"
)

// JobPhase is the lifecycle phase of a job within a WorkflowRun.
// +kubebuilder:validation:Enum=Pending;Waiting;Running;Succeeded;Failed;Skipped
type JobPhase string

const (
	JobPending   JobPhase = "Pending"
	JobWaiting   JobPhase = "Waiting"
	JobRunning   JobPhase = "Running"
	JobSucceeded JobPhase = "Succeeded"
	JobFailed    JobPhase = "Failed"
	JobSkipped   JobPhase = "Skipped"
)

// StepPhase is the lifecycle phase of a step within a WorkflowRun job.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type StepPhase string

const (
	StepPending   StepPhase = "Pending"
	StepRunning   StepPhase = "Running"
	StepSucceeded StepPhase = "Succeeded"
	StepFailed    StepPhase = "Failed"
)

const (
	// WorkflowRunAcceptedCondition reports whether the WorkflowRun was accepted by the controller.
	WorkflowRunAcceptedCondition = "Accepted"

	// WorkflowRunUIDLabel identifies direct child resources owned by a WorkflowRun.
	WorkflowRunUIDLabel = "kruntimes.io/workflowrun-uid"
	// WorkflowJobLabel identifies the workflow job that owns a child Run.
	WorkflowJobLabel = "kruntimes.io/workflow-job"
	// WorkflowStepLabel identifies the workflow step that owns a child Run.
	WorkflowStepLabel = "kruntimes.io/workflow-step"
	// WorkflowOutputAnnotationPrefix identifies frozen reusable Workflow output
	// expressions on a materialized child WorkflowRun. The suffix is the output
	// name from the source Workflow.
	WorkflowOutputAnnotationPrefix = "kruntimes.io/workflow-output."
)

// +kubebuilder:object:generate=true
// WorkflowRunSpec defines one workflow execution instance.
// +kubebuilder:validation:XValidation:rule="self.jobs.all(name, !has(self.jobs[name].needs) || self.jobs[name].needs.all(need, need in self.jobs && need != name))",message="each dependency must name another job in this WorkflowRun"
// +kubebuilder:validation:XValidation:rule="self.jobs == oldSelf.jobs",message="jobs are immutable after WorkflowRun creation"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.cancelRequested) || !oldSelf.cancelRequested || (has(self.cancelRequested) && self.cancelRequested)",message="cancelRequested may not transition from true to false"
type WorkflowRunSpec struct {
	// Jobs is a map of inline job names to job specs. Jobs run in parallel
	// unless constrained by needs; the map order is not significant.
	// Job names become Kubernetes label values on child Runs.
	// +required
	// +kubebuilder:validation:MinProperties=1
	// +kubebuilder:validation:MaxProperties=64
	// +kubebuilder:validation:XValidation:rule="self.all(name, size(name) <= 63 && name.matches('^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$'))",message="job names must be valid Kubernetes label values with at most 63 characters"
	Jobs map[string]JobSpec `json:"jobs,omitempty"`

	// CancelRequested requests cancellation of this WorkflowRun. Once observed,
	// the controller stops creating child Runs and requests cancellation of
	// active child Runs.
	// +optional
	CancelRequested bool `json:"cancelRequested,omitempty"`
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

	// SnapshotName is the name of the immutable ControllerRevision that captures
	// the WorkflowRun's resolved execution definitions.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	SnapshotName string `json:"snapshotName,omitempty"`

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

// +kubebuilder:object:generate=true
// JobStatus tracks the execution status of a WorkflowRun job.
type JobStatus struct {
	// Phase is the current phase of the job.
	Phase JobPhase `json:"phase"`

	// Pre is the resolved list of predecessor jobs that must complete before
	// this job can start.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=63
	Pre []string `json:"pre,omitempty"`

	// WorkflowRunName is the child WorkflowRun created for a job-level reusable
	// Workflow call. It is empty for inline jobs.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	WorkflowRunName string `json:"workflowRunName,omitempty"`

	// Outputs is the bounded key-value output map exposed by this job. Inline
	// jobs derive values from their steps; reusable Workflow calls derive values
	// from their child WorkflowRun output contract.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Outputs map[string]string `json:"outputs,omitempty"`

	// Steps tracks each step in the original job step order.
	// +optional
	// +kubebuilder:validation:MaxItems=128
	Steps []StepStatus `json:"steps,omitempty"`
}

// +kubebuilder:object:generate=true
// StepStatus tracks the execution status of a WorkflowRun step.
type StepStatus struct {
	// Name is the step name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`
	Name string `json:"name"`

	// Phase is the current phase of the step.
	Phase StepPhase `json:"phase"`

	// RunName is the name of the Run CRD created for this step.
	// +optional
	RunName string `json:"runName,omitempty"`

	// Outputs is key-value pairs exposed by this step.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Outputs map[string]string `json:"outputs,omitempty"`
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
