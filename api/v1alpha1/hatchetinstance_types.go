package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// TLSStrategy is the worker TLS strategy passed as HATCHET_CLIENT_TLS_STRATEGY.
// +kubebuilder:validation:Enum=none;tls;mtls
type TLSStrategy string

const (
	TLSStrategyNone TLSStrategy = "none"
	TLSStrategyTLS  TLSStrategy = "tls"
	TLSStrategyMTLS TLSStrategy = "mtls"
)

// FromNamespaces selects which namespaces may reference an instance.
// +kubebuilder:validation:Enum=Same;Selector;All
type FromNamespaces string

const (
	NamespacesFromSame     FromNamespaces = "Same"
	NamespacesFromSelector FromNamespaces = "Selector"
	NamespacesFromAll      FromNamespaces = "All"
)

// WorkerSpec holds the connection settings written into worker Secrets.
type WorkerSpec struct {
	// HostPort is the Hatchet engine gRPC address (HATCHET_CLIENT_HOST_PORT).
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	HostPort string `json:"hostPort"`

	// TLSStrategy is the worker TLS strategy (HATCHET_CLIENT_TLS_STRATEGY).
	// +kubebuilder:default=tls
	// +optional
	TLSStrategy TLSStrategy `json:"tlsStrategy,omitempty"`

	// TLSRootCA is an optional PEM bundle (HATCHET_CLIENT_TLS_ROOT_CA).
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	TLSRootCA string `json:"tlsRootCA,omitempty"`
}

// AllowedNamespaces controls which namespaces may reference an instance.
// +kubebuilder:validation:XValidation:rule="self.from != 'Selector' || has(self.selector)",message="selector is required when from is Selector"
type AllowedNamespaces struct {
	// From selects the allowed namespaces.
	// +kubebuilder:default=Same
	// +optional
	From FromNamespaces `json:"from,omitempty"`

	// Selector matches allowed namespaces when From is Selector.
	// +optional
	Selector *metav1.LabelSelector `json:"selector,omitempty"`
}

// NetworkPolicyPorts overrides the target ports of the egress NetworkPolicy.
type NetworkPolicyPorts struct {
	// Engine is the engine pod port; defaults to the port of worker.hostPort.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Engine int32 `json:"engine,omitempty"`

	// API is the API pod port; defaults to the port of apiURL.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	API int32 `json:"api,omitempty"`
}

// NetworkPolicySpec enables an egress NetworkPolicy for worker pods in tenant namespaces.
type NetworkPolicySpec struct {
	// EngineSelector matches the Hatchet engine pods in the instance namespace.
	// +kubebuilder:validation:MinProperties=1
	EngineSelector map[string]string `json:"engineSelector"`

	// APISelector matches the Hatchet API pods in the instance namespace.
	// +kubebuilder:validation:MinProperties=1
	APISelector map[string]string `json:"apiSelector"`

	// Ports overrides the target pod ports.
	// +optional
	Ports *NetworkPolicyPorts `json:"ports,omitempty"`
}

// HatchetInstanceSpec defines a Hatchet installation that tenants can reference.
type HatchetInstanceSpec struct {
	// APIURL is the Hatchet API base URL (also HATCHET_CLIENT_SERVER_URL).
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:Pattern=`^https?://`
	APIURL string `json:"apiURL"`

	// ServiceUserID is the UUID of the Hatchet user the controller acts as.
	// +kubebuilder:validation:Format=uuid
	// +kubebuilder:validation:MaxLength=36
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="serviceUserID is immutable"
	ServiceUserID string `json:"serviceUserID"`

	// Worker holds the connection settings written into worker Secrets.
	Worker WorkerSpec `json:"worker"`

	// AllowedNamespaces controls which namespaces may reference this instance.
	// +kubebuilder:default={from: Same}
	// +optional
	AllowedNamespaces AllowedNamespaces `json:"allowedNamespaces,omitempty"`

	// NetworkPolicy enables an egress NetworkPolicy per tenant; absent means disabled.
	// +optional
	NetworkPolicy *NetworkPolicySpec `json:"networkPolicy,omitempty"`
}

// HatchetInstanceStatus defines the observed state of HatchetInstance.
type HatchetInstanceStatus struct {
	// ObservedGeneration is the generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// PublicKeysetSecret is the Secret holding the public keyset (key `keyset`).
	// +kubebuilder:validation:MaxLength=253
	// +optional
	PublicKeysetSecret string `json:"publicKeysetSecret,omitempty"`

	// Conditions describe the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// HatchetInstance is a Hatchet installation that HatchetTenants reference.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=hi,categories=hatchet
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="API",type=string,JSONPath=`.spec.apiURL`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type HatchetInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HatchetInstanceSpec   `json:"spec"`
	Status HatchetInstanceStatus `json:"status,omitempty"`
}

// HatchetInstanceList contains a list of HatchetInstance.
// +kubebuilder:object:root=true
type HatchetInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HatchetInstance `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &HatchetInstance{}, &HatchetInstanceList{})
		return nil
	})
}
