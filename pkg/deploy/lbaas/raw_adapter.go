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
	GetLBService(ctx context.Context, id string) (json.RawMessage, error)
	CreateLBServiceVIP(ctx context.Context, lbServiceID, subnetID string) (json.RawMessage, error)
	GetLBServiceVIPs(ctx context.Context, lbServiceID, subnetID string) ([]json.RawMessage, error)
	DeleteLBServiceVIP(ctx context.Context, lbServiceID, vipID string) error
	ListLBVirtualServers(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	GetLBVirtualServer(ctx context.Context, lbServiceID, vsID string) (json.RawMessage, error)
	CreateLBVirtualServer(ctx context.Context, lbServiceID string, query url.Values) (json.RawMessage, error)
	DeleteLBVirtualServer(ctx context.Context, lbServiceID, vsID string) error
	ListLBServiceCertificates(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	CreateLBServiceCertificate(ctx context.Context, lbServiceID string, query url.Values) (json.RawMessage, error)
	DeleteLBServiceCertificate(ctx context.Context, lbServiceID, certificateID string) error
	AttachLBVirtualServerCertificate(ctx context.Context, lbServiceID, virtualServerID, certificateID string) error
	DetachLBVirtualServerCertificate(ctx context.Context, lbServiceID, virtualServerID, certificateID string) error
	GetCompute(ctx context.Context, id string) (json.RawMessage, error)
}

// RawNetworkClient is optional because older CMP clients and test doubles did
// not expose network lookup. Production clients implement it so explicit
// Service subnet overrides can be validated against their VPC before create.
type RawNetworkClient interface {
	GetNetwork(ctx context.Context, id string) (json.RawMessage, error)
}

type rawAdapter struct{ raw RawClient }

func NewRawClient(raw RawClient) Client { return rawAdapter{raw: raw} }

func NewCertificateClientFromRaw(raw RawClient) CertificateClient {
	return rawCertificateAdapter{raw: raw}
}

// NewFromRaw enables every provider capability implemented by the underlying
// client. This keeps callers on the stack deployer path from silently losing
// pool/member reconciliation when they construct a deployer from the legacy
// raw CMP client.
func NewFromRaw(raw RawClient, vpcID string) *Deployer {
	d := New(NewRawClient(raw), vpcID)
	if networks, ok := raw.(RawNetworkClient); ok {
		d.networks = rawNetworkAdapter{raw: networks}
	}
	if pools, ok := raw.(RawPoolClient); ok {
		d.pools = NewPoolManager(NewPoolClientFromRaw(pools))
	}
	if monitors, ok := raw.(RawMonitorClient); ok {
		d.monitors = NewMonitorManager(rawMonitorAdapter{raw: monitors})
	}
	if rules, ok := raw.(RawRoutingRuleClient); ok {
		d.routingRules = NewRoutingRuleManager(rawRoutingRuleAdapter{raw: rules})
	}
	if certificates, ok := raw.(RawClient); ok {
		d.certificates = NewCertificateManager(NewCertificateClientFromRaw(certificates))
	}
	return d
}

type rawNetworkAdapter struct{ raw RawNetworkClient }

func (a rawNetworkAdapter) GetNetwork(ctx context.Context, id string) (Network, error) {
	raw, err := a.raw.GetNetwork(ctx, id)
	if err != nil {
		return Network{}, err
	}
	var network struct {
		ID     string `json:"id"`
		VPCID  string `json:"vpc_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &network); err != nil {
		return Network{}, fmt.Errorf("parsing CMP network %q: %w", id, err)
	}
	if strings.TrimSpace(network.ID) == "" {
		network.ID = strings.TrimSpace(id)
	}
	return Network{ID: strings.TrimSpace(network.ID), VPCID: strings.TrimSpace(network.VPCID), Status: strings.TrimSpace(network.Status)}, nil
}

func (a rawAdapter) ListLBServices(
	ctx context.Context,
	opts *f5client.ListLoadBalancersOptions,
) ([]LBService, error) {

	items, err := a.raw.ListLBServices(ctx, opts)
	if err != nil {
		return nil, err
	}

	out := make([]LBService, 0, len(items))
	for _, raw := range items {
		var svc struct {
			ID              string `json:"id"`
			Name            string `json:"name"`
			Status          string `json:"status"`
			OperatingStatus string `json:"operating_status"`
			VPCID           string `json:"vpc_id"`
			VPCName         string `json:"vpc_name"`
			NetworkID       string `json:"network_id"`
			Region          string `json:"region"`
			VPC             struct {
				Region string `json:"region"`
			} `json:"vpc"`
		}
		if json.Unmarshal(raw, &svc) == nil {
			region := strings.TrimSpace(svc.Region)
			if region == "" {
				region = strings.TrimSpace(svc.VPC.Region)
			}
			out = append(out, LBService{
				ID:              strings.TrimSpace(svc.ID),
				Name:            strings.TrimSpace(svc.Name),
				Status:          strings.TrimSpace(svc.Status),
				OperatingStatus: strings.TrimSpace(svc.OperatingStatus),
				VPCID:           strings.TrimSpace(svc.VPCID), VPCName: strings.TrimSpace(svc.VPCName),
				NetworkID: strings.TrimSpace(svc.NetworkID), Region: region,
			})
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

func (a rawAdapter) GetLBService(
	ctx context.Context, id string) (LBService, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return LBService{}, fmt.Errorf("LBService ID must not be empty")
	}

	raw, err := a.raw.GetLBService(ctx, id)
	if err != nil {
		return LBService{}, err
	}

	var svc struct {
		ID              string `json:"id"`
		Name            string `json:"name"`
		Status          string `json:"status"`
		OperatingStatus string `json:"operating_status"`
		VPCID           string `json:"vpc_id"`
		VPCName         string `json:"vpc_name"`
		NetworkID       string `json:"network_id"`
		Region          string `json:"region"`
		VPC             struct {
			Region string `json:"region"`
		} `json:"vpc"`
	}

	if err := json.Unmarshal(raw, &svc); err != nil {
		return LBService{}, fmt.Errorf(
			"parsing LBService %q response: %w",
			id,
			err,
		)
	}

	svc.ID = strings.TrimSpace(svc.ID)
	svc.Name = strings.TrimSpace(svc.Name)

	if svc.ID == "" {
		return LBService{}, fmt.Errorf(
			"CMP returned LBService %q without an ID",
			id,
		)
	}

	region := strings.TrimSpace(svc.Region)
	if region == "" {
		region = strings.TrimSpace(svc.VPC.Region)
	}
	return LBService{
		ID:              svc.ID,
		Name:            svc.Name,
		Status:          strings.TrimSpace(svc.Status),
		OperatingStatus: strings.TrimSpace(svc.OperatingStatus),
		VPCID:           strings.TrimSpace(svc.VPCID), VPCName: strings.TrimSpace(svc.VPCName),
		NetworkID: strings.TrimSpace(svc.NetworkID), Region: region,
	}, nil
}

func (a rawAdapter) ListVIPs(ctx context.Context, lbServiceID, subnetID string) ([]VIP, error) {
	items, err := a.raw.GetLBServiceVIPs(ctx, lbServiceID, subnetID)
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
func (a rawAdapter) CreateVIP(ctx context.Context, lbServiceID, subnetID string) (VIP, error) {
	raw, err := a.raw.CreateLBServiceVIP(ctx, lbServiceID, subnetID)
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
		if vs := parseVirtualServer(raw); vs.ID != "" {
			out = append(out, vs)
		}
	}
	return out, nil
}
func (a rawAdapter) GetVirtualServer(ctx context.Context, lbServiceID, vsID string) (VirtualServer, error) {
	raw, err := a.raw.GetLBVirtualServer(ctx, lbServiceID, vsID)
	if err != nil {
		return VirtualServer{}, err
	}
	vs := parseVirtualServer(raw)
	if vs.ID == "" {
		return VirtualServer{}, fmt.Errorf("CMP returned virtual server %q without an ID", vsID)
	}
	return vs, nil
}
func (a rawAdapter) CreateVirtualServer(ctx context.Context, lbServiceID string, spec VirtualServerSpec) (VirtualServer, error) {
	q := url.Values{}
	q.Set("name", spec.Name)
	q.Set("vip_port_id", spec.VIPPortID)
	q.Set("protocol", spec.Protocol)
	q.Set("port", fmt.Sprintf("%d", spec.Port))
	q.Set("routing_algorithm", spec.RoutingAlgorithm)
	if spec.PoolName != "" {
		q.Set("pool_name", spec.PoolName)
	}
	if spec.MonitorName != "" {
		q.Set("monitor_name", spec.MonitorName)
	}
	if spec.MonitorInterval != 0 {
		q.Set("interval", fmt.Sprintf("%d", spec.MonitorInterval))
	}
	if spec.MonitorProtocol != "" {
		q.Set("monitor_protocol", strings.ToUpper(spec.MonitorProtocol))
	}
	if spec.MonitorPath != "" {
		q.Set("monitor_path", spec.MonitorPath)
	}
	if spec.MonitorPort != 0 {
		q.Set("monitor_port", fmt.Sprintf("%d", spec.MonitorPort))
	}
	if spec.MonitorTimeout != 0 {
		q.Set("timeout", fmt.Sprintf("%d", spec.MonitorTimeout))
	}
	if spec.PersistenceType != "" {
		q.Set("persistence_type", spec.PersistenceType)
	}
	q.Set("persistence_enabled", fmt.Sprintf("%t", spec.PersistenceEnabled))
	q.Set("x_forwarded_for", fmt.Sprintf("%t", spec.XForwardedFor))
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
			"max_conn":        node.MaxConnections,
			"instance_name":   node.InstanceName,
			"source_type":     node.SourceType,
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

func (a rawAdapter) GetCompute(ctx context.Context, id string) (Compute, error) {
	raw, err := a.raw.GetCompute(ctx, id)
	if err != nil {
		return Compute{}, err
	}
	compute := parseCompute(raw)
	if compute.ID == "" {
		compute.ID = strings.TrimSpace(id)
	}
	if compute.ID == "" {
		return Compute{}, fmt.Errorf("CMP returned compute %q without an ID", id)
	}
	return compute, nil
}

type rawCertificateAdapter struct{ raw RawClient }

func (a rawCertificateAdapter) ListCertificates(ctx context.Context, lbServiceID string) ([]CertificateResource, error) {
	items, err := a.raw.ListLBServiceCertificates(ctx, lbServiceID)
	if err != nil {
		return nil, err
	}
	out := make([]CertificateResource, 0, len(items))
	for _, raw := range items {
		if cert := parseCertificate(raw); cert.ID != "" {
			out = append(out, cert)
		}
	}
	return out, nil
}

func (a rawCertificateAdapter) UploadCertificate(ctx context.Context, lbServiceID string, spec CertificateSpec) (CertificateResource, error) {
	q := url.Values{}
	q.Set("name", spec.Name)
	if spec.Certificate != "" {
		q.Set("sslCert", spec.Certificate)
	}
	if spec.PrivateKey != "" {
		q.Set("sslPvtKey", spec.PrivateKey)
	}
	if spec.CA != "" {
		q.Set("caCert", spec.CA)
	}
	raw, err := a.raw.CreateLBServiceCertificate(ctx, lbServiceID, q)
	if err != nil {
		return CertificateResource{}, err
	}
	return parseCertificate(raw), nil
}

func (a rawCertificateAdapter) DeleteCertificate(ctx context.Context, lbServiceID, certificateID string) error {
	return a.raw.DeleteLBServiceCertificate(ctx, lbServiceID, certificateID)
}

func (a rawCertificateAdapter) BindCertificate(ctx context.Context, lbServiceID, virtualServerID, certificateID string) error {
	return a.raw.AttachLBVirtualServerCertificate(ctx, lbServiceID, virtualServerID, certificateID)
}

func (a rawCertificateAdapter) UnbindCertificate(ctx context.Context, lbServiceID, virtualServerID, certificateID string) error {
	return a.raw.DetachLBVirtualServerCertificate(ctx, lbServiceID, virtualServerID, certificateID)
}

func parseCertificate(raw json.RawMessage) CertificateResource {
	var cert struct {
		ID          string   `json:"id"`
		Name        string   `json:"name"`
		Fingerprint string   `json:"fingerprint"`
		SecretName  string   `json:"secret_name"`
		Certificate string   `json:"ssl_cert"`
		PrivateKey  string   `json:"ssl_pvt_key"`
		CA          string   `json:"ca_cert"`
		HostNames   []string `json:"host_names"`
		AltName     string   `json:"certificate_name"`
		AltID       string   `json:"certificate_id"`
	}
	if json.Unmarshal(raw, &cert) != nil {
		return CertificateResource{}
	}
	id := strings.TrimSpace(cert.ID)
	if id == "" {
		id = strings.TrimSpace(cert.AltID)
	}
	name := strings.TrimSpace(cert.Name)
	if name == "" {
		name = strings.TrimSpace(cert.AltName)
	}
	return CertificateResource{ID: id, Name: name, Fingerprint: strings.TrimSpace(cert.Fingerprint), SecretName: strings.TrimSpace(cert.SecretName), Certificate: strings.TrimSpace(cert.Certificate), PrivateKey: strings.TrimSpace(cert.PrivateKey), CA: strings.TrimSpace(cert.CA), HostNames: append([]string(nil), cert.HostNames...)}
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
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return VIP{}
	}
	id := stringFromAny(obj["id"])
	if id == "" {
		id = stringFromAny(obj["id_str"])
	}
	if id == "" {
		return VIP{}
	}
	address := firstString(obj, "ip_address", "address", "fixed_ip")
	if address == "" {
		if values := portIPs(obj); len(values) != 0 {
			address = values[0]
		}
	}
	attached, _ := obj["attached_to_lb"].(bool)
	return VIP{
		ID:           id,
		Address:      address,
		Status:       firstString(obj, "status"),
		NetworkID:    firstString(obj, "network_id", "network"),
		IPVersion:    firstString(obj, "ip_version"),
		AttachedToLB: attached,
	}
}

func parseVirtualServer(raw json.RawMessage) VirtualServer {
	var vs struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		VIPPortID  string `json:"vip_port_id"`
		VIPAddress string `json:"vip"`
		Status     string `json:"status"`
		Protocol   string `json:"protocol"`
		Port       int32  `json:"port"`
		VIPPort    struct {
			ID int `json:"id"`
		} `json:"vip_port"`
	}
	if json.Unmarshal(raw, &vs) == nil {
		id := strings.TrimSpace(vs.ID)
		if id != "" {
			vipPortID := strings.TrimSpace(vs.VIPPortID)
			if vipPortID == "" && vs.VIPPort.ID != 0 {
				vipPortID = fmt.Sprintf("%d", vs.VIPPort.ID)
			}
			return VirtualServer{
				ID:         id,
				Name:       strings.TrimSpace(vs.Name),
				VIPPortID:  vipPortID,
				VIPAddress: strings.TrimSpace(vs.VIPAddress),
				Status:     strings.TrimSpace(vs.Status), Protocol: strings.ToUpper(strings.TrimSpace(vs.Protocol)), Port: vs.Port,
			}
		}
	}

	var numeric struct {
		ID        int    `json:"id"`
		VIPPortID int    `json:"vip_port_id"`
		Name      string `json:"name"`
	}
	if json.Unmarshal(raw, &numeric) == nil && numeric.ID != 0 {
		vipPortID := ""
		if numeric.VIPPortID != 0 {
			vipPortID = fmt.Sprintf("%d", numeric.VIPPortID)
		}
		return VirtualServer{ID: fmt.Sprintf("%d", numeric.ID), Name: strings.TrimSpace(numeric.Name), VIPPortID: vipPortID}
	}

	return VirtualServer{}
}

func parseCompute(raw json.RawMessage) Compute {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return Compute{}
	}
	return Compute{
		ID:        firstString(obj, "id", "uuid", "compute_id"),
		VPCID:     firstString(obj, "vpc_id", "vpc"),
		NetworkID: firstString(obj, "network_id", "subnet_id"),
		Status:    firstString(obj, "status", "operating_status"),
		Ports:     parseComputePorts(obj["ports"]),
		Name:      firstString(obj, "instance_name", "name"),
		Region:    firstString(obj, "region"),
	}
}

func parseComputePorts(value any) []ComputePort {
	switch typed := value.(type) {
	case []any:
		out := make([]ComputePort, 0, len(typed))
		for _, item := range typed {
			if port := parseComputePort(item); port.ID != 0 || len(port.FixedIPs) != 0 {
				out = append(out, port)
			}
		}
		return out
	case map[string]any:
		out := make([]ComputePort, 0, len(typed))
		for _, item := range typed {
			if port := parseComputePort(item); port.ID != 0 || len(port.FixedIPs) != 0 {
				out = append(out, port)
			}
		}
		return out
	case string:
		var decoded any
		if json.Unmarshal([]byte(typed), &decoded) == nil {
			return parseComputePorts(decoded)
		}
	}
	return nil
}

func parseComputePort(value any) ComputePort {
	obj, ok := value.(map[string]any)
	if !ok {
		return ComputePort{}
	}
	return ComputePort{
		ID:         intFromAny(firstAny(obj, "id", "id_str", "port_id", "provider_port_id")),
		FixedIPs:   portIPs(obj),
		NetworkID:  firstString(obj, "network_id", "subnet_id"),
		DeviceID:   firstString(obj, "device_id", "resource_id", "compute_id"),
		DeviceType: inferResourceType(firstString(obj, "device_type", "device_owner", "resource_type")),
	}
}

func portIPs(obj map[string]any) []string {
	if ip := firstString(obj, "fixed_ip", "ip_address", "ip"); ip != "" {
		return []string{ip}
	}
	out := []string{}
	switch fixed := obj["fixed_ips"].(type) {
	case string:
		var decoded any
		if json.Unmarshal([]byte(fixed), &decoded) == nil {
			return portIPs(map[string]any{"fixed_ips": decoded})
		}
		if value := strings.TrimSpace(fixed); value != "" {
			return []string{value}
		}
	case []any:
		for _, item := range fixed {
			switch value := item.(type) {
			case string:
				if value = strings.TrimSpace(value); value != "" {
					out = append(out, value)
				}
			case map[string]any:
				if ip := firstString(value, "ip_address", "ip", "fixed_ip"); ip != "" {
					out = append(out, ip)
				}
			}
		}
	}
	return out
}

func firstString(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringFromAny(obj[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstAny(obj map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := obj[key]; ok {
			return value
		}
	}
	return nil
}

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strings.TrimSpace(fmt.Sprintf("%.0f", typed))
	case int:
		return fmt.Sprintf("%d", typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	default:
		return ""
	}
}

func intFromAny(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case string:
		var out int
		if _, err := fmt.Sscanf(strings.TrimSpace(typed), "%d", &out); err == nil {
			return out
		}
	}
	return 0
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
	q.Set("name", s.Name)
	q.Set("monitor_protocol", s.Protocol)
	q.Set("path", s.Path)
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
	q.Set("virtual_server_pool_id", s.PoolID)
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
