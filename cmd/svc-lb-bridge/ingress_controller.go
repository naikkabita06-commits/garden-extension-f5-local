// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	lbbackend "github.com/gardener/gardener-extension-f5/pkg/backend"
	lbaasdeploy "github.com/gardener/gardener-extension-f5/pkg/deploy/lbaas"
	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	lbingress "github.com/gardener/gardener-extension-f5/pkg/ingress"
	f5metrics "github.com/gardener/gardener-extension-f5/pkg/metrics"
	"github.com/gardener/gardener-extension-f5/pkg/model"
	lbstatus "github.com/gardener/gardener-extension-f5/pkg/status"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	ingressClassName      = "f5"
	ingressFinalizerName  = "f5.extensions.gardener.cloud/ingress-lb"
	annIngressLBServiceID = "f5.extensions.gardener.cloud/ingress-lb-service-id"
	annIngressVIPPortID   = "f5.extensions.gardener.cloud/ingress-vip-port-id"
	annIngressVSID        = "f5.extensions.gardener.cloud/ingress-virtual-server-id"
	annIngressVIPAddress  = "f5.extensions.gardener.cloud/ingress-vip-address"
)

// ingressReconciler handles Ingress resources with IngressClass "f5" and
// provisions HTTP/HTTPS Virtual Servers via CMP LBaaS.
type ingressReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	cmp   cmpLBaaS
	vpcID string
}

func newIngressReconciler(c client.Client, scheme *runtime.Scheme, cmp cmpLBaaS, vpcID string) *ingressReconciler {
	return &ingressReconciler{
		Client: c,
		Scheme: scheme,
		cmp:    cmp,
		vpcID:  vpcID,
	}
}

// parseIngressConfig reads per-Ingress LB configuration from annotations.
func parseIngressConfig(ing *networkingv1.Ingress) lbServiceConfig {
	return lbannotations.ParseObject(ing)
}

