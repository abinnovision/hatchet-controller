package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/hatchet"
	"github.com/abinnovision/hatchet-controller/internal/keyset"
)

// Hatchet webhook auth types.
const (
	authBasic  = "BASIC"
	authAPIKey = "API_KEY"
	authHMAC   = "HMAC"
)

// ensureWebhooks reconciles spec.webhooks against Hatchet and prunes webhooks removed from the spec.
// It records the webhooks it created in status and returns the first failure; a conditionError is retried when any failure is.
func (r *HatchetTenantReconciler) ensureWebhooks(ctx context.Context, hc hatchet.Client, tenant *hatchetv1alpha1.HatchetTenant,
	instance *hatchetv1alpha1.HatchetInstance) error {
	tenantID := tenant.Status.TenantID
	var first error
	retry := false
	fail := func(name string, err error) {
		err = webhookError(name, err)
		var cerr *conditionError
		retry = retry || !errors.As(err, &cerr) || cerr.retry
		if first == nil {
			first = err
		}
	}

	var fingerprintKey []byte
	if len(tenant.Spec.Webhooks) > 0 {
		var signing corev1.Secret
		key := client.ObjectKey{Namespace: instance.Namespace, Name: SigningKeySecretName(instance)}
		if err := r.Get(ctx, key, &signing); err != nil {
			return fmt.Errorf("reading signing key: %w", err)
		}
		fingerprintKey = signing.Data[keyset.PrivateKey]
	}

	desired := make(map[string]bool, len(tenant.Spec.Webhooks))
	for _, w := range tenant.Spec.Webhooks {
		desired[w.Name] = true
	}
	tracked := make(map[string]string, len(tenant.Status.Webhooks))
	next := make([]hatchetv1alpha1.WebhookStatus, 0, len(tenant.Spec.Webhooks))
	// Prune first so removed webhooks free their slots under the Hatchet webhook limit.
	for _, st := range tenant.Status.Webhooks {
		tracked[st.Name] = st.Fingerprint
		if desired[st.Name] {
			continue
		}
		if err := hc.DeleteWebhook(ctx, tenantID, st.Name); err != nil && !errors.Is(err, hatchet.ErrNotFound) {
			fail(st.Name, err)
			next = append(next, st)
		}
	}

	for _, w := range tenant.Spec.Webhooks {
		fp, err := r.ensureWebhook(ctx, hc, tenant, fingerprintKey, w, tracked[w.Name])
		if err != nil {
			fail(w.Name, err)
			if old, ok := tracked[w.Name]; ok {
				next = append(next, hatchetv1alpha1.WebhookStatus{Name: w.Name, Fingerprint: old})
			}
			continue
		}
		next = append(next, hatchetv1alpha1.WebhookStatus{Name: w.Name, Fingerprint: fp})
	}

	tenant.Status.Webhooks = next
	var cerr *conditionError
	if errors.As(first, &cerr) {
		cerr.retry = retry
	}
	return first
}

// ensureWebhook creates, updates or re-creates one webhook and returns the fingerprint of its auth.
func (r *HatchetTenantReconciler) ensureWebhook(ctx context.Context, hc hatchet.Client, tenant *hatchetv1alpha1.HatchetTenant,
	fingerprintKey []byte, w hatchetv1alpha1.Webhook, trackedFingerprint string) (string, error) {
	want, fp, err := r.resolveWebhook(ctx, tenant.Namespace, fingerprintKey, w)
	if err != nil {
		return "", err
	}
	tenantID := tenant.Status.TenantID

	got, err := hc.GetWebhook(ctx, tenantID, w.Name)
	switch {
	case errors.Is(err, hatchet.ErrNotFound):
	case err != nil:
		return "", hatchetError(err)
	case trackedFingerprint == fp && got.SourceName == want.SourceName && got.AuthType == want.AuthType:
		if webhookNeedsUpdate(want, got) {
			err = hc.UpdateWebhook(ctx, tenantID, w.Name, want.EventKeyExpression, want.ScopeExpression, want.StaticPayload)
			if err != nil {
				return "", hatchetError(err)
			}
		}
		return fp, nil
	default:
		// Source and auth cannot be patched, so the webhook is re-created from the spec.
		if err := hc.DeleteWebhook(ctx, tenantID, w.Name); err != nil && !errors.Is(err, hatchet.ErrNotFound) {
			return "", hatchetError(err)
		}
		r.Recorder.Eventf(tenant, nil, corev1.EventTypeNormal, "WebhookRecreated", "RecreateWebhook",
			"Re-created webhook %s because its source or auth changed or it was not created by the controller", w.Name)
	}

	if err := hc.CreateWebhook(ctx, tenantID, want); err != nil {
		return "", hatchetError(err)
	}
	return fp, nil
}

