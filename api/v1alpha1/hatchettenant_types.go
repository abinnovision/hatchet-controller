package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// InstanceRef references a HatchetInstance.
type InstanceRef struct {
	// Name of the HatchetInstance.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Namespace of the HatchetInstance.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace"`
}

// HatchetTenantSpec defines the desired Hatchet tenant.
// +kubebuilder:validation:XValidation:rule="self.instanceRef == oldSelf.instanceRef",message="instanceRef is immutable"
// +kubebuilder:validation:XValidation:rule="has(self.secretName) == has(oldSelf.secretName) && (!has(self.secretName) || self.secretName == oldSelf.secretName)",message="secretName is immutable"
type HatchetTenantSpec struct {
	// InstanceRef references the HatchetInstance hosting the tenant.
	InstanceRef InstanceRef `json:"instanceRef"`

	// DisplayName is the Hatchet tenant name; defaults to metadata.name.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// SecretName is the worker Secret name; defaults to <name>-hatchet.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// TokenTTL is the lifetime of issued API tokens; rotation happens at 2/3 of it.
	// The API server defaults it, so it is set on objects read from the cluster.
	// +kubebuilder:default="720h"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('24h')",message="tokenTTL must be at least 24h"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('8760h')",message="tokenTTL must be at most 8760h"
	// +optional
	TokenTTL *metav1.Duration `json:"tokenTTL,omitempty"`
}

// HatchetTenantStatus defines the observed state of HatchetTenant.
type HatchetTenantStatus struct {
	// ObservedGeneration is the generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// TenantID mirrors the tenant-id annotation once bound.
	// +kubebuilder:validation:MaxLength=36
	// +optional
	TenantID string `json:"tenantID,omitempty"`

	// Slug is the Hatchet tenant slug.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Slug string `json:"slug,omitempty"`

	// TokenExpiresAt is the expiry of the token in the worker Secret.
	// +optional
	TokenExpiresAt *metav1.Time `json:"tokenExpiresAt,omitempty"`

	// Conditions describe the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// HatchetTenant declares a Hatchet tenant and its worker credential Secret.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ht,categories=hatchet
// +kubebuilder:validation:XValidation:rule="size(self.metadata.name) <= 63",message="metadata.name must be at most 63 characters"
// +kubebuilder:printcolumn:name="Instance",type=string,JSONPath=`.spec.instanceRef.name`
// +kubebuilder:printcolumn:name="Slug",type=string,JSONPath=`.status.slug`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HatchetTenant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HatchetTenantSpec   `json:"spec"`
	Status HatchetTenantStatus `json:"status,omitempty"`
}

// HatchetTenantList contains a list of HatchetTenant.
// +kubebuilder:object:root=true
type HatchetTenantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HatchetTenant `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &HatchetTenant{}, &HatchetTenantList{})
		return nil
	})
}
