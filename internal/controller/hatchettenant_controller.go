package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/hatchet"
)

// InstanceRefIndex is the HatchetTenant field index on spec.instanceRef, keyed "<namespace>/<name>".
const InstanceRefIndex = "spec.instanceRef"

// WebhookSecretIndex is the HatchetTenant field index on the Secret names referenced by spec.webhooks.
const WebhookSecretIndex = "spec.webhooks.secretRefs"

const (
	// defaultTokenTTL applies when spec.tokenTTL is unset (the API server defaults it).
	defaultTokenTTL = 8760 * time.Hour
	// recheckInterval requeues conditions that may be fixed outside watched objects.
	recheckInterval = 5 * time.Minute
	// cacheLagRequeue requeues when the cache lags behind an uncached read.
	cacheLagRequeue = time.Second
	// revocationGracePeriod delays RevocationFailed so transient errors do not surface.
	revocationGracePeriod = 10 * time.Minute
)

// Worker Secret data keys read by the Hatchet SDKs.
const (
	envToken       = "HATCHET_CLIENT_TOKEN"
	envTenantID    = "HATCHET_CLIENT_TENANT_ID"
	envHostPort    = "HATCHET_CLIENT_HOST_PORT"
	envServerURL   = "HATCHET_CLIENT_SERVER_URL"
	envTLSStrategy = "HATCHET_CLIENT_TLS_STRATEGY"
	envTLSRootCA   = "HATCHET_CLIENT_TLS_ROOT_CA"
)

// HatchetTenantReconciler binds HatchetTenants to Hatchet tenants and maintains their worker Secrets.
type HatchetTenantReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader performs the single uncached read before irreversible external side effects.
	APIReader client.Reader
	Recorder  events.EventRecorder
	// ClientFactory builds the Hatchet client for the referenced instance.
	ClientFactory ClientFactory
	// Clock drives token rotation; tests inject a fake.
	Clock clock.PassiveClock
	// NetworkPolicyEnabled enables NetworkPolicy management; it requires the networkpolicy RBAC component.
	NetworkPolicyEnabled bool
}

// NewHatchetTenantReconciler returns a HatchetTenantReconciler wired to the manager.
func NewHatchetTenantReconciler(mgr ctrl.Manager, factory ClientFactory, networkPolicyEnabled bool) *HatchetTenantReconciler {
	return &HatchetTenantReconciler{
		Client:               mgr.GetClient(),
		Scheme:               mgr.GetScheme(),
		APIReader:            mgr.GetAPIReader(),
		Recorder:             mgr.GetEventRecorder(hatchetv1alpha1.ControllerName),
		ClientFactory:        factory,
		Clock:                clock.RealClock{},
		NetworkPolicyEnabled: networkPolicyEnabled,
	}
}

// conditionError ends a reconcile with Ready=False. Retry selects backoff over a periodic recheck.
// The message is sanitized for the condition; err keeps the cause for logs.
type conditionError struct {
	reason  string
	message string
	retry   bool
	err     error
}

func (e *conditionError) Error() string {
	if e.err != nil {
		return e.reason + ": " + e.message + ": " + e.err.Error()
	}
	return e.reason + ": " + e.message
}

func (e *conditionError) Unwrap() error { return e.err }

// hatchetError maps a Hatchet client error to a Ready reason with a sanitized message.
func hatchetError(err error) *conditionError {
	message := "request to Hatchet failed"
	var herr *hatchet.Error
	if errors.As(err, &herr) {
		message = herr.Message
	}
	reason := hatchetv1alpha1.ReasonHatchetError
	if errors.Is(err, hatchet.ErrUnauthorized) {
		reason = hatchetv1alpha1.ReasonUnauthorized
	}
	return &conditionError{reason: reason, message: message, retry: true, err: err}
}

