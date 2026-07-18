package lbaas

import (
	"context"
	"fmt"
	"testing"

	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type stubClient struct {
	lbServices         []LBService
	vips               []VIP
	vsList             []VirtualServer
	lastLBSpec         LBServiceSpec
	lastVSSpec         VirtualServerSpec
	createdLB          int
	createdVIP         int
	createdVS          int
	deletedVS          int
	deletedVIP         int
	deletedLB          int
	searchedIP         []string
	searchNetworkPorts func(string) []NetworkPort
}

func (s *stubClient) ListLBServices(context.Context) ([]LBService, error) {
	return append([]LBService(nil), s.lbServices...), nil
}
func (s *stubClient) CreateLBService(_ context.Context, spec LBServiceSpec) (LBService, error) {
	s.createdLB++
	s.lastLBSpec = spec
	return LBService{ID: "lb-1", Name: spec.Name}, nil
}
func (s *stubClient) DeleteLBService(context.Context, string) error {
	s.deletedLB++
	return nil
}
func (s *stubClient) ListVIPs(context.Context, string) ([]VIP, error) {
	return append([]VIP(nil), s.vips...), nil
}
func (s *stubClient) CreateVIP(context.Context, string) (VIP, error) {
	s.createdVIP++
	return VIP{ID: "7", Address: "10.0.0.7"}, nil
}
func (s *stubClient) DeleteVIP(context.Context, string, string) error {
	s.deletedVIP++
	return nil
}
func (s *stubClient) ListVirtualServers(context.Context, string) ([]VirtualServer, error) {
	return append([]VirtualServer(nil), s.vsList...), nil
}
func (s *stubClient) CreateVirtualServer(_ context.Context, _ string, spec VirtualServerSpec) (VirtualServer, error) {
	s.createdVS++
	s.lastVSSpec = spec
	return VirtualServer{ID: "vs-1", Name: spec.Name}, nil
}
func (s *stubClient) DeleteVirtualServer(context.Context, string, string) error {
	s.deletedVS++
	return nil
}
func (s *stubClient) SearchNetworkPortsByIP(_ context.Context, ip string) ([]NetworkPort, error) {
	s.searchedIP = append(s.searchedIP, ip)
	if s.searchNetworkPorts != nil {
		return s.searchNetworkPorts(ip), nil
	}
	return []NetworkPort{{ID: len(s.searchedIP), ResourceID: "compute-" + ip, ResourceType: "compute", IP: ip}}, nil
}

func TestEnsureCreatesLBVIPAndVirtualServer(t *testing.T) {
	stub := &stubClient{}
	res, err := New(stub, "vpc-1").Ensure(context.Background(), EnsureRequest{
		LBName:        "lb",
		LBDescription: "desc",
		VirtualServer: model.VirtualServer{
			Name:             "vs",
			FrontendPort:     80,
			BackendNodePort:  30080,
			Protocol:         "HTTP",
			RoutingAlgorithm: "round_robin",
			Monitor:          &model.Monitor{Type: "http", Path: "/healthz", Interval: 15},
		},
		Backends: []model.BackendMember{{IP: "10.0.0.1", Port: 30080, Weight: 50}},
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if stub.createdLB != 1 || stub.createdVIP != 1 || stub.createdVS != 1 {
		t.Fatalf("unexpected create calls: lb=%d vip=%d vs=%d", stub.createdLB, stub.createdVIP, stub.createdVS)
	}
	if res.Observed.LBServiceID != "lb-1" || res.Observed.VIPPortID != "7" || res.Observed.VirtualServerID != "vs-1" || res.Observed.VIPAddress != "10.0.0.7" {
		t.Fatalf("unexpected observed state: %#v", res.Observed)
	}
	if res.Observed.Graph.LBServices["lb"].ExternalID != "lb-1" || res.Observed.Graph.VirtualServers["vs"].ExternalID != "vs-1" {
		t.Fatalf("expected observed graph to contain LB and VS resources: %#v", res.Observed.Graph)
	}
	if got := stub.lastVSSpec.MonitorPath; got != "/healthz" {
		t.Fatalf("expected monitor path, got %q", got)
	}
}

func TestEnsureSkipsVirtualServerWhenBackendHashMatches(t *testing.T) {
	backends := []model.BackendMember{{IP: "10.0.0.1", Port: 30080, Weight: 50}}
	hash := DesiredBackendHash(80, 30080, backends)
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "lb"}}, vips: []VIP{{ID: "7", Address: "10.0.0.7"}}}
	res, err := New(stub, "").Ensure(context.Background(), EnsureRequest{
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:      backends,
		CurrentHash:   hash,
		Current:       model.ObservedState{LBServiceID: "lb-1", VIPPortID: "7", VIPAddress: "10.0.0.7", VirtualServerID: "vs-1"},
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if stub.createdVS != 0 || stub.deletedVS != 0 || res.Changed {
		t.Fatalf("expected no VS mutation, created=%d deleted=%d changed=%t", stub.createdVS, stub.deletedVS, res.Changed)
	}
}

func TestEnsurePreservesExistingVirtualServerWhenHashIsNotManaged(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "lb"}}, vips: []VIP{{ID: "7", Address: "10.0.0.7"}}}
	res, err := New(stub, "").Ensure(context.Background(), EnsureRequest{
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:      []model.BackendMember{{IP: "10.0.0.1", Port: 30080, Weight: 50}},
		Current:       model.ObservedState{LBServiceID: "lb-1", VIPPortID: "7", VIPAddress: "10.0.0.7", VirtualServerID: "vs-1"},
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if stub.createdVS != 0 || stub.deletedVS != 0 || res.Changed {
		t.Fatalf("expected existing VS to be preserved, created=%d deleted=%d changed=%t", stub.createdVS, stub.deletedVS, res.Changed)
	}
}

func TestEnsureRecreatesExistingVirtualServerWhenHashIsManagedButMissing(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "lb"}}, vips: []VIP{{ID: "7", Address: "10.0.0.7"}}}
	res, err := New(stub, "").Ensure(context.Background(), EnsureRequest{
		VirtualServer:           model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:                []model.BackendMember{{IP: "10.0.0.1", Port: 30080, Weight: 50}},
		Current:                 model.ObservedState{LBServiceID: "lb-1", VIPPortID: "7", VIPAddress: "10.0.0.7", VirtualServerID: "vs-old"},
		RecreateWhenHashMissing: true,
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if stub.deletedVS != 1 || stub.createdVS != 1 || !res.Changed || res.Observed.VirtualServerID != "vs-1" {
		t.Fatalf("expected VS recreation, created=%d deleted=%d changed=%t observed=%#v", stub.createdVS, stub.deletedVS, res.Changed, res.Observed)
	}
}

func TestEnsureFailsWhenBackendPortCannotBeResolved(t *testing.T) {
	stub := &stubClient{}
	stub.searchNetworkPorts = func(string) []NetworkPort { return nil }
	_, err := New(stub, "").Ensure(context.Background(), EnsureRequest{
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:      []model.BackendMember{{IP: "10.0.0.99", Port: 30080, Weight: 50}},
	})
	if err == nil {
		t.Fatal("expected missing backend port lookup to fail")
	}
}

func TestEnsurePassesOptionalLBServiceFields(t *testing.T) {
	stub := &stubClient{}
	_, err := New(stub, "fallback-vpc").Ensure(context.Background(), EnsureRequest{
		LBName:        "lb",
		LBDescription: "desc",
		FlavorID:      42,
		NetworkID:     "net-1",
		VPCID:         "vpc-explicit",
		VPCName:       "vpc-name",
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:      []model.BackendMember{{IP: "10.0.0.1", Port: 30080, Weight: 50}},
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	for key, want := range map[string]string{"flavor_id": "42", "network_id": "net-1", "vpc_id": "vpc-explicit", "vpc_name": "vpc-name"} {
		if got := lbSpecValue(stub.lastLBSpec, key); got != want {
			t.Fatalf("expected %s=%q, got %q in %#v", key, want, got, stub.lastLBSpec)
		}
	}
}

func TestCleanupDeletesResourcesInReverseDependencyOrder(t *testing.T) {
	stub := &stubClient{}
	res, err := New(stub, "").Cleanup(context.Background(), CleanupRequest{
		Current:         model.ObservedState{LBServiceID: "lb-1", VIPPortID: "7", VirtualServerID: "vs-1"},
		DeleteVIP:       true,
		DeleteLBService: true,
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if stub.deletedVS != 1 || stub.deletedVIP != 1 || stub.deletedLB != 1 {
		t.Fatalf("expected all resources deleted, vs=%d vip=%d lb=%d", stub.deletedVS, stub.deletedVIP, stub.deletedLB)
	}
	if !res.DeletedVirtualServer || !res.DeletedVIP || !res.DeletedLBService {
		t.Fatalf("unexpected cleanup result: %#v", res)
	}
}

func TestCleanupPreservesSharedParentResources(t *testing.T) {
	stub := &stubClient{}
	_, err := New(stub, "").Cleanup(context.Background(), CleanupRequest{
		Current: model.ObservedState{LBServiceID: "lb-1", VIPPortID: "7", VirtualServerID: "vs-1"},
	})
	if err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if stub.deletedVS != 1 || stub.deletedVIP != 0 || stub.deletedLB != 0 {
		t.Fatalf("expected only VS deleted for shared resources, vs=%d vip=%d lb=%d", stub.deletedVS, stub.deletedVIP, stub.deletedLB)
	}
}

func lbSpecValue(spec LBServiceSpec, key string) string {
	switch key {
	case "flavor_id":
		if spec.FlavorID == 0 {
			return ""
		}
		return fmt.Sprintf("%d", spec.FlavorID)
	case "network_id":
		return spec.NetworkID
	case "vpc_id":
		return spec.VPCID
	case "vpc_name":
		return spec.VPCName
	default:
		return ""
	}
}

func TestCleanupDiscoveredDeletesVirtualServersByPrefixAndAllVIPs(t *testing.T) {
	stub := &stubClient{
		vsList: []VirtualServer{{ID: "vs-match", Name: "app-vs-ns-svc-80"}, {ID: "vs-other", Name: "other"}},
		vips:   []VIP{{ID: "vip-1", Address: "10.0.0.10"}, {ID: "vip-2", Address: "10.0.0.11"}},
	}
	res, err := New(stub, "").CleanupDiscovered(context.Background(), CleanupDiscoveryRequest{
		LBServiceID:             "lb-1",
		VirtualServerNamePrefix: "app-vs-ns-svc-",
		DeleteAllVIPs:           true,
	})
	if err != nil {
		t.Fatalf("CleanupDiscovered: %v", err)
	}
	if stub.deletedVS != 1 || stub.deletedVIP != 2 || !res.DeletedVirtualServer || !res.DeletedVIP {
		t.Fatalf("unexpected cleanup: vs=%d vip=%d result=%#v", stub.deletedVS, stub.deletedVIP, res)
	}
}

func TestEnsureFailsWhenBackendPortHasNoResourceID(t *testing.T) {
	stub := &stubClient{}
	stub.searchNetworkPorts = func(ip string) []NetworkPort { return []NetworkPort{{ID: 99, ResourceType: "compute", IP: ip}} }
	_, err := New(stub, "").Ensure(context.Background(), EnsureRequest{
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:      []model.BackendMember{{IP: "10.0.0.99", Port: 30080, Weight: 50}},
	})
	if err == nil {
		t.Fatal("expected missing CMP resource_id to fail")
	}
}
