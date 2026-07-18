package ingress

import (
	"fmt"
	"strings"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	"github.com/gardener/gardener-extension-f5/pkg/backend"
	"github.com/gardener/gardener-extension-f5/pkg/model"

	networkingv1 "k8s.io/api/networking/v1"
)

const SourceKind = "Ingress"

type BuildOptions struct {
	FrontendPort int32
	BackendPort  int32
	Protocol     string
	ClusterUID   string
}

func BuildLoadBalancerStack(ing *networkingv1.Ingress, cfg lbannotations.LBConfig, nodes []backend.Node, opts BuildOptions) (*model.LoadBalancerStack, error) {
	if ing == nil {
		return nil, fmt.Errorf("ingress must not be nil")
	}
	if opts.FrontendPort == 0 {
		return nil, fmt.Errorf("ingress frontend port must not be zero")
	}
	if opts.BackendPort == 0 {
		return nil, fmt.Errorf("ingress backend NodePort must not be zero")
	}
	protocol := strings.TrimSpace(opts.Protocol)
	if protocol == "" {
		protocol = ProtocolForIngress(ing)
	}

	group := GroupName(ing)
	owner := model.Owner{Kind: SourceKind, Namespace: ing.Namespace, Name: ing.Name, UID: string(ing.UID)}
	stack := &model.LoadBalancerStack{
		Owner:     owner,
		Ownership: model.OwnershipFor(owner, opts.ClusterUID, "stack", group),
		Config:    cfg,
		LBService: model.LBService{
			Name:        LBServiceName(ing),
			Description: fmt.Sprintf("Ingress LB for %s/%s", ing.Namespace, ing.Name),
			Ownership:   model.OwnershipFor(owner, opts.ClusterUID, "lb-service", group),
		},
		VIP: model.VIP{
			Name:      VIPName(ing),
			Ownership: model.OwnershipFor(owner, opts.ClusterUID, "vip", group),
		},
	}
	vs := model.VirtualServer{
		Name:             VirtualServerName(ing),
		FrontendPort:     opts.FrontendPort,
		BackendNodePort:  opts.BackendPort,
		Protocol:         protocol,
		RoutingAlgorithm: cfg.RoutingAlgorithm,
		PersistenceType:  cfg.PersistenceType,
		DrainingTimeout:  cfg.DrainingTimeout,
		SourceRanges:     append([]string(nil), cfg.SourceRanges...),
		Monitor:          &model.Monitor{Type: cfg.HealthType, Path: cfg.HealthPath, Interval: cfg.HealthInterval},
		Ownership:        model.OwnershipFor(owner, opts.ClusterUID, "virtual-server", group),
	}
	pool := model.Pool{Name: PoolName(ing), Monitor: vs.Monitor, Ownership: model.OwnershipFor(owner, opts.ClusterUID, "pool", group)}
	port := model.ServicePort{Name: "ingress", FrontendPort: opts.FrontendPort, NodePort: opts.BackendPort, Protocol: protocol}
	for _, n := range nodes {
		member := model.BackendMember{IP: n.IP, Port: opts.BackendPort, Weight: n.Weight}
		pool.Members = append(pool.Members, member)
		port.Backends = append(port.Backends, member)
	}
	vs.DefaultPoolName = pool.Name
	stack.VirtualServers = append(stack.VirtualServers, vs)
	stack.Pools = append(stack.Pools, pool)
	stack.Ports = append(stack.Ports, port)
	return stack, nil
}

func ProtocolForIngress(ing *networkingv1.Ingress) string {
	if ing != nil && len(ing.Spec.TLS) > 0 {
		return "HTTPS"
	}
	return "HTTP"
}

func FrontendPortForProtocol(protocol string) int32 {
	if strings.EqualFold(strings.TrimSpace(protocol), "HTTPS") {
		return 443
	}
	return 80
}

func GroupName(ing *networkingv1.Ingress) string {
	if ing == nil || ing.Annotations == nil {
		return ""
	}
	return strings.TrimSpace(ing.Annotations[lbannotations.VIPGroup])
}

func LBServiceName(ing *networkingv1.Ingress) string {
	if group := GroupName(ing); group != "" {
		return safeName(fmt.Sprintf("ing-group-%s-%s", ing.Namespace, group))
	}
	return safeName(fmt.Sprintf("ing-%s-%s", ing.Namespace, ing.Name))
}

func VIPName(ing *networkingv1.Ingress) string {
	return safeName(fmt.Sprintf("ing-vip-%s-%s", ing.Namespace, ing.Name))
}

func VirtualServerName(ing *networkingv1.Ingress) string {
	return safeName(fmt.Sprintf("ing-vs-%s-%s", ing.Namespace, ing.Name))
}

func PoolName(ing *networkingv1.Ingress) string {
	return safeName(fmt.Sprintf("ing-pool-%s-%s", ing.Namespace, ing.Name))
}

func safeName(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "x"
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "x"
	}
	return out
}