// +kubebuilder:rbac:groups=hatchet-controller.abigroup.io,resources=hatchettenants,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=hatchet-controller.abigroup.io,resources=hatchettenants/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hatchet-controller.abigroup.io,resources=hatchettenants/finalizers,verbs=update
// +kubebuilder:rbac:groups=hatchet-controller.abigroup.io,resources=hatchetinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile binds the tenant, issues and rotates the worker token and maintains the NetworkPolicy.
func (r *HatchetTenantReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var tenant hatchetv1alpha1.HatchetTenant
	if err := r.Get(ctx, req.NamespacedName, &tenant); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	instance := &hatchetv1alpha1.HatchetInstance{}
	ref := tenant.Spec.InstanceRef
	if err := r.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, instance); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		instance = nil
	}

	if !tenant.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &tenant, instance)
	}
	if controllerutil.AddFinalizer(&tenant, hatchetv1alpha1.TenantFinalizer) {
		if err := r.Update(ctx, &tenant); err != nil {
			return ctrl.Result{}, err
		}
	}

	base := tenant.DeepCopy()
	result, err := r.reconcile(ctx, &tenant, instance)
	var cerr *conditionError
	if errors.As(err, &cerr) {
		setCondition(&tenant.Status.Conditions, tenant.Generation, hatchetv1alpha1.ConditionReady, false, cerr.reason, cerr.message)
		result = ctrl.Result{RequeueAfter: recheckInterval}
		if !cerr.retry {
			err = nil
		}
	}

	tenant.Status.ObservedGeneration = tenant.Generation
	if perr := r.Status().Patch(ctx, &tenant, client.MergeFrom(base)); perr != nil {
		return ctrl.Result{}, errors.Join(err, perr)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

// reconcile runs the Accepted, binding, display name, token, NetworkPolicy, webhook and member steps.
func (r *HatchetTenantReconciler) reconcile(ctx context.Context, tenant *hatchetv1alpha1.HatchetTenant,
	instance *hatchetv1alpha1.HatchetInstance) (ctrl.Result, error) {
	if err := r.accept(ctx, tenant, instance); err != nil {
		return ctrl.Result{}, err
	}

	hc, err := r.ClientFactory(ctx, instance)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeueAfter, err := r.bind(ctx, hc, tenant); err != nil || requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, err
	}

	if err := r.ensureDisplayName(ctx, hc, tenant); err != nil {
		return ctrl.Result{}, err
	}

	rotateAt, requeueAfter, err := r.ensureToken(ctx, hc, tenant, instance)
	if err != nil || requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, err
	}

	if instance.Spec.NetworkPolicy != nil {
		if !r.NetworkPolicyEnabled {
			return ctrl.Result{}, &conditionError{reason: hatchetv1alpha1.ReasonForbidden,
				message: "the instance enables networkPolicy but the controller runs without --enable-network-policy"}
		}
		if requeueAfter, err := r.ensureNetworkPolicy(ctx, tenant, instance); err != nil || requeueAfter > 0 {
			return ctrl.Result{RequeueAfter: requeueAfter}, err
		}
	}

	// A webhook or member failure does not block the worker Secret above, and neither blocks the other.
	webhookErr := r.ensureWebhooks(ctx, hc, tenant, instance)
	memberErr := r.ensureMembers(ctx, hc, tenant)
	if webhookErr != nil {
		// Keep the retry of the member failure that is not returned.
		var werr, merr *conditionError
		if errors.As(webhookErr, &werr) && !werr.retry && memberErr != nil && (!errors.As(memberErr, &merr) || merr.retry) {
			werr.retry = true
		}
		return ctrl.Result{}, webhookErr
	}
	if memberErr != nil {
		return ctrl.Result{}, memberErr
	}

	setCondition(&tenant.Status.Conditions, tenant.Generation, hatchetv1alpha1.ConditionReady, true,
		hatchetv1alpha1.ReasonReady, "Tenant is bound and the worker Secret is up to date")
	// Recheck periodically so consent changes (e.g. namespace labels) are noticed.
	return ctrl.Result{RequeueAfter: max(min(rotateAt.Sub(r.Clock.Now()), recheckInterval), time.Second)}, nil
}

