//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/controller"
	"github.com/abinnovision/hatchet-controller/internal/hatchet"
	"github.com/abinnovision/hatchet-controller/internal/keyset"
)

func TestHatchet(t *testing.T) {
	for _, version := range versions() {
		t.Run(version, func(t *testing.T) {
			s := setup(t, version)
			for _, tc := range []struct {
				name string
				fn   func(*testing.T, *suite)
			}{
				{"InstanceOwnsGeneratedKeyset", testInstanceOwnsGeneratedKeyset},
				{"ForeignKeysetJWTGets403", testForeignKeysetJWTGets403},
				{"ClientAcceptsDotSlug", testClientAcceptsDotSlug},
				{"ClientReportsSlugTakenWithExactMessage", testClientReportsSlugTaken},
				{"ClientTokenLifecycle", testClientTokenLifecycle},
				{"TenantCreatesSecretWithWorkingToken", testTenantCreatesSecretWithWorkingToken},
				{"TenantSlugHasNamespaceNameAndUIDFormat", testTenantSlugFormat},
				{"TenantIDAnnotationIsWrittenBack", testAnnotationWrittenBack},
				{"StatusLossRebindsFromAnnotation", testStatusLossRebinds},
				{"RecreateFromManifestRebinds", testRecreateFromManifestRebinds},
				{"RemovedAnnotationIsReAdded", testRemovedAnnotationReAdded},
				{"ChangedAnnotationGivesTenantIDConflict", testChangedAnnotationConflict},
				{"InheritsSameNamespaceTenant", testInheritSameNamespace},
				{"InheritsManuallyCreatedTenant", testInheritManualTenant},
				{"RejectsInheritOfForeignNamespaceTenant", testInheritForeignNamespaceRejected},
				{"ForeignSecretGivesConflictWithoutMinting", testForeignSecretConflict},
				{"FailureAfterCreateGivesSlugTakenAndRecovers", testFailureAfterCreateSlugTaken},
				{"ScheduledRotationKeepsOldToken", testScheduledRotation},
				{"DeletedSecretRotatesAndRevokes", testDeletedSecretRotatesAndRevokes},
				{"DeletedSecretRetryLeavesOneToken", testDeletedSecretRetryLeavesOneToken},
				{"DeletedTenantRevokesTokens", testDeletedTenantRevokesTokens},
				{"DisallowedNamespaceIsNotAccepted", testDisallowedNamespace},
				// Last: changes the shared instance spec.
				{"NetworkPolicyUsesPortOverrides", testNetworkPolicyPorts},
			} {
				t.Run(tc.name, func(t *testing.T) { tc.fn(t, s) })
			}
		})
	}
}

func testInstanceOwnsGeneratedKeyset(t *testing.T, s *suite) {
	var instance hatchetv1alpha1.HatchetInstance
	if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(s.instance), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.Status.PublicKeysetSecret != controller.PublicKeysetSecretName(&instance) {
		t.Errorf("status.publicKeysetSecret = %q", instance.Status.PublicKeysetSecret)
	}
	for _, name := range []string{controller.SigningKeySecretName(&instance), controller.PublicKeysetSecretName(&instance)} {
		var secret corev1.Secret
		if err := env.Client.Get(context.Background(), key(instance.Namespace, name), &secret); err != nil {
			t.Fatal(err)
		}
		if !metav1.IsControlledBy(&secret, &instance) {
			t.Errorf("secret %s is not controlled by the instance: %+v", name, secret.OwnerReferences)
		}
	}
}

func testForeignKeysetJWTGets403(t *testing.T, s *suite) {
	private, err := keyset.Generate()
	if err != nil {
		t.Fatal(err)
	}
	signer, err := keyset.NewSigner(private, keyset.Issuer, keyset.Audience)
	if err != nil {
		t.Fatal(err)
	}
	c := hatchet.New(s.hatchet.APIURL, signer, s.instance.Spec.ServiceUserID, http.DefaultClient)
	_, err = c.CreateTenant(context.Background(), "foreign", "foreign-"+rand.String(8))
	var herr *hatchet.Error
	if !errors.As(err, &herr) || herr.StatusCode != http.StatusForbidden || !errors.Is(err, hatchet.ErrUnauthorized) {
		t.Fatalf("CreateTenant with a foreign keyset: err = %v, want 403 ErrUnauthorized", err)
	}
}

