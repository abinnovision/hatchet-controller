// Package v1alpha1 contains API Schema definitions for the hatchet-controller v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=hatchet-controller.abigroup.io
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "hatchet-controller.abigroup.io", Version: "v1alpha1"}

	// SchemeBuilder collects the functions that add this group-version's types to a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(func(s *runtime.Scheme) error {
		metav1.AddToGroupVersion(s, GroupVersion)
		return nil
	})

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