// accept sets the Accepted condition: the instance exists and allows the tenant's namespace.
func (r *HatchetTenantReconciler) accept(ctx context.Context, tenant *hatchetv1alpha1.HatchetTenant,
	instance *hatchetv1alpha1.HatchetInstance) error {
	reject := func(reason, message string) error {
		setCondition(&tenant.Status.Conditions, tenant.Generation, hatchetv1alpha1.ConditionAccepted, false, reason, message)
		return &conditionError{reason: reason, message: message}
	}
	ref := tenant.Spec.InstanceRef
	if instance == nil || !instance.DeletionTimestamp.IsZero() {
		return reject(hatchetv1alpha1.ReasonInstanceNotFound, fmt.Sprintf("HatchetInstance %s/%s not found", ref.Namespace, ref.Name))
	}

	var nsLabels map[string]string
	if instance.Spec.AllowedNamespaces.From == hatchetv1alpha1.NamespacesFromSelector {
		var ns corev1.Namespace
		if err := r.Get(ctx, client.ObjectKey{Name: tenant.Namespace}, &ns); err != nil {
			return err
		}
		nsLabels = ns.Labels
	}
	allowed, err := namespaceAllowed(instance.Spec.AllowedNamespaces, instance.Namespace, tenant.Namespace, nsLabels)
	if err != nil {
		return reject(hatchetv1alpha1.ReasonNotAllowed, fmt.Sprintf("invalid allowedNamespaces selector: %v", err))
	}
	if !allowed {
		// Losing consent stops reconciliation but keeps the existing Secret and token.
		return reject(hatchetv1alpha1.ReasonNotAllowed,
			fmt.Sprintf("HatchetInstance %s/%s does not allow namespace %s", ref.Namespace, ref.Name, tenant.Namespace))
	}
	setCondition(&tenant.Status.Conditions, tenant.Generation, hatchetv1alpha1.ConditionAccepted, true,
		hatchetv1alpha1.ReasonAccepted, "HatchetInstance accepts this tenant")
	return nil
}

// namespaceAllowed reports whether allowed admits namespace for an instance in instanceNamespace.
func namespaceAllowed(allowed hatchetv1alpha1.AllowedNamespaces, instanceNamespace, namespace string, nsLabels map[string]string) (bool, error) {
	switch allowed.From {
	case hatchetv1alpha1.NamespacesFromAll:
		return true, nil
	case hatchetv1alpha1.NamespacesFromSelector:
		if allowed.Selector == nil {
			return false, nil
		}
		selector, err := metav1.LabelSelectorAsSelector(allowed.Selector)
		if err != nil {
			return false, err
		}
		return selector.Matches(labels.Set(nsLabels)), nil
	default:
		return namespace == instanceNamespace, nil
	}
}

// bind resolves the Hatchet tenant from the annotation and status, inheriting or creating it.
func (r *HatchetTenantReconciler) bind(ctx context.Context, hc hatchet.Client, tenant *hatchetv1alpha1.HatchetTenant) (time.Duration, error) {
	annotated := tenant.Annotations[hatchetv1alpha1.TenantIDAnnotation]
	bound := tenant.Status.TenantID

	if bound != "" {
		if annotated == "" {
			return 0, r.patchTenantID(ctx, tenant, bound)
		}
		if annotated != bound {
			return 0, tenantIDConflict(annotated, bound)
		}
		return 0, nil
	}

	if annotated != "" {
		if _, err := uuid.Parse(annotated); err != nil {
			return 0, &conditionError{reason: hatchetv1alpha1.ReasonInheritRejected, message: fmt.Sprintf(
				"annotation %s is not a valid UUID", hatchetv1alpha1.TenantIDAnnotation)}
		}
		// One message for both cases so the result does not reveal whether a tenant exists.
		rejected := &conditionError{reason: hatchetv1alpha1.ReasonInheritRejected, message: fmt.Sprintf(
			"tenant %s does not exist or was created by the controller for another namespace", annotated)}
		t, err := hc.GetTenant(ctx, annotated)
		if errors.Is(err, hatchet.ErrNotFound) {
			return 0, rejected
		}
		if err != nil {
			return 0, hatchetError(err)
		}
		if !inheritAllowed(t.Slug, tenant.Namespace) {
			return 0, rejected
		}
		tenant.Status.TenantID = t.ID
		tenant.Status.Slug = t.Slug
		return 0, nil
	}

	// Confirm with one uncached read that the tenant is still unbound before creating it.
	var latest hatchetv1alpha1.HatchetTenant
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(tenant), &latest); err != nil {
		return 0, err
	}
	if latest.Annotations[hatchetv1alpha1.TenantIDAnnotation] != "" || latest.Status.TenantID != "" {
		return cacheLagRequeue, nil
	}

	slug := tenantSlug(tenant)
	t, err := hc.CreateTenant(ctx, tenantDisplayName(tenant), slug)
	if errors.Is(err, hatchet.ErrSlugTaken) {
		return 0, &conditionError{reason: hatchetv1alpha1.ReasonSlugTaken, message: fmt.Sprintf(
			"tenant slug %s already exists. If a TenantCreated Event names its ID, set it as annotation %s "+
				"(or ask the Hatchet admin for the tenant with this slug), or recreate the HatchetTenant",
			slug, hatchetv1alpha1.TenantIDAnnotation)}
	}
	if err != nil {
		return 0, hatchetError(err)
	}
	r.Recorder.Eventf(tenant, nil, corev1.EventTypeNormal, "TenantCreated", "CreateTenant",
		"Created Hatchet tenant %s with slug %s", t.ID, t.Slug)
	if err := r.patchTenantID(ctx, tenant, t.ID); err != nil {
		return 0, err
	}
	tenant.Status.TenantID = t.ID
	tenant.Status.Slug = t.Slug
	return 0, nil
}

