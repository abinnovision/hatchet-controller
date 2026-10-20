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
	// +kubebuilder:default="8760h"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="duration(self) >= duration('24h')",message="tokenTTL must be at least 24h"
	// +kubebuilder:validation:XValidation:rule="duration(self) <= duration('8760h')",message="tokenTTL must be at most 8760h"
	// +optional
	TokenTTL *metav1.Duration `json:"tokenTTL,omitempty"`

	// Webhooks are incoming Hatchet webhooks managed for the tenant.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MaxItems=32
	// +optional
	Webhooks []Webhook `json:"webhooks,omitempty"`

	// Members are people invited to the tenant. The controller only manages members it invited.
	// +listType=map
	// +listMapKey=email
	// +kubebuilder:validation:MaxItems=100
	// +optional
	Members []Member `json:"members,omitempty"`
}

// Member is a person invited to the Hatchet tenant.
type Member struct {
	// Email of the person; it must be lowercase because Hatchet compares invite emails exactly.
	// +kubebuilder:validation:MaxLength=254
	// +kubebuilder:validation:Pattern=`^[^A-Z\s@]+@[^A-Z\s@]+\.[^A-Z\s@]+$`
	// +kubebuilder:validation:XValidation:rule="self == self.lowerAscii()",message="email must be lowercase"
	Email string `json:"email"`

	// Role of the person in the tenant; VIEWER needs Hatchet v0.112 or newer.
	// +kubebuilder:validation:Enum=OWNER;ADMIN;MEMBER;VIEWER
	// +kubebuilder:default=MEMBER
	// +optional
	Role string `json:"role,omitempty"`
}

// Webhook is an incoming Hatchet webhook. Credentials are read from Secrets in the tenant's namespace.
type Webhook struct {
	// Name of the webhook; it is part of the receiver URL.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Source selects the provider-specific payload handling.
	// +kubebuilder:validation:Enum=GENERIC;GITHUB;STRIPE;SLACK;LINEAR;SVIX
	// +kubebuilder:default=GENERIC
	// +optional
	Source string `json:"source,omitempty"`

	// EventKeyExpression is the CEL expression that derives the event key from the payload.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	EventKeyExpression string `json:"eventKeyExpression"`

	// ScopeExpression is the CEL expression that derives the event scope from the payload.
	// +kubebuilder:validation:MaxLength=1024
	// +optional
	ScopeExpression string `json:"scopeExpression,omitempty"`

	// StaticPayload is a JSON object sent with every event.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +kubebuilder:validation:Type=object
	// +optional
	StaticPayload *runtime.RawExtension `json:"staticPayload,omitempty"`

	// Auth selects how Hatchet authenticates incoming requests.
	Auth WebhookAuth `json:"auth"`
}

// WebhookAuth is a union: exactly one member is set.
// +kubebuilder:validation:XValidation:rule="(has(self.basic) ? 1 : 0) + (has(self.apiKey) ? 1 : 0) + (has(self.hmac) ? 1 : 0) == 1",message="exactly one of basic, apiKey or hmac must be set"
type WebhookAuth struct {
	// +optional
	Basic *WebhookBasicAuth `json:"basic,omitempty"`
	// +optional
	APIKey *WebhookAPIKeyAuth `json:"apiKey,omitempty"`
	// +optional
	HMAC *WebhookHMACAuth `json:"hmac,omitempty"`
}

// SecretKeyRef selects a key of a Secret in the tenant's namespace.
type SecretKeyRef struct {
	// Name of the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Key within the Secret.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Key string `json:"key"`
}

// WebhookBasicAuth authenticates requests with HTTP basic auth.
type WebhookBasicAuth struct {
	// Username expected in the Authorization header.
	// +kubebuilder:validation:MinLength=1
	Username string `json:"username"`

	// PasswordSecretRef selects the Secret key holding the password.
	PasswordSecretRef SecretKeyRef `json:"passwordSecretRef"`
}

// WebhookAPIKeyAuth authenticates requests with a static header value.
type WebhookAPIKeyAuth struct {
	// HeaderName carries the API key.
	// +kubebuilder:validation:MinLength=1
	HeaderName string `json:"headerName"`

	// APIKeySecretRef selects the Secret key holding the API key.
	APIKeySecretRef SecretKeyRef `json:"apiKeySecretRef"`
}

// WebhookHMACAuth authenticates requests with an HMAC signature of the body.
type WebhookHMACAuth struct {
	// Algorithm is the HMAC hash.
	// +kubebuilder:validation:Enum=SHA1;SHA256;SHA512;MD5
	Algorithm string `json:"algorithm"`

	// Encoding is the signature encoding.
	// +kubebuilder:validation:Enum=HEX;BASE64;BASE64URL
	Encoding string `json:"encoding"`

	// SignatureHeaderName carries the signature.
	// +kubebuilder:validation:MinLength=1
	SignatureHeaderName string `json:"signatureHeaderName"`

	// SigningSecretRef selects the Secret key holding the signing secret.
	SigningSecretRef SecretKeyRef `json:"signingSecretRef"`
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

	// Webhooks are the webhooks the controller created, with the auth they were created with.
	// +listType=map
	// +listMapKey=name
	// +optional
	Webhooks []WebhookStatus `json:"webhooks,omitempty"`

	// Members are the people the controller invited, which it may update and remove.
	// +listType=map
	// +listMapKey=email
	// +optional
	Members []MemberStatus `json:"members,omitempty"`

	// Conditions describe the current state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// WebhookStatus records a webhook created by the controller.
type WebhookStatus struct {
	// Name of the webhook.
	Name string `json:"name"`

	// Fingerprint identifies the source and auth the webhook was created with; it is a keyed hash and holds no secret material.
	Fingerprint string `json:"fingerprint"`
}

// MemberStatus records a person invited by the controller.
type MemberStatus struct {
	// Email of the person.
	Email string `json:"email"`

	// State is Invited while the invite is pending and Member once it was accepted.
	// +kubebuilder:validation:Enum=Invited;Member
	State string `json:"state"`

	// InviteExpiresAt is the expiry of the pending invite; it is only set while invited.
	// +optional
	InviteExpiresAt *metav1.Time `json:"inviteExpiresAt,omitempty"`
}

// Member states.
const (
	MemberStateInvited = "Invited"
	MemberStateMember  = "Member"
)

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
