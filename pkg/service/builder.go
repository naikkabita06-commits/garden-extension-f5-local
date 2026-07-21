package service

import (
	"crypto/sha256"
	"encoding/hex"
	fmt "fmt"
	"strings"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	"github.com/gardener/gardener-extension-f5/pkg/backend"
	"github.com/gardener/gardener-extension-f5/pkg/model"

	corev1 "k8s.io/api/core/v1"
)

// BuildLoadBalancerStack converts a Kubernetes LoadBalancer Service snapshot
// into a typed desired-state stack. The builder does not call CMP or mutate
// Kubernetes objects.
func BuildLoadBalancerStack(svc *corev1.Service, cfg lbannotations.LBConfig, nodes []backend.Node) (*model.LoadBalancerStack, error) {
	return BuildLoadBalancerStackWithPortBackends(svc, cfg, func(corev1.ServicePort) []backend.Node { return nodes })
}

// BuildLoadBalancerStackWithPortBackends builds independent member sets for
// every Service port.
func BuildLoadBalancerStackWithPortBackends(svc *corev1.Service, cfg lbannotations.LBConfig, backendsForPort func(corev1.ServicePort) []backend.Node) (*model.LoadBalancerStack, error) {
	if svc == nil {
		return nil, fmt.Errorf("service must not be nil")
	}
	if len(svc.Spec.Ports) == 0 {
		return nil, fmt.Errorf("service has no ports")
	}

	owner := model.Owner{Kind: "Service", Namespace: svc.Namespace, Name: svc.Name, UID: string(svc.UID)}
	// Provider placement fields such as FlavorID, NetworkID, VPCID, and VPCName
	// are applied by the controller after model construction. They are Shoot-level
	// defaults and are intentionally not derived from the Kubernetes Service here.
	stack := &model.LoadBalancerStack{
		Owner:     owner,
		Ownership: model.OwnershipFor(owner, "", "stack", ""),
		Config:    cfg,
		LBService: model.LBService{
			Name:        LBServiceName(svc),
			Description: fmt.Sprintf("App LB for %s/%s", svc.Namespace, svc.Name),
			Ownership:   model.OwnershipFor(owner, "", "lb-service", VIPGroup(svc)),
		},
		VIP: model.VIP{
			Name:      VIPName(svc),
			Ownership: model.OwnershipFor(owner, "", "vip", VIPGroup(svc)),
		},
	}

	for _, p := range svc.Spec.Ports {
		if p.Port == 0 {
			return nil, fmt.Errorf("InvalidServicePort: service port %q has no frontend port", p.Name)
		}
		if p.NodePort == 0 {
			return nil, fmt.Errorf("BackendNodePortRequired: service port %q (%d) requires a NodePort backend", p.Name, p.Port)
		}
		proto := MapK8sProtocolToCMP(p.Protocol, p.Port)
		if cfg.ProtocolOverride != "" {
			proto = cfg.ProtocolOverride
		}
		sp := model.ServicePort{Name: p.Name, FrontendPort: p.Port, NodePort: p.NodePort, Protocol: proto}
		vs := model.VirtualServer{
			Name:             VirtualServerName(svc, p.Port),
			FrontendPort:     p.Port,
			BackendNodePort:  p.NodePort,
			Protocol:         proto,
			RoutingAlgorithm: cfg.RoutingAlgorithm,
			PersistenceType:  cfg.PersistenceType,
			DrainingTimeout:  cfg.DrainingTimeout,
			SourceRanges:     append([]string(nil), cfg.SourceRanges...),
			Monitor:          &model.Monitor{Name: safeName("mon-" + PoolName(svc, p.Port)), Type: cfg.HealthType, Path: cfg.HealthPath, Interval: cfg.HealthInterval},
			Ownership:        model.OwnershipFor(owner, "", "virtual-server", ""),
		}
		pool := model.Pool{Name: PoolName(svc, p.Port), Monitor: vs.Monitor, Ownership: model.OwnershipFor(owner, "", "pool", VIPGroup(svc))}
		for _, n := range backendsForPort(p) {
			member := model.BackendMember{IP: n.IP, Port: p.NodePort, Weight: n.Weight}
			sp.Backends = append(sp.Backends, member)
			pool.Members = append(pool.Members, member)
		}
		vs.DefaultPoolName = pool.Name
		stack.VirtualServers = append(stack.VirtualServers, vs)
		stack.Pools = append(stack.Pools, pool)
		stack.Ports = append(stack.Ports, sp)
	}
	return stack, nil
}

// VIPGroup returns the optional shared frontend group configured on the Service.
func VIPGroup(svc *corev1.Service) string {
	if svc == nil || svc.Annotations == nil {
		return ""
	}
	return strings.TrimSpace(svc.Annotations[lbannotations.VIPGroup])
}

func VIPName(svc *corev1.Service) string {
	if svc == nil {
		return ""
	}

	if group := VIPGroup(svc); group != "" {
		return limitedSafeName(
			fmt.Sprintf("app-vip-group-%s-%s", svc.Namespace, group),
			maxLBServiceNameLength,
		)
	}

	return limitedSafeName(
		fmt.Sprintf("app-vip-%s-%s", svc.Namespace, svc.Name),
		maxLBServiceNameLength,
	)
}

func VirtualServerName(svc *corev1.Service, frontendPort int32) string {
	if svc == nil {
		return ""
	}
	return safeName(fmt.Sprintf("app-vs-%s-%s-%d", svc.Namespace, svc.Name, frontendPort))
}

func PoolName(svc *corev1.Service, frontendPort int32) string {
	if svc == nil {
		return ""
	}
	return safeName(fmt.Sprintf("app-pool-%s-%s-%d", svc.Namespace, svc.Name, frontendPort))
}

const maxLBServiceNameLength = 20

// LBServiceName deterministically identifies the parent LBService. Services in
// the same VIP group deliberately share this parent, while all child resources
// remain owner-specific.
func LBServiceName(svc *corev1.Service) string {
	if svc == nil {
		return ""
	}

	var name string

	if group := VIPGroup(svc); group != "" {
		// Services in the same VIP group must intentionally produce
		// the same LBService name.
		name = fmt.Sprintf(
			"app-group-%s-%s",
			svc.Namespace,
			group,
		)
	} else {
		// Dedicated LBService per Kubernetes Service.
		name = fmt.Sprintf(
			"app-%s-%s",
			svc.Namespace,
			svc.Name,
		)
	}

	return limitedSafeName(name, maxLBServiceNameLength)
}

func limitedSafeName(name string, maxLength int) string {
	name = safeName(name)

	if len(name) <= maxLength {
		return name
	}

	sum := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(sum[:])[:6]

	// Reserve one character for "-".
	prefixLength := maxLength - len(hash) - 1
	prefix := strings.Trim(name[:prefixLength], "-")

	return prefix + "-" + hash
}

func safeName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))

	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') ||
			(r >= '0' && r <= '9') ||
			r == '-' {
			return r
		}
		return '-'
	}, name)

	return strings.Trim(name, "-")
}
