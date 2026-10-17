//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/controller"
	"github.com/abinnovision/hatchet-controller/internal/hatchet"
	"github.com/abinnovision/hatchet-controller/internal/keyset"
	"github.com/abinnovision/hatchet-controller/internal/testenv"
)

const (
	defaultVersion = "v0.112.10"
	timeout        = 2 * time.Minute
	allowedLabel   = "hatchet-controller.abigroup.io/test-allowed"
)

var env *testenv.Env

func TestMain(m *testing.M) {
	var err error
	env, err = testenv.StartEnvtest()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest:", err)
		os.Exit(1)
	}
	code := m.Run()
	if err := env.Stop(); err != nil {
		fmt.Fprintln(os.Stderr, "stopping envtest:", err)
	}
	os.Exit(code)
}

// versions returns the hatchet-lite tags from HATCHET_VERSIONS (comma or space separated).
func versions() []string {
	v := strings.Fields(strings.ReplaceAll(os.Getenv("HATCHET_VERSIONS"), ",", " "))
	if len(v) == 0 {
		return []string{defaultVersion}
	}
	return v
}

// suite is one Hatchet version with its HatchetInstance.
type suite struct {
	hatchet  *testenv.Hatchet
	instance *hatchetv1alpha1.HatchetInstance
	// client calls Hatchet as the service user with the instance's signing key.
	client hatchet.Client
	// allowed is the label value tenant namespaces need to be accepted.
	allowed string
}

// setup creates the instance, waits for its generated public keyset, boots Hatchet with
// it and points the instance at the mapped ports.
func setup(t *testing.T, version string) *suite {
	ctx := context.Background()
	s := &suite{allowed: rand.String(8)}

	s.instance = &hatchetv1alpha1.HatchetInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "hatchet", Namespace: newNamespace(t, nil)},
		Spec: hatchetv1alpha1.HatchetInstanceSpec{
			APIURL:        "http://127.0.0.1:1",
			ServiceUserID: uuid.NewString(),
			Worker:        hatchetv1alpha1.WorkerSpec{HostPort: "127.0.0.1:1", TLSStrategy: hatchetv1alpha1.TLSStrategyNone},
			AllowedNamespaces: hatchetv1alpha1.AllowedNamespaces{
				From:     hatchetv1alpha1.NamespacesFromSelector,
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{allowedLabel: s.allowed}},
			},
		},
	}
	mustCreate(t, s.instance)

	var public corev1.Secret
	eventually(t, func() error {
		if err := env.Client.Get(ctx, key(s.instance.Namespace, controller.PublicKeysetSecretName(s.instance)), &public); err != nil {
			return err
		}
		if len(public.Data[keyset.PublicKey]) == 0 {
			return fmt.Errorf("public keyset is empty")
		}
		return nil
	})

	h, err := testenv.StartHatchet(ctx, testenv.HatchetOptions{
		Version:       version,
		PublicKeyset:  string(public.Data[keyset.PublicKey]),
		ServiceUserID: s.instance.Spec.ServiceUserID,
	})
	if err != nil {
		t.Fatalf("starting hatchet: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Terminate(context.Background()); err != nil {
			t.Errorf("terminating hatchet: %v", err)
		}
	})
	s.hatchet = h

	patch := client.MergeFrom(s.instance.DeepCopy())
	s.instance.Spec.APIURL = h.APIURL
	s.instance.Spec.Worker.HostPort = h.GRPCHostPort
	if err := env.Client.Patch(ctx, s.instance, patch); err != nil {
		t.Fatalf("patching instance: %v", err)
	}

	var private corev1.Secret
	if err := env.Client.Get(ctx, key(s.instance.Namespace, controller.SigningKeySecretName(s.instance)), &private); err != nil {
		t.Fatalf("reading signing key: %v", err)
	}
	signer, err := keyset.NewSigner(private.Data[keyset.PrivateKey], keyset.Issuer, keyset.Audience)
	if err != nil {
		t.Fatalf("loading signing key: %v", err)
	}
	s.client = hatchet.New(h.APIURL, signer, s.instance.Spec.ServiceUserID, http.DefaultClient)
	return s
}

func key(namespace, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: namespace, Name: name}
}

// eventually retries fn until it returns nil or the timeout expires.
func eventually(t *testing.T, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := fn()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s: %v", timeout, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := env.Client.Create(context.Background(), obj); err != nil {
		t.Fatalf("creating %T: %v", obj, err)
	}
}

// newNamespace creates a namespace with a unique name and the given labels.
func newNamespace(t *testing.T, labels map[string]string) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "it-", Labels: labels}}
	mustCreate(t, ns)
	return ns.Name
}

// allowedNamespace creates a namespace the suite's instance accepts.
func (s *suite) allowedNamespace(t *testing.T) string {
	return newNamespace(t, map[string]string{allowedLabel: s.allowed})
}

