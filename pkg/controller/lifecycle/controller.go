package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"

	extensionsconfigv1alpha1 "github.com/gardener/gardener/extensions/pkg/apis/config/v1alpha1"
	extensioncontroller "github.com/gardener/gardener/extensions/pkg/controller/extension"
	extensionsutil "github.com/gardener/gardener/extensions/pkg/util"
	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	f5v1alpha1 "github.com/gardener/gardener-extension-f5/pkg/apis/f5/v1alpha1"
	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	f5metrics "github.com/gardener/gardener-extension-f5/pkg/metrics"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

// actuator implements the Gardener extension actuator interface.
type actuator struct {
	client client.Client
}

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// permanent wraps an error as a permanentError to signal the reconciler should not retry.
// Input: err (error) — the original error. Output: error — wrapped permanentError, or nil if err is nil.
func permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// NewActuator creates a new actuator that implements the Gardener extension Actuator interface.
// Input: c (client.Client) — the Seed cluster client. Output: extensioncontroller.Actuator — the actuator instance.
func NewActuator(c client.Client) extensioncontroller.Actuator {
	return &actuator{client: c}
}

const (
	cisNamespace  = "f5-cis-system"
	cisName       = "f5-cis"
	cisSecretName = "f5-cis-credentials"
	bridgeName    = "f5-svc-lb-bridge"
)

// Reconcile is the main entry point called when an Extension/f5-loadbalancer resource is created or updated.
// Input: ctx, log, ex (Extension object). Output: error — nil on success, permanentError if config is invalid, transient error to retry.
// It orchestrates: ensure F5LoadBalancerConfig → provision control-plane LB via CMP → deploy CIS into Shoot.
func (a *actuator) Reconcile(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	log = log.WithValues("extension", ex.Name, "namespace", ex.Namespace)
	log.Info("Reconciling F5 extension")

	cfg, err := a.ensureF5LoadBalancerConfig(ctx, log, ex)
	if err != nil {
		f5metrics.ReconcileErrorsTotal.WithLabelValues("gardener-extension-f5").Inc()
		return err
	}

	// Safety guard: spec.controlPlaneReady is a dev/demo shortcut that bypasses CMP provisioning.
	// Using it while enablePerShootControlPlaneVIP=true risks marking the control-plane as ready
	// without an actual VIP being provisioned, which silently breaks kube-apiserver connectivity.
	if cfg.Spec.EnablePerShootControlPlaneVIP && cfg.Spec.ControlPlaneReady != nil {
		log.Info("WARNING: spec.controlPlaneReady is set alongside enablePerShootControlPlaneVIP=true. "+
			"This bypasses CMP provisioning. Only use this in dev/kind environments where CMP is not available. "+
			"In production, remove spec.controlPlaneReady and let the controller provision the VIP via CMP.",
			"controlPlaneReady", *cfg.Spec.ControlPlaneReady,
		)
	}

	key := types.NamespacedName{Namespace: ex.Namespace, Name: ex.Name}

	// Control-plane LB:
	// - If enablePerShootControlPlaneVIP=false (default): skip per-shoot VIP provisioning entirely.
	//   The shared Seed Ingress VIP (Mechanism A / vanilla Gardener) handles control-plane access.
	//   ControlPlaneLoadBalancerReady is set to True immediately so the app-plane gate still works.
	// - If enablePerShootControlPlaneVIP=true: provision a dedicated VIP per Shoot via CMP (Mechanism B).
	//   - If spec.controlPlaneReady is set, treat it as source-of-truth (out-of-band CMP).
	//   - Otherwise, if spec.ccpApiEndpoint is set, call CMP to provision the VIP/VS.
	//   - Otherwise fall back to dev-stub behaviour.
	if !cfg.Spec.EnablePerShootControlPlaneVIP {
		a.reconcileControlPlaneStatusSharedSeedIngress(log, cfg)
	} else if cfg.Spec.ControlPlaneReady == nil && cfg.Spec.CcpApiEndpoint != "" {
		if err := a.provisionControlPlaneViaCMP(ctx, log, ex, cfg); err != nil {
			// Ensure we persist status/conditions even if we return an error (to avoid silent retries).
			if cfg.Spec.EnableApplicationLB {
				meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
					Type:               "ApplicationLoadBalancerReady",
					Status:             metav1.ConditionFalse,
					Reason:             "Blocked",
					Message:            "control-plane LB provisioning failed; application LB is blocked until control-plane LB is Ready",
					LastTransitionTime: metav1.Now(),
				})
			}
			_ = a.client.Status().Update(ctx, cfg)

			var perr permanentError
			if errors.As(err, &perr) {
				_ = a.updateExtensionOutput(ctx, log, ex, cfg, gardencorev1beta1.LastOperationStateFailed, perr.Error())
				// Permanent/config error: do not hot-loop.
				return nil
			}
			_ = a.updateExtensionOutput(ctx, log, ex, cfg, gardencorev1beta1.LastOperationStateError, err.Error())
			return err
		}
	} else {
		a.reconcileControlPlaneStatus(log, cfg)
	}

	if cfg.Spec.EnableApplicationLB {
		// In this dev implementation, we allow CIS to run only when control-plane VIP is configured.
		// This matches Story 6 (gate app-plane on CP-LB readiness) without requiring CMP integration yet.
		if !isConditionTrue(cfg.Status.Conditions, "ControlPlaneLoadBalancerReady") {
			err := fmt.Errorf("control-plane LB is not ready/configured; not deploying CIS (if CMP provisions VIP/VS out-of-band, set spec.controlPlaneReady=true once ready)")
			meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
				Type:               "ApplicationLoadBalancerReady",
				Status:             metav1.ConditionFalse,
				Reason:             "Blocked",
				Message:            err.Error(),
				LastTransitionTime: metav1.Now(),
			})
			_ = a.client.Status().Update(ctx, cfg)
			_ = a.updateExtensionOutput(ctx, log, ex, cfg, gardencorev1beta1.LastOperationStateProcessing, err.Error())
			// Not an error: we are intentionally blocked until CP is ready.
			return nil
		}

		if err := a.reconcileCISInShoot(ctx, log, ex, cfg); err != nil {
			var perr permanentError
			if errors.As(err, &perr) {
				meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
					Type:               "ApplicationLoadBalancerReady",
					Status:             metav1.ConditionFalse,
					Reason:             "ConfigError",
					Message:            perr.Error(),
					LastTransitionTime: metav1.Now(),
				})
				_ = a.client.Status().Update(ctx, cfg)
				_ = a.updateExtensionOutput(ctx, log, ex, cfg, gardencorev1beta1.LastOperationStateFailed, perr.Error())
				// Permanent/config error: do not hot-loop.
				return nil
			}

			meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
				Type:               "ApplicationLoadBalancerReady",
				Status:             metav1.ConditionFalse,
				Reason:             "ReconcileFailed",
				Message:            err.Error(),
				LastTransitionTime: metav1.Now(),
			})
			_ = a.client.Status().Update(ctx, cfg)
			_ = a.updateExtensionOutput(ctx, log, ex, cfg, gardencorev1beta1.LastOperationStateError, err.Error())
			return err
		}

		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ApplicationLoadBalancerReady",
			Status:             metav1.ConditionTrue,
			Reason:             "Reconciled",
			Message:            "svc-lb-bridge reconciled in Shoot (CMP LBaaS)",
			LastTransitionTime: metav1.Now(),
		})
		if err := a.client.Status().Update(ctx, cfg); err != nil {
			return fmt.Errorf("updating F5LoadBalancerConfig status %s: %w", key.String(), err)
		}
		_ = a.updateExtensionOutput(ctx, log, ex, cfg, gardencorev1beta1.LastOperationStateSucceeded, "reconciled")
		return nil
	}

	// App LB disabled: ensure cleanup in Shoot.
	if err := a.cleanupCISInShoot(ctx, log, ex); err != nil {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ApplicationLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "CleanupFailed",
			Message:            err.Error(),
			LastTransitionTime: metav1.Now(),
		})
		_ = a.client.Status().Update(ctx, cfg)
		_ = a.updateExtensionOutput(ctx, log, ex, cfg, gardencorev1beta1.LastOperationStateError, err.Error())
		return err
	}

	meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               "ApplicationLoadBalancerReady",
		Status:             metav1.ConditionFalse,
		Reason:             "Disabled",
		Message:            "Application LB disabled; CIS removed from Shoot",
		LastTransitionTime: metav1.Now(),
	})
	if err := a.client.Status().Update(ctx, cfg); err != nil {
		return fmt.Errorf("updating F5LoadBalancerConfig status %s: %w", key.String(), err)
	}
	_ = a.updateExtensionOutput(ctx, log, ex, cfg, gardencorev1beta1.LastOperationStateSucceeded, "application LB disabled")

	log.Info("Application LB disabled; CIS cleaned up")
	return nil
}

