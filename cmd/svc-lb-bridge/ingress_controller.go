// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	f5metrics "github.com/gardener/gardener-extension-f5/pkg/metrics"

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
// Same annotation keys as per-Service config, applied to the Ingress object.
func parseIngressConfig(ing *networkingv1.Ingress) lbServiceConfig {
	cfg := defaultLBServiceConfig()
	if ing == nil || ing.Annotations == nil {
		return cfg
	}
	if v := strings.TrimSpace(ing.Annotations[annRoutingAlgorithm]); v != "" {
		cfg.RoutingAlgorithm = v
	}
	if v := strings.TrimSpace(ing.Annotations[annHealthInterval]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.HealthInterval = int32(n)
		}
	}
	if v := strings.TrimSpace(ing.Annotations[annHealthType]); v != "" {
		lower := strings.ToLower(v)
		switch lower {
		case "tcp", "http":
			cfg.HealthType = lower
		}
	}
	if v := strings.TrimSpace(ing.Annotations[annHealthPath]); v != "" {
		cfg.HealthPath = v
		if cfg.HealthType == "tcp" {
			cfg.HealthType = "http"
		}
	}
	if v := strings.TrimSpace(ing.Annotations[annDrainingTimeout]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.DrainingTimeout = int32(n)
		}
	}
	if v := strings.TrimSpace(ing.Annotations[annSourceRanges]); v != "" {
		for _, cidr := range strings.Split(v, ",") {
			if c := strings.TrimSpace(cidr); c != "" {
				cfg.SourceRanges = append(cfg.SourceRanges, c)
			}
		}
	}
	return cfg
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

	// Determine protocol: HTTPS if TLS is configured, HTTP otherwise.
	protocol := "HTTP"
	if len(ing.Spec.TLS) > 0 {
		protocol = "HTTPS"
	}

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
		backends, err = listBackendNodes(ctx, r.Client, backendSvc)
	} else {
		backends, err = listBackendNodes(ctx, r.Client, &corev1.Service{})
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(backends) == 0 {
		log.Info("skipping: no ready nodes with endpoints found")
		return ctrl.Result{}, nil
	}

	// Determine frontend port.
	frontendPort := int32(80)
	if protocol == "HTTPS" {
		frontendPort = 443
	}

	// Parse per-Ingress LB configuration from annotations.
	ingressCfg := parseIngressConfig(ing)

	// Provision CMP resources.
	cmpStart := time.Now()
	ids, vip, err := r.ensureCMPResources(ctx, ing, frontendPort, nodePort, backends, protocol, ingressCfg)
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
	if err := r.ensureIngressStatusVIP(ctx, ing, vip); err != nil {
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

func (r *ingressReconciler) resolveBackendNodePort(ctx context.Context, ing *networkingv1.Ingress) (int32, error) {
	_, np, err := r.resolveBackendServiceAndNodePort(ctx, ing)
	return np, err
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

func (r *ingressReconciler) ensureCMPResources(ctx context.Context, ing *networkingv1.Ingress, frontendPort, nodePort int32, backends []backendNode, protocol string, cfg lbServiceConfig) (*f5client.CMPResourceIDs, string, error) {
	ids := &f5client.CMPResourceIDs{}

	// Step 1: Create or reuse LB Service.
	existingLBID := ing.Annotations[annIngressLBServiceID]
	if existingLBID != "" {
		ids.LBServiceID = existingLBID
	} else {
		var lbName string
		if g := strings.TrimSpace(ing.Annotations[annVIPGroup]); g != "" {
			lbName = sanitizeKey(fmt.Sprintf("ing-group-%s-%s", ing.Namespace, g))
		} else {
			lbName = sanitizeKey(fmt.Sprintf("ing-%s-%s", ing.Namespace, ing.Name))
		}

		// Look up existing LBService by name (required for vip-group sharing).
		if foundID, err := r.findLBServiceByName(ctx, lbName); err == nil && foundID != "" {
			ids.LBServiceID = foundID
		} else {
			form := url.Values{}
			form.Set("name", lbName)
			form.Set("description", fmt.Sprintf("Ingress LB for %s/%s", ing.Namespace, ing.Name))
			if r.vpcID != "" {
				form.Set("vpc_id", r.vpcID)
			}

			raw, err := r.cmp.CreateLBService(ctx, form)
			if err != nil {
				return nil, "", fmt.Errorf("creating LB service via CMP: %w", err)
			}
			var created struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
				return nil, "", fmt.Errorf("parsing LB Service create response: %s", string(raw))
			}
			ids.LBServiceID = created.ID
		}
	}

	// Step 2: Create or reuse VIP.
	existingVIPID := ing.Annotations[annIngressVIPPortID]
	vip := ing.Annotations[annIngressVIPAddress]
	if existingVIPID != "" {
		ids.VIPPortID = existingVIPID
	} else {
		// For vip-group: try to find an existing VIP on the LBService first.
		if vips, listErr := r.cmp.GetLBServiceVIPs(ctx, ids.LBServiceID); listErr == nil && len(vips) > 0 {
			for _, v := range vips {
				var vipInt struct {
					ID      int    `json:"id"`
					Address string `json:"address"`
				}
				if json.Unmarshal(v, &vipInt) == nil && vipInt.ID != 0 {
					ids.VIPPortID = fmt.Sprintf("%d", vipInt.ID)
					vip = vipInt.Address
					break
				}
				var vipStr struct {
					ID      string `json:"id"`
					Address string `json:"address"`
				}
				if json.Unmarshal(v, &vipStr) == nil && strings.TrimSpace(vipStr.ID) != "" {
					ids.VIPPortID = strings.TrimSpace(vipStr.ID)
					vip = vipStr.Address
					break
				}
			}
		}
	}
	// If still no VIP, create one.
	if ids.VIPPortID == "" {
		raw, err := r.cmp.CreateLBServiceVIP(ctx, ids.LBServiceID)
		if err != nil {
			return ids, "", fmt.Errorf("creating VIP via CMP on LB %s: %w", ids.LBServiceID, err)
		}
		var created struct {
			ID      int    `json:"id"`
			Address string `json:"address"`
		}
		if err := json.Unmarshal(raw, &created); err == nil && created.ID != 0 {
			ids.VIPPortID = fmt.Sprintf("%d", created.ID)
			vip = created.Address
		} else {
			var createdStr struct {
				ID      string `json:"id"`
				Address string `json:"address"`
			}
			if json.Unmarshal(raw, &createdStr) == nil && createdStr.ID != "" {
				ids.VIPPortID = createdStr.ID
				vip = createdStr.Address
			} else {
				return ids, "", fmt.Errorf("parsing VIP create response: %s", string(raw))
			}
		}
		// If address not in create response, fetch VIPs.
		if vip == "" {
			vips, err := r.cmp.GetLBServiceVIPs(ctx, ids.LBServiceID)
			if err == nil {
				for _, v := range vips {
					var vipInfo struct {
						Address string `json:"address"`
					}
					if json.Unmarshal(v, &vipInfo) == nil && vipInfo.Address != "" {
						vip = vipInfo.Address
						break
					}
				}
			}
		}
	}

	// Step 3: Create or reuse Virtual Server.
	existingVSID := ing.Annotations[annIngressVSID]
	if existingVSID != "" {
		ids.VirtualServerID = existingVSID
	} else {
		nodes := make([]map[string]interface{}, 0, len(backends))
		for i, bn := range backends {
			nodes = append(nodes, map[string]interface{}{
				"compute_id":      fmt.Sprintf("node-%d", i),
				"compute_ip":      bn.IP,
				"backend_port_id": i + 1,
				"port":            nodePort,
				"weight":          bn.Weight,
			})
		}

		vsName := sanitizeKey(fmt.Sprintf("ing-vs-%s-%s", ing.Namespace, ing.Name))
		query := url.Values{}
		query.Set("name", vsName)
		query.Set("vip_port_id", ids.VIPPortID)
		query.Set("protocol", protocol)
		query.Set("port", fmt.Sprintf("%d", frontendPort))
		query.Set("routing_algorithm", cfg.RoutingAlgorithm)
		query.Set("interval", fmt.Sprintf("%d", cfg.HealthInterval))
		if cfg.HealthType != "" && cfg.HealthType != "tcp" {
			query.Set("monitor_type", cfg.HealthType)
		}
		if cfg.HealthPath != "" {
			query.Set("monitor_path", cfg.HealthPath)
		}
		if cfg.PersistenceType != "" {
			query.Set("persistence_type", cfg.PersistenceType)
		}
		if cfg.DrainingTimeout > 0 {
			query.Set("connection_draining_timeout", fmt.Sprintf("%d", cfg.DrainingTimeout))
		}
		if r.vpcID != "" {
			query.Set("vpc_id", r.vpcID)
		}
		if len(cfg.SourceRanges) > 0 {
			query.Set("allowed_cidrs", strings.Join(cfg.SourceRanges, ","))
		}
		for _, n := range nodes {
			nodeJSON, _ := json.Marshal(n)
			query.Add("nodes", string(nodeJSON))
		}

		raw, err := r.cmp.CreateLBVirtualServer(ctx, ids.LBServiceID, query)
		if err != nil {
			return ids, vip, fmt.Errorf("creating virtual server via CMP: %w", err)
		}
		var created struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &created) == nil {
			if created.ID != "" {
				ids.VirtualServerID = created.ID
			} else if created.Name != "" {
				ids.VirtualServerID = created.Name
			}
		}
	}

	return ids, vip, nil
}

func (r *ingressReconciler) cleanupCMPResources(ctx context.Context, ing *networkingv1.Ingress) error {
	vsID := ing.Annotations[annIngressVSID]
	lbID := ing.Annotations[annIngressLBServiceID]
	vipID := ing.Annotations[annIngressVIPPortID]

	if vsID != "" && lbID != "" {
		if err := r.cmp.DeleteLBVirtualServer(ctx, lbID, vsID); err != nil && !f5client.IsNotFound(err) {
			return fmt.Errorf("deleting virtual server %s: %w", vsID, err)
		}
	}

	// If this LBService is shared (vip-group), skip VIP+LBService deletion
	// when other Ingresses still reference the same LBService.
	shared := r.isLBServiceShared(ctx, ing, lbID)

	if !shared && vipID != "" && lbID != "" {
		if err := r.cmp.DeleteLBServiceVIP(ctx, lbID, vipID); err != nil && !f5client.IsNotFound(err) {
			return fmt.Errorf("deleting VIP %s: %w", vipID, err)
		}
	}
	if !shared && lbID != "" {
		if err := r.cmp.DeleteLBService(ctx, lbID); err != nil && !f5client.IsNotFound(err) {
			return fmt.Errorf("deleting LB service %s: %w", lbID, err)
		}
	}
	return nil
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

// findLBServiceByName searches CMP for an LBService with the given name.
func (r *ingressReconciler) findLBServiceByName(ctx context.Context, name string) (string, error) {
	items, err := r.cmp.ListLBServices(ctx, nil)
	if err != nil {
		return "", err
	}
	for _, raw := range items {
		var svc struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &svc) != nil {
			continue
		}
		if strings.TrimSpace(svc.Name) == name && strings.TrimSpace(svc.ID) != "" {
			return strings.TrimSpace(svc.ID), nil
		}
	}
	return "", nil
}

func (r *ingressReconciler) ensureIngressStatusVIP(ctx context.Context, ing *networkingv1.Ingress, vip string) error {
	if vip == "" {
		return nil
	}
	currentIP := ""
	if len(ing.Status.LoadBalancer.Ingress) > 0 {
		currentIP = strings.TrimSpace(ing.Status.LoadBalancer.Ingress[0].IP)
	}
	if currentIP == vip {
		return nil
	}

	base := ing.DeepCopy()
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: vip}}
	if err := r.Status().Patch(ctx, ing, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("updating Ingress status with VIP %s: %w", vip, err)
	}
	return nil
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
