package lbaas

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type rawAdapterStub struct {
	compute             json.RawMessage
	network             json.RawMessage
	lastListVIPSubnet   string
	lastCreateVIPSubnet string
}

func (s rawAdapterStub) ListLBServices(context.Context, *f5client.ListLoadBalancersOptions) ([]json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) CreateLBService(context.Context, url.Values) (json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) GetLBService(
	context.Context,
	string,
) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"lb-1","name":"test-lb"}`), nil
}
func (s rawAdapterStub) DeleteLBService(context.Context, string) error { return nil }
func (s *rawAdapterStub) CreateLBServiceVIP(_ context.Context, _, subnetID string) (json.RawMessage, error) {
	s.lastCreateVIPSubnet = subnetID
	return json.RawMessage(`{"id":"vip-1"}`), nil
}
func (s *rawAdapterStub) GetLBServiceVIPs(_ context.Context, _, subnetID string) ([]json.RawMessage, error) {
	s.lastListVIPSubnet = subnetID
	return []json.RawMessage{json.RawMessage(`{"id":"vip-1","address":"10.0.0.7"}`)}, nil
}
func (s rawAdapterStub) DeleteLBServiceVIP(context.Context, string, string) error { return nil }
func (s rawAdapterStub) ListLBVirtualServers(context.Context, string) ([]json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) GetLBVirtualServer(context.Context, string, string) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"vs-1","status":"Active","protocol":"TCP","port":8080,"vip_port":{"id":1}}`), nil
}
func (s rawAdapterStub) CreateLBVirtualServer(context.Context, string, url.Values) (json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) DeleteLBVirtualServer(context.Context, string, string) error { return nil }
func (s rawAdapterStub) ListLBServiceCertificates(context.Context, string) ([]json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) CreateLBServiceCertificate(context.Context, string, url.Values) (json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) DeleteLBServiceCertificate(context.Context, string, string) error { return nil }
func (s rawAdapterStub) AttachLBVirtualServerCertificate(context.Context, string, string, string) error {
	return nil
}
func (s rawAdapterStub) DetachLBVirtualServerCertificate(context.Context, string, string, string) error {
	return nil
}
func (s rawAdapterStub) GetCompute(context.Context, string) (json.RawMessage, error) {
	if len(s.compute) != 0 {
		return s.compute, nil
	}
	return json.RawMessage(`{"id":"compute-1","vpc_id":"vpc-1","network_id":"net-1","ports":[{"id":5001,"fixed_ips":[{"ip_address":"10.0.0.1"}],"network_id":"net-1","device_id":"compute-1","device_type":"compute"}]}`), nil
}
func (s rawAdapterStub) GetNetwork(context.Context, string) (json.RawMessage, error) {
	if len(s.network) != 0 {
		return s.network, nil
	}
	return json.RawMessage(`{"id":"subnet-1","vpc_id":"vpc-1","status":"Active"}`), nil
}
func TestParseVIPPreservesNumericIDPlacementAndReadiness(t *testing.T) {
	vip := parseVIP(json.RawMessage(`{"id":36944,"fixed_ips":["10.0.20.28"],"status":"Active","network_id":"subnet-1","ip_version":"IPv4","attached_to_lb":false}`))
	if vip.ID != "36944" || vip.Address != "10.0.20.28" || vip.Status != "Active" || vip.NetworkID != "subnet-1" || vip.IPVersion != "IPv4" || vip.AttachedToLB {
		t.Fatalf("unexpected VIP parse result: %#v", vip)
	}
}

func TestParseComputePreservesAllFixedIPs(t *testing.T) {
	compute := parseCompute(json.RawMessage(`{"id":"compute-1","instance_name":"worker-1","vpc_id":"vpc-1","network_id":"subnet-1","region":"dev","ports":[{"id":38135,"fixed_ips":["10.10.1.145","10.10.1.146"],"network_id":"subnet-1","device_id":"compute-1","device_type":"compute"}]}`))
	if compute.ID != "compute-1" || compute.Name != "worker-1" || compute.Region != "dev" || len(compute.Ports) != 1 || len(compute.Ports[0].FixedIPs) != 2 || compute.Ports[0].ID != 38135 {
		t.Fatalf("unexpected compute parse result: %#v", compute)
	}
}