type extensionProviderStatus struct {
	ControlPlaneVIP   string `json:"controlPlaneVip,omitempty"`
	ControlPlanePort  int32  `json:"controlPlanePort,omitempty"`
	BigIPManagementIP string `json:"bigIpManagementIp,omitempty"`
	// CMP resource IDs — populated during Migrate so Restore can re-hydrate F5LoadBalancerConfig.status
	// on the destination Seed without re-provisioning resources that already exist.
	LBServiceID       string `json:"lbServiceId,omitempty"`
	VIPPortID         string `json:"vipPortId,omitempty"`
	VirtualServerID   string `json:"virtualServerId,omitempty"`
	VirtualServerName string `json:"virtualServerName,omitempty"`
}

// updateExtensionOutput writes the VIP, port, and management IP back into the Extension's status.providerStatus.
// Input: ctx, log, ex (Extension), cfg (F5LoadBalancerConfig), state (Success/Error), description (human-readable message).
// Output: error — nil on success, error if status patch fails.
func (a *actuator) updateExtensionOutput(
	ctx context.Context,
	log logr.Logger,
	ex *extensionsv1alpha1.Extension,
	cfg *f5v1alpha1.F5LoadBalancerConfig,
	state gardencorev1beta1.LastOperationState,
	description string,
) error {
	if ex == nil {
		return nil
	}

	// Best-effort: discover the frontend port the control-plane VIP should expose.
	port := int32(443)
	if backends, err := a.discoverKubeAPIServerBackends(ctx, log, ex); err == nil {
		port = chooseFrontendPort(backends)
	}

	vip := cfg.Status.VIP
	if vip == "" {
		vip = cfg.Spec.ControlPlaneVIP
	}

	bigipMgmt := ""
	if cfg.Spec.CIS != nil {
		bigipMgmt = cfg.Spec.CIS.BigIPURL
		if u, err := url.Parse(cfg.Spec.CIS.BigIPURL); err == nil {
			if host := u.Hostname(); host != "" {
				bigipMgmt = host
			}
		}
	}

	ps := extensionProviderStatus{
		ControlPlaneVIP:   vip,
		ControlPlanePort:  port,
		BigIPManagementIP: bigipMgmt,
	}
	raw, err := json.Marshal(ps)
	if err != nil {
		return err
	}

	patch := client.MergeFrom(ex.DeepCopy())
	ex.Status.ProviderStatus = &runtime.RawExtension{Raw: raw}
	ex.Status.ObservedGeneration = ex.Generation
	ex.Status.LastError = nil
	ex.Status.LastOperation = &gardencorev1beta1.LastOperation{
		Type:           gardencorev1beta1.LastOperationTypeReconcile,
		State:          state,
		Progress:       progressForState(state),
		Description:    description,
		LastUpdateTime: metav1.Now(),
	}

	if err := a.client.Status().Patch(ctx, ex, patch); err != nil {
		return err
	}
	return nil
}

// progressForState maps a LastOperationState (Processing/Succeeded/Error) to a progress percentage (0-100).
// Input: state (LastOperationState). Output: int32 — progress value (50 for Processing, 100 for Succeeded, 0 for Error).
func progressForState(state gardencorev1beta1.LastOperationState) int32 {
	switch state {
	case gardencorev1beta1.LastOperationStateSucceeded:
		return 100
	case gardencorev1beta1.LastOperationStateFailed:
		return 100
	case gardencorev1beta1.LastOperationStateError:
		return 80
	case gardencorev1beta1.LastOperationStateProcessing:
		return 50
	case gardencorev1beta1.LastOperationStatePending:
		return 10
	default:
		return 1
	}
}

// ensureF5LoadBalancerConfig creates or syncs the F5LoadBalancerConfig CR from the Extension's providerConfig.
// Input: ctx, log, ex (Extension with providerConfig). Output: *F5LoadBalancerConfig — the created/updated CR, error on failure.
// If the CR exists and spec differs from providerConfig, it patches the CR. Sets owner reference to the Extension.
func (a *actuator) ensureF5LoadBalancerConfig(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) (*f5v1alpha1.F5LoadBalancerConfig, error) {
	key := types.NamespacedName{Namespace: ex.Namespace, Name: ex.Name}

	desiredSpec, specErr := desiredSpecFromProviderConfig(ex)
	if specErr != nil {
		return nil, specErr
	}

	cfg := &f5v1alpha1.F5LoadBalancerConfig{}
	if err := a.client.Get(ctx, key, cfg); err == nil {
		// If the config already exists and is owned by this Extension, keep its spec in sync
		// with the Extension.providerConfig. This avoids a common footgun where the CR was
		// created with defaults and later providerConfig changes are ignored.
		if hasProviderConfig(ex) {
			if isControlledByExtension(cfg, ex) {
				if !reflect.DeepEqual(cfg.Spec, desiredSpec) {
					baseObj := cfg.DeepCopyObject()
					base, ok := baseObj.(*f5v1alpha1.F5LoadBalancerConfig)
					if !ok || base == nil {
						return nil, fmt.Errorf("deep-copying F5LoadBalancerConfig %s failed", key.String())
					}
					cfg.Spec = desiredSpec
					ensureExtensionOwnerReference(cfg, ex)
					if err := a.client.Patch(ctx, cfg, client.MergeFrom(base)); err != nil {
						return nil, fmt.Errorf("patching F5LoadBalancerConfig %s spec: %w", key.String(), err)
					}
					log.Info("Updated F5LoadBalancerConfig spec from Extension.providerConfig", "config", key.String())
				}
			} else {
				log.Info("F5LoadBalancerConfig exists but is not controlled by Extension; skipping spec sync", "config", key.String())
			}
		}
		return cfg, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("getting F5LoadBalancerConfig %s: %w", key.String(), err)
	}

	newCfg := &f5v1alpha1.F5LoadBalancerConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ex.Namespace,
			Name:      ex.Name,
		},
		Spec: desiredSpec,
	}
	ensureExtensionOwnerReference(newCfg, ex)

	log.Info("F5LoadBalancerConfig not found; creating", "config", key.String())
	if err := a.client.Create(ctx, newCfg); err != nil {
		return nil, fmt.Errorf("creating F5LoadBalancerConfig %s: %w", key.String(), err)
	}

	return newCfg, nil
}

// hasProviderConfig checks whether the Extension has a non-empty providerConfig JSON blob.
// Input: ex (Extension). Output: bool — true if providerConfig exists and is non-empty.
func hasProviderConfig(ex *extensionsv1alpha1.Extension) bool {
	return ex != nil && ex.Spec.ProviderConfig != nil && len(ex.Spec.ProviderConfig.Raw) > 0
}

// desiredSpecFromProviderConfig parses the Extension's providerConfig JSON into an F5LoadBalancerConfigSpec.
// Input: ex (Extension). Output: F5LoadBalancerConfigSpec — parsed spec, error if JSON is malformed (permanent).
// Supports two JSON formats: {"spec": {...}} wrapper or flat {...} fields directly.
func desiredSpecFromProviderConfig(ex *extensionsv1alpha1.Extension) (f5v1alpha1.F5LoadBalancerConfigSpec, error) {
	spec := f5v1alpha1.F5LoadBalancerConfigSpec{}
	// Safe default: do not deploy CIS unless explicitly enabled via providerConfig.
	spec.EnableApplicationLB = false

	if !hasProviderConfig(ex) {
		return spec, nil
	}

	raw := ex.Spec.ProviderConfig.Raw
	// providerConfig may be either:
	//  - {"spec": { ...fields of F5LoadBalancerConfigSpec... }}
	//  - { ...fields of F5LoadBalancerConfigSpec... }
	var obj map[string]json.RawMessage
	unmarshalErr := json.Unmarshal(raw, &obj)
	if unmarshalErr == nil {
		if _, ok := obj["spec"]; ok {
			var wrapper struct {
				Spec f5v1alpha1.F5LoadBalancerConfigSpec `json:"spec"`
			}
			if err := json.Unmarshal(raw, &wrapper); err != nil {
				return spec, permanent(fmt.Errorf("decoding Extension.spec.providerConfig as wrapper with spec failed: %w", err))
			}
			spec = wrapper.Spec
			return spec, nil
		}
		if err := json.Unmarshal(raw, &spec); err != nil {
			return spec, permanent(fmt.Errorf("decoding Extension.spec.providerConfig as F5LoadBalancerConfigSpec failed: %w", err))
		}
		return spec, nil
	}
	return spec, permanent(fmt.Errorf("decoding Extension.spec.providerConfig failed: %w", unmarshalErr))
}

