package lbaas

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
)

type RawClient interface {
	ListLBServices(ctx context.Context, opts *f5client.ListLoadBalancersOptions) ([]json.RawMessage, error)
	CreateLBService(ctx context.Context, form url.Values) (json.RawMessage, error)
	DeleteLBService(ctx context.Context, id string) error
	CreateLBServiceVIP(ctx context.Context, lbServiceID string) (json.RawMessage, error)
	GetLBServiceVIPs(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	DeleteLBServiceVIP(ctx context.Context, lbServiceID, vipID string) error
	ListLBVirtualServers(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	CreateLBVirtualServer(ctx context.Context, lbServiceID string, query url.Values) (json.RawMessage, error)
	DeleteLBVirtualServer(ctx context.Context, lbServiceID, vsID string) error
	SearchNetworkPortsByIP(ctx context.Context, ip string) ([]json.RawMessage, error)
}

type rawAdapter struct{ raw RawClient }

func NewRawClient(raw RawClient) Client { return rawAdapter{raw: raw} }

// NewFromRaw enables every provider capability implemented by the underlying
// client. This keeps callers on the stack deployer path from silently losing
// pool/member reconciliation when they construct a deployer from the legacy
// raw CMP client.
func NewFromRaw(raw RawClient, vpcID string) *Deployer {
	d := New(NewRawClient(raw), vpcID)
	if pools, ok := raw.(RawPoolClient); ok {
		d.pools = NewPoolManager(NewPoolClientFromRaw(pools))
	}
	if monitors, ok := raw.(RawMonitorClient); ok {
		d.monitors = NewMonitorManager(rawMonitorAdapter{raw: monitors})
	}
	if rules, ok := raw.(RawRoutingRuleClient); ok {
		d.routingRules = NewRoutingRuleManager(rawRoutingRuleAdapter{raw: rules})
	}
	return d
}

func (a rawAdapter) ListLBServices(ctx context.Context) ([]LBService, error) {
	items, err := a.raw.ListLBServices(ctx, nil)
	if err != nil {
		return nil, err
	}
	out := make([]LBService, 0, len(items))
	for _, raw := range items {
		var svc struct{ ID, Name string }
		if json.Unmarshal(raw, &svc) == nil {
			out = append(out, LBService{ID: strings.TrimSpace(svc.ID), Name: strings.TrimSpace(svc.Name)})
		}
	}
	return out, nil
}

func (a rawAdapter) CreateLBService(ctx context.Context, spec LBServiceSpec) (LBService, error) {
	form := url.Values{}
	form.Set("name", spec.Name)
	form.Set("description", spec.Description)
	if spec.FlavorID != 0 {
		form.Set("flavor_id", fmt.Sprintf("%d", spec.FlavorID))
	}
	if spec.NetworkID != "" {
		form.Set("network_id", spec.NetworkID)
	}
	if spec.VPCID != "" {
		form.Set("vpc_id", spec.VPCID)
	}
	if spec.VPCName != "" {
		form.Set("vpc_name", spec.VPCName)
	}
	raw, err := a.raw.CreateLBService(ctx, form)
	if err != nil {
		return LBService{}, err
	}
	var created struct{ ID, Name string }
	if err := json.Unmarshal(raw, &created); err != nil || strings.TrimSpace(created.ID) == "" {
		return LBService{}, fmt.Errorf("parsing LB Service response: %s", string(raw))
	}
	return LBService{ID: strings.TrimSpace(created.ID), Name: strings.TrimSpace(created.Name)}, nil
}
func (a rawAdapter) DeleteLBService(ctx context.Context, id string) error {
	return a.raw.DeleteLBService(ctx, id)
}

func (a rawAdapter) ListVIPs(ctx context.Context, lbServiceID string) ([]VIP, error) {
	items, err := a.raw.GetLBServiceVIPs(ctx, lbServiceID)
	if err != nil {
		return nil, err
	}
	out := make([]VIP, 0, len(items))
	for _, raw := range items {
		if vip := parseVIP(raw); vip.ID != "" {
			out = append(out, vip)
		}
	}
	return out, nil
}
func (a rawAdapter) CreateVIP(ctx context.Context, lbServiceID string) (VIP, error) {
	raw, err := a.raw.CreateLBServiceVIP(ctx, lbServiceID)
	if err != nil {
		return VIP{}, err
	}
	vip := parseVIP(raw)
	if vip.ID == "" {
		return VIP{}, fmt.Errorf("VIP created but no ID returned: %s", string(raw))
	}
	return vip, nil
}
func (a rawAdapter) DeleteVIP(ctx context.Context, lbServiceID, vipID string) error {
	return a.raw.DeleteLBServiceVIP(ctx, lbServiceID, vipID)
}

func (a rawAdapter) ListVirtualServers(ctx context.Context, lbServiceID string) ([]VirtualServer, error) {
	items, err := a.raw.ListLBVirtualServers(ctx, lbServiceID)
	if err != nil {
		return nil, err
	}
	out := make([]VirtualServer, 0, len(items))
	for _, raw := range items {
		var vs struct{ ID, Name string }
		if json.Unmarshal(raw, &vs) == nil {
			out = append(out, VirtualServer{ID: strings.TrimSpace(vs.ID), Name: strings.TrimSpace(vs.Name)})
		}
	}
	return out, nil
}
func (a rawAdapter) CreateVirtualServer(ctx context.Context, lbServiceID string, spec VirtualServerSpec) (VirtualServer, error) {
	q := url.Values{}
	q.Set("name", spec.Name)
	q.Set("vip_port_id", spec.VIPPortID)
	q.Set("protocol", spec.Protocol)
	q.Set("port", fmt.Sprintf("%d", spec.Port))
	q.Set("routing_algorithm", spec.RoutingAlgorithm)
	if spec.MonitorInterval != 0 {
		q.Set("interval", fmt.Sprintf("%d", spec.MonitorInterval))
	}
	if spec.MonitorType != "" && spec.MonitorType != "tcp" {
		q.Set("monitor_type", spec.MonitorType)
	}
	if spec.MonitorPath != "" {
		q.Set("monitor_path", spec.MonitorPath)
	}
	if spec.PersistenceType != "" {
		q.Set("persistence_type", spec.PersistenceType)
	}
	if spec.DrainingTimeout > 0 {
		q.Set("connection_draining_timeout", fmt.Sprintf("%d", spec.DrainingTimeout))
	}
	if spec.VPCID != "" {
		q.Set("vpc_id", spec.VPCID)
	}
	if len(spec.AllowedCIDRs) > 0 {
		q.Set("allowed_cidrs", strings.Join(spec.AllowedCIDRs, ","))
	}
	for _, node := range spec.Nodes {
		resourceType := strings.TrimSpace(node.ResourceType)
		if resourceType == "" {
			resourceType = "compute"
		}
		b, _ := json.Marshal(map[string]interface{}{
			"resource_id":     node.ResourceID,
			"resource_type":   resourceType,
			"resource_ip":     node.ResourceIP,
			"backend_port_id": node.BackendPortID,
			"port":            node.Port,
			"weight":          node.Weight,
		})
		q.Add("nodes", string(b))
	}
	raw, err := a.raw.CreateLBVirtualServer(ctx, lbServiceID, q)
	if err != nil {
		return VirtualServer{}, err
	}
	var created struct{ ID, Name string }
	if json.Unmarshal(raw, &created) == nil {
		return VirtualServer{ID: strings.TrimSpace(created.ID), Name: strings.TrimSpace(created.Name)}, nil
	}
	return VirtualServer{Name: spec.Name}, nil
}
func (a rawAdapter) DeleteVirtualServer(ctx context.Context, lbServiceID, vsID string) error {
	return a.raw.DeleteLBVirtualServer(ctx, lbServiceID, vsID)
}

func (a rawAdapter) SearchNetworkPortsByIP(ctx context.Context, ip string) ([]NetworkPort, error) {
	items, err := a.raw.SearchNetworkPortsByIP(ctx, ip)
	if err != nil {
		return nil, err
	}
	out := make([]NetworkPort, 0, len(items))
	for _, raw := range items {
		if port := parseNetworkPort(raw); port.ID != 0 {
			out = append(out, port)
		}
	}
	return out, nil
}

type RawPoolClient interface {
	ListLBVirtualServerPools(ctx context.Context, lbServiceID, vsID string) ([]json.RawMessage, error)
	CreateLBVirtualServerPool(ctx context.Context, lbServiceID, virtualServerID string, query url.Values) (json.RawMessage, error)
	GetLBVirtualServerPool(ctx context.Context, lbServiceID, virtualServerID, poolID string) (json.RawMessage, error)
	DeleteLBVirtualServerPool(ctx context.Context, lbServiceID, virtualServerID, poolID string) error
	SetDefaultLBVirtualServerPool(ctx context.Context, lbServiceID, virtualServerID, poolID string) error
	CreateLBVirtualServerPoolMember(ctx context.Context, lbServiceID, virtualServerID, poolID string, query url.Values) (json.RawMessage, error)
	UpdateLBVirtualServerPoolMember(ctx context.Context, lbServiceID, virtualServerID, poolID, memberID string, query url.Values) (json.RawMessage, error)
	DeleteLBVirtualServerPoolMember(ctx context.Context, lbServiceID, virtualServerID, poolID, memberID string) error
}

type rawPoolAdapter struct{ raw RawPoolClient }

func NewPoolClientFromRaw(raw RawPoolClient) PoolClient { return rawPoolAdapter{raw: raw} }

func (a rawPoolAdapter) ListPools(ctx context.Context, lbServiceID, virtualServerID string) ([]PoolResource, error) {
	items, err := a.raw.ListLBVirtualServerPools(ctx, lbServiceID, virtualServerID)
	if err != nil {
		return nil, err
	}
	out := make([]PoolResource, 0, len(items))
	for _, item := range items {
		if pool := parsePool(item); strings.TrimSpace(pool.ID) != "" {
			out = append(out, pool)
		}
	}
	return out, nil
}

func (a rawPoolAdapter) CreatePool(ctx context.Context, lbServiceID, virtualServerID string, spec PoolSpec) (PoolResource, error) {
	q := poolSpecQuery(spec)
	raw, err := a.raw.CreateLBVirtualServerPool(ctx, lbServiceID, virtualServerID, q)
	if err != nil {
		return PoolResource{}, err
	}
	return parsePool(raw), nil
}

func (a rawPoolAdapter) GetPool(ctx context.Context, lbServiceID, virtualServerID, poolID string) (PoolResource, error) {
	raw, err := a.raw.GetLBVirtualServerPool(ctx, lbServiceID, virtualServerID, poolID)
	if err != nil {
		return PoolResource{}, err
	}
	return parsePool(raw), nil
}

func (a rawPoolAdapter) DeletePool(ctx context.Context, lbServiceID, virtualServerID, poolID string) error {
	return a.raw.DeleteLBVirtualServerPool(ctx, lbServiceID, virtualServerID, poolID)
}

func (a rawPoolAdapter) SetDefaultPool(ctx context.Context, lbServiceID, virtualServerID, poolID string) error {
	return a.raw.SetDefaultLBVirtualServerPool(ctx, lbServiceID, virtualServerID, poolID)
}

func (a rawPoolAdapter) CreatePoolMember(ctx context.Context, lbServiceID, virtualServerID, poolID string, spec PoolMemberSpec) (PoolMemberResource, error) {
	raw, err := a.raw.CreateLBVirtualServerPoolMember(ctx, lbServiceID, virtualServerID, poolID, poolMemberSpecQuery(spec))
	if err != nil {
		return PoolMemberResource{}, err
	}
	return parsePoolMember(raw), nil
}

func (a rawPoolAdapter) UpdatePoolMember(ctx context.Context, lbServiceID, virtualServerID, poolID, memberID string, spec PoolMemberSpec) (PoolMemberResource, error) {
	raw, err := a.raw.UpdateLBVirtualServerPoolMember(ctx, lbServiceID, virtualServerID, poolID, memberID, poolMemberSpecQuery(spec))
	if err != nil {
		return PoolMemberResource{}, err
	}
	return parsePoolMember(raw), nil
}

func (a rawPoolAdapter) DeletePoolMember(ctx context.Context, lbServiceID, virtualServerID, poolID, memberID string) error {
	return a.raw.DeleteLBVirtualServerPoolMember(ctx, lbServiceID, virtualServerID, poolID, memberID)
}

func poolSpecQuery(spec PoolSpec) url.Values {
	q := url.Values{}
	q.Set("pool_name", spec.Name)
	if spec.Protocol != "" {
		q.Set("pool_members_protocol", spec.Protocol)
	}
	if spec.RoutingAlgorithm != "" {
		q.Set("routing_algorithm", spec.RoutingAlgorithm)
	}
	if spec.Monitor != nil {
		if spec.Monitor.Name != "" {
			q.Set("monitor_name", spec.Monitor.Name)
		}
		if spec.Monitor.Type != "" {
			q.Set("monitor_protocol", spec.Monitor.Type)
		}
		if spec.Monitor.Path != "" {
			q.Set("monitor_path", spec.Monitor.Path)
		}
		if spec.Monitor.Interval > 0 {
			q.Set("interval", fmt.Sprintf("%d", spec.Monitor.Interval))
		}
	}
	for _, member := range spec.Members {
		b, _ := json.Marshal(poolMemberPayload(member))
		q.Add("nodes", string(b))
	}
	return q
}

func poolMemberSpecQuery(spec PoolMemberSpec) url.Values {
	q := url.Values{}
	b, _ := json.Marshal(poolMemberPayload(spec))
	q.Set("node", string(b))
	return q
}

func poolMemberPayload(spec PoolMemberSpec) map[string]interface{} {
	resourceType := strings.TrimSpace(spec.ResourceType)
	if resourceType == "" {
		resourceType = "compute"
	}
	return map[string]interface{}{
		"resource_id":     spec.ResourceID,
		"resource_type":   resourceType,
		"resource_ip":     spec.ResourceIP,
		"backend_port_id": spec.BackendPortID,
		"port":            spec.Port,
		"weight":          spec.Weight,
	}
}

func parsePool(raw json.RawMessage) PoolResource {
	var p struct {
		ID        string            `json:"id"`
		Name      string            `json:"pool_name"`
		AltName   string            `json:"name"`
		IsDefault bool              `json:"is_default"`
		Members   []json.RawMessage `json:"members"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return PoolResource{}
	}
	name := firstNonEmpty(p.Name, p.AltName)
	res := PoolResource{ID: strings.TrimSpace(p.ID), Name: name, IsDefault: p.IsDefault}
	for _, member := range p.Members {
		res.Members = append(res.Members, parsePoolMember(member))
	}
	return res
}

func parsePoolMember(raw json.RawMessage) PoolMemberResource {
	var m struct {
		ID            string `json:"id"`
		ResourceID    string `json:"resource_id"`
		ResourceType  string `json:"resource_type"`
		ResourceIP    string `json:"resource_ip"`
		BackendPortID int    `json:"backend_port_id"`
		Port          int32  `json:"port"`
		Weight        int    `json:"weight"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return PoolMemberResource{}
	}
	return PoolMemberResource{ID: strings.TrimSpace(m.ID), ResourceID: strings.TrimSpace(m.ResourceID), ResourceType: strings.TrimSpace(m.ResourceType), ResourceIP: strings.TrimSpace(m.ResourceIP), BackendPortID: m.BackendPortID, Port: m.Port, Weight: m.Weight}
}

func parseVIP(raw json.RawMessage) VIP {
	var vip struct {
		ID      string `json:"id"`
		IDStr   string `json:"id_str"`
		Address string `json:"ip_address"`
		AltAddr string `json:"address"`
	}
	if json.Unmarshal(raw, &vip) == nil {
		id := strings.TrimSpace(vip.ID)
		if id == "" {
			id = strings.TrimSpace(vip.IDStr)
		}
		addr := strings.TrimSpace(vip.Address)
		if addr == "" {
			addr = strings.TrimSpace(vip.AltAddr)
		}
		if id != "" {
			return VIP{ID: id, Address: addr}
		}
	}
	var numeric struct {
		ID      int    `json:"id"`
		Address string `json:"ip_address"`
		AltAddr string `json:"address"`
	}
	if json.Unmarshal(raw, &numeric) == nil && numeric.ID != 0 {
		addr := strings.TrimSpace(numeric.Address)
		if addr == "" {
			addr = strings.TrimSpace(numeric.AltAddr)
		}
		return VIP{ID: fmt.Sprintf("%d", numeric.ID), Address: addr}
	}
	return VIP{}
}

func parseNetworkPort(raw json.RawMessage) NetworkPort {
	var port struct {
		ID           int    `json:"id"`
		IDString     string `json:"id_str"`
		UUID         string `json:"uuid"`
		ResourceID   string `json:"resource_id"`
		DeviceID     string `json:"device_id"`
		ComputeID    string `json:"compute_id"`
		ResourceType string `json:"resource_type"`
		DeviceOwner  string `json:"device_owner"`
		FixedIP      string `json:"fixed_ip"`
		IPAddress    string `json:"ip_address"`
		IP           string `json:"ip"`
		FixedIPs     []struct {
			IPAddress string `json:"ip_address"`
		} `json:"fixed_ips"`
	}
	if json.Unmarshal(raw, &port) != nil {
		return NetworkPort{}
	}
	id := port.ID
	if id == 0 && strings.TrimSpace(port.IDString) != "" {
		if _, err := fmt.Sscanf(strings.TrimSpace(port.IDString), "%d", &id); err != nil {
			id = 0
		}
	}
	resourceID := firstNonEmpty(port.ResourceID, port.DeviceID, port.ComputeID, port.UUID)
	resourceType := strings.TrimSpace(port.ResourceType)
	if resourceType == "" {
		resourceType = inferResourceType(port.DeviceOwner)
	}
	ip := firstNonEmpty(port.FixedIP, port.IPAddress, port.IP)
	if ip == "" && len(port.FixedIPs) > 0 {
		ip = strings.TrimSpace(port.FixedIPs[0].IPAddress)
	}
	return NetworkPort{ID: id, ResourceID: resourceID, ResourceType: resourceType, IP: ip}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func inferResourceType(deviceOwner string) string {
	owner := strings.ToLower(strings.TrimSpace(deviceOwner))
	if strings.Contains(owner, "baremetal") {
		return "baremetal"
	}
	return "compute"
}

// RawMonitorClient exposes the CMP pool-monitor endpoints. It is optional for
// backwards-compatible clients, but NewFromRaw wires it whenever available.
type RawMonitorClient interface {
	ListLBVirtualServerPoolMonitors(context.Context, string, string, string) ([]json.RawMessage, error)
	CreateLBVirtualServerPoolMonitor(context.Context, string, string, string, url.Values) (json.RawMessage, error)
	UpdateLBVirtualServerPoolMonitor(context.Context, string, string, string, string, url.Values) (json.RawMessage, error)
	DeleteLBVirtualServerPoolMonitor(context.Context, string, string, string, string) error
}
type rawMonitorAdapter struct{ raw RawMonitorClient }

func (a rawMonitorAdapter) ListMonitors(ctx context.Context, lb, vs, pool string) ([]MonitorResource, error) {
	items, err := a.raw.ListLBVirtualServerPoolMonitors(ctx, lb, vs, pool)
	if err != nil {
		return nil, err
	}
	out := make([]MonitorResource, 0, len(items))
	for _, item := range items {
		if m := parseMonitor(item); m.ID != "" {
			out = append(out, m)
		}
	}
	return out, nil
}
func (a rawMonitorAdapter) CreateMonitor(ctx context.Context, lb, vs, pool string, spec MonitorSpec) (MonitorResource, error) {
	raw, err := a.raw.CreateLBVirtualServerPoolMonitor(ctx, lb, vs, pool, monitorSpecQuery(spec))
	if err != nil {
		return MonitorResource{}, err
	}
	return parseMonitor(raw), nil
}
func (a rawMonitorAdapter) UpdateMonitor(ctx context.Context, lb, vs, pool, id string, spec MonitorSpec) (MonitorResource, error) {
	raw, err := a.raw.UpdateLBVirtualServerPoolMonitor(ctx, lb, vs, pool, id, monitorSpecQuery(spec))
	if err != nil {
		return MonitorResource{}, err
	}
	return parseMonitor(raw), nil
}
func (a rawMonitorAdapter) DeleteMonitor(ctx context.Context, lb, vs, pool, id string) error {
	return a.raw.DeleteLBVirtualServerPoolMonitor(ctx, lb, vs, pool, id)
}
func monitorSpecQuery(s MonitorSpec) url.Values {
	q := url.Values{}
	q.Set("monitor_name", s.Name)
	q.Set("monitor_protocol", s.Protocol)
	q.Set("monitor_path", s.Path)
	if s.Interval > 0 {
		q.Set("interval", fmt.Sprintf("%d", s.Interval))
	}
	return q
}
func parseMonitor(raw json.RawMessage) MonitorResource {
	var m struct {
		ID       string `json:"id"`
		Name     string `json:"monitor_name"`
		AltName  string `json:"name"`
		Protocol string `json:"monitor_protocol"`
		Path     string `json:"monitor_path"`
		Interval int32  `json:"interval"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return MonitorResource{}
	}
	return MonitorResource{ID: strings.TrimSpace(m.ID), Name: firstNonEmpty(m.Name, m.AltName), Protocol: m.Protocol, Path: m.Path, Interval: m.Interval}
}

// RawRoutingRuleClient exposes CMP virtual-server routing-rule endpoints.
type RawRoutingRuleClient interface {
	ListLBVirtualServerRoutingRules(context.Context, string, string) ([]json.RawMessage, error)
	CreateLBVirtualServerRoutingRule(context.Context, string, string, url.Values) (json.RawMessage, error)
	DeleteLBVirtualServerRoutingRule(context.Context, string, string, string) error
}
type rawRoutingRuleAdapter struct{ raw RawRoutingRuleClient }

func (a rawRoutingRuleAdapter) ListRoutingRules(ctx context.Context, lb, vs string) ([]RoutingRuleResource, error) {
	items, err := a.raw.ListLBVirtualServerRoutingRules(ctx, lb, vs)
	if err != nil {
		return nil, err
	}
	out := make([]RoutingRuleResource, 0, len(items))
	for _, item := range items {
		if r := parseRoutingRule(item); r.ID != "" {
			out = append(out, r)
		}
	}
	return out, nil
}
func (a rawRoutingRuleAdapter) CreateRoutingRule(ctx context.Context, lb, vs string, s RoutingRuleSpec) (RoutingRuleResource, error) {
	q := url.Values{}
	q.Set("host", s.Host)
	q.Set("path", s.Path)
	q.Set("match_type", s.MatchType)
	q.Set("pool_id", s.PoolID)
	raw, err := a.raw.CreateLBVirtualServerRoutingRule(ctx, lb, vs, q)
	if err != nil {
		return RoutingRuleResource{}, err
	}
	return parseRoutingRule(raw), nil
}
func (a rawRoutingRuleAdapter) DeleteRoutingRule(ctx context.Context, lb, vs, id string) error {
	return a.raw.DeleteLBVirtualServerRoutingRule(ctx, lb, vs, id)
}
func parseRoutingRule(raw json.RawMessage) RoutingRuleResource {
	var r RoutingRuleResource
	if json.Unmarshal(raw, &r) != nil {
		return RoutingRuleResource{}
	}
	return r
}