func testClientAcceptsDotSlug(t *testing.T, s *suite) {
	slug := "it-ns.name-" + rand.String(8)
	created, err := s.client.CreateTenant(context.Background(), "dot slug", slug)
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	got, err := s.client.GetTenant(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if got.Slug != slug {
		t.Fatalf("slug = %q, want %q", got.Slug, slug)
	}
}

func testClientReportsSlugTaken(t *testing.T, s *suite) {
	slug := "taken-" + rand.String(8)
	if _, err := s.client.CreateTenant(context.Background(), "first", slug); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	_, err := s.client.CreateTenant(context.Background(), "second", slug)
	var herr *hatchet.Error
	if !errors.Is(err, hatchet.ErrSlugTaken) || !errors.As(err, &herr) || herr.Message != "Tenant with that slug already exists." {
		t.Fatalf("second CreateTenant: err = %v, want ErrSlugTaken with Hatchet's message", err)
	}
}

func testClientTokenLifecycle(t *testing.T, s *suite) {
	ctx := context.Background()
	tenant, err := s.client.CreateTenant(ctx, "tokens", "tokens-"+rand.String(8))
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	token, err := s.client.CreateAPIToken(ctx, tenant.ID, "it-token", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if name, ok := s.tokenIDs(t, tenant.ID)[token.ID]; !ok || name != "it-token" {
		t.Fatalf("token %s missing from list", token.ID)
	}
	s.requireTokenValid(t, tenant.ID, []byte(token.Value))

	if err := s.client.RevokeAPIToken(ctx, tenant.ID, token.ID); err != nil {
		t.Fatalf("RevokeAPIToken: %v", err)
	}
	if _, ok := s.tokenIDs(t, tenant.ID)[token.ID]; ok {
		t.Fatalf("revoked token %s still listed", token.ID)
	}
	s.waitTokenRevoked(t, tenant.ID, []byte(token.Value))
}

func testTenantCreatesSecretWithWorkingToken(t *testing.T, s *suite) {
	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "orders"))
	secret := workerSecret(t, tenant)

	want := map[string]string{
		"HATCHET_CLIENT_TENANT_ID":    tenant.Status.TenantID,
		"HATCHET_CLIENT_HOST_PORT":    s.hatchet.GRPCHostPort,
		"HATCHET_CLIENT_SERVER_URL":   s.hatchet.APIURL,
		"HATCHET_CLIENT_TLS_STRATEGY": "none",
	}
	for k, v := range want {
		if got := string(secret.Data[k]); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	ref := metav1.GetControllerOf(secret)
	if ref == nil || ref.UID != tenant.UID || ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Errorf("controller ownerRef = %+v, want tenant %s with blockOwnerDeletion", ref, tenant.UID)
	}
	if tenant.Status.TokenExpiresAt == nil {
		t.Error("status.tokenExpiresAt is not set")
	}
	s.requireTokenValid(t, tenant.Status.TenantID, secret.Data["HATCHET_CLIENT_TOKEN"])
}

func testTenantSlugFormat(t *testing.T, s *suite) {
	ns := s.allowedNamespace(t)
	tenant := waitReady(t, s.newTenant(t, ns, "slugs"))
	want := fmt.Sprintf("%s.slugs-%s", ns, tenant.UID[:8])
	if tenant.Status.Slug != want {
		t.Fatalf("status.slug = %q, want %q", tenant.Status.Slug, want)
	}
	got, err := s.client.GetTenant(context.Background(), tenant.Status.TenantID)
	if err != nil {
		t.Fatalf("GetTenant: %v", err)
	}
	if got.Slug != want {
		t.Fatalf("hatchet slug = %q, want %q", got.Slug, want)
	}
}

func testAnnotationWrittenBack(t *testing.T, s *suite) {
	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "writeback"))
	if got := tenant.Annotations[hatchetv1alpha1.TenantIDAnnotation]; got == "" || got != tenant.Status.TenantID {
		t.Fatalf("annotation = %q, want status.tenantID %q", got, tenant.Status.TenantID)
	}
}

func testStatusLossRebinds(t *testing.T, s *suite) {
	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "statusloss"))
	boundID := tenant.Status.TenantID

	tenant.Status = hatchetv1alpha1.HatchetTenantStatus{}
	if err := env.Client.Status().Update(context.Background(), tenant); err != nil {
		t.Fatalf("clearing status: %v", err)
	}
	tenant = waitReady(t, tenant)
	if tenant.Status.TenantID != boundID {
		t.Fatalf("status.tenantID = %q, want %q", tenant.Status.TenantID, boundID)
	}
}