// isControlledByExtension checks if the F5LoadBalancerConfig has an ownerReference pointing to the given Extension.
// Input: cfg (F5LoadBalancerConfig), ex (Extension). Output: bool — true if cfg is owned by ex.
func isControlledByExtension(cfg *f5v1alpha1.F5LoadBalancerConfig, ex *extensionsv1alpha1.Extension) bool {
	if cfg == nil || ex == nil {
		return false
	}
	for _, ref := range cfg.OwnerReferences {
		if ref.Controller != nil && *ref.Controller && ref.UID == ex.UID {
			return true
		}
		// Backward-compatible: if the ownerRef matches kind/name but UID changed (e.g. delete/recreate),
		// we still consider it controlled so we can repair the ownerRef.
		if ref.Controller != nil && *ref.Controller && ref.Kind == ex.Kind && ref.Name == ex.Name {
			return true
		}
	}
	return false
}

// ensureExtensionOwnerReference adds or updates the controller ownerReference on the F5LoadBalancerConfig to the Extension.
// Input: cfg (F5LoadBalancerConfig to modify), ex (Extension that should own it). Output: none (modifies cfg in-place).
func ensureExtensionOwnerReference(cfg *f5v1alpha1.F5LoadBalancerConfig, ex *extensionsv1alpha1.Extension) {
	if cfg == nil || ex == nil {
		return
	}
	ctrlTrue := true
	ref := metav1.OwnerReference{
		APIVersion: ex.APIVersion,
		Kind:       ex.Kind,
		Name:       ex.Name,
		UID:        ex.UID,
		Controller: &ctrlTrue,
	}

	// Replace any existing controller ownerRef for this kind/name; otherwise append.
	for i := range cfg.OwnerReferences {
		r := cfg.OwnerReferences[i]
		if r.Kind == ref.Kind && r.Name == ref.Name && r.Controller != nil && *r.Controller {
			cfg.OwnerReferences[i] = ref
			return
		}
	}
	cfg.OwnerReferences = append(cfg.OwnerReferences, ref)
}

func ptr[T any](v T) *T { return &v }

// provisionControlPlaneViaCMP provisions the control-plane LB via CMP LBaaS API (LBService → VIP → VirtualServer).
// Input: ctx, log, ex (Extension), cfg (F5LoadBalancerConfig with CcpApiEndpoint and credentials).
// Output: error — nil on success; updates cfg.Status with VIP, LBServiceID, VirtualServerID on success.
func (a *actuator) provisionControlPlaneViaCMP(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension, cfg *f5v1alpha1.F5LoadBalancerConfig) error {
	if cfg.Spec.ControlPlaneVIP == "" {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "NotConfigured",
			Message:            "spec.controlPlaneVIP is empty",
			LastTransitionTime: metav1.Now(),
		})
		return permanent(fmt.Errorf("spec.controlPlaneVIP must not be empty"))
	}
	if cfg.Spec.CcpApiEndpoint == "" {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "NotConfigured",
			Message:            "spec.ccpApiEndpoint is empty (CMP/CCP provisioning disabled)",
			LastTransitionTime: metav1.Now(),
		})
		return permanent(fmt.Errorf("spec.ccpApiEndpoint must not be empty"))
	}
	if cfg.Spec.TenantOrPartition == "" {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "ConfigError",
			Message:            "spec.tenantOrPartition is empty",
			LastTransitionTime: metav1.Now(),
		})
		return permanent(fmt.Errorf("spec.tenantOrPartition must not be empty"))
	}

	if cfg.Spec.CredentialsSecretRef == nil || cfg.Spec.CredentialsSecretRef.Name == "" || cfg.Spec.CredentialsSecretRef.Namespace == "" {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "ConfigError",
			Message:            "spec.credentialsSecretRef.name and spec.credentialsSecretRef.namespace must be set when spec.ccpApiEndpoint is set",
			LastTransitionTime: metav1.Now(),
		})
		return permanent(fmt.Errorf("spec.credentialsSecretRef must be set when spec.ccpApiEndpoint is set"))
	}

	seedSecretRef := *cfg.Spec.CredentialsSecretRef
	seedSecret := &corev1.Secret{}
	if err := a.client.Get(ctx, types.NamespacedName{Namespace: seedSecretRef.Namespace, Name: seedSecretRef.Name}, seedSecret); err != nil {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "ConfigError",
			Message:            fmt.Sprintf("getting credentials secret %s/%s failed", seedSecretRef.Namespace, seedSecretRef.Name),
			LastTransitionTime: metav1.Now(),
		})
		return permanent(fmt.Errorf("getting credentials secret %s/%s: %w", seedSecretRef.Namespace, seedSecretRef.Name, err))
	}

	username, userErr := readSecretKey(seedSecret, "username", "F5_USERNAME")
	password, passErr := readSecretKey(seedSecret, "password", "F5_PASSWORD")

	ceAuth, ceErr := readSecretKey(seedSecret, "Ce-Auth", "ce-auth", "ceAuth", "ce_auth")
	projectID, projErr := readSecretKey(seedSecret, "project-id", "projectId", "project_id")

	// Automatic Ce-Auth token refresh: if long-lived API key credentials are
	// present (api-key-id + api-secret), generate a fresh short-lived Ce-Auth
	// token and update the Secret. This ensures tokens never expire while the
	// controller is running.
	ceAuth, ceErr = a.refreshCeAuthIfNeeded(ctx, log, seedSecret, ceAuth, ceErr)

	orgName := cfg.Spec.TenantOrPartition
	if orgFromSecret, orgErr := readSecretKey(seedSecret, "organisation-name", "organisationName", "organisation_name"); orgErr == nil && orgFromSecret != "" {
		orgName = orgFromSecret
	}

	backends, err := a.discoverKubeAPIServerBackends(ctx, log, ex)
	if err != nil {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "DiscoveryFailed",
			Message:            err.Error(),
			LastTransitionTime: metav1.Now(),
		})
		return err
	}

	vipPort := chooseFrontendPort(backends)

	var cmp f5client.Client
	if ceErr == nil && projErr == nil {
		cmp, err = f5client.NewClientWithCeAuth(log, cfg.Spec.CcpApiEndpoint, orgName, projectID, ceAuth)
	} else {
		if userErr != nil || passErr != nil {
			// Preserve the original secret-key error message if basic auth is attempted.
			missing := userErr
			if missing == nil {
				missing = passErr
			}
			meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
				Type:               "ControlPlaneLoadBalancerReady",
				Status:             metav1.ConditionFalse,
				Reason:             "ConfigError",
				Message:            fmt.Sprintf("credentials secret must contain either Ce-Auth + project-id (CMP token auth) or username/password (basic auth): %v", missing),
				LastTransitionTime: metav1.Now(),
			})
			return permanent(fmt.Errorf("invalid credentials secret: %v", missing))
		}
		cmp, err = f5client.NewClientWithBasicAuth(log, cfg.Spec.CcpApiEndpoint, orgName, username, password)
	}
	if err != nil {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "ConfigError",
			Message:            err.Error(),
			LastTransitionTime: metav1.Now(),
		})
		return permanent(err)
	}

	// Non-mutating CMP/CCP probe: helps distinguish endpoint reachability vs bad credentials.
	probe, perr := cmp.Probe(ctx)
	if perr != nil {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "CMPUnreachable",
			Message:            perr.Error(),
			LastTransitionTime: metav1.Now(),
		})
		return perr
	}
	if probe != nil {
		log.Info("CMP/CCP probe response",
			"method", probe.Method,
			"url", probe.URL,
			"statusCode", probe.StatusCode,
			"status", probe.Status,
			"requestID", probe.RequestID,
		)
		switch probe.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
				Type:               "ControlPlaneLoadBalancerReady",
				Status:             metav1.ConditionFalse,
				Reason:             "Unauthorized",
				Message:            fmt.Sprintf("CMP/CCP probe returned %s (reqID=%s)", probe.Status, probe.RequestID),
				LastTransitionTime: metav1.Now(),
			})
			return permanent(fmt.Errorf("CMP/CCP credentials rejected: %s", probe.Status))
		}
		if probe.StatusCode >= 500 {
			meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
				Type:               "ControlPlaneLoadBalancerReady",
				Status:             metav1.ConditionFalse,
				Reason:             "CMPUnavailable",
				Message:            fmt.Sprintf("CMP/CCP probe returned %s (reqID=%s)", probe.Status, probe.RequestID),
				LastTransitionTime: metav1.Now(),
			})
			return fmt.Errorf("CMP/CCP probe returned %s", probe.Status)
		}
	}

	log.Info("Provisioning control-plane VIP/VS via CMP/CCP API",
		"ccpApiEndpoint", cfg.Spec.CcpApiEndpoint,
		"tenantOrPartition", cfg.Spec.TenantOrPartition,
		"organisationName", orgName,
		"vip", cfg.Spec.ControlPlaneVIP,
		"vipPort", vipPort,
		"backendCount", len(backends),
	)

	// Configure CMP LBaaS provisioning parameters from spec.
	cmp.SetCMPLBaaSConfig(f5client.CMPLBaaSConfig{
		FlavorID:         cfg.Spec.FlavorID,
		NetworkID:        cfg.Spec.NetworkID,
		VPCID:            cfg.Spec.VPCID,
		VPCName:          cfg.Spec.VPCName,
		RoutingAlgorithm: cfg.Spec.RoutingAlgorithm,
		MonitorInterval:  cfg.Spec.MonitorInterval,
	})

	cmpStart := time.Now()
	ids, err := cmp.EnsureControlPlaneVirtualServer(ctx, cfg.Spec.ControlPlaneVIP, vipPort, backends)
	f5metrics.CMPAPICallDuration.WithLabelValues("gardener-extension-f5", "EnsureControlPlaneVirtualServer").Observe(time.Since(cmpStart).Seconds())
	if err != nil {
		f5metrics.CMPAPICallsTotal.WithLabelValues("gardener-extension-f5", "EnsureControlPlaneVirtualServer", "error").Inc()
		f5metrics.VIPAllocationsTotal.WithLabelValues("gardener-extension-f5", "error").Inc()
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "ProvisionFailed",
			Message:            err.Error(),
			LastTransitionTime: metav1.Now(),
		})
		return err
	}
	f5metrics.CMPAPICallsTotal.WithLabelValues("gardener-extension-f5", "EnsureControlPlaneVirtualServer", "success").Inc()
	f5metrics.VIPAllocationsTotal.WithLabelValues("gardener-extension-f5", "success").Inc()

	cfg.Status.VIP = cfg.Spec.ControlPlaneVIP
	if ids != nil {
		cfg.Status.LBServiceID = ids.LBServiceID
		cfg.Status.VIPPortID = ids.VIPPortID
		cfg.Status.VirtualServerID = ids.VirtualServerID
		if ids.VirtualServerName != "" {
			cfg.Status.VirtualServerName = ids.VirtualServerName
		}
	}
	if cfg.Status.VirtualServerName == "" {
		cfg.Status.VirtualServerName = "cp-apiserver-vs"
	}
	meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               "ControlPlaneLoadBalancerReady",
		Status:             metav1.ConditionTrue,
		Reason:             "Provisioned",
		Message:            "Control-plane VIP/VS provisioned via CMP/CCP API",
		LastTransitionTime: metav1.Now(),
	})

	return nil
}

