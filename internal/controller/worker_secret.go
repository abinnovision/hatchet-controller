package controller

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/hatchet"
)

// controllerSlug matches slugs in the controller format <namespace>.<name>-<uid8>.
var controllerSlug = regexp.MustCompile(`^[a-z0-9-]{1,63}\.[a-z0-9.-]+-[0-9a-f]{8}$`)

// tenantSlug returns the Hatchet slug <namespace>.<name>-<first 8 chars of the CR UID>.
func tenantSlug(tenant *hatchetv1alpha1.HatchetTenant) string {
	uid := string(tenant.UID)
	if len(uid) > 8 {
		uid = uid[:8]
	}
	return fmt.Sprintf("%s.%s-%s", tenant.Namespace, tenant.Name, uid)
}

// tenantDisplayName returns spec.displayName, or <namespace>/<name> when it is empty.
func tenantDisplayName(tenant *hatchetv1alpha1.HatchetTenant) string {
	if tenant.Spec.DisplayName != "" {
		return tenant.Spec.DisplayName
	}
	return tenant.Namespace + "/" + tenant.Name
}

// inheritAllowed reports whether a tenant with slug may be inherited by a CR in namespace:
// controller-format slugs must carry the namespace prefix, other (manual) slugs are accepted.
func inheritAllowed(slug, namespace string) bool {
	return !controllerSlug.MatchString(slug) || strings.HasPrefix(slug, namespace+".")
}

// tokenPrefix is the name prefix of all API tokens issued for a CR.
func tokenPrefix(tenant *hatchetv1alpha1.HatchetTenant) string {
	return "k8s:" + string(tenant.UID) + ":"
}

// rotationTime is the scheduled rotation at issued-at + 2/3 of the token lifetime.
func rotationTime(issuedAt, expiresAt time.Time) time.Time {
	return issuedAt.Add(expiresAt.Sub(issuedAt) * 2 / 3)
}

// tokenTimes reads the issued-at and expires-at annotations of a worker Secret.
func tokenTimes(secret *corev1.Secret) (issuedAt, expiresAt time.Time, ok bool) {
	issuedAt, err := time.Parse(time.RFC3339, secret.Annotations[hatchetv1alpha1.IssuedAtAnnotation])
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	expiresAt, err = time.Parse(time.RFC3339, secret.Annotations[hatchetv1alpha1.ExpiresAtAnnotation])
	if err != nil || !expiresAt.After(issuedAt) {
		return time.Time{}, time.Time{}, false
	}
	return issuedAt, expiresAt, true
}

// issuedToken is a freshly minted token with its lifetime.
type issuedToken struct {
	hatchet.Token
	issuedAt  time.Time
	expiresAt time.Time
}

// writeWorkerSecret sets the worker Secret data from the instance. A nil token keeps the stored one.
func writeWorkerSecret(secret *corev1.Secret, tenant *hatchetv1alpha1.HatchetTenant,
	instance *hatchetv1alpha1.HatchetInstance, token *issuedToken) {
	if secret.CreationTimestamp.IsZero() {
		secret.Type = corev1.SecretTypeOpaque
	}
	tlsStrategy := instance.Spec.Worker.TLSStrategy
	if tlsStrategy == "" {
		tlsStrategy = hatchetv1alpha1.TLSStrategyTLS
	}
	data := map[string][]byte{
		envToken:       secret.Data[envToken],
		envTenantID:    []byte(tenant.Status.TenantID),
		envHostPort:    []byte(instance.Spec.Worker.HostPort),
		envServerURL:   []byte(instance.Spec.APIURL),
		envTLSStrategy: []byte(tlsStrategy),
	}
	if instance.Spec.Worker.TLSRootCA != "" {
		data[envTLSRootCA] = []byte(instance.Spec.Worker.TLSRootCA)
	}
	if token != nil {
		data[envToken] = []byte(token.Value)
		if secret.Annotations == nil {
			secret.Annotations = map[string]string{}
		}
		secret.Annotations[hatchetv1alpha1.TokenIDAnnotation] = token.ID
		secret.Annotations[hatchetv1alpha1.IssuedAtAnnotation] = token.issuedAt.Format(time.RFC3339)
		secret.Annotations[hatchetv1alpha1.ExpiresAtAnnotation] = token.expiresAt.Format(time.RFC3339)
	}
	secret.Data = data
}