// resolveWebhook builds the Hatchet webhook with credentials read from Secrets and fingerprints its source and auth.
// The Secret cache only holds controller-labeled Secrets, so the reads bypass it.
func (r *HatchetTenantReconciler) resolveWebhook(ctx context.Context, namespace string, fingerprintKey []byte,
	w hatchetv1alpha1.Webhook) (hatchet.Webhook, string, error) {
	out := hatchet.Webhook{
		Name: w.Name, SourceName: w.Source, EventKeyExpression: w.EventKeyExpression, ScopeExpression: w.ScopeExpression,
	}
	if w.StaticPayload != nil && len(w.StaticPayload.Raw) > 0 {
		if err := json.Unmarshal(w.StaticPayload.Raw, &out.StaticPayload); err != nil {
			return out, "", &conditionError{reason: hatchetv1alpha1.ReasonInvalidSpec, message: "staticPayload is not a JSON object", err: err}
		}
	}

	parts := []string{out.SourceName}
	secret := func(ref hatchetv1alpha1.SecretKeyRef) (string, error) {
		value, err := r.secretValue(ctx, namespace, ref)
		parts = append(parts, value)
		return value, err
	}
	var err error
	switch a := w.Auth; {
	case a.Basic != nil:
		out.AuthType = authBasic
		out.Auth.Username = a.Basic.Username
		parts = append(parts, authBasic, a.Basic.Username)
		out.Auth.Password, err = secret(a.Basic.PasswordSecretRef)
	case a.APIKey != nil:
		out.AuthType = authAPIKey
		out.Auth.HeaderName = a.APIKey.HeaderName
		parts = append(parts, authAPIKey, a.APIKey.HeaderName)
		out.Auth.APIKey, err = secret(a.APIKey.APIKeySecretRef)
	case a.HMAC != nil:
		out.AuthType = authHMAC
		out.Auth.Algorithm, out.Auth.Encoding, out.Auth.SignatureHeaderName = a.HMAC.Algorithm, a.HMAC.Encoding, a.HMAC.SignatureHeaderName
		parts = append(parts, authHMAC, a.HMAC.Algorithm, a.HMAC.Encoding, a.HMAC.SignatureHeaderName)
		out.Auth.SigningSecret, err = secret(a.HMAC.SigningSecretRef)
	default:
		err = &conditionError{reason: hatchetv1alpha1.ReasonInvalidSpec, message: "auth must set one of basic, apiKey or hmac"}
	}
	if err != nil {
		return out, "", err
	}
	return out, fingerprint(fingerprintKey, parts...), nil
}

// secretValue reads a non-empty Secret key.
func (r *HatchetTenantReconciler) secretValue(ctx context.Context, namespace string, ref hatchetv1alpha1.SecretKeyRef) (string, error) {
	invalid := func(format string, args ...any) error {
		return &conditionError{reason: hatchetv1alpha1.ReasonInvalidSpec, message: fmt.Sprintf(format, args...)}
	}
	var secret corev1.Secret
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", invalid("secret %s not found", ref.Name)
		}
		return "", fmt.Errorf("reading secret %s: %w", ref.Name, err)
	}
	value, ok := secret.Data[ref.Key]
	if !ok {
		return "", invalid("secret %s has no key %s", ref.Name, ref.Key)
	}
	if len(value) == 0 {
		return "", invalid("secret %s key %s is empty", ref.Name, ref.Key)
	}
	return string(value), nil
}

// fingerprint is an HMAC of parts under key, so status never holds a brute-forceable hash of a credential.
func fingerprint(key []byte, parts ...string) string {
	h := hmac.New(sha256.New, key)
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// webhookNeedsUpdate reports whether the mutable fields of got differ from want.
func webhookNeedsUpdate(want, got hatchet.Webhook) bool {
	samePayload := len(want.StaticPayload) == 0 && len(got.StaticPayload) == 0 ||
		reflect.DeepEqual(want.StaticPayload, got.StaticPayload)
	return want.EventKeyExpression != got.EventKeyExpression || want.ScopeExpression != got.ScopeExpression || !samePayload
}

// webhookError names the webhook in a conditionError message; other errors are wrapped as they are.
func webhookError(name string, err error) error {
	var cerr *conditionError
	if !errors.As(err, &cerr) {
		return fmt.Errorf("webhook %s: %w", name, err)
	}
	out := *cerr
	out.message = fmt.Sprintf("webhook %s: %s", name, cerr.message)
	if out.reason == hatchetv1alpha1.ReasonHatchetError {
		out.reason = hatchetv1alpha1.ReasonWebhookFailed
	}
	return &out
}
