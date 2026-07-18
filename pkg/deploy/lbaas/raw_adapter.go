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

func NewRawClient(raw RawClient) Client                { return rawAdapter{raw: raw} }
func NewFromRaw(raw RawClient, vpcID string) *Deployer { return New(NewRawClient(raw), vpcID) }

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
