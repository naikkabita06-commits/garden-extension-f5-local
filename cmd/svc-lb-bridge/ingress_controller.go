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
	lbfinalizers "github.com/gardener/gardener-extension-f5/pkg/finalizers"
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
			// A group member leaving must first converge the graph built from the
			// remaining members. Deleting the departing member's stored graph
			// directly would remove routes and pools still needed by the group.
			remaining, err := r.remainingGroupMembers(ctx, ing)
			if err != nil {
				return ctrl.Result{}, err
			}
			if len(remaining) > 0 {
				stack, err := lbingress.BuildGroupLoadBalancerStack(remaining, parseIngressConfig(canonicalIngress(remaining)), lbingress.GroupStackBuildOptions{BackendResolver: r.resolveGroupBackend(ctx)})
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("building remaining ingress group: %w", err)
				}
				ids, vip, graph, err := r.ensureCMPResources(ctx, ing, stack)
				if err != nil {
					return ctrl.Result{}, fmt.Errorf("reconciling remaining ingress group: %w", err)
				}
				if err := r.persistGroupObserved(ctx, remaining, ids, vip, graph); err != nil {
					return ctrl.Result{}, err
				}
				if _, err := lbfinalizers.Remove(ctx, r.Client, ing, ingressFinalizerName); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
			log.Info("cleaning up CMP LBaaS resources for Ingress")
			r.Recorder.Eventf(ing, corev1.EventTypeNormal, "DeletingLoadBalancer", "Deleting CMP LBaaS resources for Ingress (LB=%s)", ing.Annotations[annIngressLBServiceID])
			if err := r.cleanupCMPResources(ctx, ing); err != nil {
				r.Recorder.Eventf(ing, corev1.EventTypeWarning, "DeleteFailed", "CMP LBaaS cleanup failed: %v", err)
				return ctrl.Result{}, fmt.Errorf("CMP cleanup failed; retrying: %w", err)
			}
			r.Recorder.Event(ing, corev1.EventTypeNormal, "DeletedLoadBalancer", "CMP LBaaS resources deleted successfully")

			if _, err := lbfinalizers.Remove(ctx, r.Client, ing, ingressFinalizerName); err != nil {
				return ctrl.Result{}, err
			}
			f5metrics.ManagedServicesTotal.WithLabelValues("ingress-lb").Dec()
		}
		return ctrl.Result{}, nil
	}

	// Reconcile the entire IngressGroup, not the single event object. Persisting
	// the resulting graph on every member makes any member a safe restart point.
	all := &networkingv1.IngressList{}
	if err := r.List(ctx, all, client.InNamespace(ing.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	if oldGroup := observedIngressGroup(ing); oldGroup != "" && oldGroup != lbingress.GroupName(ing) {
		remaining := ingressGroupMembers(all.Items, ing.Namespace, oldGroup, ing.Name, r.isOwnedIngress)
		if len(remaining) > 0 {
			oldStack, err := lbingress.BuildGroupLoadBalancerStack(remaining, parseIngressConfig(canonicalIngress(remaining)), lbingress.GroupStackBuildOptions{BackendResolver: r.resolveGroupBackend(ctx)})
			if err != nil {
				return ctrl.Result{}, err
			}
			ids, vip, graph, err := r.ensureCMPResources(ctx, ing, oldStack)
			if err != nil {
				return ctrl.Result{}, err
			}
			if err := r.persistGroupObserved(ctx, remaining, ids, vip, graph); err != nil {
				return ctrl.Result{}, err
			}
		}
		// Do not let the new group adopt the old group's observed parent IDs.
		base := ing.DeepCopy()
		delete(ing.Annotations, annObservedGraph)
		delete(ing.Annotations, annIngressLBServiceID)
		delete(ing.Annotations, annIngressVIPPortID)
		delete(ing.Annotations, annIngressVSID)
		delete(ing.Annotations, annIngressVIPAddress)
		if err := r.Patch(ctx, ing, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	members, _, err := lbingress.ResolveGroup(ing, all.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	groupMembers := make([]*networkingv1.Ingress, 0, len(members))
	for _, member := range members {
		if r.isOwnedIngress(member) && member.DeletionTimestamp.IsZero() {
			groupMembers = append(groupMembers, member)
		}
	}
	// ResolveGroup sorts members, so the canonical member makes shared frontend
	// configuration independent of which member generated the event.
	ingressCfg := parseIngressConfig(canonicalIngress(groupMembers))
	stack, err := lbingress.BuildGroupLoadBalancerStack(groupMembers, ingressCfg, lbingress.GroupStackBuildOptions{
		BackendResolver: r.resolveGroupBackend(ctx),
	})
	if err != nil {
		r.Recorder.Eventf(ing, corev1.EventTypeWarning, "BuildLoadBalancerModelFailed", "Error building Ingress load-balancer model: %v", err)
		return ctrl.Result{}, err
	}
	// TLS certificates require a certificate-capable CMP client. If it is not
	// configured, surface a clear error rather than silently provisioning a
	// partially secure frontend.
	if len(stack.Certificates) > 0 && !r.hasCertificateCapability() {
		r.Recorder.Event(ing, corev1.EventTypeWarning, "TLSNotSupported", "TLS requires the CMP CertificateManager capability")
		return ctrl.Result{}, fmt.Errorf("TLS certificate reconciliation is not configured")
	}
	if err := r.populateTLSCertificates(ctx, ing.Namespace, stack); err != nil {
		r.Recorder.Eventf(ing, corev1.EventTypeWarning, "InvalidCertificate", "%v", err)
		return ctrl.Result{}, err
	}

	// Add the finalizer only after validation has produced a deployable graph.
	if !controllerutil.ContainsFinalizer(ing, ingressFinalizerName) {
		if _, err := lbfinalizers.Ensure(ctx, r.Client, ing, ingressFinalizerName); err != nil {
			return ctrl.Result{}, err
		}
		f5metrics.ManagedServicesTotal.WithLabelValues("ingress-lb").Inc()
	}

	// Provision CMP resources.
	cmpStart := time.Now()
	ids, vip, graph, err := r.ensureCMPResources(ctx, ing, stack)
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

	if err := r.persistGroupObserved(ctx, groupMembers, ids, vip, graph); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("Ingress group reconciled", "vip", vip, "members", len(groupMembers))
	r.Recorder.Eventf(ing, corev1.EventTypeNormal, "EnsuredLoadBalancer", "Ingress group reconciled via CMP LBaaS (VIP=%s, members=%d)", vip, len(groupMembers))
	return ctrl.Result{}, nil
}

// remainingGroupMembers returns live F5 Ingresses sharing the departing
// object's group. It intentionally ignores foreign Ingress classes: they are
// not safe ownership references for an F5-managed frontend.
func (r *ingressReconciler) remainingGroupMembers(ctx context.Context, departing *networkingv1.Ingress) ([]*networkingv1.Ingress, error) {
	items := &networkingv1.IngressList{}
	if err := r.List(ctx, items, client.InNamespace(departing.Namespace)); err != nil {
		return nil, err
	}
	group := lbingress.GroupName(departing)
	members := make([]*networkingv1.Ingress, 0)
	if group == "" {
		// Ungrouped Ingresses are explicitly singleton groups. They must never
		// borrow another ungrouped object's observed graph during finalization.
		return members, nil
	}
	for i := range items.Items {
		candidate := &items.Items[i]
		if candidate.Name == departing.Name || !candidate.DeletionTimestamp.IsZero() || !r.isOwnedIngress(candidate) {
			continue
		}
		if group == "" || lbingress.GroupName(candidate) == group {
			members = append(members, candidate)
		}
	}
	return members, nil
}

func observedIngressGroup(ing *networkingv1.Ingress) string {
	graph, ok := readObservedGraph(ing.Annotations)
	if !ok {
		return ""
	}
	for _, lb := range graph.LBServices {
		if lb.Ownership.SourceKind == "IngressGroup" {
			return lb.Ownership.SharedGroup
		}
	}
	return ""
}

func ingressGroupMembers(items []networkingv1.Ingress, namespace, group, exclude string, owned func(*networkingv1.Ingress) bool) []*networkingv1.Ingress {
	members := make([]*networkingv1.Ingress, 0)
	for i := range items {
		item := &items[i]
		if item.Namespace == namespace && item.Name != exclude && item.DeletionTimestamp.IsZero() && lbingress.GroupName(item) == group && owned(item) {
			members = append(members, item)
		}
	}
	return members
}

func canonicalIngress(members []*networkingv1.Ingress) *networkingv1.Ingress {
	if len(members) == 0 {
		return nil
	}
	canonical := members[0]
	for _, member := range members[1:] {
		if member.Namespace+"/"+member.Name < canonical.Namespace+"/"+canonical.Name {
			canonical = member
		}
	}
	return canonical
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
	return svc, 0, fmt.Errorf("service %s/%s has no port matching ingress backend port name=%q number=%d", namespace, name, port.Name, port.Number)
}

// resolveGroupBackend supplies the model builder with a backend set per
// ServicePort. ListReadyNodeBackends applies EndpointSlice readiness and
// traffic-policy filtering, so pools contain only eligible nodes.
func (r *ingressReconciler) resolveGroupBackend(ctx context.Context) func(string, networkingv1.IngressServiceBackend) (lbingress.BackendSet, error) {
	return func(namespace string, backend networkingv1.IngressServiceBackend) (lbingress.BackendSet, error) {
		svc, nodePort, err := r.getServiceAndNodePort(ctx, namespace, backend.Name, backend.Port)
		if err != nil {
			return lbingress.BackendSet{}, err
		}
		if svc == nil {
			return lbingress.BackendSet{}, fmt.Errorf("backend service %s/%s was not found", namespace, backend.Name)
		}
		if nodePort == 0 {
			return lbingress.BackendSet{}, fmt.Errorf("BackendNodePortRequired: backend service %s/%s", namespace, backend.Name)
		}
		nodes, err := lbbackend.ListReadyNodeBackends(ctx, r.Client, svc)
		if err != nil {
			return lbingress.BackendSet{}, err
		}
		return lbingress.BackendSet{NodePort: nodePort, Nodes: nodes}, nil
	}
}

// persistGroupObserved mirrors provider identifiers and status to every live
// group member. This allows reconciliation to resume from any member after a
// controller restart and ensures users see one consistent group frontend.
func (r *ingressReconciler) persistGroupObserved(ctx context.Context, members []*networkingv1.Ingress, ids *f5client.CMPResourceIDs, vip string, graph model.ObservedGraph) error {
	for _, member := range members {
		base := member.DeepCopy()
		if member.Annotations == nil {
			member.Annotations = map[string]string{}
		}
		member.Annotations[annIngressLBServiceID] = ids.LBServiceID
		member.Annotations[annIngressVIPPortID] = ids.VIPPortID
		member.Annotations[annIngressVSID] = ids.VirtualServerID
		member.Annotations[annIngressVIPAddress] = vip
		if err := writeObservedGraph(member.Annotations, graph); err != nil {
			return err
		}
		if err := r.Patch(ctx, member, client.MergeFrom(base)); err != nil {
			return err
		}
		if err := lbstatus.EnsureIngressVIP(ctx, r.Client, member, vip); err != nil {
			return err
		}
	}
	return nil
}

func (r *ingressReconciler) hasCertificateCapability() bool {
	return r.cmp != nil
}

func (r *ingressReconciler) populateTLSCertificates(ctx context.Context, namespace string, stack *model.LoadBalancerStack) error {
	if stack == nil || len(stack.Certificates) == 0 {
		return nil
	}
	cache := map[string]lbingress.TLSMaterial{}
	for i := range stack.Certificates {
		cert := &stack.Certificates[i]
		secretName := strings.TrimSpace(cert.SecretName)
		if secretName == "" {
			return fmt.Errorf("certificate %q has no Secret reference", cert.Name)
		}
		material, ok := cache[secretName]
		if !ok {
			secret := &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil {
				return fmt.Errorf("reading TLS Secret %s/%s: %w", namespace, secretName, err)
			}
			validated, err := lbingress.ReadTLSSecret(secret)
			if err != nil {
				return err
			}
			cache[secretName] = validated
			material = validated
		}
		cert.Fingerprint = material.Fingerprint
		cert.Certificate = string(material.Certificate)
		cert.PrivateKey = string(material.PrivateKey)
		cert.CA = string(material.CA)
	}
	return nil
}

func (r *ingressReconciler) ensureCMPResources(ctx context.Context, ing *networkingv1.Ingress, stack *model.LoadBalancerStack) (*f5client.CMPResourceIDs, string, model.ObservedGraph, error) {
	current := model.ObservedState{}
	if ing != nil && ing.Annotations != nil {
		current.LBServiceID = strings.TrimSpace(ing.Annotations[annIngressLBServiceID])
		current.VIPPortID = strings.TrimSpace(ing.Annotations[annIngressVIPPortID])
		current.VirtualServerID = strings.TrimSpace(ing.Annotations[annIngressVSID])
		current.VIPAddress = strings.TrimSpace(ing.Annotations[annIngressVIPAddress])
	}
	if stack == nil || len(stack.VirtualServers) == 0 {
		return nil, current.VIPAddress, current.Graph, fmt.Errorf("ingress load-balancer stack is empty")
	}
	if graph, ok := readObservedGraph(ing.Annotations); ok {
		current.Graph = graph
	}
	result, err := lbaasdeploy.NewFromRaw(r.cmp, r.vpcID).EnsureStack(ctx, lbaasdeploy.StackEnsureRequest{Stack: stack, Current: current})
	if err != nil {
		return nil, current.VIPAddress, current.Graph, err
	}
	observed := result.Observed
	vs := observed.Graph.VirtualServers[stack.VirtualServers[0].Name]
	ids := &f5client.CMPResourceIDs{LBServiceID: observed.LBServiceID, VIPPortID: observed.VIPPortID, VirtualServerID: vs.ExternalID, VirtualServerName: vs.Name}
	return ids, result.Observed.VIPAddress, result.Observed.Graph, nil
}

func (r *ingressReconciler) cleanupCMPResources(ctx context.Context, ing *networkingv1.Ingress) error {
	observed := model.ObservedState{
		LBServiceID:     strings.TrimSpace(ing.Annotations[annIngressLBServiceID]),
		VIPPortID:       strings.TrimSpace(ing.Annotations[annIngressVIPPortID]),
		VirtualServerID: strings.TrimSpace(ing.Annotations[annIngressVSID]),
	}
	if graph, ok := readObservedGraph(ing.Annotations); ok {
		observed.Graph = graph
	}
	observed.EnsureGraph()
	shared := r.isLBServiceShared(ctx, ing, observed.LBServiceID)
	_, err := lbaasdeploy.NewFromRaw(r.cmp, r.vpcID).CleanupStack(ctx, lbaasdeploy.CleanupRequest{Current: observed, DeleteVIP: !shared, DeleteLBService: !shared})
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
		if !controllerutil.ContainsFinalizer(&ing, ingressFinalizerName) {
			continue
		}
		if graph, ok := readObservedGraph(ing.Annotations); ok {
			for _, parent := range graph.LBServices {
				// Group graphs are intentionally owned by the synthetic
				// IngressGroup, so every live member with the same group graph is
				// a reference. Legacy per-Ingress graphs retain their stricter
				// source-object ownership check.
				if parent.ExternalID == lbID && ((parent.Ownership.SourceKind == "IngressGroup" && parent.Ownership.SharedGroup == lbingress.GroupName(self)) || (parent.Ownership.SourceKind == "Ingress" && parent.Ownership.SourceNamespace == ing.Namespace && parent.Ownership.SourceName == ing.Name && (parent.Ownership.SourceUID == "" || parent.Ownership.SourceUID == string(ing.UID)))) {
					return true
				}
			}
			continue
		}
		if ing.Annotations[annIngressLBServiceID] == lbID {
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

func listManagedIngressRequestsForSecret(ctx context.Context, c client.Client, namespace, secretName string) []reconcile.Request {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(secretName) == "" {
		return nil
	}
	ingList := &networkingv1.IngressList{}
	if err := c.List(ctx, ingList, client.InNamespace(namespace)); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0)
	seen := map[string]struct{}{}
	for i := range ingList.Items {
		ing := &ingList.Items[i]
		if ing.DeletionTimestamp != nil || !isOwnedIngressObject(ing) || !ingressReferencesTLSSecret(ing, secretName) {
			continue
		}
		group := lbingress.GroupName(ing)
		for j := range ingList.Items {
			candidate := &ingList.Items[j]
			if candidate.DeletionTimestamp != nil || !isOwnedIngressObject(candidate) {
				continue
			}
			if group != "" && lbingress.GroupName(candidate) != group {
				continue
			}
			if group == "" && (candidate.Namespace != ing.Namespace || candidate.Name != ing.Name) {
				continue
			}
			key := candidate.Namespace + "/" + candidate.Name
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: candidate.Namespace, Name: candidate.Name}})
		}
	}
	return reqs
}

func ingressReferencesTLSSecret(ing *networkingv1.Ingress, secretName string) bool {
	secretName = strings.TrimSpace(secretName)
	if ing == nil || secretName == "" {
		return false
	}
	for _, tls := range ing.Spec.TLS {
		if strings.TrimSpace(tls.SecretName) == secretName {
			return true
		}
	}
	return false
}

func isOwnedIngressObject(ing *networkingv1.Ingress) bool {
	if ing == nil {
		return false
	}
	if ing.Spec.IngressClassName != nil {
		return *ing.Spec.IngressClassName == ingressClassName
	}
	if ann, ok := ing.Annotations["kubernetes.io/ingress.class"]; ok {
		return ann == ingressClassName
	}
	return false
}