// chooseFrontendPort picks the VIP frontend port from discovered backends: prefers 443, then 6443, then first backend port.
// Input: backends ([]Backend — discovered apiserver endpoints). Output: int32 — the port to expose on the VIP (default 443).
func chooseFrontendPort(backends []f5client.Backend) int32 {
	// Prefer the typical kube-apiserver ports.
	for _, b := range backends {
		if b.Port == 443 {
			return 443
		}
		if b.Port == 6443 {
			return 6443
		}
	}
	// Fallback: use the first discovered backend port.
	if len(backends) > 0 {
		return backends[0].Port
	}
	return 443
}

// Delete is called when the Extension resource is deleted (Shoot deletion). Cleans up CMP resources and CIS from Shoot.
// Input: ctx, log, ex (Extension being deleted). Output: error — nil on success (best-effort cleanup, does not block on CMP failure).
func (a *actuator) Delete(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	log = log.WithValues("extension", ex.Name, "namespace", ex.Namespace)
	log.Info("Deleting F5 extension")

	cfg := &f5v1alpha1.F5LoadBalancerConfig{}
	key := types.NamespacedName{Namespace: ex.Namespace, Name: ex.Name}
	if err := a.client.Get(ctx, key, cfg); err != nil {
		log.Info("F5LoadBalancerConfig not found; nothing to delete", "key", key.String())
		return nil
	}

	// Best-effort control-plane cleanup via CMP/CCP (do not block deletion).
	if err := a.cleanupControlPlaneViaCMP(ctx, log, cfg); err != nil {
		log.Error(err, "CMP/CCP control-plane cleanup failed (best-effort)")
	}

	// Best-effort cleanup on delete (regardless of enableApplicationLB).
	return a.cleanupCISInShoot(ctx, log, ex)
}

// ForceDelete is called when the Extension is force-deleted. Delegates to Delete for now.
// Input: ctx, log, ex (Extension). Output: error — same as Delete.
func (a *actuator) ForceDelete(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	log = log.WithValues("extension", ex.Name, "namespace", ex.Namespace)
	log.Info("Force-deleting F5 extension (Story 2 stub)")
	// For now, behave the same as Delete.
	return a.Delete(ctx, log, ex)
}

// cleanupControlPlaneViaCMP deletes CMP LBaaS resources (VirtualServer → VIP → LBService) in reverse order on Shoot deletion.
// Input: ctx, log, cfg (F5LoadBalancerConfig with CMP resource IDs in status). Output: error — nil on success or if no CMP endpoint configured.
func (a *actuator) cleanupControlPlaneViaCMP(ctx context.Context, log logr.Logger, cfg *f5v1alpha1.F5LoadBalancerConfig) error {
	if !cfg.Spec.EnablePerShootControlPlaneVIP {
		// Per-shoot VIP was never provisioned; nothing to clean up on F5/CMP.
		return nil
	}
	if cfg.Spec.CcpApiEndpoint == "" {
		return nil
	}
	if cfg.Spec.TenantOrPartition == "" {
		return nil
	}
	if cfg.Spec.CredentialsSecretRef == nil {
		return nil
	}
	seedSecretRef := *cfg.Spec.CredentialsSecretRef
	if seedSecretRef.Name == "" || seedSecretRef.Namespace == "" {
		return nil
	}

	seedSecret := &corev1.Secret{}
	if err := a.client.Get(ctx, types.NamespacedName{Namespace: seedSecretRef.Namespace, Name: seedSecretRef.Name}, seedSecret); err != nil {
		return fmt.Errorf("getting credentials secret %s/%s: %w", seedSecretRef.Namespace, seedSecretRef.Name, err)
	}

	username, userErr := readSecretKey(seedSecret, "username", "F5_USERNAME")
	password, passErr := readSecretKey(seedSecret, "password", "F5_PASSWORD")

	ceAuth, ceErr := readSecretKey(seedSecret, "Ce-Auth", "ce-auth", "ceAuth", "ce_auth")
	projectID, projErr := readSecretKey(seedSecret, "project-id", "projectId", "project_id")

	// Refresh Ce-Auth token from API key if available (same as provisioning path).
	ceAuth, ceErr = a.refreshCeAuthIfNeeded(ctx, log, seedSecret, ceAuth, ceErr)

	orgName := cfg.Spec.TenantOrPartition
	if orgFromSecret, orgErr := readSecretKey(seedSecret, "organisation-name", "organisationName", "organisation_name"); orgErr == nil && orgFromSecret != "" {
		orgName = orgFromSecret
	}

	var cmp f5client.Client
	var err error
	if ceErr == nil && projErr == nil {
		cmp, err = f5client.NewClientWithCeAuth(log, cfg.Spec.CcpApiEndpoint, orgName, projectID, ceAuth)
	} else if userErr == nil && passErr == nil {
		cmp, err = f5client.NewClientWithBasicAuth(log, cfg.Spec.CcpApiEndpoint, orgName, username, password)
	} else {
		// If credentials aren't present, skip cleanup (best-effort).
		return nil
	}
	if err != nil {
		return err
	}

	log.Info("Cleaning up control-plane VIP/VS via CMP/CCP API (best-effort)",
		"ccpApiEndpoint", cfg.Spec.CcpApiEndpoint,
		"organisationName", orgName,
		"virtualServerName", cfg.Status.VirtualServerName,
	)

	delStart := time.Now()
	delErr := cmp.DeleteControlPlaneVirtualServer(ctx, &f5client.CMPResourceIDs{
		LBServiceID:       cfg.Status.LBServiceID,
		VIPPortID:         cfg.Status.VIPPortID,
		VirtualServerID:   cfg.Status.VirtualServerID,
		VirtualServerName: cfg.Status.VirtualServerName,
	})
	f5metrics.CMPAPICallDuration.WithLabelValues("gardener-extension-f5", "DeleteControlPlaneVirtualServer").Observe(time.Since(delStart).Seconds())
	if delErr != nil {
		f5metrics.CMPAPICallsTotal.WithLabelValues("gardener-extension-f5", "DeleteControlPlaneVirtualServer", "error").Inc()
	} else {
		f5metrics.CMPAPICallsTotal.WithLabelValues("gardener-extension-f5", "DeleteControlPlaneVirtualServer", "success").Inc()
	}
	return delErr
}

