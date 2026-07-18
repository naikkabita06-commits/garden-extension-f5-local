package finalizers

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Ensure adds a finalizer with conflict-safe merge patching. It returns true
// when the object was changed.
func Ensure(ctx context.Context, c client.Client, obj client.Object, name string) (bool, error) {
	if controllerutil.ContainsFinalizer(obj, name) {
		return false, nil
	}
	base := obj.DeepCopyObject().(client.Object)
	controllerutil.AddFinalizer(obj, name)
	return true, c.Patch(ctx, obj, client.MergeFrom(base))
}

// Remove removes a finalizer with conflict-safe merge patching. It returns true
// when the object was changed.
func Remove(ctx context.Context, c client.Client, obj client.Object, name string) (bool, error) {
	if !controllerutil.ContainsFinalizer(obj, name) {
		return false, nil
	}
	base := obj.DeepCopyObject().(client.Object)
	controllerutil.RemoveFinalizer(obj, name)
	return true, c.Patch(ctx, obj, client.MergeFrom(base))
}