// patchTenantID writes the tenant-id annotation with an optimistic lock, retried in-process.
func (r *HatchetTenantReconciler) patchTenantID(ctx context.Context, tenant *hatchetv1alpha1.HatchetTenant, id string) error {
	retriable := func(err error) bool {
		var cerr *conditionError
		return !apierrors.IsNotFound(err) && !errors.As(err, &cerr)
	}
	err := retry.OnError(retry.DefaultBackoff, retriable, func() error {
		var latest hatchetv1alpha1.HatchetTenant
		if err := r.Get(ctx, client.ObjectKeyFromObject(tenant), &latest); err != nil {
			return err
		}
		current := latest.Annotations[hatchetv1alpha1.TenantIDAnnotation]
		if current == id {
			return nil
		}
		if current != "" {
			return tenantIDConflict(current, id)
		}
		patch := client.MergeFromWithOptions(latest.DeepCopy(), client.MergeFromWithOptimisticLock{})
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations[hatchetv1alpha1.TenantIDAnnotation] = id
		return r.Patch(ctx, &latest, patch)
	})
	if err != nil {
		return err
	}
	if tenant.Annotations == nil {
		tenant.Annotations = map[string]string{}
	}
	tenant.Annotations[hatchetv1alpha1.TenantIDAnnotation] = id
	return nil
}

// tenantIDConflict reports a tenant-id annotation that differs from the bound tenant ID.
func tenantIDConflict(annotated, bound string) *conditionError {
	return &conditionError{reason: hatchetv1alpha1.ReasonTenantIDConflict, message: fmt.Sprintf(
		"annotation %s is %s but the tenant is bound to %s", hatchetv1alpha1.TenantIDAnnotation, annotated, bound)}
}

// ensureDisplayName renames the Hatchet tenant when its name differs from the desired one.
func (r *HatchetTenantReconciler) ensureDisplayName(ctx context.Context, hc hatchet.Client, tenant *hatchetv1alpha1.HatchetTenant) error {
	t, err := hc.GetTenant(ctx, tenant.Status.TenantID)
	if err != nil {
		return hatchetError(err)
	}
	name := tenantDisplayName(tenant)
	if t.Name == name {
		return nil
	}
	if err := hc.UpdateTenantName(ctx, t.ID, name); err != nil {
		return hatchetError(err)
	}
	r.Recorder.Eventf(tenant, nil, corev1.EventTypeNormal, "TenantRenamed", "UpdateTenant",
		"Renamed Hatchet tenant %s to %q", t.ID, name)
	return nil
}

