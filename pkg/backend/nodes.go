package backend

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Node is an eligible load-balancer backend node.
type Node struct {
	IP     string
	Weight int
}

// ListReadyNodeBackends returns Ready node InternalIPs, optionally narrowed to
// nodes that have ready EndpointSlice endpoints for the supplied Service. If no
// EndpointSlice data exists, it falls back to all Ready nodes to preserve the
// existing controller behavior for environments that do not publish slices.
func ListReadyNodeBackends(ctx context.Context, c client.Client, svc *corev1.Service) ([]Node, error) {
	targetNodes := nodesWithReadyEndpoints(ctx, c, svc)
	// Local external traffic policy means the provider must target only nodes
	// that have ready local endpoints. Falling back to every ready node would
	// produce black-holed connections and defeats Kubernetes' policy contract.
	localOnly := svc != nil && svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyTypeLocal

	nl := &corev1.NodeList{}
	if err := c.List(ctx, nl); err != nil {
		return nil, err
	}

	out := make([]Node, 0, len(nl.Items))
	for i := range nl.Items {
		n := &nl.Items[i]
		if !IsNodeReady(n) {
			continue
		}
		epCount := 0
		if len(targetNodes) > 0 {
			count, ok := targetNodes[n.Name]
			if !ok {
				continue
			}
			epCount = count
		} else if localOnly {
			continue
		}
		if ip := InternalIP(n); ip != "" {
			weight := 50
			if epCount > 0 {
				weight = epCount * 50
			}
			out = append(out, Node{IP: ip, Weight: weight})
		}
	}
	return out, nil
}

// ListNodeInternalIPs returns all node InternalIPs without readiness filtering.
// This preserves the existing seed-service-lb behavior where the Seed ingress
// gateway is targeted through every Seed node.
func ListNodeInternalIPs(ctx context.Context, c client.Client) ([]string, error) {
	nl := &corev1.NodeList{}
	if err := c.List(ctx, nl); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(nl.Items))
	for i := range nl.Items {
		if ip := InternalIP(&nl.Items[i]); ip != "" {
			out = append(out, ip)
		}
	}
	return out, nil
}

// IsNodeReady returns true if the node has condition Ready=True.
func IsNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// InternalIP returns the first non-empty NodeInternalIP.
func InternalIP(node *corev1.Node) string {
	for _, a := range node.Status.Addresses {
		if a.Type == corev1.NodeInternalIP && strings.TrimSpace(a.Address) != "" {
			return strings.TrimSpace(a.Address)
		}
	}
	return ""
}

func nodesWithReadyEndpoints(ctx context.Context, c client.Client, svc *corev1.Service) map[string]int {
	if svc == nil {
		return nil
	}
	epsList := &discoveryv1.EndpointSliceList{}
	sel := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: svc.Name})
	if err := c.List(ctx, epsList, &client.ListOptions{Namespace: svc.Namespace, LabelSelector: sel}); err != nil {
		return nil
	}

	nodes := make(map[string]int)
	for i := range epsList.Items {
		for j := range epsList.Items[i].Endpoints {
			ep := &epsList.Items[i].Endpoints[j]
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if ep.NodeName != nil && *ep.NodeName != "" {
				nodes[*ep.NodeName]++
			}
		}
	}
	return nodes
}

// ListReadyNodeBackendsForPort returns ready backend nodes that have ready
// EndpointSlice endpoints for one Service port. It is the port-aware variant
// used by multi-port Service reconciliation; unrelated endpoint ports cannot
// accidentally populate another listener's pool.
func ListReadyNodeBackendsForPort(ctx context.Context, c client.Client, svc *corev1.Service, servicePort corev1.ServicePort) ([]Node, error) {
	targetNodes := nodesWithReadyEndpointsForPort(ctx, c, svc, servicePort)
	if svc != nil && svc.Spec.ExternalTrafficPolicy == corev1.ServiceExternalTrafficPolicyTypeLocal && len(targetNodes) == 0 {
		return nil, nil
	}
	if len(targetNodes) == 0 {
		// Cluster policy retains the established ready-node fallback when a
		// cluster does not publish EndpointSlices.
		return ListReadyNodeBackends(ctx, c, svc)
	}
	nl := &corev1.NodeList{}
	if err := c.List(ctx, nl); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(targetNodes))
	for i := range nl.Items {
		n := &nl.Items[i]
		if !IsNodeReady(n) {
			continue
		}
		count, ok := targetNodes[n.Name]
		if !ok {
			continue
		}
		if ip := InternalIP(n); ip != "" {
			out = append(out, Node{IP: ip, Weight: count * 50})
		}
	}
	return out, nil
}

func nodesWithReadyEndpointsForPort(ctx context.Context, c client.Client, svc *corev1.Service, servicePort corev1.ServicePort) map[string]int {
	if svc == nil {
		return nil
	}
	epsList := &discoveryv1.EndpointSliceList{}
	sel := labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: svc.Name})
	if err := c.List(ctx, epsList, &client.ListOptions{Namespace: svc.Namespace, LabelSelector: sel}); err != nil {
		return nil
	}
	nodes := map[string]int{}
	for i := range epsList.Items {
		slice := &epsList.Items[i]
		portMatches := false
		for _, p := range slice.Ports {
			if servicePort.Name != "" && p.Name != nil && *p.Name == servicePort.Name {
				portMatches = true
			}
			if servicePort.Name == "" && p.Port != nil && *p.Port == servicePort.Port {
				portMatches = true
			}
		}
		if !portMatches {
			continue
		}
		for j := range slice.Endpoints {
			ep := &slice.Endpoints[j]
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if ep.NodeName != nil && *ep.NodeName != "" {
				nodes[*ep.NodeName]++
			}
		}
	}
	return nodes
}