func testRecreateFromManifestRebinds(t *testing.T, s *suite) {
	ctx := context.Background()
	ns := s.allowedNamespace(t)
	tenant := waitReady(t, s.newTenant(t, ns, "restore"))
	boundID := tenant.Status.TenantID
	secret := workerSecret(t, tenant)

	deleteTenant(t, tenant)
	// envtest runs no garbage collector; remove the owned Secret as GC would.
	if err := env.Client.Delete(ctx, secret); err != nil {
		t.Fatalf("deleting worker secret: %v", err)
	}

	restored := waitReady(t, s.newTenant(t, ns, "restore", withAnnotation(boundID)))
	if restored.UID == tenant.UID {
		t.Fatal("expected a new UID")
	}
	if restored.Status.TenantID != boundID {
		t.Fatalf("status.tenantID = %q, want %q", restored.Status.TenantID, boundID)
	}
}

func testRemovedAnnotationReAdded(t *testing.T, s *suite) {
	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "readd"))
	boundID := tenant.Status.TenantID

	patch := client.MergeFrom(tenant.DeepCopy())
	delete(tenant.Annotations, hatchetv1alpha1.TenantIDAnnotation)
	if err := env.Client.Patch(context.Background(), tenant, patch); err != nil {
		t.Fatalf("removing annotation: %v", err)
	}
	eventually(t, func() error {
		if err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tenant), tenant); err != nil {
			return err
		}
		if got := tenant.Annotations[hatchetv1alpha1.TenantIDAnnotation]; got != boundID {
			return fmt.Errorf("annotation = %q, want %q", got, boundID)
		}
		return nil
	})
}

func testChangedAnnotationConflict(t *testing.T, s *suite) {
	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "conflict"))

	patch := client.MergeFrom(tenant.DeepCopy())
	tenant.Annotations[hatchetv1alpha1.TenantIDAnnotation] = "00000000-0000-4000-8000-000000000000"
	if err := env.Client.Patch(context.Background(), tenant, patch); err != nil {
		t.Fatalf("changing annotation: %v", err)
	}
	// Annotation changes do not bump the generation; bump it so the condition check sees a fresh reconcile.
	bump(t, tenant)
	waitCondition(t, tenant, hatchetv1alpha1.ConditionReady, metav1.ConditionFalse, hatchetv1alpha1.ReasonTenantIDConflict)
}

func testInheritSameNamespace(t *testing.T, s *suite) {
	ns := s.allowedNamespace(t)
	first := waitReady(t, s.newTenant(t, ns, "first"))

	second := waitReady(t, s.newTenant(t, ns, "second", withAnnotation(first.Status.TenantID)))
	if second.Status.TenantID != first.Status.TenantID {
		t.Fatalf("status.tenantID = %q, want %q", second.Status.TenantID, first.Status.TenantID)
	}
	s.requireTokenValid(t, second.Status.TenantID, workerSecret(t, second).Data["HATCHET_CLIENT_TOKEN"])
}

func testInheritManualTenant(t *testing.T, s *suite) {
	ctx := context.Background()
	id, err := s.hatchet.CreateTenantAsAdmin(ctx, "manual", "manual-"+rand.String(8))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.hatchet.AddMember(ctx, id, s.instance.Spec.ServiceUserID); err != nil {
		t.Fatalf("adding service user as member: %v", err)
	}

	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "manual", withAnnotation(id)))
	if tenant.Status.TenantID != id {
		t.Fatalf("status.tenantID = %q, want %q", tenant.Status.TenantID, id)
	}
	s.requireTokenValid(t, id, workerSecret(t, tenant).Data["HATCHET_CLIENT_TOKEN"])
}

func testInheritForeignNamespaceRejected(t *testing.T, s *suite) {
	victim := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "victim"))

	thief := s.newTenant(t, s.allowedNamespace(t), "thief", withAnnotation(victim.Status.TenantID))
	thief = waitCondition(t, thief, hatchetv1alpha1.ConditionReady, metav1.ConditionFalse, hatchetv1alpha1.ReasonInheritRejected)
	if thief.Status.TenantID != "" {
		t.Fatalf("status.tenantID = %q, want empty", thief.Status.TenantID)
	}
	want := fmt.Sprintf("tenant %s does not exist or was created by the controller for another namespace", victim.Status.TenantID)
	if c := meta.FindStatusCondition(thief.Status.Conditions, hatchetv1alpha1.ConditionReady); c.Message != want {
		t.Errorf("Ready message = %q, want %q", c.Message, want)
	}
}