// Restore is called on the destination Seed after a Shoot migration. It re-creates the F5LoadBalancerConfig
// from providerConfig, re-hydrates CMP resource IDs from the migrated providerStatus, marks the control-plane
// LB as Ready (the VIP/VS still exists on CMP), and re-deploys svc-lb-bridge if application LB is enabled.
// Input: ctx, log, ex (Extension). Output: error — nil on success.
func (a *actuator) Restore(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	log = log.WithValues("extension", ex.Name, "namespace", ex.Namespace)
	log.Info("Restoring F5 extension on new Seed")

	// Re-create F5LoadBalancerConfig from providerConfig (idempotent).
	cfg, err := a.ensureF5LoadBalancerConfig(ctx, log, ex)
	if err != nil {
		return err
	}

	// Re-hydrate CMP resource IDs from the migrated providerStatus so this Seed knows
	// which CMP resources already exist and can clean them up correctly on deletion.
	if ex.Status.ProviderStatus != nil && len(ex.Status.ProviderStatus.Raw) > 0 {
		var migrated extensionProviderStatus
		if jsonErr := json.Unmarshal(ex.Status.ProviderStatus.Raw, &migrated); jsonErr == nil {
			statusBase := cfg.DeepCopyObject().(*f5v1alpha1.F5LoadBalancerConfig)
			if migrated.ControlPlaneVIP != "" {
				cfg.Status.VIP = migrated.ControlPlaneVIP
			}
			if migrated.LBServiceID != "" {
				cfg.Status.LBServiceID = migrated.LBServiceID
			}
			if migrated.VIPPortID != "" {
				cfg.Status.VIPPortID = migrated.VIPPortID
			}
			if migrated.VirtualServerID != "" {
				cfg.Status.VirtualServerID = migrated.VirtualServerID
			}
			if migrated.VirtualServerName != "" {
				cfg.Status.VirtualServerName = migrated.VirtualServerName
			}
			if err := a.client.Status().Patch(ctx, cfg, client.MergeFrom(statusBase)); err != nil {
				return fmt.Errorf("restoring F5LoadBalancerConfig CMP IDs from migrated providerStatus: %w", err)
			}
			log.Info("Restored CMP resource IDs from migrated providerStatus",
				"vip", cfg.Status.VIP,
				"virtualServerId", cfg.Status.VirtualServerID,
				"lbServiceId", cfg.Status.LBServiceID,
			)
		} else {
			log.Error(jsonErr, "Could not parse migrated providerStatus; proceeding without CMP IDs (cleanup on deletion may be incomplete)")
		}
	}

	// Mark control-plane LB as Ready. The VIP/VS still exists on CMP — we are just taking over management.
	condBase := cfg.DeepCopyObject().(*f5v1alpha1.F5LoadBalancerConfig)
	if cfg.Status.VIP != "" || (cfg.Spec.ControlPlaneReady != nil && *cfg.Spec.ControlPlaneReady) {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionTrue,
			Reason:             "Restored",
			Message:            fmt.Sprintf("Restored from Shoot migration; VIP %s is active on CMP", cfg.Status.VIP),
			LastTransitionTime: metav1.Now(),
		})
	} else if !cfg.Spec.EnablePerShootControlPlaneVIP {
		// Mechanism A — shared Seed Ingress VIP handles control-plane access; no per-Shoot VIP needed.
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionTrue,
			Reason:             "SharedSeedIngressVIP",
			Message:            "Control-plane uses shared Seed Ingress VIP (Mechanism A); restored on new Seed",
			LastTransitionTime: metav1.Now(),
		})
	}
	if err := a.client.Status().Patch(ctx, cfg, client.MergeFrom(condBase)); err != nil {
		return fmt.Errorf("updating F5LoadBalancerConfig conditions after restore: %w", err)
	}

	// Re-deploy svc-lb-bridge into the Shoot if application LB is enabled.
	if cfg.Spec.EnableApplicationLB {
		if err := a.reconcileCISInShoot(ctx, log, ex, cfg); err != nil {
			return fmt.Errorf("restoring svc-lb-bridge in Shoot after migration: %w", err)
		}
	}

	_ = a.updateExtensionOutput(ctx, log, ex, cfg, gardencorev1beta1.LastOperationStateSucceeded, "restored on new Seed")
	log.Info("Restore complete")
	return nil
}

// Migrate is called on the source Seed before a Shoot is migrated to another Seed.
// It persists CMP resource IDs into Extension.status.providerStatus so they survive the migration transfer
// and can be re-hydrated by Restore() on the destination Seed.
// It does NOT delete CMP resources — the VIP/VS must stay active during the migration window.
// Input: ctx, log, ex (Extension). Output: error — nil on success.
func (a *actuator) Migrate(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	log = log.WithValues("extension", ex.Name, "namespace", ex.Namespace)
	log.Info("Migrating F5 extension — persisting CMP resource IDs into providerStatus")

	cfg := &f5v1alpha1.F5LoadBalancerConfig{}
	key := types.NamespacedName{Namespace: ex.Namespace, Name: ex.Name}
	if err := a.client.Get(ctx, key, cfg); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("F5LoadBalancerConfig not found; nothing to migrate")
			return nil
		}
		return fmt.Errorf("getting F5LoadBalancerConfig for migration: %w", err)
	}

	// Persist CMP resource IDs into Extension.status.providerStatus so they are transferred
	// to the destination Seed via Gardener's migration backup mechanism.
	port := int32(443)
	if backends, err := a.discoverKubeAPIServerBackends(ctx, log, ex); err == nil {
		port = chooseFrontendPort(backends)
	}
	ps := extensionProviderStatus{
		ControlPlaneVIP:   cfg.Status.VIP,
		ControlPlanePort:  port,
		LBServiceID:       cfg.Status.LBServiceID,
		VIPPortID:         cfg.Status.VIPPortID,
		VirtualServerID:   cfg.Status.VirtualServerID,
		VirtualServerName: cfg.Status.VirtualServerName,
	}
	raw, err := json.Marshal(ps)
	if err != nil {
		return fmt.Errorf("marshalling migrated providerStatus: %w", err)
	}

	patch := client.MergeFrom(ex.DeepCopy())
	ex.Status.ProviderStatus = &runtime.RawExtension{Raw: raw}
	ex.Status.LastOperation = &gardencorev1beta1.LastOperation{
		Type:           gardencorev1beta1.LastOperationTypeMigrate,
		State:          gardencorev1beta1.LastOperationStateSucceeded,
		Progress:       100,
		Description:    "CMP resource IDs persisted for Seed migration",
		LastUpdateTime: metav1.Now(),
	}
	if err := a.client.Status().Patch(ctx, ex, patch); err != nil {
		return fmt.Errorf("patching Extension status for migration: %w", err)
	}

	// Best-effort: remove svc-lb-bridge from the Shoot. The destination Seed will re-deploy it during Restore.
	if removeErr := a.cleanupCISInShoot(ctx, log, ex); removeErr != nil {
		log.Error(removeErr, "Could not remove svc-lb-bridge from Shoot during migration (best-effort); continuing")
	}

	log.Info("Migration handoff complete — CMP resources remain active on BIG-IP",
		"vip", cfg.Status.VIP,
		"virtualServerId", cfg.Status.VirtualServerID,
		"lbServiceId", cfg.Status.LBServiceID,
	)
	return nil
}

