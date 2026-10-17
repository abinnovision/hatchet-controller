package controller

import (
	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
)

// SigningKeySecretName is the Secret holding the instance's private keyset.
func SigningKeySecretName(instance *hatchetv1alpha1.HatchetInstance) string {
	return instance.Name + "-signing-key"
}

// PublicKeysetSecretName is the Secret holding the instance's public keyset.
func PublicKeysetSecretName(instance *hatchetv1alpha1.HatchetInstance) string {
	return instance.Name + "-exchange-keyset"
}

// WorkerSecretName is the worker Secret of a tenant.
func WorkerSecretName(tenant *hatchetv1alpha1.HatchetTenant) string {
	if tenant.Spec.SecretName != "" {
		return tenant.Spec.SecretName
	}
	return tenant.Name + "-hatchet"
}

// NetworkPolicyName is the egress NetworkPolicy of a tenant.
func NetworkPolicyName(tenant *hatchetv1alpha1.HatchetTenant) string {
	return "hatchet-worker-egress-" + tenant.Name
}