// ensureToken issues, rotates and stores the worker token. It returns the next rotation time.
func (r *HatchetTenantReconciler) ensureToken(ctx context.Context, hc hatchet.Client, tenant *hatchetv1alpha1.HatchetTenant,
	instance *hatchetv1alpha1.HatchetInstance) (time.Time, time.Duration, error) {
	owned := ownedObject{
		client: r.Client, apiReader: r.APIReader, scheme: r.Scheme,
		owner: tenant, ownerKind: "HatchetTenant", component: "worker-credentials",
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: WorkerSecretName(tenant), Namespace: tenant.Namespace}}
	err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret)
	missing := apierrors.IsNotFound(err)
	if err != nil && !missing {
		return time.Time{}, 0, err
	}
	if missing || !metav1.IsControlledBy(secret, tenant) {
		// Confirm with one uncached read before minting.
		requeueAfter, err := owned.resolve(ctx, secret)
		if err != nil || requeueAfter > 0 {
			return time.Time{}, requeueAfter, ownershipCondition(err)
		}
		missing = true
		secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: WorkerSecretName(tenant), Namespace: tenant.Namespace}}
	}

	now := r.Clock.Now()
	issuedAt, expiresAt, ok := tokenTimes(secret)
	if !missing && ok && now.Before(rotationTime(issuedAt, expiresAt)) {
		// Keep the token and refresh the connection settings.
		requeueAfter, err := owned.ensure(ctx, secret, func() error {
			writeWorkerSecret(secret, tenant, instance, nil)
			return nil
		})
		if err != nil {
			return time.Time{}, 0, ownershipCondition(err)
		}
		tenant.Status.TokenExpiresAt = &metav1.Time{Time: expiresAt}
		return rotationTime(issuedAt, expiresAt), requeueAfter, nil
	}

	ttl := defaultTokenTTL
	if tenant.Spec.TokenTTL != nil {
		ttl = tenant.Spec.TokenTTL.Duration
	}
	token, err := hc.CreateAPIToken(ctx, tenant.Status.TenantID, tokenPrefix(tenant)+fmt.Sprint(now.Unix()), ttl)
	if err != nil {
		return time.Time{}, 0, hatchetError(err)
	}
	// A missing Secret is the documented rotate-and-revoke action: revoke every other token of this
	// CR before storing the new one. On failure the Secret stays missing, so the next reconcile
	// mints again and revokes this token as another one.
	if missing {
		if err := r.revokeTokens(ctx, hc, tenant, token.ID); err != nil {
			return time.Time{}, 0, hatchetError(err)
		}
	}
	issued := &issuedToken{Token: token, issuedAt: now.UTC().Truncate(time.Second), expiresAt: now.Add(ttl).UTC().Truncate(time.Second)}

	requeueAfter, err := owned.ensure(ctx, secret, func() error {
		writeWorkerSecret(secret, tenant, instance, issued)
		return nil
	})
	if err != nil || requeueAfter > 0 {
		revoke := true
		if requeueAfter == 0 && !isOwnershipError(err) {
			// Possibly ambiguous write error: revoke only if the Secret does not hold the token.
			stored, rerr := r.tokenStored(ctx, secret, token.ID)
			if rerr != nil {
				return time.Time{}, 0, errors.Join(err, rerr)
			}
			revoke = !stored
		}
		if revoke {
			if rerr := hc.RevokeAPIToken(ctx, tenant.Status.TenantID, token.ID); rerr != nil {
				return time.Time{}, 0, hatchetError(rerr)
			}
			return time.Time{}, requeueAfter, ownershipCondition(err)
		}
	}
	tenant.Status.TokenExpiresAt = &metav1.Time{Time: issued.expiresAt}
	return rotationTime(issued.issuedAt, issued.expiresAt), 0, nil
}

// tokenStored re-reads the Secret through the APIReader and reports whether it holds tokenID.
func (r *HatchetTenantReconciler) tokenStored(ctx context.Context, secret *corev1.Secret, tokenID string) (bool, error) {
	var current corev1.Secret
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(secret), &current); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return current.Annotations[hatchetv1alpha1.TokenIDAnnotation] == tokenID, nil
}