// getShootClient creates a controller-runtime client that talks to the Shoot's kube-apiserver (via kubeconfig in Seed Secret).
// Input: ctx, shootNamespace (Seed namespace containing the Shoot's kubeconfig Secret). Output: client.Client for the Shoot, error on failure.
func (a *actuator) getShootClient(ctx context.Context, shootNamespace string) (client.Client, error) {
	shootScheme := runtime.NewScheme()

	if err := clientgoscheme.AddToScheme(shootScheme); err != nil {
		return nil, fmt.Errorf("adding core scheme: %w", err)
	}
	if err := appsv1.AddToScheme(shootScheme); err != nil {
		return nil, fmt.Errorf("adding apps scheme: %w", err)
	}
	if err := rbacv1.AddToScheme(shootScheme); err != nil {
		return nil, fmt.Errorf("adding rbac scheme: %w", err)
	}

	_, shootClient, err := extensionsutil.NewClientForShoot(
		ctx,
		a.client,
		shootNamespace,
		client.Options{Scheme: shootScheme},
		extensionsconfigv1alpha1.RESTOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("creating shoot client: %w", err)
	}

	logger := logf.FromContext(ctx).WithName("shoot-client")

	var nsList corev1.NamespaceList
	if err := shootClient.List(ctx, &nsList); err != nil {
		return nil, fmt.Errorf("listing Shoot namespaces: %w", err)
	}

	logger.Info(
		"Connected to Shoot",
		"shootNamespace", shootNamespace,
		"namespaceCount", len(nsList.Items),
	)

	var kubeSystem corev1.Namespace
	if err := shootClient.Get(
		ctx,
		client.ObjectKey{Name: "kube-system"},
		&kubeSystem,
	); err != nil {
		return nil, fmt.Errorf("reading Shoot kube-system namespace: %w", err)
	}

	logger.Info(
		"Shoot cluster identity",
		"kubeSystemUID", string(kubeSystem.UID),
	)

	return shootClient, nil
}

// reconcileCISInShoot deploys the Shoot-side Service→CMP LBaaS bridge into the Shoot cluster for application-plane
// load balancing.
//
// NOTE: This is CMP-only mode (no AS3). We do not deploy CIS into the Shoot. The bridge watches Services of
// type LoadBalancer, provisions CMP LBaaS resources (LBService→VIP→VirtualServer with node backends), and mirrors
// the VIP into Service.status.
//
// Input: ctx, log, ex (Extension), cfg (F5LoadBalancerConfig with bridge image and CMP credentials ref).
// Output: error — permanentError if config/credentials are missing, transient error if Shoot is unreachable.
// Creates: namespace f5-cis-system, RBAC, svc-lb-bridge Deployment.
func (a *actuator) reconcileCISInShoot(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension, cfg *f5v1alpha1.F5LoadBalancerConfig) error {
	if cfg.Spec.CIS == nil {
		return permanent(fmt.Errorf("spec.cis must be set when enableApplicationLB=true"))
	}
	if strings.TrimSpace(cfg.Spec.CIS.BridgeImage) == "" {
		return permanent(fmt.Errorf("spec.cis.bridgeImage must be set when enableApplicationLB=true (CMP-only mode)"))
	}
	if cfg.Spec.CcpApiEndpoint == "" {
		return permanent(fmt.Errorf("spec.ccpApiEndpoint must not be empty when enableApplicationLB=true"))
	}
	if cfg.Spec.TenantOrPartition == "" {
		return permanent(fmt.Errorf("spec.tenantOrPartition must not be empty when enableApplicationLB=true"))
	}
	if cfg.Spec.CredentialsSecretRef == nil || cfg.Spec.CredentialsSecretRef.Name == "" || cfg.Spec.CredentialsSecretRef.Namespace == "" {
		return permanent(fmt.Errorf("spec.credentialsSecretRef.name and spec.credentialsSecretRef.namespace must be set when enableApplicationLB=true"))
	}
	if strings.TrimSpace(cfg.Spec.VPCID) == "" {
		return permanent(fmt.Errorf("spec.vpcId must not be empty when enableApplicationLB=true"))
	}

	if strings.TrimSpace(cfg.Spec.VPCName) == "" {
		return permanent(fmt.Errorf("spec.vpcName must not be empty when enableApplicationLB=true"))
	}

	if strings.TrimSpace(cfg.Spec.NetworkID) == "" {
		return permanent(fmt.Errorf("spec.networkId must not be empty when enableApplicationLB=true"))
	}

	if cfg.Spec.FlavorID <= 0 {
		return permanent(fmt.Errorf("spec.flavorId must be greater than zero when enableApplicationLB=true"))
	}

	shootClient, err := a.getShootClient(ctx, ex.Namespace)
	if err != nil {
		return err
	}

	// Read credentials from the referenced seed secret (we will copy them into the shoot).
	seedSecretRef := *cfg.Spec.CredentialsSecretRef
	seedSecret := &corev1.Secret{}
	if err := a.client.Get(ctx, types.NamespacedName{Namespace: seedSecretRef.Namespace, Name: seedSecretRef.Name}, seedSecret); err != nil {
		return permanent(fmt.Errorf("getting credentials secret %s/%s: %w", seedSecretRef.Namespace, seedSecretRef.Name, err))
	}

	ceAuth, err := readSecretKey(seedSecret, "Ce-Auth", "ce-auth", "ceAuth", "ce_auth")
	if err != nil {
		// Not an immediate failure — refreshCeAuthIfNeeded may generate it.
	}
	// Automatic Ce-Auth token refresh for application-plane credentials.
	ceAuth, err = a.refreshCeAuthIfNeeded(ctx, log, seedSecret, ceAuth, err)
	if err != nil {
		return permanent(err)
	}
	projectID, err := readSecretKey(seedSecret, "project-id", "projectId", "project_id")
	if err != nil {
		return permanent(err)
	}
	orgName := cfg.Spec.TenantOrPartition
	if orgFromSecret, orgErr := readSecretKey(seedSecret, "organisation-name", "organisationName", "organisation_name"); orgErr == nil && orgFromSecret != "" {
		orgName = orgFromSecret
	}

	// 1) Ensure namespace.
	shootNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cisNamespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, shootClient, shootNS, func() error { return nil }); err != nil {
		return fmt.Errorf("ensuring shoot namespace %s: %w", cisNamespace, err)
	}

	// 2) Ensure service account.
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: cisName, Namespace: cisNamespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, shootClient, sa, func() error { return nil }); err != nil {
		return fmt.Errorf("ensuring serviceaccount %s/%s: %w", cisNamespace, cisName, err)
	}

	// 3) Ensure RBAC.
	cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: cisName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, shootClient, cr, func() error {
		cr.Rules = []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"get", "list", "watch", "patch", "update"}},
			{APIGroups: []string{""}, Resources: []string{"nodes", "endpoints"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"services/status"}, Verbs: []string{"update", "patch"}},
			{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
			{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses"}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses/status"}, Verbs: []string{"update", "patch"}},
			{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"networkpolicies"}, Verbs: []string{"get", "list", "create", "update", "patch", "delete"}},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensuring clusterrole %s: %w", cisName, err)
	}

	crb := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: cisName}}
	if _, err := controllerutil.CreateOrUpdate(ctx, shootClient, crb, func() error {
		crb.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: cisName}
		crb.Subjects = []rbacv1.Subject{{Kind: "ServiceAccount", Name: cisName, Namespace: cisNamespace}}
		return nil
	}); err != nil {
		return fmt.Errorf("ensuring clusterrolebinding %s: %w", cisName, err)
	}

	// 4) Ensure svc-lb-bridge deployment.
	bridge := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: bridgeName, Namespace: cisNamespace}}
	bridgeLabels := map[string]string{"app": bridgeName}
	if _, err := controllerutil.CreateOrUpdate(ctx, shootClient, bridge, func() error {
		bridge.Labels = bridgeLabels
		bridge.Spec.Selector = &metav1.LabelSelector{MatchLabels: bridgeLabels}
		bridge.Spec.Replicas = ptrInt32(1)
		bridge.Spec.Template.ObjectMeta.Labels = bridgeLabels
		bridge.Spec.Template.Spec.ServiceAccountName = cisName
		bridge.Spec.Template.Spec.Containers = []corev1.Container{
			{
				Name:            "bridge",
				Image:           cfg.Spec.CIS.BridgeImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         []string{"/svc-lb-bridge"},
				Env: []corev1.EnvVar{
					{Name: "CMP_ENDPOINT", Value: strings.TrimRight(cfg.Spec.CcpApiEndpoint, "/")},
					{Name: "CMP_CE_AUTH", Value: ceAuth},
					{Name: "CMP_ORGANISATION_NAME", Value: orgName},
					{Name: "CMP_PROJECT_ID", Value: projectID},
					{Name: "CMP_VPC_ID", Value: strings.TrimSpace(cfg.Spec.VPCID)},
					{Name: "CMP_VPC_NAME", Value: strings.TrimSpace(cfg.Spec.VPCName)},
					{Name: "CMP_NETWORK_ID", Value: strings.TrimSpace(cfg.Spec.NetworkID)},
					{Name: "CMP_LB_FLAVOR_ID", Value: strconv.FormatInt(int64(cfg.Spec.FlavorID), 10)},
					{Name: "F5_SVC_LB_LOADBALANCER_CLASS", Value: "f5.extensions.gardener.cloud/bigip"},
				},
			},
		}
		return nil
	}); err != nil {
		return fmt.Errorf("ensuring svc-lb-bridge deployment %s/%s: %w", cisNamespace, bridgeName, err)
	}

	// Non-blocking readiness check: if the bridge is not yet available, return a
	// transient error so the reconciler requeues rather than blocking this worker thread.
	dep := &appsv1.Deployment{}
	if err := shootClient.Get(ctx, types.NamespacedName{Namespace: cisNamespace, Name: bridgeName}, dep); err != nil {
		return fmt.Errorf("getting svc-lb-bridge deployment %s/%s: %w", cisNamespace, bridgeName, err)
	}
	if dep.Status.AvailableReplicas < 1 {
		log.Info("svc-lb-bridge not yet ready; will recheck on next reconcile", "namespace", cisNamespace, "deployment", bridgeName)
		return fmt.Errorf("svc-lb-bridge deployment %s/%s not yet ready (0 available replicas)", cisNamespace, bridgeName)
	}

	log.Info("Reconciled svc-lb-bridge in Shoot", "namespace", cisNamespace, "deployment", bridgeName, "cmpEndpoint", cfg.Spec.CcpApiEndpoint, "organisationName", orgName)
	return nil
}