func (r *ingressReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("ingress-lb").WithValues("ingress", req.NamespacedName.String())

	ing := &networkingv1.Ingress{}
	if err := r.Get(ctx, req.NamespacedName, ing); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Only handle Ingress resources with our IngressClass.
	if !r.isOwnedIngress(ing) {
		return ctrl.Result{}, nil
	}

	// Handle deletion.
	if !ing.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(ing, ingressFinalizerName) {
			log.Info("cleaning up CMP LBaaS resources for Ingress")
			r.Recorder.Eventf(ing, corev1.EventTypeNormal, "DeletingLoadBalancer", "Deleting CMP LBaaS resources for Ingress (LB=%s)", ing.Annotations[annIngressLBServiceID])
			if err := r.cleanupCMPResources(ctx, ing); err != nil {
				r.Recorder.Eventf(ing, corev1.EventTypeWarning, "DeleteFailed", "CMP LBaaS cleanup failed: %v", err)
				return ctrl.Result{}, fmt.Errorf("CMP cleanup failed; retrying: %w", err)
			}
			r.Recorder.Event(ing, corev1.EventTypeNormal, "DeletedLoadBalancer", "CMP LBaaS resources deleted successfully")

			base := ing.DeepCopy()
			controllerutil.RemoveFinalizer(ing, ingressFinalizerName)
			if err := r.Patch(ctx, ing, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			f5metrics.ManagedServicesTotal.WithLabelValues("ingress-lb").Dec()
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer.
	if !controllerutil.ContainsFinalizer(ing, ingressFinalizerName) {
		base := ing.DeepCopy()
		controllerutil.AddFinalizer(ing, ingressFinalizerName)
		if err := r.Patch(ctx, ing, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		f5metrics.ManagedServicesTotal.WithLabelValues("ingress-lb").Inc()
	}

	if err := lbingress.ValidateSupported(ing); err != nil {
		log.Info("skipping unsupported Ingress", "reason", err)
		r.Recorder.Eventf(ing, corev1.EventTypeWarning, "UnsupportedIngress", "Unsupported F5 Ingress configuration: %v", err)
		return ctrl.Result{}, nil
	}

	// Determine protocol: HTTPS if TLS is configured, HTTP otherwise.
	protocol := lbingress.ProtocolForIngress(ing)

	// Resolve the backend NodePort from the first rule's service.
	backendSvc, nodePort, err := r.resolveBackendServiceAndNodePort(ctx, ing)
	if err != nil {
		log.Error(err, "cannot resolve backend NodePort")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	if nodePort == 0 {
		log.Info("skipping: no backend service with NodePort found")
		return ctrl.Result{}, nil
	}

	// Collect backend nodes (only ready nodes with ready endpoints, weighted by pod count).
	var backends []backendNode
	if backendSvc != nil {
		backends, err = lbbackend.ListReadyNodeBackends(ctx, r.Client, backendSvc)
	} else {
		backends, err = lbbackend.ListReadyNodeBackends(ctx, r.Client, nil)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(backends) == 0 {
		log.Info("skipping: no ready nodes with endpoints found")
		return ctrl.Result{}, nil
	}

	// Determine frontend port.
	frontendPort := lbingress.FrontendPortForProtocol(protocol)

	// Parse per-Ingress LB configuration from annotations and build desired state.
	ingressCfg := parseIngressConfig(ing)
	stack, err := lbingress.BuildLoadBalancerStack(ing, ingressCfg, backends, lbingress.BuildOptions{
		FrontendPort: frontendPort,
		BackendPort:  nodePort,
		Protocol:     protocol,
	})
	if err != nil {
		r.Recorder.Eventf(ing, corev1.EventTypeWarning, "BuildLoadBalancerModelFailed", "Error building Ingress load-balancer model: %v", err)
		return ctrl.Result{}, err
	}

	// Provision CMP resources.
	cmpStart := time.Now()
	ids, vip, err := r.ensureCMPResources(ctx, ing, stack)
	f5metrics.CMPAPICallDuration.WithLabelValues("ingress-lb", "EnsureLB").Observe(time.Since(cmpStart).Seconds())
	if err != nil {
		f5metrics.CMPAPICallsTotal.WithLabelValues("ingress-lb", "EnsureLB", "error").Inc()
		f5metrics.ReconcileErrorsTotal.WithLabelValues("ingress-lb").Inc()
		if rle, ok := f5client.IsRateLimited(err); ok {
			r.Recorder.Eventf(ing, corev1.EventTypeWarning, "RateLimited", "CMP API rate limited; retrying after %s", rle.RetryAfter)
			log.Info("CMP rate limited; requeuing", "retryAfter", rle.RetryAfter)
			return ctrl.Result{RequeueAfter: rle.RetryAfter}, nil
		}
		r.Recorder.Eventf(ing, corev1.EventTypeWarning, "SyncLoadBalancerFailed", "Error ensuring CMP LBaaS resources: %v", err)
		return ctrl.Result{}, err
	}
	f5metrics.CMPAPICallsTotal.WithLabelValues("ingress-lb", "EnsureLB", "success").Inc()

	// Store CMP resource IDs in annotations.
	base := ing.DeepCopy()
	if ing.Annotations == nil {
		ing.Annotations = map[string]string{}
	}
	ing.Annotations[annIngressLBServiceID] = ids.LBServiceID
	ing.Annotations[annIngressVIPPortID] = ids.VIPPortID
	ing.Annotations[annIngressVSID] = ids.VirtualServerID
	ing.Annotations[annIngressVIPAddress] = vip
	if err := r.Patch(ctx, ing, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	// Update Ingress status with VIP.
	if err := lbstatus.EnsureIngressVIP(ctx, r.Client, ing, vip); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Ingress reconciled", "vip", vip, "protocol", protocol)
	r.Recorder.Eventf(ing, corev1.EventTypeNormal, "EnsuredLoadBalancer", "Ingress reconciled via CMP LBaaS (VIP=%s, protocol=%s, backends=%d)", vip, protocol, len(backends))
	return ctrl.Result{}, nil
}

// isOwnedIngress checks if this Ingress should be handled by the F5 controller.
func (r *ingressReconciler) isOwnedIngress(ing *networkingv1.Ingress) bool {
	// Check spec.ingressClassName.
	if ing.Spec.IngressClassName != nil {
		return *ing.Spec.IngressClassName == ingressClassName
	}
	// Fallback: check deprecated annotation.
	if ann, ok := ing.Annotations["kubernetes.io/ingress.class"]; ok {
		return ann == ingressClassName
	}
	return false
}

// resolveBackendNodePort finds the NodePort for the primary backend service
// referenced by the Ingress rules.
func (r *ingressReconciler) resolveBackendServiceAndNodePort(ctx context.Context, ing *networkingv1.Ingress) (*corev1.Service, int32, error) {
	// Try rules first.
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP == nil {
			continue
		}
		for _, path := range rule.HTTP.Paths {
			if path.Backend.Service == nil {
				continue
			}
			svc, np, err := r.getServiceAndNodePort(ctx, ing.Namespace, path.Backend.Service.Name, path.Backend.Service.Port)
			if err != nil {
				return nil, 0, err
			}
			if np > 0 {
				return svc, np, nil
			}
		}
	}
	// Fallback to default backend.
	if ing.Spec.DefaultBackend != nil && ing.Spec.DefaultBackend.Service != nil {
		return r.getServiceAndNodePort(ctx, ing.Namespace, ing.Spec.DefaultBackend.Service.Name, ing.Spec.DefaultBackend.Service.Port)
	}
	return nil, 0, nil
}

func (r *ingressReconciler) getServiceAndNodePort(ctx context.Context, namespace, name string, port networkingv1.ServiceBackendPort) (*corev1.Service, int32, error) {
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	for _, p := range svc.Spec.Ports {
		if port.Name != "" && p.Name == port.Name {
			return svc, p.NodePort, nil
		}
		if port.Number != 0 && p.Port == port.Number {
			return svc, p.NodePort, nil
		}
	}
	// If only one port exists, use it.
	if len(svc.Spec.Ports) == 1 {
		return svc, svc.Spec.Ports[0].NodePort, nil
	}
	return svc, 0, nil
}

func (r *ingressReconciler) ensureCMPResources(ctx context.Context, ing *networkingv1.Ingress, stack *model.LoadBalancerStack) (*f5client.CMPResourceIDs, string, error) {
	current := model.ObservedState{}
	if ing != nil && ing.Annotations != nil {
		current.LBServiceID = strings.TrimSpace(ing.Annotations[annIngressLBServiceID])
		current.VIPPortID = strings.TrimSpace(ing.Annotations[annIngressVIPPortID])
		current.VirtualServerID = strings.TrimSpace(ing.Annotations[annIngressVSID])
		current.VIPAddress = strings.TrimSpace(ing.Annotations[annIngressVIPAddress])
	}
	if stack == nil || len(stack.VirtualServers) == 0 || len(stack.Ports) == 0 {
		return nil, current.VIPAddress, fmt.Errorf("ingress load-balancer stack is empty")
	}
	vs := stack.VirtualServers[0]
	result, err := lbaasdeploy.NewFromRaw(r.cmp, r.vpcID).Ensure(ctx, lbaasdeploy.EnsureRequest{
		LBName:        stack.LBService.Name,
		LBDescription: stack.LBService.Description,
		VirtualServer: vs,
		Backends:      stack.Ports[0].Backends,
		Current:       current,
	})
	if err != nil {
		return nil, current.VIPAddress, err
	}
	ids := &f5client.CMPResourceIDs{
		LBServiceID:       result.Observed.LBServiceID,
		VIPPortID:         result.Observed.VIPPortID,
		VirtualServerID:   result.Observed.VirtualServerID,
		VirtualServerName: result.Observed.VirtualServerName,
	}
	return ids, result.Observed.VIPAddress, nil
}

func (r *ingressReconciler) cleanupCMPResources(ctx context.Context, ing *networkingv1.Ingress) error {
	observed := model.ObservedState{
		LBServiceID:     strings.TrimSpace(ing.Annotations[annIngressLBServiceID]),
		VIPPortID:       strings.TrimSpace(ing.Annotations[annIngressVIPPortID]),
		VirtualServerID: strings.TrimSpace(ing.Annotations[annIngressVSID]),
	}
	shared := r.isLBServiceShared(ctx, ing, observed.LBServiceID)
	_, err := lbaasdeploy.NewFromRaw(r.cmp, r.vpcID).Cleanup(ctx, lbaasdeploy.CleanupRequest{
		Current:         observed,
		DeleteVIP:       !shared,
		DeleteLBService: !shared,
	})
	return err
}

// isLBServiceShared checks whether any other Ingress still references the
// same CMP LBService ID. Used for vip-group ref-counting.
func (r *ingressReconciler) isLBServiceShared(ctx context.Context, self *networkingv1.Ingress, lbID string) bool {
	if strings.TrimSpace(lbID) == "" {
		return false
	}
	ingList := &networkingv1.IngressList{}
	if err := r.List(ctx, ingList, client.InNamespace(self.Namespace)); err != nil {
		return false
	}
	for _, ing := range ingList.Items {
		if ing.Name == self.Name && ing.Namespace == self.Namespace {
			continue
		}
		if ing.Annotations[annIngressLBServiceID] == lbID && controllerutil.ContainsFinalizer(&ing, ingressFinalizerName) {
			return true
		}
	}
	return false
}

// listManagedIngressRequests returns reconcile requests for all Ingresses
// managed by this controller. Used when nodes change.
func listManagedIngressRequests(ctx context.Context, c client.Client) []reconcile.Request {
	ingList := &networkingv1.IngressList{}
	if err := c.List(ctx, ingList); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, ing := range ingList.Items {
		if ing.Annotations[annIngressLBServiceID] != "" {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: ing.Name, Namespace: ing.Namespace},
			})
		}
	}
	return reqs
}
