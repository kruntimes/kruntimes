// Package v1alpha1 contains API Schema definitions for the kruntimes v1alpha1 API group.
// +groupName=runtimes.kruntimes.io
// +versionName=v1alpha1
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "runtimes.kruntimes.io", Version: "v1alpha1"}

	//nolint:staticcheck // scheme.Builder is deliberately used for kubebuilder integration
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