func testForeignSecretConflict(t *testing.T, s *suite) {
	ctx := context.Background()
	ns := s.allowedNamespace(t)
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "squatter-hatchet", Namespace: ns},
		Data:       map[string][]byte{"owner": []byte("someone else")},
	}
	mustCreate(t, foreign)

	tenant := s.newTenant(t, ns, "squatter")
	tenant = waitCondition(t, tenant, hatchetv1alpha1.ConditionReady, metav1.ConditionFalse, hatchetv1alpha1.ReasonConflict)
	tenantID := tenant.Annotations[hatchetv1alpha1.TenantIDAnnotation]
	if tenantID == "" {
		t.Fatal("tenant is not bound; cannot check api tokens")
	}
	before := s.tokenIDs(t, tenantID)

	for range 3 {
		bump(t, tenant)
		tenant = waitCondition(t, tenant, hatchetv1alpha1.ConditionReady, metav1.ConditionFalse, hatchetv1alpha1.ReasonConflict)
	}

	if after := s.tokenIDs(t, tenantID); !maps.Equal(before, after) {
		t.Errorf("api tokens changed: before %v, after %v", before, after)
	}
	var got corev1.Secret
	if err := env.Client.Get(ctx, client.ObjectKeyFromObject(foreign), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.OwnerReferences) != 0 || len(got.Data) != 1 || string(got.Data["owner"]) != "someone else" ||
		got.ResourceVersion != foreign.ResourceVersion {
		t.Errorf("foreign secret was modified: %+v", got)
	}
}

func testFailureAfterCreateSlugTaken(t *testing.T, s *suite) {
	orphan := make(chan hatchet.Tenant, 1)
	env.FailAfterCreate(func(t hatchet.Tenant) { orphan <- t })
	t.Cleanup(env.ResetFailures)

	tenant := s.newTenant(t, s.allowedNamespace(t), "orphan")
	tenant = waitCondition(t, tenant, hatchetv1alpha1.ConditionReady, metav1.ConditionFalse, hatchetv1alpha1.ReasonSlugTaken)
	var created hatchet.Tenant
	select {
	case created = <-orphan:
	case <-time.After(timeout):
		t.Fatal("injected tenant:create failure did not fire")
	}

	patch := client.MergeFrom(tenant.DeepCopy())
	tenant.Annotations = map[string]string{hatchetv1alpha1.TenantIDAnnotation: created.ID}
	if err := env.Client.Patch(context.Background(), tenant, patch); err != nil {
		t.Fatalf("setting annotation: %v", err)
	}
	bump(t, tenant)
	tenant = waitReady(t, tenant)
	if tenant.Status.TenantID != created.ID {
		t.Fatalf("status.tenantID = %q, want orphan %q", tenant.Status.TenantID, created.ID)
	}
}

func testScheduledRotation(t *testing.T, s *suite) {
	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "rotate", func(t *hatchetv1alpha1.HatchetTenant) {
		t.Spec.TokenTTL = &metav1.Duration{Duration: 24 * time.Hour}
	}))
	old := workerSecret(t, tenant)
	oldID := old.Annotations[hatchetv1alpha1.TokenIDAnnotation]

	// Past 2/3 of the 24h TTL.
	now := env.Clock.Now()
	env.Clock.SetTime(now.Add(17 * time.Hour))
	t.Cleanup(func() { env.Clock.SetTime(now) })
	bump(t, tenant)

	current := waitTokenChange(t, tenant, oldID)
	s.requireTokenValid(t, tenant.Status.TenantID, current.Data["HATCHET_CLIENT_TOKEN"])
	s.requireTokenValid(t, tenant.Status.TenantID, old.Data["HATCHET_CLIENT_TOKEN"])
	if _, ok := s.tokenIDs(t, tenant.Status.TenantID)[oldID]; !ok {
		t.Errorf("old token %s was revoked", oldID)
	}
}

func testDeletedSecretRotatesAndRevokes(t *testing.T, s *suite) {
	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "leaked"))
	old := workerSecret(t, tenant)
	oldID := old.Annotations[hatchetv1alpha1.TokenIDAnnotation]

	if err := env.Client.Delete(context.Background(), old); err != nil {
		t.Fatalf("deleting worker secret: %v", err)
	}

	current := waitTokenChange(t, tenant, oldID)
	s.requireTokenValid(t, tenant.Status.TenantID, current.Data["HATCHET_CLIENT_TOKEN"])
	s.waitTokenRevoked(t, tenant.Status.TenantID, old.Data["HATCHET_CLIENT_TOKEN"])
	if _, ok := s.tokenIDs(t, tenant.Status.TenantID)[oldID]; ok {
		t.Errorf("old token %s is still listed", oldID)
	}
}

