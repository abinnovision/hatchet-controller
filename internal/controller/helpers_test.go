package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/hatchet"
)

func TestTenantSlug(t *testing.T) {
	tenant := &hatchetv1alpha1.HatchetTenant{ObjectMeta: metav1.ObjectMeta{
		Name: "orders", Namespace: "team-orders", UID: "3f2a9c1d-1111-2222-3333-444455556666",
	}}
	got := tenantSlug(tenant)
	if want := "team-orders.orders-3f2a9c1d"; got != want {
		t.Fatalf("tenantSlug = %q, want %q", got, want)
	}
	if !controllerSlug.MatchString(got) {
		t.Fatalf("slug %q does not match the controller format", got)
	}
}

func TestInheritAllowed(t *testing.T) {
	tests := []struct {
		slug, namespace string
		want            bool
	}{
		{"team-orders.orders-3f2a9c1d", "team-orders", true},
		{"team-orders.orders-3f2a9c1d", "team-billing", false},
		{"team-orders.sub.name-3f2a9c1d", "team-orders", true},
		{"manual-tenant", "team-billing", true},
		{"Manual.Tenant-3f2a9c1d", "team-billing", true},
	}
	for _, tt := range tests {
		if got := inheritAllowed(tt.slug, tt.namespace); got != tt.want {
			t.Errorf("inheritAllowed(%q, %q) = %v, want %v", tt.slug, tt.namespace, got, tt.want)
		}
	}
}

func TestRotationTime(t *testing.T) {
	issued := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	got := rotationTime(issued, issued.Add(30*time.Hour))
	if want := issued.Add(20 * time.Hour); !got.Equal(want) {
		t.Fatalf("rotationTime = %v, want %v", got, want)
	}
}

func TestNamespaceAllowed(t *testing.T) {
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{"hatchet-controller.abigroup.io/tenants": "allowed"}}
	tests := []struct {
		name     string
		allowed  hatchetv1alpha1.AllowedNamespaces
		ns       string
		nsLabels map[string]string
		want     bool
	}{
		{"default same", hatchetv1alpha1.AllowedNamespaces{}, "hatchet", nil, true},
		{"same other", hatchetv1alpha1.AllowedNamespaces{From: hatchetv1alpha1.NamespacesFromSame}, "team", nil, false},
		{"all", hatchetv1alpha1.AllowedNamespaces{From: hatchetv1alpha1.NamespacesFromAll}, "team", nil, true},
		{"selector match", hatchetv1alpha1.AllowedNamespaces{From: hatchetv1alpha1.NamespacesFromSelector, Selector: selector},
			"team", map[string]string{"hatchet-controller.abigroup.io/tenants": "allowed"}, true},
		{"selector miss", hatchetv1alpha1.AllowedNamespaces{From: hatchetv1alpha1.NamespacesFromSelector, Selector: selector},
			"team", nil, false},
	}
	for _, tt := range tests {
		got, err := namespaceAllowed(tt.allowed, "hatchet", tt.ns, tt.nsLabels)
		if err != nil || got != tt.want {
			t.Errorf("%s: namespaceAllowed = %v, %v, want %v", tt.name, got, err, tt.want)
		}
	}
}

func TestPolicyPorts(t *testing.T) {
	instance := func(hostPort, apiURL string, tls hatchetv1alpha1.TLSStrategy, ports *hatchetv1alpha1.NetworkPolicyPorts) *hatchetv1alpha1.HatchetInstance {
		return &hatchetv1alpha1.HatchetInstance{Spec: hatchetv1alpha1.HatchetInstanceSpec{
			APIURL:        apiURL,
			Worker:        hatchetv1alpha1.WorkerSpec{HostPort: hostPort, TLSStrategy: tls},
			NetworkPolicy: &hatchetv1alpha1.NetworkPolicySpec{Ports: ports},
		}}
	}
	tests := []struct {
		name        string
		in          *hatchetv1alpha1.HatchetInstance
		engine, api int32
	}{
		{"from urls", instance("engine:7070", "http://api:8080", "", nil), 7070, 8080},
		{"defaults tls", instance("engine", "https://api", hatchetv1alpha1.TLSStrategyTLS, nil), 443, 443},
		{"defaults plain", instance("engine", "http://api", hatchetv1alpha1.TLSStrategyNone, nil), 80, 80},
		{"overrides", instance("engine:7070", "http://api:80", "", &hatchetv1alpha1.NetworkPolicyPorts{Engine: 7077, API: 8888}), 7077, 8888},
	}
	for _, tt := range tests {
		engine, api, err := policyPorts(tt.in)
		if err != nil || engine != tt.engine || api != tt.api {
			t.Errorf("%s: policyPorts = %d, %d, %v, want %d, %d", tt.name, engine, api, err, tt.engine, tt.api)
		}
	}
}

func TestFingerprint(t *testing.T) {
	key := []byte("signing-key")
	base := fingerprint(key, "GITHUB", "HMAC", "SHA256", "value")
	if got := fingerprint(key, "GITHUB", "HMAC", "SHA256", "value"); got != base {
		t.Errorf("fingerprint is not stable: %q != %q", got, base)
	}
	if fingerprint([]byte("other-key"), "GITHUB", "HMAC", "SHA256", "value") == base {
		t.Error("fingerprint ignores a change of key")
	}
	for name, parts := range map[string][]string{
		"credential value": {"GITHUB", "HMAC", "SHA256", "other"},
		"auth field":       {"GITHUB", "HMAC", "SHA512", "value"},
		"source":           {"STRIPE", "HMAC", "SHA256", "value"},
		"part boundary":    {"GITHUBHMAC", "", "SHA256", "value"},
	} {
		if fingerprint(key, parts...) == base {
			t.Errorf("fingerprint ignores a change of %s", name)
		}
	}
}

func TestWebhookNeedsUpdate(t *testing.T) {
	base := hatchet.Webhook{EventKeyExpression: "'e'", ScopeExpression: "'s'", StaticPayload: map[string]any{"a": "b"}}
	tests := []struct {
		name string
		mod  func(*hatchet.Webhook)
		want bool
	}{
		{"equal", func(*hatchet.Webhook) {}, false},
		{"event key", func(w *hatchet.Webhook) { w.EventKeyExpression = "'x'" }, true},
		{"scope", func(w *hatchet.Webhook) { w.ScopeExpression = "" }, true},
		{"payload", func(w *hatchet.Webhook) { w.StaticPayload = map[string]any{"a": "c"} }, true},
		{"payload removed", func(w *hatchet.Webhook) { w.StaticPayload = nil }, true},
	}
	for _, tt := range tests {
		got := base
		tt.mod(&got)
		if n := webhookNeedsUpdate(base, got); n != tt.want {
			t.Errorf("%s: webhookNeedsUpdate = %v, want %v", tt.name, n, tt.want)
		}
	}
	if webhookNeedsUpdate(hatchet.Webhook{EventKeyExpression: "e"}, hatchet.Webhook{EventKeyExpression: "e", StaticPayload: map[string]any{}}) {
		t.Error("nil and empty payloads should be equal")
	}
}