// readSecretKey reads the first matching key from a Secret's data (tries Data then StringData for each key).
// Input: secret (Secret), keys (variadic key names to try in order). Output: string value, error if none found.
func readSecretKey(secret *corev1.Secret, keys ...string) (string, error) {
	for _, k := range keys {
		if b, ok := secret.Data[k]; ok && len(b) > 0 {
			return string(b), nil
		}
		if s, ok := secret.StringData[k]; ok && s != "" {
			return s, nil
		}
	}
	return "", fmt.Errorf("secret %s/%s is missing required key(s): %v", secret.Namespace, secret.Name, keys)
}

// defaultCeAuthTokenValidity is how long generated Ce-Auth tokens are valid.
// We use 5 minutes (matching the Python script default of 299s) and regenerate
// on every reconcile cycle, so the token is always fresh.
const defaultCeAuthTokenValidity = 299 * time.Second

// refreshCeAuthIfNeeded checks whether the credentials Secret contains long-lived
// API key credentials (api-key-id + api-secret). If so, it generates a fresh
// short-lived Ce-Auth token, updates the Secret, and returns the new token.
//
// This provides automatic credential rotation: operators store the long-lived API
// keys once, and the controller keeps the Ce-Auth token fresh on every reconcile.
//
// If the Secret already has a valid Ce-Auth token and no API key credentials,
// it returns the existing token as-is (manual token mode — no auto-refresh).
func (a *actuator) refreshCeAuthIfNeeded(ctx context.Context, log logr.Logger, secret *corev1.Secret, existingCeAuth string, existingErr error) (string, error) {
	apiKeyID, keyIDErr := readSecretKey(secret, "api-key-id", "api_key_id", "apiKeyId", "CMP_API_KEY_ID")
	apiSecret, secretErr := readSecretKey(secret, "api-secret", "api_secret", "apiSecret", "CMP_API_SECRET")

	if keyIDErr != nil || secretErr != nil {
		// No API key credentials in Secret — fall back to existing Ce-Auth token.
		return existingCeAuth, existingErr
	}

	// Generate a fresh Ce-Auth token from the long-lived API key.
	freshToken := f5client.GenerateCeAuthToken(apiKeyID, apiSecret, defaultCeAuthTokenValidity)
	log.Info("Generated fresh Ce-Auth token from API key credentials",
		"secret", fmt.Sprintf("%s/%s", secret.Namespace, secret.Name),
		"apiKeyID", apiKeyID,
		"validity", defaultCeAuthTokenValidity.String(),
	)

	// Update the Secret with the fresh token so downstream consumers
	// (svc-lb-bridge Deployment env vars) automatically get the new value.
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	secret.Data["Ce-Auth"] = []byte(freshToken)
	if err := a.client.Update(ctx, secret); err != nil {
		log.Error(err, "failed to update Secret with refreshed Ce-Auth token; using token in-memory only")
		// Non-fatal: we still have the fresh token for this reconcile cycle.
	}

	return freshToken, nil
}

// buildCISArgs constructs the command-line arguments for the F5 CIS container (bigip-url, partition, pool-member-type=nodeport).
// Input: bigipURL (BIG-IP management URL), partition (BIG-IP partition name), extra (additional CLI args). Output: []string — the full args list.
func buildCISArgs(bigipURL, partition string, extra []string) []string {
	// Default CIS agent selection:
	// - We explicitly choose "cccl" (iControl REST/imperative) to avoid relying on AS3 being installed on BIG-IP.
	// - If the user provides --agent=... in spec.cis.extraArgs, we respect that and do not override.
	extra = ensureArgWithPrefix(extra, "--agent", "--agent=cccl")

	args := []string{
		"--bigip-username=$(BIGIP_USERNAME)",
		"--bigip-password=$(BIGIP_PASSWORD)",
		"--bigip-url=" + bigipURL,
		"--bigip-partition=" + partition,
		"--namespace=all",
		"--pool-member-type=nodeport",
	}
	return append(args, extra...)
}

// cisExtraArgs appends bridge-specific CIS args (--manage-ingress=true, --ingress-class=f5) when svc-lb-bridge is enabled.
// Input: extra (existing extra args), bridgeEnabled (true if bridgeImage is set). Output: []string — modified args with bridge flags if needed.
func cisExtraArgs(extra []string, bridgeEnabled bool) []string {
	if !bridgeEnabled {
		return extra
	}

	// The service-LB bridge produces Ingress resources and expects CIS to act on them.
	extra = ensureArgWithPrefix(extra, "--manage-ingress", "--manage-ingress=true")
	extra = ensureArgWithPrefix(extra, "--ingress-class", "--ingress-class=f5")
	return extra
}

// ensureArgWithPrefix adds a CLI argument to the list only if no argument with the given prefix already exists (prevents duplicates).
// Input: args (existing args), prefix (e.g., "--manage-ingress"), value (full arg e.g., "--manage-ingress=true"). Output: []string — args with value appended if not present.
func ensureArgWithPrefix(args []string, prefix, value string) []string {
	for _, a := range args {
		if a == value || strings.HasPrefix(a, prefix+"=") || a == prefix {
			return args
		}
	}
	return append(args, value)
}

func ptrInt32(v int32) *int32 { return &v }

