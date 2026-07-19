package ingress

import (
	"fmt"
	"sort"
	"strings"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	"github.com/gardener/gardener-extension-f5/pkg/backend"
	"github.com/gardener/gardener-extension-f5/pkg/model"

	networkingv1 "k8s.io/api/networking/v1"
)

// BackendSet is the resolved target set for one distinct Ingress ServicePort.
// Resolution is deliberately supplied by the controller: model construction
// must not read Kubernetes objects or make CMP calls.
type BackendSet struct {
	NodePort int32
	Nodes    []backend.Node
}

// GroupStackBuildOptions configures construction of the desired graph for an
// entire IngressGroup. BackendResolver is called once per distinct backend
// ServicePort and must return the NodePort and EndpointSlice-filtered nodes.
type GroupStackBuildOptions struct {
	ClusterUID        string
	HTTPFrontendPort  int32
	HTTPSFrontendPort int32
	BackendResolver   func(namespace string, backend networkingv1.IngressServiceBackend) (BackendSet, error)
}

// BuildGroupLoadBalancerStack builds one graph for all members of an
// IngressGroup. It creates one pool per distinct ServicePort, merges every
// host/path rule, and rejects ambiguous routes rather than allowing the order
// of informer events to decide traffic routing.
func BuildGroupLoadBalancerStack(members []*networkingv1.Ingress, cfg lbannotations.LBConfig, opts GroupStackBuildOptions) (*model.LoadBalancerStack, error) {
	if len(members) == 0 {
		return nil, fmt.Errorf("ingress group has no members")
	}
	if opts.BackendResolver == nil {
		return nil, fmt.Errorf("ingress group backend resolver is required")
	}
	if opts.HTTPFrontendPort == 0 {
		opts.HTTPFrontendPort = 80
	}
	if opts.HTTPSFrontendPort == 0 {
		opts.HTTPSFrontendPort = 443
	}

	ordered := append([]*networkingv1.Ingress(nil), members...)
	for _, ing := range ordered {
		if ing == nil {
			return nil, fmt.Errorf("ingress group contains a nil member")
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Namespace+"/"+ordered[i].Name < ordered[j].Namespace+"/"+ordered[j].Name
	})
	anchor := ordered[0]
	group := GroupName(anchor)
	for _, ing := range ordered {
		if ing == nil || ing.Namespace != anchor.Namespace || GroupName(ing) != group {
			return nil, fmt.Errorf("ingress group members must share namespace and group identity")
		}
		if err := validateIngressShape(ing); err != nil {
			return nil, fmt.Errorf("invalid ingress %s/%s: %w", ing.Namespace, ing.Name, err)
		}
	}

	identity := groupIdentity(anchor.Namespace, group, anchor.Name)
	owner := GroupOwner(anchor.Namespace, identity)
	stack := &model.LoadBalancerStack{
		Owner: owner, Ownership: model.OwnershipFor(owner, opts.ClusterUID, "stack", group), Config: cfg,
		LBService: model.LBService{Name: LBServiceName(anchor), Description: fmt.Sprintf("Ingress group LB for %s", identity), Ownership: model.OwnershipFor(owner, opts.ClusterUID, "lb-service", group)},
		VIP:       model.VIP{Name: safeName("ing-vip-" + strings.ReplaceAll(identity, "/", "-")), Ownership: model.OwnershipFor(owner, opts.ClusterUID, "vip", group)},
	}

	// A group always needs HTTP for its HTTP routes. HTTPS is added if any
	// member uses TLS; both listeners use the same pool/rule graph.
	stack.VirtualServers = append(stack.VirtualServers, groupVirtualServer(owner, opts.ClusterUID, group, identity, "HTTP", opts.HTTPFrontendPort, cfg))
	needsTLS := false
	poolByBackend := map[string]string{}
	routes := map[string]string{}
	defaultPool := ""
	priority := int32(1)
	for _, ing := range ordered {
		if len(ing.Spec.TLS) > 0 {
			needsTLS = true
		}
		poolFor := func(be networkingv1.IngressBackend) (string, error) {
			key := backendKey(be)
			if name := poolByBackend[key]; name != "" {
				return name, nil
			}
			set, err := opts.BackendResolver(ing.Namespace, *be.Service)
			if err != nil {
				return "", err
			}
			if set.NodePort == 0 {
				return "", fmt.Errorf("BackendNodePortRequired: backend %s", key)
			}
			name := safeName("ing-pool-" + anchor.Namespace + "-" + be.Service.Name + "-" + be.Service.Port.Name + fmt.Sprintf("-%d", be.Service.Port.Number))
			pool := model.Pool{Name: name, Service: be.Service.Name, PortName: be.Service.Port.Name, Port: be.Service.Port.Number, Monitor: &model.Monitor{Name: safeName("mon-" + name), Type: cfg.HealthType, Path: cfg.HealthPath, Interval: cfg.HealthInterval}, Ownership: model.OwnershipFor(owner, opts.ClusterUID, "pool", group)}
			for _, node := range set.Nodes {
				pool.Members = append(pool.Members, model.BackendMember{IP: node.IP, Port: set.NodePort, Weight: node.Weight})
			}
			stack.Pools, poolByBackend[key] = append(stack.Pools, pool), name
			return name, nil
		}
		addRoute := func(host, path, match string, be networkingv1.IngressBackend, isDefault bool) error {
			pool, err := poolFor(be)
			if err != nil {
				return err
			}
			key := strings.ToLower(strings.TrimSpace(host)) + "|" + match + "|" + path
			if previous, exists := routes[key]; exists && previous != pool {
				return fmt.Errorf("RouteConflict: %s maps to both %s and %s", key, previous, pool)
			}
			if _, exists := routes[key]; exists {
				return nil
			}
			routes[key] = pool
			if isDefault {
				if defaultPool != "" && defaultPool != pool {
					return fmt.Errorf("DefaultBackendConflict: group has multiple default backends")
				}
				defaultPool = pool
				return nil
			}
			stack.RoutingRules = append(stack.RoutingRules, model.RoutingRule{Name: safeName(fmt.Sprintf("ing-rule-%s-%d", strings.ReplaceAll(identity, "/", "-"), priority)), Host: strings.ToLower(strings.TrimSpace(host)), Path: path, MatchType: match, PoolName: pool, Priority: priority, Ownership: model.OwnershipFor(owner, opts.ClusterUID, "routing-rule", group)})
			priority++
			return nil
		}
		if ing.Spec.DefaultBackend != nil {
			if err := addRoute("", "/", "default", *ing.Spec.DefaultBackend, true); err != nil {
				return nil, err
			}
		}
		for _, rule := range ing.Spec.Rules {
			for _, path := range rule.HTTP.Paths {
				match := "prefix"
				if *path.PathType == networkingv1.PathTypeExact {
					match = "exact"
				}
				if err := addRoute(rule.Host, path.Path, match, path.Backend, false); err != nil {
					return nil, err
				}
			}
		}
		for _, tls := range ing.Spec.TLS {
			stack.Certificates = append(stack.Certificates, model.Certificate{Name: safeName("ing-cert-" + anchor.Namespace + "-" + tls.SecretName), SecretName: tls.SecretName, Hosts: append([]string(nil), tls.Hosts...), Ownership: model.OwnershipFor(owner, opts.ClusterUID, "certificate", group)})
		}
	}
	for i := range stack.VirtualServers {
		stack.VirtualServers[i].DefaultPoolName = defaultPool
	}
	if needsTLS {
		https := groupVirtualServer(owner, opts.ClusterUID, group, identity, "HTTPS", opts.HTTPSFrontendPort, cfg)
		https.DefaultPoolName = defaultPool
		stack.VirtualServers = append(stack.VirtualServers, https)
	}
	return stack, nil
}

func groupVirtualServer(owner model.Owner, clusterUID, group, identity, protocol string, port int32, cfg lbannotations.LBConfig) model.VirtualServer {
	return model.VirtualServer{Name: safeName(fmt.Sprintf("ing-vs-%s-%s", strings.ReplaceAll(identity, "/", "-"), strings.ToLower(protocol))), FrontendPort: port, Protocol: protocol, RoutingAlgorithm: cfg.RoutingAlgorithm, PersistenceType: cfg.PersistenceType, DrainingTimeout: cfg.DrainingTimeout, SourceRanges: append([]string(nil), cfg.SourceRanges...), Ownership: model.OwnershipFor(owner, clusterUID, "virtual-server", group)}
}

func backendKey(be networkingv1.IngressBackend) string {
	return be.Service.Name + ":" + be.Service.Port.Name + fmt.Sprintf(":%d", be.Service.Port.Number)
}
