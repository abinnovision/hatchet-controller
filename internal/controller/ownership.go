package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	hatchetv1alpha1 "github.com/abinnovision/hatchet-controller/api/v1alpha1"
)

// ComponentLabel identifies the role of a generated object.
const ComponentLabel = "app.kubernetes.io/component"

// errForeign is returned by mutate functions for existing objects not controlled by the owner.
var errForeign = errors.New("object is not controlled by this resource")

// conflictError reports an existing object that is not controlled by the owner.
type conflictError struct {
	obj client.Object
}

func (e *conflictError) Error() string {
	return fmt.Sprintf("%s %s already exists and is not controlled by this resource", kindOf(e.obj), e.obj.GetName())
}

func kindOf(obj client.Object) string {
	return reflect.TypeOf(obj).Elem().Name()
}

// ownedObject ensures obj is controlled by owner with the managed-by and component labels and
// applies mutate. Existing objects controlled by someone else are left unchanged. AlreadyExists
// and foreign-looking cached objects are resolved with one uncached read.
type ownedObject struct {
	client    client.Client
	apiReader client.Reader
	scheme    *runtime.Scheme
	owner     client.Object
	ownerKind string
	component string
}

// ensure returns a non-zero requeueAfter when the object could not be written yet.
func (o ownedObject) ensure(ctx context.Context, obj client.Object, mutate func() error) (requeueAfter time.Duration, err error) {
	_, err = controllerutil.CreateOrUpdate(ctx, o.client, obj, func() error {
		if obj.GetUID() != "" && !metav1.IsControlledBy(obj, o.owner) {
			return errForeign
		}
		if err := mutate(); err != nil {
			return err
		}
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[hatchetv1alpha1.ManagedByLabel] = hatchetv1alpha1.ControllerName
		labels[ComponentLabel] = o.component
		obj.SetLabels(labels)
		return controllerutil.SetControllerReference(o.owner, obj, o.scheme)
	})
	if err == nil {
		return 0, nil
	}
	if !errors.Is(err, errForeign) && !apierrors.IsAlreadyExists(err) {
		return 0, err
	}
	requeueAfter, err = o.resolve(ctx, obj)
	if err == nil && requeueAfter == 0 {
		// Deleted since the failed write.
		requeueAfter = cacheLagRequeue
	}
	return requeueAfter, err
}

// resolve classifies obj with one uncached read. It returns zero if obj does not exist,
// cacheLagRequeue if obj is ours and the cache lags (relabeling an object that lost the
// managed-by label and is therefore invisible to the cache), recheckInterval for a previous
// owner with the same kind and name pending GC, and a *conflictError for foreign objects.
func (o ownedObject) resolve(ctx context.Context, obj client.Object) (requeueAfter time.Duration, err error) {
	current := obj.DeepCopyObject().(client.Object)
	if err := o.apiReader.Get(ctx, client.ObjectKeyFromObject(current), current); err != nil {
		return 0, client.IgnoreNotFound(err)
	}
	if ref := metav1.GetControllerOf(current); !metav1.IsControlledBy(current, o.owner) {
		if ref != nil && ref.Kind == o.ownerKind && ref.Name == o.owner.GetName() {
			return recheckInterval, nil
		}
		return 0, &conflictError{obj: current}
	}
	if current.GetLabels()[hatchetv1alpha1.ManagedByLabel] != hatchetv1alpha1.ControllerName {
		base := current.DeepCopyObject().(client.Object)
		labels := current.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[hatchetv1alpha1.ManagedByLabel] = hatchetv1alpha1.ControllerName
		current.SetLabels(labels)
		if err := o.client.Patch(ctx, current, client.MergeFrom(base)); err != nil {
			return 0, err
		}
	}
	return cacheLagRequeue, nil
}