func testDeletedSecretRetryLeavesOneToken(t *testing.T, s *suite) {
	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "retry"))
	old := workerSecret(t, tenant)
	oldID := old.Annotations[hatchetv1alpha1.TokenIDAnnotation]

	env.FailNextRevoke()
	t.Cleanup(env.ResetFailures)
	if err := env.Client.Delete(context.Background(), old); err != nil {
		t.Fatalf("deleting worker secret: %v", err)
	}

	current := waitTokenChange(t, tenant, oldID)
	s.waitTokenRevoked(t, tenant.Status.TenantID, old.Data["HATCHET_CLIENT_TOKEN"])
	s.requireTokenValid(t, tenant.Status.TenantID, current.Data["HATCHET_CLIENT_TOKEN"])
	var owned []string
	for id, name := range s.tokenIDs(t, tenant.Status.TenantID) {
		if strings.HasPrefix(name, "k8s:"+string(tenant.UID)+":") {
			owned = append(owned, id)
		}
	}
	if currentID := current.Annotations[hatchetv1alpha1.TokenIDAnnotation]; len(owned) != 1 || owned[0] != currentID {
		t.Errorf("listed tokens = %v, want only %s", owned, currentID)
	}
}

func testDeletedTenantRevokesTokens(t *testing.T, s *suite) {
	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "deleted"))
	token := workerSecret(t, tenant).Data["HATCHET_CLIENT_TOKEN"]

	deleteTenant(t, tenant)

	s.waitTokenRevoked(t, tenant.Status.TenantID, token)
	for id, name := range s.tokenIDs(t, tenant.Status.TenantID) {
		if strings.HasPrefix(name, "k8s:"+string(tenant.UID)+":") {
			t.Errorf("token %s (%s) is still listed", id, name)
		}
	}
}

func testDisallowedNamespace(t *testing.T, s *suite) {
	tenant := s.newTenant(t, newNamespace(t, nil), "outsider")
	waitCondition(t, tenant, hatchetv1alpha1.ConditionAccepted, metav1.ConditionFalse, hatchetv1alpha1.ReasonNotAllowed)
}

func testNetworkPolicyPorts(t *testing.T, s *suite) {
	ctx := context.Background()
	patch := client.MergeFrom(s.instance.DeepCopy())
	s.instance.Spec.NetworkPolicy = &hatchetv1alpha1.NetworkPolicySpec{
		EngineSelector: map[string]string{"app": "engine"},
		APISelector:    map[string]string{"app": "api"},
		Ports:          &hatchetv1alpha1.NetworkPolicyPorts{Engine: 17070, API: 18080},
	}
	if err := env.Client.Patch(ctx, s.instance, patch); err != nil {
		t.Fatalf("enabling network policy: %v", err)
	}

	tenant := waitReady(t, s.newTenant(t, s.allowedNamespace(t), "netpol"))
	var np networkingv1.NetworkPolicy
	eventually(t, func() error {
		return env.Client.Get(ctx, key(tenant.Namespace, controller.NetworkPolicyName(tenant)), &np)
	})
	ports := map[int32]bool{}
	for _, rule := range np.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil {
				ports[p.Port.IntVal] = true
			}
		}
	}
	if !ports[17070] || !ports[18080] {
		t.Fatalf("egress ports = %v, want 17070 and 18080", ports)
	}
	if !metav1.IsControlledBy(&np, tenant) {
		t.Errorf("network policy is not controlled by the tenant")
	}
}

// deleteTenant deletes the tenant and waits until its finalizer is released.
func deleteTenant(t *testing.T, tenant *hatchetv1alpha1.HatchetTenant) {
	t.Helper()
	if err := env.Client.Delete(context.Background(), tenant); err != nil {
		t.Fatalf("deleting tenant: %v", err)
	}
	eventually(t, func() error {
		var got hatchetv1alpha1.HatchetTenant
		err := env.Client.Get(context.Background(), client.ObjectKeyFromObject(tenant), &got)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err == nil {
			c := meta.FindStatusCondition(got.Status.Conditions, hatchetv1alpha1.ConditionReady)
			err = fmt.Errorf("tenant still exists, Ready = %+v", c)
		}
		return err
	})
}