// revokeTokens revokes all non-expired tokens of the tenant CR except keepID.
func (r *HatchetTenantReconciler) revokeTokens(ctx context.Context, hc hatchet.Client, tenant *hatchetv1alpha1.HatchetTenant, keepID string) error {
	tokens, err := hc.ListAPITokens(ctx, tenant.Status.TenantID)
	if err != nil {
		return err
	}
	now := r.Clock.Now()
	for _, t := range tokens {
		if t.ID == keepID || !strings.HasPrefix(t.Name, tokenPrefix(tenant)) || !t.ExpiresAt.After(now) {
			continue
		}
		if err := hc.RevokeAPIToken(ctx, tenant.Status.TenantID, t.ID); err != nil {
			return err
		}
	}
	return nil
}

// ensureNetworkPolicy maintains the worker egress NetworkPolicy.
func (r *HatchetTenantReconciler) ensureNetworkPolicy(ctx context.Context, tenant *hatchetv1alpha1.HatchetTenant,
	instance *hatchetv1alpha1.HatchetInstance) (time.Duration, error) {
	spec, err := egressPolicySpec(instance)
	if err != nil {
		return 0, &conditionError{reason: hatchetv1alpha1.ReasonInvalidSpec, message: "networkPolicy ports: " + err.Error()}
	}
	owned := ownedObject{
		client: r.Client, apiReader: r.APIReader, scheme: r.Scheme,
		owner: tenant, ownerKind: "HatchetTenant", component: "worker-egress",
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: NetworkPolicyName(tenant), Namespace: tenant.Namespace}}
	requeueAfter, err := owned.ensure(ctx, np, func() error {
		np.Spec = spec
		return nil
	})
	if apierrors.IsForbidden(err) {
		return 0, &conditionError{reason: hatchetv1alpha1.ReasonForbidden,
			message: "the controller lacks NetworkPolicy permissions; install the networkpolicy component"}
	}
	return requeueAfter, ownershipCondition(err)
}

// reconcileDelete revokes the CR's tokens, deletes its webhooks, members and invites and releases the finalizer.
func (r *HatchetTenantReconciler) reconcileDelete(ctx context.Context, tenant *hatchetv1alpha1.HatchetTenant,
	instance *hatchetv1alpha1.HatchetInstance) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(tenant, hatchetv1alpha1.TenantFinalizer) {
		return ctrl.Result{}, nil
	}

	// Without a usable instance there is nothing to revoke against; release immediately.
	if instance != nil && instance.DeletionTimestamp.IsZero() && tenant.Status.TenantID != "" {
		err := r.revokeAll(ctx, tenant, instance)
		if err != nil {
			if r.Clock.Since(tenant.DeletionTimestamp.Time) >= revocationGracePeriod {
				base := tenant.DeepCopy()
				message := "token revocation failed; delete the HatchetInstance or remove the finalizer to give up"
				var cerr *conditionError
				if errors.As(err, &cerr) {
					message = cerr.message + "; delete the HatchetInstance or remove the finalizer to give up"
				}
				setCondition(&tenant.Status.Conditions, tenant.Generation, hatchetv1alpha1.ConditionReady, false,
					hatchetv1alpha1.ReasonRevocationFailed, message)
				if perr := r.Status().Patch(ctx, tenant, client.MergeFrom(base)); perr != nil {
					return ctrl.Result{}, errors.Join(err, perr)
				}
			}
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(tenant, hatchetv1alpha1.TenantFinalizer)
	return ctrl.Result{}, r.Update(ctx, tenant)
}

func (r *HatchetTenantReconciler) revokeAll(ctx context.Context, tenant *hatchetv1alpha1.HatchetTenant,
	instance *hatchetv1alpha1.HatchetInstance) error {
	hc, err := r.ClientFactory(ctx, instance)
	if err != nil {
		return err
	}
	if err := r.revokeTokens(ctx, hc, tenant, ""); err != nil {
		return hatchetError(err)
	}
	for _, w := range tenant.Status.Webhooks {
		if err := hc.DeleteWebhook(ctx, tenant.Status.TenantID, w.Name); err != nil && !errors.Is(err, hatchet.ErrNotFound) {
			return scopedError("webhook "+w.Name, hatchetv1alpha1.ReasonWebhookFailed, err)
		}
	}
	return r.deleteTrackedMembers(ctx, hc, tenant)
}

// SetupWithManager registers the field index and the controller with the manager.
func (r *HatchetTenantReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &hatchetv1alpha1.HatchetTenant{}, InstanceRefIndex,
		func(obj client.Object) []string {
			ref := obj.(*hatchetv1alpha1.HatchetTenant).Spec.InstanceRef
			return []string{ref.Namespace + "/" + ref.Name}
		}); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &hatchetv1alpha1.HatchetTenant{}, WebhookSecretIndex,
		func(obj client.Object) []string {
			return webhookSecretNames(obj.(*hatchetv1alpha1.HatchetTenant))
		}); err != nil {
		return err
	}

	// The manager cache only holds controller-labeled Secrets, so referenced Secrets are watched
	// through a separate metadata-only cache that never stores Secret data.
	secretCache, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme:           mgr.GetScheme(),
		Mapper:           mgr.GetRESTMapper(),
		DefaultTransform: cache.TransformStripManagedFields(),
	})
	if err != nil {
		return err
	}
	if err := mgr.Add(secretCache); err != nil {
		return err
	}
	secretMeta := &metav1.PartialObjectMetadata{}
	secretMeta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))

	b := ctrl.NewControllerManagedBy(mgr).
		For(&hatchetv1alpha1.HatchetTenant{}).
		Owns(&corev1.Secret{}).
		Watches(&hatchetv1alpha1.HatchetInstance{}, handler.EnqueueRequestsFromMapFunc(r.tenantsForInstance)).
		WatchesRawSource(source.Kind[client.Object](secretCache, secretMeta,
			handler.EnqueueRequestsFromMapFunc(r.tenantsForSecret))).
		Named("hatchettenant")
	if r.NetworkPolicyEnabled {
		b = b.Owns(&networkingv1.NetworkPolicy{})
	}
	return b.Complete(r)
}

