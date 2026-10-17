package controller

import (
	"context"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/hatchet"
	"github.com/abinnovision/hatchet-controller/internal/keyset"
)

// ClientFactory builds a Hatchet client for an instance.
type ClientFactory func(ctx context.Context, instance *hatchetv1alpha1.HatchetInstance) (hatchet.Client, error)

// NewClientFactory returns a ClientFactory that signs with the instance's signing key Secret.
func NewClientFactory(c client.Reader) ClientFactory {
	httpClient := &http.Client{
		Timeout: 30 * time.Second,
		// Never follow redirects so the exchange token is only sent to apiURL.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context, instance *hatchetv1alpha1.HatchetInstance) (hatchet.Client, error) {
		var secret corev1.Secret
		key := client.ObjectKey{Namespace: instance.Namespace, Name: SigningKeySecretName(instance)}
		if err := c.Get(ctx, key, &secret); err != nil {
			return nil, fmt.Errorf("reading signing key: %w", err)
		}
		if !metav1.IsControlledBy(&secret, instance) {
			return nil, fmt.Errorf("signing key Secret %s is not controlled by HatchetInstance %s", secret.Name, instance.Name)
		}
		signer, err := keyset.NewSigner(secret.Data[keyset.PrivateKey], keyset.Issuer, keyset.Audience)
		if err != nil {
			return nil, fmt.Errorf("loading signing key: %w", err)
		}
		return hatchet.New(instance.Spec.APIURL, signer, instance.Spec.ServiceUserID, httpClient), nil
	}
}
