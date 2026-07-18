package lbaas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type Deployer struct {
	client Client
	vpcID  string

	lbServices     *LBServiceManager
	vips           *VIPManager
	virtualServers *VirtualServerManager
	pools          *PoolManager
	monitors       *MonitorManager
	routingRules   *RoutingRuleManager
}

func New(client Client, vpcID string) *Deployer {
	vpcID = strings.TrimSpace(vpcID)
	return &Deployer{
		client:         client,
		vpcID:          vpcID,
		lbServices:     NewLBServiceManager(client, vpcID),
		vips:           NewVIPManager(client),
		virtualServers: NewVirtualServerManager(client, vpcID),
	}
}

type EnsureRequest struct {
	LBName        string
	LBDescription string
	FlavorID      int32
	NetworkID     string
	VPCID         string
	VPCName       string

	VirtualServer model.VirtualServer
	Backends      []model.BackendMember
	Current       model.ObservedState
	CurrentHash   string

	// RecreateWhenHashMissing enables controller-specific convergence for mutable
	// backend membership. Service LB reconciliation sets this when backend changes
	// must recreate a CMP virtual server. Seed and Ingress paths keep this false so
	// annotated existing virtual servers are reused, matching their historical
	// finalizer-safe behavior until a richer update API exists.
	RecreateWhenHashMissing bool
}

type EnsureResult struct {
	Observed    model.ObservedState
	BackendHash string
	Changed     bool
}

func (d *Deployer) Ensure(ctx context.Context, req EnsureRequest) (*EnsureResult, error) {
	result := &EnsureResult{Observed: req.Current}
	result.Observed.EnsureGraph()
	result.BackendHash = DesiredBackendHash(req.VirtualServer.FrontendPort, req.VirtualServer.BackendNodePort, req.Backends)

	lbID, changed, err := d.lbServices.Ensure(ctx, req, result.Observed.LBServiceID)
	if err != nil {
		return nil, err
	}
	result.Observed.LBServiceID = lbID
	result.Observed.Graph.LBServices[req.LBName] = model.ObservedResource{LogicalID: req.LBName, ExternalID: lbID, Name: req.LBName, Ownership: req.VirtualServer.Ownership}
	result.Changed = result.Changed || changed

	vipID, vipAddress, changed, err := d.vips.Ensure(ctx, result.Observed.LBServiceID, result.Observed.VIPPortID, result.Observed.VIPAddress)
	if err != nil {
		return result, err
	}
	result.Observed.VIPPortID = vipID
	result.Observed.VIPAddress = vipAddress
	result.Observed.Graph.VIPs["vip/"+vipID] = model.ObservedResource{LogicalID: "vip/" + vipID, ExternalID: vipID, Address: vipAddress, Ownership: req.VirtualServer.Ownership}
	result.Changed = result.Changed || changed

	vsID, vsName, changed, err := d.virtualServers.Ensure(ctx, VirtualServerEnsureRequest{
		LBServiceID:             result.Observed.LBServiceID,
		VIPPortID:               result.Observed.VIPPortID,
		Desired:                 req.VirtualServer,
		Backends:                req.Backends,
		CurrentID:               result.Observed.VirtualServerID,
		CurrentHash:             req.CurrentHash,
		DesiredHash:             result.BackendHash,
		RecreateWhenHashMissing: req.RecreateWhenHashMissing,
	})
	if err != nil {
		return result, err
	}
	result.Observed.VirtualServerID = vsID
	result.Observed.VirtualServerName = vsName
	result.Observed.Graph.VirtualServers[req.VirtualServer.Name] = model.ObservedResource{LogicalID: req.VirtualServer.Name, ExternalID: vsID, Name: vsName, Ownership: req.VirtualServer.Ownership}
	result.Changed = result.Changed || changed
	return result, nil
}

func DesiredBackendHash(frontendPort, nodePort int32, backends []model.BackendMember) string {
	sorted := make([]model.BackendMember, len(backends))
	copy(sorted, backends)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].IP < sorted[j].IP })
	b := strings.Builder{}
	b.WriteString(fmt.Sprintf("frontend=%d;nodeport=%d;", frontendPort, nodePort))
	for _, n := range sorted {
		b.WriteString(fmt.Sprintf("%s:%d;", n.IP, n.Weight))
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])
}

// StackEnsureRequest is the complete desired state used by the stack deployer.
// Current is persisted provider state from the previous reconciliation.
type StackEnsureRequest struct {
	Stack   *model.LoadBalancerStack
	Current model.ObservedState
}