// tenantsForInstance maps a HatchetInstance to the HatchetTenants referencing it.
func (r *HatchetTenantReconciler) tenantsForInstance(ctx context.Context, obj client.Object) []reconcile.Request {
	var tenants hatchetv1alpha1.HatchetTenantList
	if err := r.List(ctx, &tenants, client.MatchingFields{InstanceRefIndex: obj.GetNamespace() + "/" + obj.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing tenants for instance")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(tenants.Items))
	for _, t := range tenants.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&t)})
	}
	return requests
}

// tenantsForSecret maps a Secret to the HatchetTenants whose webhooks reference it.
func (r *HatchetTenantReconciler) tenantsForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	var tenants hatchetv1alpha1.HatchetTenantList
	if err := r.List(ctx, &tenants, client.InNamespace(obj.GetNamespace()),
		client.MatchingFields{WebhookSecretIndex: obj.GetName()}); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "listing tenants for secret")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(tenants.Items))
	for _, t := range tenants.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&t)})
	}
	return requests
}

// webhookSecretNames returns the names of the Secrets referenced by the tenant's webhooks.
func webhookSecretNames(tenant *hatchetv1alpha1.HatchetTenant) []string {
	var names []string
	for _, w := range tenant.Spec.Webhooks {
		switch a := w.Auth; {
		case a.Basic != nil:
			names = append(names, a.Basic.PasswordSecretRef.Name)
		case a.APIKey != nil:
			names = append(names, a.APIKey.APIKeySecretRef.Name)
		case a.HMAC != nil:
			names = append(names, a.HMAC.SigningSecretRef.Name)
		}
	}
	return names
}

// isOwnershipError reports a conflict with a foreign object.
func isOwnershipError(err error) bool {
	var conflict *conflictError
	return errors.As(err, &conflict)
}

// ownershipCondition maps a *conflictError to Ready=False/Conflict and passes other errors through.
func ownershipCondition(err error) error {
	var conflict *conflictError
	if errors.As(err, &conflict) {
		return &conditionError{reason: hatchetv1alpha1.ReasonConflict, message: conflict.Error()}
	}
	return err
}
