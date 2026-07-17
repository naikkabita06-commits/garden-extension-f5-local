package lifecycle

import (
	"context"

	"github.com/go-logr/logr"

	extensioncontroller "github.com/gardener/gardener/extensions/pkg/controller/extension"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"

	f5v1alpha1 "github.com/gardener/gardener-extension-f5/pkg/apis/f5/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	extensionTypeLegacy            = "f5"
	extensionTypeAirtel            = "f5-loadbalancer"
	finalizerName                  = "extensions.gardener.cloud/f5"
	controllerName                 = "f5-extension-controller"
	gardenerOperationAnnotationKey = "gardener.cloud/operation"
)

func clearGardenerOperationAnnotation(ann map[string]string) bool {
	if ann == nil {
		return false
	}
	if _, ok := ann[gardenerOperationAnnotationKey]; !ok {
		return false
	}
	delete(ann, gardenerOperationAnnotationKey)
	return true
}

func isSupportedExtensionType(t string) bool {
	switch t {
	case extensionTypeLegacy, extensionTypeAirtel:
		return true
	default:
		return false
	}
}

type ExtensionReconciler struct {
	Client   client.Client
	Log      logr.Logger
	Scheme   *runtime.Scheme
	Actuator extensioncontroller.Actuator
	Recorder record.EventRecorder
}

func AddToManager(mgr ctrl.Manager, log logr.Logger) error {
	r := &ExtensionReconciler{
		Client:   mgr.GetClient(),
		Log:      log.WithName(controllerName),
		Scheme:   mgr.GetScheme(),
		Actuator: NewActuator(mgr.GetClient()),
		Recorder: mgr.GetEventRecorderFor(controllerName),
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&extensionsv1alpha1.Extension{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToExtensions)).
		Complete(r)
}

// mapSecretToExtensions maps a Secret change to the Extensions that reference it via their
// F5LoadBalancerConfig's credentialsSecretRef. This enables credential rotation: when the
// Secret is updated, the Extension is re-reconciled, which re-reads credentials and updates
// the svc-lb-bridge Deployment (triggering a rolling restart with fresh tokens).
func (r *ExtensionReconciler) mapSecretToExtensions(ctx context.Context, obj client.Object) []reconcile.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	cfgList := &f5v1alpha1.F5LoadBalancerConfigList{}
	if err := r.Client.List(ctx, cfgList, client.InNamespace(secret.Namespace)); err != nil {
		r.Log.Error(err, "failed to list F5LoadBalancerConfigs for Secret watch", "secret", secret.Name, "namespace", secret.Namespace)
		return nil
	}

	var requests []reconcile.Request
	for i := range cfgList.Items {
		cfg := &cfgList.Items[i]
		if cfg.Spec.CredentialsSecretRef != nil &&
			cfg.Spec.CredentialsSecretRef.Name == secret.Name &&
			(cfg.Spec.CredentialsSecretRef.Namespace == "" || cfg.Spec.CredentialsSecretRef.Namespace == secret.Namespace) {
			requests = append(requests, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: cfg.Namespace,
					Name:      cfg.Name,
				},
			})
		}
	}

	if len(requests) > 0 {
		r.Log.Info("Credentials Secret changed; re-reconciling Extensions", "secret", secret.Name, "namespace", secret.Namespace, "extensionCount", len(requests))
	}
	return requests
}

func (r *ExtensionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("extension", req.NamespacedName.String())

	ex := &extensionsv1alpha1.Extension{}
	if err := r.Client.Get(ctx, req.NamespacedName, ex); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !isSupportedExtensionType(ex.Spec.Type) {
		return ctrl.Result{}, nil
	}

	if ex.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(ex, finalizerName) {
			controllerutil.AddFinalizer(ex, finalizerName)
			if err := r.Client.Update(ctx, ex); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		if err := r.Actuator.Reconcile(ctx, log, ex); err != nil {
			log.Error(err, "Actuator Reconcile failed")
			r.Recorder.Eventf(ex, corev1.EventTypeWarning, "ReconcileFailed", "F5 extension reconciliation failed: %v", err)
			return ctrl.Result{}, err
		}
		r.Recorder.Event(ex, corev1.EventTypeNormal, "Reconciled", "F5 extension reconciled successfully")

		// Gardener handshake: gardenlet sets gardener.cloud/operation and expects the extension controller
		// to clear it once the operation has been handled.
		//
		// If we don't clear it, gardenlet will keep waiting and the Shoot reconciliation will not progress.
		// We only clear the operation key and intentionally keep gardener.cloud/timestamp.
		latest := &extensionsv1alpha1.Extension{}
		if err := r.Client.Get(ctx, req.NamespacedName, latest); err != nil {
			return ctrl.Result{}, err
		}
		base := latest.DeepCopy()
		if clearGardenerOperationAnnotation(latest.Annotations) {
			patch := client.MergeFrom(base)
			if err := r.Client.Patch(ctx, latest, patch); err != nil {
				log.Error(err, "Failed clearing gardener operation annotation")
				return ctrl.Result{}, err
			}
			log.V(1).Info("Cleared gardener operation annotation", "key", gardenerOperationAnnotationKey)
		}
	} else {
		if controllerutil.ContainsFinalizer(ex, finalizerName) {
			if err := r.Actuator.Delete(ctx, log, ex); err != nil {
				log.Error(err, "Actuator Delete failed")
				r.Recorder.Eventf(ex, corev1.EventTypeWarning, "DeleteFailed", "F5 extension deletion failed: %v", err)
				return ctrl.Result{}, err
			}
			r.Recorder.Event(ex, corev1.EventTypeNormal, "Deleted", "F5 extension resources cleaned up successfully")

			// Same handshake on delete: clear operation key if present.
			latest := &extensionsv1alpha1.Extension{}
			if err := r.Client.Get(ctx, req.NamespacedName, latest); err == nil {
				base := latest.DeepCopy()
				if clearGardenerOperationAnnotation(latest.Annotations) {
					patch := client.MergeFrom(base)
					_ = r.Client.Patch(ctx, latest, patch)
				}
			}

			controllerutil.RemoveFinalizer(ex, finalizerName)
			if err := r.Client.Update(ctx, ex); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{}, nil
}