// newTenant creates a HatchetTenant referencing the suite's instance.
func (s *suite) newTenant(t *testing.T, namespace, name string, mutate ...func(*hatchetv1alpha1.HatchetTenant)) *hatchetv1alpha1.HatchetTenant {
	t.Helper()
	tenant := &hatchetv1alpha1.HatchetTenant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: hatchetv1alpha1.HatchetTenantSpec{
			InstanceRef: hatchetv1alpha1.InstanceRef{Name: s.instance.Name, Namespace: s.instance.Namespace},
		},
	}
	for _, m := range mutate {
		m(tenant)
	}
	mustCreate(t, tenant)
	return tenant
}

// withAnnotation sets the tenant-id annotation.
func withAnnotation(id string) func(*hatchetv1alpha1.HatchetTenant) {
	return func(t *hatchetv1alpha1.HatchetTenant) {
		t.Annotations = map[string]string{hatchetv1alpha1.TenantIDAnnotation: id}
	}
}

// waitCondition waits until the condition has the status and reason for the current generation.
func waitCondition(t *testing.T, obj client.Object, conditionType string, status metav1.ConditionStatus, reason string) *hatchetv1alpha1.HatchetTenant {
	t.Helper()
	var tenant hatchetv1alpha1.HatchetTenant
	eventually(t, func() error {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(obj), &tenant); err != nil {
			return err
		}
		c := meta.FindStatusCondition(tenant.Status.Conditions, conditionType)
		if c == nil || c.Status != status || c.Reason != reason || c.ObservedGeneration != tenant.Generation {
			return fmt.Errorf("%s condition is %+v, want %s/%s", conditionType, c, status, reason)
		}
		return nil
	})
	return &tenant
}

func waitReady(t *testing.T, obj client.Object) *hatchetv1alpha1.HatchetTenant {
	t.Helper()
	return waitCondition(t, obj, hatchetv1alpha1.ConditionReady, metav1.ConditionTrue, hatchetv1alpha1.ReasonReady)
}

func workerSecret(t *testing.T, tenant *hatchetv1alpha1.HatchetTenant) *corev1.Secret {
	t.Helper()
	var secret corev1.Secret
	if err := env.Client.Get(context.Background(), key(tenant.Namespace, controller.WorkerSecretName(tenant)), &secret); err != nil {
		t.Fatalf("reading worker secret: %v", err)
	}
	return &secret
}

// waitTokenChange waits until the worker Secret holds a token other than oldTokenID.
func waitTokenChange(t *testing.T, tenant *hatchetv1alpha1.HatchetTenant, oldTokenID string) *corev1.Secret {
	t.Helper()
	var secret corev1.Secret
	eventually(t, func() error {
		if err := env.Client.Get(context.Background(), key(tenant.Namespace, controller.WorkerSecretName(tenant)), &secret); err != nil {
			return err
		}
		if id := secret.Annotations[hatchetv1alpha1.TokenIDAnnotation]; id == "" || id == oldTokenID {
			return fmt.Errorf("token-id is %q, waiting for a new token", id)
		}
		return nil
	})
	return &secret
}

// tokenStatus calls a tenant-scoped REST endpoint with the token and returns the status code.
func (s *suite) tokenStatus(t *testing.T, tenantID string, token []byte) int {
	t.Helper()
	code, err := s.hatchet.APITokenStatus(context.Background(), tenantID, string(token))
	if err != nil {
		t.Fatalf("calling hatchet: %v", err)
	}
	return code
}

// requireTokenValid fails unless the token is accepted.
func (s *suite) requireTokenValid(t *testing.T, tenantID string, token []byte) {
	t.Helper()
	if code := s.tokenStatus(t, tenantID, token); code != http.StatusOK {
		t.Fatalf("token rejected with %d, want 200", code)
	}
}

// waitTokenRevoked waits until the token is rejected with 401 or 403.
func (s *suite) waitTokenRevoked(t *testing.T, tenantID string, token []byte) {
	t.Helper()
	eventually(t, func() error {
		if code := s.tokenStatus(t, tenantID, token); code != http.StatusUnauthorized && code != http.StatusForbidden {
			return fmt.Errorf("token returned %d, want 401 or 403", code)
		}
		return nil
	})
}

// tokenIDs returns the IDs of the tenant's non-revoked API tokens.
func (s *suite) tokenIDs(t *testing.T, tenantID string) map[string]string {
	t.Helper()
	tokens, err := s.client.ListAPITokens(context.Background(), tenantID)
	if err != nil {
		t.Fatalf("listing api tokens: %v", err)
	}
	ids := map[string]string{}
	for _, tok := range tokens {
		ids[tok.ID] = tok.Name
	}
	return ids
}

// bump changes the display name to force a reconcile and updates tenant in place.
func bump(t *testing.T, tenant *hatchetv1alpha1.HatchetTenant) {
	t.Helper()
	patch := client.MergeFrom(tenant.DeepCopy())
	tenant.Spec.DisplayName = rand.String(8)
	if err := env.Client.Patch(context.Background(), tenant, patch); err != nil {
		t.Fatalf("patching tenant: %v", err)
	}
}
