package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
)

func TestResolveWebhookDefaultsSvixHMAC(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svix"}, Data: map[string][]byte{"key": []byte("whsec_x")}}
	r := &HatchetTenantReconciler{APIReader: fake.NewClientBuilder().WithObjects(secret).Build()}
	w := hatchetv1alpha1.Webhook{Name: "svix", Source: "SVIX", EventKeyExpression: "'svix'", Auth: hatchetv1alpha1.WebhookAuth{
		HMAC: &hatchetv1alpha1.WebhookHMACAuth{SigningSecretRef: hatchetv1alpha1.SecretKeyRef{Name: "svix", Key: "key"}},
	}}

	got, _, err := r.resolveWebhook(context.Background(), "ns", []byte("k"), w)
	if err != nil {
		t.Fatalf("resolveWebhook: %v", err)
	}
	if got.Auth.Algorithm != "SHA256" || got.Auth.Encoding != "BASE64" || got.Auth.SignatureHeaderName != "svix-signature" {
		t.Errorf("auth = %+v", got.Auth)
	}
}