func TestRawAdapterGetComputeParsesPorts(t *testing.T) {
	adapter := NewRawClient(&rawAdapterStub{compute: json.RawMessage(`{
		"id":"compute-1",
		"vpc_id":"vpc-1",
		"network_id":"net-1",
		"ports":[{"id":38135,"fixed_ips":[{"ip_address":"10.10.1.145"}],"network_id":"net-1","device_id":"compute-1","device_type":"compute"}]
	}`)})

	compute, err := adapter.GetCompute(context.Background(), "compute-1")
	if err != nil {
		t.Fatalf("GetCompute failed: %v", err)
	}
	if compute.ID != "compute-1" || compute.VPCID != "vpc-1" || compute.NetworkID != "net-1" {
		t.Fatalf("unexpected compute metadata: %#v", compute)
	}
	if len(compute.Ports) != 1 || compute.Ports[0].ID != 38135 || len(compute.Ports[0].FixedIPs) != 1 || compute.Ports[0].FixedIPs[0] != "10.10.1.145" || compute.Ports[0].DeviceID != "compute-1" {
		t.Fatalf("unexpected compute ports: %#v", compute.Ports)
	}
}

func TestRawNetworkAdapterPreservesVPCAndStatus(t *testing.T) {
	adapter := rawNetworkAdapter{raw: rawAdapterStub{network: json.RawMessage(`{"id":"subnet-1","vpc_id":"vpc-1","status":"Active"}`)}}
	network, err := adapter.GetNetwork(context.Background(), "subnet-1")
	if err != nil {
		t.Fatalf("GetNetwork failed: %v", err)
	}
	if network.ID != "subnet-1" || network.VPCID != "vpc-1" || network.Status != "Active" {
		t.Fatalf("unexpected network placement: %#v", network)
	}
}

func TestRawAdapterForwardsSubnetToVIPOperations(t *testing.T) {
	raw := &rawAdapterStub{}
	adapter := NewRawClient(raw)

	if _, err := adapter.ListVIPs(context.Background(), "lb-1", "subnet-1"); err != nil {
		t.Fatalf("ListVIPs failed: %v", err)
	}
	if raw.lastListVIPSubnet != "subnet-1" {
		t.Fatalf("expected ListVIPs subnet %q, got %q", "subnet-1", raw.lastListVIPSubnet)
	}

	if _, err := adapter.CreateVIP(context.Background(), "lb-1", "subnet-2"); err != nil {
		t.Fatalf("CreateVIP failed: %v", err)
	}
	if raw.lastCreateVIPSubnet != "subnet-2" {
		t.Fatalf("expected CreateVIP subnet %q, got %q", "subnet-2", raw.lastCreateVIPSubnet)
	}
}

type rawPoolAdapterStub struct {
	pools         []json.RawMessage
	createPoolQ   url.Values
	memberQ       url.Values
	defaultPool   string
	deletedPool   string
	deletedMember string
}

func (s *rawPoolAdapterStub) ListLBVirtualServerPools(context.Context, string, string) ([]json.RawMessage, error) {
	return append([]json.RawMessage(nil), s.pools...), nil
}
func (s *rawPoolAdapterStub) CreateLBVirtualServerPool(_ context.Context, _, _ string, q url.Values) (json.RawMessage, error) {
	s.createPoolQ = q
	return json.RawMessage(`{"id":"pool-1","pool_name":"pool-web","is_default":false}`), nil
}
func (s *rawPoolAdapterStub) GetLBVirtualServerPool(context.Context, string, string, string) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"pool-1","pool_name":"pool-web"}`), nil
}
func (s *rawPoolAdapterStub) DeleteLBVirtualServerPool(_ context.Context, _, _, poolID string) error {
	s.deletedPool = poolID
	return nil
}
func (s *rawPoolAdapterStub) SetDefaultLBVirtualServerPool(_ context.Context, _, _, poolID string) error {
	s.defaultPool = poolID
	return nil
}
func (s *rawPoolAdapterStub) CreateLBVirtualServerPoolMember(_ context.Context, _, _, _ string, q url.Values) (json.RawMessage, error) {
	s.memberQ = q
	return json.RawMessage(`{"id":"member-1","resource_ip":"10.0.0.1","backend_port_id":5001,"port":30080,"weight":50}`), nil
}
func (s *rawPoolAdapterStub) UpdateLBVirtualServerPoolMember(context.Context, string, string, string, string, url.Values) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"member-1"}`), nil
}
func (s *rawPoolAdapterStub) DeleteLBVirtualServerPoolMember(_ context.Context, _, _, _, memberID string) error {
	s.deletedMember = memberID
	return nil
}