// cleanupCISInShoot deletes the svc-lb-bridge deployment and supporting RBAC/SA from the Shoot on Extension deletion.
// Input: ctx, log, ex (Extension being deleted). Output: nil (best-effort; logs errors but does not block deletion).
func (a *actuator) cleanupCISInShoot(ctx context.Context, log logr.Logger, ex *extensionsv1alpha1.Extension) error {
	shootClient, err := a.getShootClient(ctx, ex.Namespace)
	if err != nil {
		log.Error(err, "Skipping svc-lb-bridge cleanup in Shoot (best-effort)")
		return nil
	}

	_ = shootClient.Delete(ctx, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: bridgeName, Namespace: cisNamespace}})
	_ = shootClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: cisSecretName, Namespace: cisNamespace}})
	_ = shootClient.Delete(ctx, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: cisName, Namespace: cisNamespace}})
	_ = shootClient.Delete(ctx, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: cisName}})
	_ = shootClient.Delete(ctx, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: cisName}})

	log.Info("Cleaned up svc-lb-bridge in Shoot (best-effort)", "namespace", cisNamespace)
	return nil
}

// reconcileControlPlaneStatus evaluates the F5LoadBalancerConfig spec and sets the ControlPlaneLoadBalancerReady condition.
// Input: log, cfg (F5LoadBalancerConfig — reads spec.controlPlaneVIP and spec.controlPlaneReady). Output: none (modifies cfg.Status.Conditions in-place).
// reconcileControlPlaneStatusSharedSeedIngress sets ControlPlaneLoadBalancerReady=True without allocating any
// per-Shoot VIP. This is the default (Mechanism A) path: control-plane access uses the shared Seed Ingress VIP
// managed by Gardener/cloud-provider, not by this extension. Setting the condition to True here allows the
// app-plane gate (enableApplicationLB) to proceed when desired.
func (a *actuator) reconcileControlPlaneStatusSharedSeedIngress(log logr.Logger, cfg *f5v1alpha1.F5LoadBalancerConfig) {
	meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               "ControlPlaneLoadBalancerReady",
		Status:             metav1.ConditionTrue,
		Reason:             "SharedSeedIngressVIP",
		Message:            "Per-shoot control-plane VIP disabled (enablePerShootControlPlaneVIP=false); shared Seed Ingress VIP is used",
		LastTransitionTime: metav1.Now(),
	})
	log.Info("Control-plane LB: using shared Seed Ingress VIP (enablePerShootControlPlaneVIP=false)")
}

func (a *actuator) reconcileControlPlaneStatus(log logr.Logger, cfg *f5v1alpha1.F5LoadBalancerConfig) {
	if cfg.Spec.ControlPlaneVIP == "" {
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionFalse,
			Reason:             "NotConfigured",
			Message:            "spec.controlPlaneVIP is empty",
			LastTransitionTime: metav1.Now(),
		})
		return
	}

	// If the user explicitly sets controlPlaneReady, we treat it as the source of truth.
	// This supports the flow where CMP provisions/configures VIP/VS out-of-band.
	if cfg.Spec.ControlPlaneReady != nil {
		if !*cfg.Spec.ControlPlaneReady {
			meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
				Type:               "ControlPlaneLoadBalancerReady",
				Status:             metav1.ConditionFalse,
				Reason:             "NotReady",
				Message:            "spec.controlPlaneReady=false (waiting for CMP/out-of-band control-plane VIP/VS to be ready)",
				LastTransitionTime: metav1.Now(),
			})
			return
		}

		cfg.Status.VIP = cfg.Spec.ControlPlaneVIP
		if cfg.Status.VirtualServerName == "" {
			cfg.Status.VirtualServerName = "cp-apiserver-vs"
		}
		meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
			Type:               "ControlPlaneLoadBalancerReady",
			Status:             metav1.ConditionTrue,
			Reason:             "ExternalProvisioned",
			Message:            "Control-plane VIP/VS marked ready via spec.controlPlaneReady=true (CMP/out-of-band)",
			LastTransitionTime: metav1.Now(),
		})
		return
	}

	// Dev stub: if VIP is present we treat control-plane LB as ready.
	cfg.Status.VIP = cfg.Spec.ControlPlaneVIP
	if cfg.Status.VirtualServerName == "" {
		cfg.Status.VirtualServerName = "cp-apiserver-vs"
	}
	meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               "ControlPlaneLoadBalancerReady",
		Status:             metav1.ConditionTrue,
		Reason:             "Configured",
		Message:            "Control-plane VIP configured (dev stub; CMP automation not wired yet)",
		LastTransitionTime: metav1.Now(),
	})

	log.Info("Control-plane LB marked ready (dev stub)", "vip", cfg.Spec.ControlPlaneVIP)
}

// isConditionTrue checks if a named condition exists in the list and has Status=True.
// Input: conditions ([]Condition), condType (e.g., "ControlPlaneLoadBalancerReady"). Output: bool — true if condition is True.
func isConditionTrue(conditions []metav1.Condition, condType string) bool {
	cond := meta.FindStatusCondition(conditions, condType)
	return cond != nil && cond.Status == metav1.ConditionTrue
}

// discoverKubeAPIServerBackends reads the kube-apiserver Service Endpoints in the Shoot's Seed namespace to find apiserver pod IPs and ports.
// Input: ctx, log, ex (Extension — its namespace is the Shoot technical namespace on Seed). Output: []Backend (IP:Port pairs), error if no endpoints found.
// These backends become pool members in the CMP VirtualServer for control-plane LB.
func (a *actuator) discoverKubeAPIServerBackends(
	ctx context.Context,
	log logr.Logger,
	ex *extensionsv1alpha1.Extension,
) ([]f5client.Backend, error) {
	svcNamespace := ex.Namespace
	svcName := "kube-apiserver"

	log = log.WithValues("apiserverService", svcName, "apiserverNamespace", svcNamespace)

	svc := &corev1.Service{}
	if err := a.client.Get(ctx, types.NamespacedName{Namespace: svcNamespace, Name: svcName}, svc); err != nil {
		return nil, fmt.Errorf("getting kube-apiserver Service %s/%s: %w", svcNamespace, svcName, err)
	}

	sliceList := &discoveryv1.EndpointSliceList{}
	if err := a.client.List(ctx, sliceList, client.InNamespace(svcNamespace), client.MatchingLabels{"kubernetes.io/service-name": svcName}); err != nil {
		return nil, fmt.Errorf("listing kube-apiserver EndpointSlices %s/%s: %w", svcNamespace, svcName, err)
	}

	var backends []f5client.Backend
	for _, slice := range sliceList.Items {
		for _, endpoint := range slice.Endpoints {
			if endpoint.Addresses == nil || len(endpoint.Addresses) == 0 {
				continue
			}
			for _, port := range slice.Ports {
				for _, addr := range endpoint.Addresses {
					backends = append(backends, f5client.Backend{
						IP:   addr,
						Port: *port.Port,
					})
				}
			}
		}
	}

	if len(backends) == 0 {
		return nil, fmt.Errorf("no kube-apiserver endpoints found for %s/%s", svcNamespace, svcName)
	}

	return backends, nil
}

// updateF5Status writes the VIP address and VirtualServer name into the F5LoadBalancerConfig status sub-resource.
// Input: ctx, log, ex (Extension — used to locate the F5LoadBalancerConfig by same name/namespace), vip (allocated VIP), vsName (VS identifier).
// Output: error — nil on success, error if GET or status update fails.
func (a *actuator) updateF5Status(
	ctx context.Context,
	log logr.Logger,
	ex *extensionsv1alpha1.Extension,
	vip, vsName string,
) error {
	cfg := &f5v1alpha1.F5LoadBalancerConfig{}
	key := types.NamespacedName{
		Namespace: ex.Namespace,
		Name:      ex.Name,
	}

	if err := a.client.Get(ctx, key, cfg); err != nil {
		return fmt.Errorf("getting F5LoadBalancerConfig %s: %w", key.String(), err)
	}

	cfg.Status.VIP = vip
	cfg.Status.VirtualServerName = vsName

	if err := a.client.Status().Update(ctx, cfg); err != nil {
		return fmt.Errorf("updating F5LoadBalancerConfig status %s: %w", key.String(), err)
	}

	log.Info("Updated F5LoadBalancerConfig status",
		"f5Config", key.String(),
		"vip", vip,
		"virtualServerName", vsName,
	)

	return nil
}