// StackEnsureResult is the graph returned after all supported resource managers
// have reconciled the desired stack.
type StackEnsureResult struct {
	Observed model.ObservedState
	Changed  bool
}

// NewWithResourceManagers constructs a stack deployer. Pool, monitor, and rule
// clients are explicit because CMP installations expose these capabilities
// independently from the legacy LB/VIP/virtual-server client.
func NewWithResourceManagers(client Client, vpcID string, pools PoolClient, monitors MonitorClient, rules RoutingRuleClient) *Deployer {
	d := New(client, vpcID)
	if pools != nil {
		d.pools = NewPoolManager(pools)
	}
	if monitors != nil {
		d.monitors = NewMonitorManager(monitors)
	}
	if rules != nil {
		d.routingRules = NewRoutingRuleManager(rules)
	}
	return d
}

// EnsureStack reconciles LB service, VIP, virtual servers, pools, monitors,
// members, and routing rules in dependency order. Certificates require a CMP
// certificate API and are deliberately rejected until a CertificateManager is
// supplied instead of silently provisioning an incomplete HTTPS stack.
func (d *Deployer) EnsureStack(ctx context.Context, req StackEnsureRequest) (*StackEnsureResult, error) {
	if req.Stack == nil {
		return nil, fmt.Errorf("load-balancer stack must not be nil")
	}
	if len(req.Stack.VirtualServers) == 0 {
		return nil, fmt.Errorf("load-balancer stack has no virtual servers")
	}
	if len(req.Stack.Certificates) != 0 {
		return nil, fmt.Errorf("certificate reconciliation requires a CertificateManager")
	}
	if len(req.Stack.Pools) != 0 && d.pools == nil {
		return nil, fmt.Errorf("pool reconciliation requires a PoolManager")
	}
	if len(req.Stack.RoutingRules) != 0 && d.routingRules == nil {
		return nil, fmt.Errorf("routing-rule reconciliation requires a RoutingRuleManager")
	}
	for _, pool := range req.Stack.Pools {
		if pool.Monitor != nil && d.monitors == nil {
			return nil, fmt.Errorf("monitor reconciliation requires a MonitorManager")
		}
	}

	observed := req.Current
	observed.EnsureGraph()
	lbReq := EnsureRequest{
		LBName: req.Stack.LBService.Name, LBDescription: req.Stack.LBService.Description,
		FlavorID: req.Stack.LBService.FlavorID, NetworkID: req.Stack.LBService.NetworkID,
		VPCID: req.Stack.LBService.VPCID, VPCName: req.Stack.LBService.VPCName,
		VirtualServer: req.Stack.VirtualServers[0], Current: observed,
	}
	lbID, changed, err := d.lbServices.Ensure(ctx, lbReq, observed.LBServiceID)
	if err != nil {
		return nil, err
	}
	observed.LBServiceID = lbID
	observed.Graph.LBServices[req.Stack.LBService.Name] = model.ObservedResource{LogicalID: req.Stack.LBService.Name, ExternalID: lbID, Name: req.Stack.LBService.Name, Ownership: req.Stack.LBService.Ownership}

	vipID, vipAddress, vipChanged, err := d.vips.Ensure(ctx, lbID, observed.VIPPortID, observed.VIPAddress)
	if err != nil {
		return nil, err
	}
	observed.VIPPortID, observed.VIPAddress = vipID, vipAddress
	observed.Graph.VIPs[req.Stack.VIP.Name] = model.ObservedResource{LogicalID: req.Stack.VIP.Name, ExternalID: vipID, Name: req.Stack.VIP.Name, Address: vipAddress, Ownership: req.Stack.VIP.Ownership}
	changed = changed || vipChanged

	for _, vs := range req.Stack.VirtualServers {
		backends := stackBackends(req.Stack, vs)
		currentID := observed.Graph.VirtualServers[vs.Name].ExternalID
		if currentID == "" && len(req.Stack.VirtualServers) == 1 {
			currentID = observed.VirtualServerID
		}
		backendHash := DesiredBackendHash(vs.FrontendPort, vs.BackendNodePort, backends)
		vsID, vsName, vsChanged, err := d.virtualServers.Ensure(ctx, VirtualServerEnsureRequest{LBServiceID: lbID, VIPPortID: vipID, Desired: vs, Backends: backends, CurrentID: currentID, DesiredHash: backendHash})
		if err != nil {
			return nil, err
		}
		observed.Graph.VirtualServers[vs.Name] = model.ObservedResource{LogicalID: vs.Name, ExternalID: vsID, Name: vsName, Ownership: vs.Ownership}
		if len(req.Stack.VirtualServers) == 1 {
			observed.VirtualServerID, observed.VirtualServerName = vsID, vsName
		}
		changed = changed || vsChanged

		poolIDs := map[string]string{}
		for _, pool := range poolsForVirtualServer(req.Stack, vs) {
			memberSpecs, err := d.virtualServers.poolMemberSpecs(ctx, pool.Members)
			if err != nil {
				return nil, err
			}
			resource, poolChanged, err := d.pools.Ensure(ctx, lbID, vsID, pool, memberSpecs, pool.Name == vs.DefaultPoolName)
			if err != nil {
				return nil, err
			}
			poolIDs[pool.Name] = resource.ID
			observed.Graph.Pools[vs.Name+"/"+pool.Name] = model.ObservedResource{LogicalID: vs.Name + "/" + pool.Name, ExternalID: resource.ID, Name: resource.Name, Ownership: pool.Ownership}
			for _, member := range resource.Members {
				key := vs.Name + "/" + pool.Name + "/" + poolMemberKey(member.ResourceID, member.ResourceIP, member.BackendPortID, member.Port)
				observed.Graph.Members[key] = model.ObservedResource{
					LogicalID: key, ExternalID: member.ID,
					Name: member.ResourceIP + ":" + fmt.Sprintf("%d", member.Port), Ownership: pool.Ownership,
				}
			}
			changed = changed || poolChanged
			if d.monitors != nil {
				monitor, monitorChanged, err := d.monitors.Ensure(ctx, lbID, vsID, resource.ID, pool.Monitor)
				if err != nil {
					return nil, err
				}
				if monitor.ID != "" {
					observed.Graph.Monitors[vs.Name+"/"+pool.Name] = model.ObservedResource{LogicalID: vs.Name + "/" + pool.Name, ExternalID: monitor.ID, Name: monitor.Name, Ownership: pool.Ownership}
				}
				changed = changed || monitorChanged
			}
		}
		if d.routingRules != nil {
			desiredRules := make([]RoutingRuleSpec, 0)
			for _, rule := range req.Stack.RoutingRules {
				poolID, ok := poolIDs[rule.PoolName]
				if !ok {
					return nil, fmt.Errorf("routing rule %q references pool %q that is not attached to virtual server %q", rule.Name, rule.PoolName, vs.Name)
				}
				desiredRules = append(desiredRules, RoutingRuleSpec{Host: rule.Host, Path: rule.Path, MatchType: rule.MatchType, PoolID: poolID})
			}
			rules, rulesChanged, err := d.routingRules.Ensure(ctx, lbID, vsID, desiredRules)
			if err != nil {
				return nil, err
			}
			for _, rule := range rules {
				observed.Graph.RoutingRules[routingRuleKey(rule.Host, rule.Path, rule.MatchType, rule.PoolID)] = model.ObservedResource{LogicalID: routingRuleKey(rule.Host, rule.Path, rule.MatchType, rule.PoolID), ExternalID: rule.ID, Ownership: vs.Ownership}
			}
			changed = changed || rulesChanged
		}
	}
	return &StackEnsureResult{Observed: observed, Changed: changed}, nil
}

func stackBackends(stack *model.LoadBalancerStack, vs model.VirtualServer) []model.BackendMember {
	for _, pool := range stack.Pools {
		if pool.Name == vs.DefaultPoolName {
			return append([]model.BackendMember(nil), pool.Members...)
		}
	}
	for _, port := range stack.Ports {
		if port.FrontendPort == vs.FrontendPort {
			return append([]model.BackendMember(nil), port.Backends...)
		}
	}
	return nil
}

func poolsForVirtualServer(stack *model.LoadBalancerStack, vs model.VirtualServer) []model.Pool {
	needed := map[string]struct{}{}
	if vs.DefaultPoolName != "" {
		needed[vs.DefaultPoolName] = struct{}{}
	}
	for _, rule := range stack.RoutingRules {
		needed[rule.PoolName] = struct{}{}
	}
	out := make([]model.Pool, 0, len(needed))
	for _, pool := range stack.Pools {
		if _, ok := needed[pool.Name]; ok {
			out = append(out, pool)
		}
	}
	return out
}
