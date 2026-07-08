package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// ActionInputType is the supported type of an Action input.
// +kubebuilder:validation:Enum=string
type ActionInputType string

const (
	// ActionInputString is a string input.
	ActionInputString ActionInputType = "string"
)

// +kubebuilder:object:generate=true
// ActionInputSpec defines one reusable Action input.
type ActionInputSpec struct {
	// Type is the input type.
	// +kubebuilder:default=string
	// +optional
	Type ActionInputType `json:"type,omitempty"`

	// Required marks the input as required.
	// +optional
	Required bool `json:"required,omitempty"`

	// Default is the default string value used when the caller does not pass this input.
	// +optional
	// +kubebuilder:validation:MaxLength=8192
	Default string `json:"default,omitempty"`
}

// +kubebuilder:object:generate=true
// ActionOutputSpec defines one reusable Action output.
type ActionOutputSpec struct {
	// Value is a ${{ }} expression evaluated after the Action steps run.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=8192
	Value string `json:"value"`
}

// +kubebuilder:object:generate=true
// ActionSpec defines the desired state of Action.
type ActionSpec struct {
	// Inputs defines parameters accepted by this Action.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Inputs map[string]ActionInputSpec `json:"inputs,omitempty"`

	// Outputs defines values exposed by this Action after its steps complete.
	// +optional
	// +kubebuilder:validation:MaxProperties=64
	Outputs map[string]ActionOutputSpec `json:"outputs,omitempty"`

	// Steps is the reusable step sequence. In the first version, Action steps
	// support run steps only; nested uses remains a future extension.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=128
	Steps []StepSpec `json:"steps"`
}

// +kubebuilder:object:generate=true
// ActionStatus defines the observed state of Action.
type ActionStatus struct {
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
// +kubebuilder:resource:path=actions,scope=Namespaced

// Action is the Schema for the actions API.
type Action struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ActionSpec   `json:"spec,omitempty"`
	Status ActionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ActionList contains a list of Action.
type ActionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Action `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Action{}, &ActionList{})
}
