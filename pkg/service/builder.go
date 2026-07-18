package service

import (
	fmt "fmt"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	"github.com/gardener/gardener-extension-f5/pkg/backend"
	"github.com/gardener/gardener-extension-f5/pkg/model"

	corev1 "k8s.io/api/core/v1"
)

// BuildLoadBalancerStack converts a Kubernetes LoadBalancer Service snapshot
// into a typed desired-state stack. The builder does not call CMP or mutate
// Kubernetes objects.
func BuildLoadBalancerStack(svc *corev1.Service, cfg lbannotations.LBConfig, nodes []backend.Node) (*model.LoadBalancerStack, error) {
	if svc == nil {
		return nil, fmt.Errorf("service must not be nil")
	}
	if len(svc.Spec.Ports) == 0 {
		return nil, fmt.Errorf("service has no ports")
	}

	owner := model.Owner{Kind: "Service", Namespace: svc.Namespace, Name: svc.Name, UID: string(svc.UID)}
	stack := &model.LoadBalancerStack{
		Owner:     owner,
		Ownership: model.OwnershipFor(owner, "", "stack", ""),
		Config:    cfg,
	}

	for _, p := range svc.Spec.Ports {
		if p.Port == 0 || p.NodePort == 0 {
			continue
		}
		proto := MapK8sProtocolToCMP(p.Protocol, p.Port)
		if cfg.ProtocolOverride != "" {
			proto = cfg.ProtocolOverride
		}
		sp := model.ServicePort{Name: p.Name, FrontendPort: p.Port, NodePort: p.NodePort, Protocol: proto}
		vs := model.VirtualServer{
			Name:             p.Name,
			FrontendPort:     p.Port,
			BackendNodePort:  p.NodePort,
			Protocol:         proto,
			RoutingAlgorithm: cfg.RoutingAlgorithm,
			PersistenceType:  cfg.PersistenceType,
			DrainingTimeout:  cfg.DrainingTimeout,
			SourceRanges:     append([]string(nil), cfg.SourceRanges...),
			Monitor:          &model.Monitor{Type: cfg.HealthType, Path: cfg.HealthPath, Interval: cfg.HealthInterval},
			Ownership:        model.OwnershipFor(owner, "", "virtual-server", ""),
		}
		pool := model.Pool{Name: p.Name, Monitor: vs.Monitor, Ownership: model.OwnershipFor(owner, "", "pool", "")}
		for _, n := range nodes {
			member := model.BackendMember{IP: n.IP, Port: p.NodePort, Weight: n.Weight}
			sp.Backends = append(sp.Backends, member)
			pool.Members = append(pool.Members, member)
		}
		vs.DefaultPoolName = pool.Name
		stack.VirtualServers = append(stack.VirtualServers, vs)
		stack.Pools = append(stack.Pools, pool)
		stack.Ports = append(stack.Ports, sp)
	}
	if len(stack.Ports) == 0 {
		return nil, fmt.Errorf("service has no usable LoadBalancer NodePorts")
	}
	return stack, nil
}
