package v1alpha1_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/testenv"
)

var k8s client.Client

func TestMain(m *testing.M) {
	env, err := testenv.Environment()
	if errors.Is(err, testenv.ErrNoAssets) {
		fmt.Fprintln(os.Stderr, "skipping envtest tests:", err)
		os.Exit(m.Run())
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintln(os.Stderr, "starting envtest:", err)
		os.Exit(1)
	}
	k8s, err = client.New(cfg, client.Options{Scheme: env.Scheme})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = env.Stop()
	os.Exit(code)
}

func requireEnv(t *testing.T) {
	t.Helper()
	if k8s == nil {
		t.Skip(testenv.ErrNoAssets)
	}
}

func newTenant(name string) *v1alpha1.HatchetTenant {
	return &v1alpha1.HatchetTenant{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1alpha1.HatchetTenantSpec{InstanceRef: v1alpha1.InstanceRef{Name: "hatchet", Namespace: "hatchet"}},
	}
}

func newInstance(name string) *v1alpha1.HatchetInstance {
	return &v1alpha1.HatchetInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.HatchetInstanceSpec{
			APIURL:        "http://hatchet:8080",
			ServiceUserID: "0b6b7e3c-4c4e-4f0a-9d5e-2f0d6c1b9a11",
			Worker:        v1alpha1.WorkerSpec{HostPort: "hatchet:7070"},
		},
	}
}

func uniqueName(prefix string) string {
	return prefix + "-" + rand.String(6)
}

func create(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8s.Create(context.Background(), obj); err != nil {
		t.Fatalf("create: %v", err)
	}
}

func requireRejected(t *testing.T, err error, msg string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected rejection containing %q, got success", msg)
	}
	if !strings.Contains(err.Error(), msg) {
		t.Fatalf("expected rejection containing %q, got %v", msg, err)
	}
}

func TestTenantDefaultsTokenTTLTo720h(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("defaults"))
	create(t, tenant)
	if tenant.Spec.TokenTTL == nil || tenant.Spec.TokenTTL.Duration != 720*time.Hour {
		t.Fatalf("tokenTTL = %v, want 720h", tenant.Spec.TokenTTL)
	}
}

func TestInstanceDefaultsAllowedNamespacesAndTLSStrategy(t *testing.T) {
	requireEnv(t)
	instance := newInstance(uniqueName("defaults"))
	create(t, instance)
	if instance.Spec.AllowedNamespaces.From != v1alpha1.NamespacesFromSame {
		t.Fatalf("allowedNamespaces.from = %q, want Same", instance.Spec.AllowedNamespaces.From)
	}
	if instance.Spec.Worker.TLSStrategy != v1alpha1.TLSStrategyTLS {
		t.Fatalf("worker.tlsStrategy = %q, want tls", instance.Spec.Worker.TLSStrategy)
	}
}

func TestTenantRejectsInstanceRefChange(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("ref"))
	create(t, tenant)
	tenant.Spec.InstanceRef.Name = "other"
	requireRejected(t, k8s.Update(context.Background(), tenant), "instanceRef is immutable")
}

func TestTenantRejectsSecretNameChange(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("secret"))
	tenant.Spec.SecretName = "a"
	create(t, tenant)
	tenant.Spec.SecretName = "b"
	requireRejected(t, k8s.Update(context.Background(), tenant), "secretName is immutable")
}

func TestTenantRejectsAddingSecretName(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("secret"))
	create(t, tenant)
	tenant.Spec.SecretName = "added"
	requireRejected(t, k8s.Update(context.Background(), tenant), "secretName is immutable")
}

func TestTenantRejectsRemovingSecretName(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("secret"))
	tenant.Spec.SecretName = "set"
	create(t, tenant)
	tenant.Spec.SecretName = ""
	requireRejected(t, k8s.Update(context.Background(), tenant), "secretName is immutable")
}

func TestTenantAllowsMutableFieldChanges(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("mutable"))
	tenant.Spec.SecretName = "kept"
	create(t, tenant)
	tenant.Spec.DisplayName = "New name"
	tenant.Spec.TokenTTL = &metav1.Duration{Duration: 48 * time.Hour}
	if err := k8s.Update(context.Background(), tenant); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func TestTenantRejectsNameLongerThan63(t *testing.T) {
	requireEnv(t)
	err := k8s.Create(context.Background(), newTenant(strings.Repeat("a", 64)))
	requireRejected(t, err, "metadata.name must be at most 63 characters")
}

func TestTenantAcceptsNameOf63(t *testing.T) {
	requireEnv(t)
	create(t, newTenant(strings.Repeat("b", 63)))
}

func TestTenantRejectsTokenTTLBelow24h(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("ttl"))
	tenant.Spec.TokenTTL = &metav1.Duration{Duration: 23 * time.Hour}
	requireRejected(t, k8s.Create(context.Background(), tenant), "tokenTTL must be at least 24h")
}

func TestTenantAcceptsTokenTTLOf24h(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("ttl"))
	tenant.Spec.TokenTTL = &metav1.Duration{Duration: 24 * time.Hour}
	create(t, tenant)
}

func TestInstanceRejectsSelectorModeWithoutSelector(t *testing.T) {
	requireEnv(t)
	instance := newInstance(uniqueName("sel"))
	instance.Spec.AllowedNamespaces.From = v1alpha1.NamespacesFromSelector
	requireRejected(t, k8s.Create(context.Background(), instance), "selector is required when from is Selector")
}

func TestInstanceAcceptsSelectorModeWithSelector(t *testing.T) {
	requireEnv(t)
	instance := newInstance(uniqueName("sel"))
	instance.Spec.AllowedNamespaces = v1alpha1.AllowedNamespaces{
		From:     v1alpha1.NamespacesFromSelector,
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "a"}},
	}
	create(t, instance)
}

func TestInstanceRejectsServiceUserIDChange(t *testing.T) {
	requireEnv(t)
	instance := newInstance(uniqueName("user"))
	create(t, instance)
	instance.Spec.ServiceUserID = "1c2d3e4f-0000-4000-8000-000000000000"
	requireRejected(t, k8s.Update(context.Background(), instance), "serviceUserID is immutable")
}

func TestTenantRejectsTokenTTLAbove8760h(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("ttl"))
	tenant.Spec.TokenTTL = &metav1.Duration{Duration: 8761 * time.Hour}
	requireRejected(t, k8s.Create(context.Background(), tenant), "tokenTTL must be at most 8760h")
}

func TestTenantAcceptsTokenTTLOf8760h(t *testing.T) {
	requireEnv(t)
	tenant := newTenant(uniqueName("ttl"))
	tenant.Spec.TokenTTL = &metav1.Duration{Duration: 8760 * time.Hour}
	create(t, tenant)
}
