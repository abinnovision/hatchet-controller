package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
	"github.com/abinnovision/hatchet-controller/internal/keyset"
)

// HatchetInstanceReconciler generates and maintains the keyset Secrets of a HatchetInstance.
type HatchetInstanceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader performs uncached reads to resolve AlreadyExists and cache misses.
	APIReader client.Reader
	Recorder  events.EventRecorder
}

// NewHatchetInstanceReconciler returns a HatchetInstanceReconciler wired to the manager.
func NewHatchetInstanceReconciler(mgr ctrl.Manager) *HatchetInstanceReconciler {
	return &HatchetInstanceReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
		Recorder:  mgr.GetEventRecorder(hatchetv1alpha1.ControllerName),
	}
}

// +kubebuilder:rbac:groups=hatchet-controller.abigroup.io,resources=hatchetinstances,verbs=get;list;watch
// +kubebuilder:rbac:groups=hatchet-controller.abigroup.io,resources=hatchetinstances/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hatchet-controller.abigroup.io,resources=hatchetinstances/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// Reconcile ensures the signing key and public keyset Secrets and sets the Ready condition.
func (r *HatchetInstanceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var instance hatchetv1alpha1.HatchetInstance
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	base := instance.DeepCopy()

	requeueAfter, err := r.ensureKeys(ctx, &instance)
	var conflict *conflictError
	switch {
	case errors.As(err, &conflict):
		setCondition(&instance.Status.Conditions, instance.Generation, hatchetv1alpha1.ConditionReady, false,
			hatchetv1alpha1.ReasonConflict, conflict.Error())
	case err != nil:
		return ctrl.Result{}, err
	case requeueAfter > 0:
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	default:
		instance.Status.PublicKeysetSecret = PublicKeysetSecretName(&instance)
		setCondition(&instance.Status.Conditions, instance.Generation, hatchetv1alpha1.ConditionReady, true,
			hatchetv1alpha1.ReasonReady, "Keyset Secrets are up to date")
	}

	instance.Status.ObservedGeneration = instance.Generation
	if err := r.Status().Patch(ctx, &instance, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// ensureKeys creates the signing key once and publishes the public keyset derived from it.
func (r *HatchetInstanceReconciler) ensureKeys(ctx context.Context, instance *hatchetv1alpha1.HatchetInstance) (time.Duration, error) {
	owned := ownedObject{
		client: r.Client, apiReader: r.APIReader, scheme: r.Scheme,
		owner: instance, ownerKind: "HatchetInstance", component: "signing-key",
	}
	signingKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: SigningKeySecretName(instance), Namespace: instance.Namespace}}
	requeueAfter, err := owned.ensure(ctx, signingKey, func() error {
		if !signingKey.CreationTimestamp.IsZero() {
			return nil
		}
		private, err := keyset.Generate()
		if err != nil {
			return fmt.Errorf("generating signing key: %w", err)
		}
		signingKey.Type = corev1.SecretTypeOpaque
		signingKey.Data = map[string][]byte{keyset.PrivateKey: private}
		return nil
	})
	if err != nil || requeueAfter > 0 {
		return requeueAfter, err
	}

	public, err := keyset.PublicKeysetBase64(signingKey.Data[keyset.PrivateKey])
	if err != nil {
		return 0, fmt.Errorf("deriving public keyset from Secret %s: %w", signingKey.Name, err)
	}

	owned.component = "exchange-keyset"
	publicKeyset := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: PublicKeysetSecretName(instance), Namespace: instance.Namespace}}
	return owned.ensure(ctx, publicKeyset, func() error {
		if publicKeyset.CreationTimestamp.IsZero() {
			publicKeyset.Type = corev1.SecretTypeOpaque
		}
		publicKeyset.Data = map[string][]byte{keyset.PublicKey: []byte(public)}
		return nil
	})
}

// SetupWithManager registers the controller with the manager.
func (r *HatchetInstanceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&hatchetv1alpha1.HatchetInstance{}).
		Owns(&corev1.Secret{}).
		Named("hatchetinstance").
		Complete(r)
}

// setCondition sets a condition with the given generation.
func setCondition(conditions *[]metav1.Condition, generation int64, conditionType string, ok bool, reason, message string) {
	status := metav1.ConditionFalse
	if ok {
		status = metav1.ConditionTrue
	}
	meta.SetStatusCondition(conditions, metav1.Condition{
		Type:               conditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}