func TestRawPoolAdapterEncodesSwaggerQueries(t *testing.T) {
	raw := &rawPoolAdapterStub{}
	adapter := NewPoolClientFromRaw(raw)
	pool, err := adapter.CreatePool(context.Background(), "lb-1", "vs-1", PoolSpec{Name: "pool-web", Monitor: &model.Monitor{Name: "mon-web", Type: "http", Path: "/healthz", Interval: 15}, Members: []PoolMemberSpec{{ResourceID: "compute-1", ResourceType: "compute", ResourceIP: "10.0.0.1", BackendPortID: 5001, Port: 30080, Weight: 50}}})
	if err != nil {
		t.Fatalf("CreatePool failed: %v", err)
	}
	if pool.ID != "pool-1" || pool.Name != "pool-web" {
		t.Fatalf("unexpected pool: %#v", pool)
	}
	if raw.createPoolQ.Get("pool_name") != "pool-web" || raw.createPoolQ.Get("monitor_name") != "mon-web" || raw.createPoolQ.Get("monitor_path") != "/healthz" || raw.createPoolQ.Get("interval") != "15" {
		t.Fatalf("unexpected pool query: %v", raw.createPoolQ)
	}
	if len(raw.createPoolQ["nodes"]) != 1 {
		t.Fatalf("expected node payload in pool query: %v", raw.createPoolQ)
	}

	member, err := adapter.CreatePoolMember(context.Background(), "lb-1", "vs-1", "pool-1", PoolMemberSpec{ResourceID: "compute-1", ResourceType: "compute", ResourceIP: "10.0.0.1", BackendPortID: 5001, Port: 30080, Weight: 50})
	if err != nil {
		t.Fatalf("CreatePoolMember failed: %v", err)
	}
	if member.ID != "member-1" || member.BackendPortID != 5001 {
		t.Fatalf("unexpected member: %#v", member)
	}
	if raw.memberQ.Get("node") == "" {
		t.Fatalf("expected node query payload, got %v", raw.memberQ)
	}
}

func TestRawPoolAdapterListsPools(t *testing.T) {
	raw := &rawPoolAdapterStub{pools: []json.RawMessage{json.RawMessage(`{"id":"pool-1","pool_name":"pool-web"}`)}}
	pools, err := NewPoolClientFromRaw(raw).ListPools(context.Background(), "lb-1", "vs-1")
	if err != nil {
		t.Fatalf("ListPools failed: %v", err)
	}
	if len(pools) != 1 || pools[0].ID != "pool-1" || pools[0].Name != "pool-web" {
		t.Fatalf("unexpected pools: %#v", pools)
	}
}

func TestMonitorSpecQueryUsesMonitorEndpointParameterNames(t *testing.T) {
	q := monitorSpecQuery(MonitorSpec{Name: "mon-web", Protocol: "HTTP", Path: "/healthz", Interval: 15})
	if q.Get("name") != "mon-web" || q.Get("monitor_protocol") != "HTTP" || q.Get("path") != "/healthz" || q.Get("interval") != "15" {
		t.Fatalf("unexpected monitor endpoint query: %v", q)
	}
	if q.Get("monitor_name") != "" || q.Get("monitor_path") != "" {
		t.Fatalf("legacy pool query names must not be used: %v", q)
	}
}
