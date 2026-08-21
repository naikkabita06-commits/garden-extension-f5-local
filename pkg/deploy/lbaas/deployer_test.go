package lbaas

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type certificateBinding struct {
	virtualServerID string
	certificateID   string
}

type stubCertificateClient struct {
	resources []CertificateResource
	uploaded  []CertificateSpec
	bindings  []certificateBinding
}

func (s *stubCertificateClient) ListCertificates(context.Context, string) ([]CertificateResource, error) {
	return append([]CertificateResource(nil), s.resources...), nil
}
func (s *stubCertificateClient) UploadCertificate(_ context.Context, _ string, spec CertificateSpec) (CertificateResource, error) {
	s.uploaded = append(s.uploaded, spec)
	created := CertificateResource{ID: "cert-" + strconv.Itoa(len(s.uploaded)), Name: spec.Name}
	s.resources = append(s.resources, created)
	return created, nil
}
func (s *stubCertificateClient) DeleteCertificate(context.Context, string, string) error { return nil }
func (s *stubCertificateClient) BindCertificate(_ context.Context, _ string, virtualServerID, certificateID string) error {
	s.bindings = append(s.bindings, certificateBinding{virtualServerID: virtualServerID, certificateID: certificateID})
	return nil
}
func (s *stubCertificateClient) UnbindCertificate(context.Context, string, string, string) error {
	return nil
}

type stubClient struct {
	lbServices      []LBService
	vips            []VIP
	vsList          []VirtualServer
	lastLBSpec      LBServiceSpec
	lastVSSpec      VirtualServerSpec
	createdLB       int
	createdVIP      int
	createdVS       int
	createdVSResult *VirtualServer
	lastVIPSubnet   string
	deletedVS       int
	deletedVIP      int
	deletedLB       int
	computes        map[string]Compute
	networks        map[string]Network
}

func (s *stubClient) ListLBServices(
	_ context.Context,
	_ *f5client.ListLoadBalancersOptions,
) ([]LBService, error) {
	return append([]LBService(nil), s.lbServices...), nil
}
func (s *stubClient) CreateLBService(_ context.Context, spec LBServiceSpec) (LBService, error) {
	s.createdLB++
	s.lastLBSpec = spec
	created := LBService{ID: "lb-1", Name: spec.Name, Status: "Active", OperatingStatus: "Ready", VPCID: spec.VPCID, VPCName: spec.VPCName, NetworkID: spec.NetworkID}
	s.lbServices = append(s.lbServices, created)
	return created, nil
}
func (s *stubClient) DeleteLBService(_ context.Context, id string) error {
	s.deletedLB++
	id = strings.TrimSpace(id)
	for i := range s.lbServices {
		if strings.TrimSpace(s.lbServices[i].ID) == id {
			s.lbServices = append(s.lbServices[:i], s.lbServices[i+1:]...)
			break
		}
	}
	return nil
}

func (s *stubClient) GetLBService(
	_ context.Context,
	id string,
) (LBService, error) {
	if s.deletedLB > 0 {
		return LBService{}, &f5client.HTTPStatusError{StatusCode: http.StatusNotFound, Status: "404 Not Found"}
	}
	for _, svc := range s.lbServices {
		if strings.TrimSpace(svc.ID) == strings.TrimSpace(id) {
			return svc, nil
		}
	}

	return LBService{}, &f5client.HTTPStatusError{
		StatusCode: http.StatusNotFound,
		Status:     "404 Not Found",
	}
}

func (s *stubClient) ListVIPs(_ context.Context, _, subnetID string) ([]VIP, error) {
	s.lastVIPSubnet = strings.TrimSpace(subnetID)
	return append([]VIP(nil), s.vips...), nil
}
func (s *stubClient) CreateVIP(_ context.Context, _, subnetID string) (VIP, error) {
	s.createdVIP++
	s.lastVIPSubnet = strings.TrimSpace(subnetID)
	return VIP{ID: "7", Address: "10.0.0.7"}, nil
}
func (s *stubClient) DeleteVIP(context.Context, string, string) error {
	s.deletedVIP++
	return nil
}
func (s *stubClient) ListVirtualServers(context.Context, string) ([]VirtualServer, error) {
	return append([]VirtualServer(nil), s.vsList...), nil
}
func (s *stubClient) GetVirtualServer(_ context.Context, _, id string) (VirtualServer, error) {
	for _, vs := range s.vsList {
		if strings.TrimSpace(vs.ID) == strings.TrimSpace(id) {
			return vs, nil
		}
	}
	return VirtualServer{}, &f5client.HTTPStatusError{StatusCode: http.StatusNotFound, Status: "404 Not Found"}
}
func (s *stubClient) CreateVirtualServer(_ context.Context, _ string, spec VirtualServerSpec) (VirtualServer, error) {
	s.createdVS++
	s.lastVSSpec = spec
	if s.createdVSResult != nil {
		s.vsList = append(s.vsList, *s.createdVSResult)
		return *s.createdVSResult, nil
	}
	created := VirtualServer{ID: "vs-1", Name: spec.Name, VIPPortID: spec.VIPPortID, Status: "Active", Protocol: spec.Protocol, Port: spec.Port}
	s.vsList = append(s.vsList, created)
	return created, nil
}
func (s *stubClient) DeleteVirtualServer(_ context.Context, _ string, id string) error {
	s.deletedVS++
	for i := range s.vsList {
		if strings.TrimSpace(s.vsList[i].ID) == strings.TrimSpace(id) {
			s.vsList = append(s.vsList[:i], s.vsList[i+1:]...)
			break
		}
	}
	return nil
}

func (s *stubClient) GetCompute(_ context.Context, id string) (Compute, error) {
	if s.computes != nil {
		if compute, ok := s.computes[id]; ok {
			return compute, nil
		}
	}
	return Compute{ID: id, Name: "node-1", VPCID: "vpc-1", NetworkID: "net-1", Ports: []ComputePort{{ID: 5001, FixedIPs: []string{"10.0.0.1"}, NetworkID: "net-1", DeviceID: id, DeviceType: "compute"}}}, nil
}

func (s *stubClient) GetNetwork(_ context.Context, id string) (Network, error) {
	if network, ok := s.networks[id]; ok {
		return network, nil
	}
	return Network{}, &f5client.HTTPStatusError{StatusCode: http.StatusNotFound, Status: "404 Not Found"}
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
		Backends: []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30080, Weight: 50}},
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
	backends := []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30080, Weight: 50}}
	hash := DesiredBackendHash(80, 30080, backends)
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "lb", Status: "Active"}}, vips: []VIP{{ID: "7", Address: "10.0.0.7"}}, vsList: []VirtualServer{{ID: "vs-1", Name: "vs", VIPPortID: "7", Status: "Active", Protocol: "HTTP", Port: 80}}}
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

func TestDesiredBackendHashIncludesBackendPort(t *testing.T) {
	first := DesiredBackendHash(80, 30080, []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30080, Weight: 10}})
	second := DesiredBackendHash(80, 30080, []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30081, Weight: 10}})
	if first == second {
		t.Fatal("backend hash must change when the CMP member port changes")
	}
}

func TestDesiredVirtualServerHashIncludesReplacementFields(t *testing.T) {
	backends := []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30080, Weight: 10}}
	base := model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP", RoutingAlgorithm: "round_robin"}
	changed := base
	changed.PersistenceType = "source_ip"
	if DesiredVirtualServerHash(base, backends) == DesiredVirtualServerHash(changed, backends) {
		t.Fatal("virtual-server hash must change when a replacement field changes")
	}
}

func TestEnsurePreservesExistingVirtualServerWhenHashIsNotManaged(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "lb", Status: "Active"}}, vips: []VIP{{ID: "7", Address: "10.0.0.7"}}, vsList: []VirtualServer{{ID: "vs-1", Name: "vs", VIPPortID: "7", Status: "Active", Protocol: "HTTP", Port: 80}}}
	res, err := New(stub, "").Ensure(context.Background(), EnsureRequest{
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:      []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30080, Weight: 50}},
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
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "lb", Status: "Active"}}, vips: []VIP{{ID: "7", Address: "10.0.0.7"}}, vsList: []VirtualServer{{ID: "vs-old", Name: "vs", VIPPortID: "7", Status: "Active", Protocol: "HTTP", Port: 80}}}
	_, err := New(stub, "").Ensure(context.Background(), EnsureRequest{
		VirtualServer:           model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:                []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30080, Weight: 50}},
		Current:                 model.ObservedState{LBServiceID: "lb-1", VIPPortID: "7", VIPAddress: "10.0.0.7", VirtualServerID: "vs-old"},
		RecreateWhenHashMissing: true,
	})
	if _, ok := IsProvisioningPending(err); !ok {
		t.Fatalf("expected deletion wait, got %v", err)
	}
	if stub.deletedVS != 1 || stub.createdVS != 0 {
		t.Fatalf("expected delete-before-recreate wait, created=%d deleted=%d", stub.createdVS, stub.deletedVS)
	}
}

func TestEnsureFailsWhenBackendComputeIdentityIsMissing(t *testing.T) {
	stub := &stubClient{}
	_, err := New(stub, "").Ensure(context.Background(), EnsureRequest{
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:      []model.BackendMember{{IP: "10.0.0.99", Port: 30080, Weight: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "no CMP compute identity") {
		t.Fatalf("expected missing backend compute identity to fail, got %v", err)
	}
}

func TestEnsurePassesOptionalLBServiceFields(t *testing.T) {
	stub := &stubClient{computes: map[string]Compute{"compute-1": {ID: "compute-1", VPCID: "vpc-explicit", NetworkID: "net-1", Ports: []ComputePort{{ID: 5001, FixedIPs: []string{"10.0.0.1"}, NetworkID: "net-1", DeviceID: "compute-1", DeviceType: "compute"}}}}}
	_, err := New(stub, "vpc-explicit").Ensure(context.Background(), EnsureRequest{
		LBName:        "lb",
		LBDescription: "desc",
		FlavorID:      42,
		NetworkID:     "net-1",
		VPCID:         "vpc-explicit",
		VPCName:       "vpc-name",
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:      []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30080, Weight: 50}},
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

func TestCleanupStackDeletesOnlyRecordedGraphResources(t *testing.T) {
	stub := &stubClient{}
	state := model.ObservedState{Graph: model.NewObservedGraph()}
	state.Graph.LBServices["lb"] = model.ObservedResource{LogicalID: "lb", ExternalID: "lb-1"}
	state.Graph.VIPs["vip"] = model.ObservedResource{LogicalID: "vip", ExternalID: "vip-1"}
	state.Graph.VirtualServers["listener"] = model.ObservedResource{LogicalID: "listener", ExternalID: "vs-1"}

	result, err := New(stub, "").CleanupStack(context.Background(), CleanupRequest{Current: state, DeleteVIP: true, DeleteLBService: true})
	if err != nil {
		t.Fatalf("CleanupStack: %v", err)
	}
	if stub.deletedVS != 1 || stub.deletedVIP != 1 || stub.deletedLB != 1 {
		t.Fatalf("expected graph resources only to be deleted, got vs=%d vip=%d lb=%d", stub.deletedVS, stub.deletedVIP, stub.deletedLB)
	}
	if !result.DeletedVirtualServer || !result.DeletedVIP || !result.DeletedLBService {
		t.Fatalf("unexpected cleanup result: %#v", result)
	}
}

func TestCleanupStackRejectsAmbiguousLBServiceGraph(t *testing.T) {
	stub := &stubClient{}
	state := model.ObservedState{Graph: model.NewObservedGraph()}
	state.Graph.LBServices["one"] = model.ObservedResource{LogicalID: "one", ExternalID: "lb-1"}
	state.Graph.LBServices["two"] = model.ObservedResource{LogicalID: "two", ExternalID: "lb-2"}
	_, err := New(stub, "").CleanupStack(context.Background(), CleanupRequest{Current: state, DeleteLBService: true})
	if err == nil || !strings.Contains(err.Error(), "multiple LB service IDs") {
		t.Fatalf("expected ambiguous parent cleanup error, got %v", err)
	}
	if stub.deletedLB != 0 {
		t.Fatalf("ambiguous graph must not delete a parent, got %d deletes", stub.deletedLB)
	}
}

func TestDeleteObsoleteVirtualServersRemovesOnlyUndesiredListener(t *testing.T) {
	stub := &stubClient{}
	deployer := New(stub, "")
	observed := model.ObservedState{Graph: model.NewObservedGraph()}
	observed.Graph.VirtualServers["keep"] = model.ObservedResource{LogicalID: "keep", ExternalID: "vs-keep"}
	observed.Graph.VirtualServers["remove"] = model.ObservedResource{LogicalID: "remove", ExternalID: "vs-remove"}

	changed, err := deployer.deleteObsoleteVirtualServers(context.Background(), "lb-1", &observed, &model.LoadBalancerStack{VirtualServers: []model.VirtualServer{{Name: "keep"}}})
	if err != nil {
		t.Fatalf("deleteObsoleteVirtualServers: %v", err)
	}
	if !changed {
		t.Fatal("expected obsolete virtual-server cleanup to report a change")
	}
	if stub.deletedVS != 1 {
		t.Fatalf("expected one obsolete listener deletion, got %d", stub.deletedVS)
	}
	if _, ok := observed.Graph.VirtualServers["remove"]; ok {
		t.Fatalf("expected obsolete listener to be removed from graph: %#v", observed.Graph.VirtualServers)
	}
	if _, ok := observed.Graph.VirtualServers["keep"]; !ok {
		t.Fatalf("expected desired listener to remain in graph: %#v", observed.Graph.VirtualServers)
	}
}

func TestDeleteObsoletePoolsDeletesChildrenBeforePool(t *testing.T) {
	client := &stubPoolClient{}
	deployer := New(&stubClient{}, "")
	deployer.pools = NewPoolManager(client)
	observed := model.ObservedState{Graph: model.NewObservedGraph()}
	observed.Graph.VirtualServers["keep"] = model.ObservedResource{LogicalID: "keep", ExternalID: "vs-keep"}
	observed.Graph.Pools["keep/old"] = model.ObservedResource{LogicalID: "keep/old", ExternalID: "pool-old"}
	observed.Graph.Members["keep/old/member"] = model.ObservedResource{LogicalID: "keep/old/member", ExternalID: "member-old"}

	changed, err := deployer.deleteObsoletePools(context.Background(), "lb-1", &observed, &model.LoadBalancerStack{VirtualServers: []model.VirtualServer{{Name: "keep"}}})
	if err != nil {
		t.Fatalf("deleteObsoletePools: %v", err)
	}
	if !changed {
		t.Fatal("expected obsolete pool cleanup to report a change")
	}
	if len(client.deletedMemberIDs) != 1 || client.deletedMemberIDs[0] != "member-old" || client.deletedPoolID != "pool-old" {
		t.Fatalf("expected member then pool deletion, members=%#v pool=%q", client.deletedMemberIDs, client.deletedPoolID)
	}
	if len(observed.Graph.Pools) != 0 || len(observed.Graph.Members) != 0 {
		t.Fatalf("expected obsolete resources removed from graph: %#v", observed.Graph)
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

func TestEnsureFailsWhenComputeHasNoMatchingPort(t *testing.T) {
	stub := &stubClient{computes: map[string]Compute{"compute-1": {ID: "compute-1", VPCID: "vpc-1", NetworkID: "net-1"}}}
	_, err := New(stub, "").Ensure(context.Background(), EnsureRequest{
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP"},
		Backends:      []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.99", Port: 30080, Weight: 1}},
	})
	if err == nil || !strings.Contains(err.Error(), "absent from CMP compute") {
		t.Fatalf("expected missing compute port to fail, got %v", err)
	}
}

func TestEnsureRejectsAmbiguousComputePorts(t *testing.T) {
	stub := &stubClient{computes: map[string]Compute{"compute-1": {ID: "compute-1", VPCID: "vpc-1", NetworkID: "net-1", Ports: []ComputePort{
		{ID: 1, FixedIPs: []string{"10.0.0.1"}, NetworkID: "net-1", DeviceID: "compute-1", DeviceType: "compute"},
		{ID: 2, FixedIPs: []string{"10.0.0.1"}, NetworkID: "net-1", DeviceID: "compute-1", DeviceType: "compute"},
	}}}}
	_, err := New(stub, "vpc-1").Ensure(context.Background(), EnsureRequest{NetworkID: "net-1", VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080}, Backends: []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30080}}})
	if err == nil || !strings.Contains(err.Error(), "multiple CMP compute ports") {
		t.Fatalf("expected ambiguous identity error, got %v", err)
	}
}

func TestEnsureRejectsVirtualServerCreateWithoutProviderID(t *testing.T) {
	stub := &stubClient{createdVSResult: &VirtualServer{Name: "vs"}}
	_, err := New(stub, "vpc-1").Ensure(context.Background(), EnsureRequest{NetworkID: "net-1", VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080}, Backends: []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 30080}}})
	if err == nil || !strings.Contains(err.Error(), "without a provider id") {
		t.Fatalf("expected missing provider ID error, got %v", err)
	}
}

func TestVirtualServerManagerPreservesTerminalResourceWithoutRepair(t *testing.T) {
	stub := &stubClient{vsList: []VirtualServer{{ID: "vs-failed", Name: "vs", VIPPortID: "7", Status: "Failed", Protocol: "TCP", Port: 8080}}}
	_, _, changed, err := NewVirtualServerManager(stub, "vpc-1").Ensure(context.Background(), VirtualServerEnsureRequest{
		LBServiceID: "lb-1",
		VIPPortID:   "7",
		Desired:     model.VirtualServer{Name: "vs", FrontendPort: 8080, Protocol: "TCP"},
		CurrentID:   "vs-failed",
	})
	if _, ok := IsTerminalProvisioning(err); !ok {
		t.Fatalf("expected terminal provisioning error, got %v", err)
	}
	if changed || stub.deletedVS != 0 {
		t.Fatalf("terminal virtual server must be preserved, changed=%t deleted=%d", changed, stub.deletedVS)
	}
}

func TestVirtualServerManagerDeletesTerminalResourceOnlyForRepair(t *testing.T) {
	stub := &stubClient{vsList: []VirtualServer{{ID: "vs-failed", Name: "vs", VIPPortID: "7", Status: "Failed", Protocol: "TCP", Port: 8080}}}
	_, _, changed, err := NewVirtualServerManager(stub, "vpc-1").Ensure(context.Background(), VirtualServerEnsureRequest{
		LBServiceID:   "lb-1",
		VIPPortID:     "7",
		Desired:       model.VirtualServer{Name: "vs", FrontendPort: 8080, Protocol: "TCP"},
		CurrentID:     "vs-failed",
		RepairTerminal: true,
	})
	if _, ok := IsProvisioningPending(err); !ok {
		t.Fatalf("expected deletion wait during authorized repair, got %v", err)
	}
	if !changed || stub.deletedVS != 1 {
		t.Fatalf("authorized repair must delete exactly once, changed=%t deleted=%d", changed, stub.deletedVS)
	}
}

func TestVirtualServerManagerRepairsTerminalResourceAfterVIPChange(t *testing.T) {
	stub := &stubClient{vsList: []VirtualServer{{ID: "vs-failed", Name: "vs", VIPPortID: "7", Status: "Failed", Protocol: "TCP", Port: 8080}}}
	_, _, changed, err := NewVirtualServerManager(stub, "vpc-1").Ensure(context.Background(), VirtualServerEnsureRequest{
		LBServiceID:   "lb-1",
		VIPPortID:     "8",
		Desired:       model.VirtualServer{Name: "vs", FrontendPort: 8080, Protocol: "TCP"},
		CurrentID:     "vs-failed",
		RepairTerminal: true,
	})
	if _, ok := IsProvisioningPending(err); !ok {
		t.Fatalf("expected deletion wait after VIP change, got %v", err)
	}
	if !changed || stub.deletedVS != 1 {
		t.Fatalf("VIP change must replace the recorded terminal virtual server, changed=%t deleted=%d", changed, stub.deletedVS)
	}
}

func TestVirtualServerManagerPreservesFailedRepairReplacement(t *testing.T) {
	stub := &stubClient{createdVSResult: &VirtualServer{ID: "vs-replacement", Name: "vs", VIPPortID: "7", Status: "Failed", Protocol: "TCP", Port: 8080}}
	_, _, changed, err := NewVirtualServerManager(stub, "vpc-1").Ensure(context.Background(), VirtualServerEnsureRequest{
		LBServiceID:   "lb-1",
		VIPPortID:     "7",
		Desired:       model.VirtualServer{Name: "vs", FrontendPort: 8080, BackendNodePort: 31146, Protocol: "TCP"},
		Backends:      []model.BackendMember{{ComputeID: "compute-1", IP: "10.0.0.1", Port: 31146, Weight: 1}},
		RepairTerminal: true,
	})
	if _, ok := IsTerminalProvisioning(err); !ok {
		t.Fatalf("expected replacement terminal error, got %v", err)
	}
	if !changed || stub.createdVS != 1 || stub.deletedVS != 0 {
		t.Fatalf("failed replacement must remain inspectable, changed=%t created=%d deleted=%d", changed, stub.createdVS, stub.deletedVS)
	}
}

func TestEnsureUsesComputeDetailsWhenBackendHasComputeID(t *testing.T) {
	stub := &stubClient{computes: map[string]Compute{
		"compute-1": {ID: "compute-1", Name: "node-1", VPCID: "vpc-1", NetworkID: "net-1", Ports: []ComputePort{{ID: 38135, FixedIPs: []string{"10.10.1.145"}, NetworkID: "net-1", DeviceID: "compute-1", DeviceType: "compute"}}},
	}}
	_, err := New(stub, "vpc-1").Ensure(context.Background(), EnsureRequest{
		LBName:        "lb",
		NetworkID:     "net-1",
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP", RoutingAlgorithm: "round_robin"},
		Backends:      []model.BackendMember{{ComputeID: "compute-1", IP: "10.10.1.145", Port: 30080, Weight: 1}},
	})
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if len(stub.lastVSSpec.Nodes) != 1 || stub.lastVSSpec.Nodes[0].ResourceID != "compute-1" || stub.lastVSSpec.Nodes[0].BackendPortID != 38135 {
		t.Fatalf("expected compute-backed node spec, got %#v", stub.lastVSSpec.Nodes)
	}
}

func TestEnsureAllowsComputeAndLBSubnetsToDifferWithinVPC(t *testing.T) {
	stub := &stubClient{computes: map[string]Compute{
		"compute-1": {ID: "compute-1", Name: "node-1", VPCID: "vpc-1", NetworkID: "other-net", Ports: []ComputePort{{ID: 38135, FixedIPs: []string{"10.10.1.145"}, NetworkID: "other-net", DeviceID: "compute-1", DeviceType: "compute"}}},
	}}
	_, err := New(stub, "vpc-1").Ensure(context.Background(), EnsureRequest{
		LBName:        "lb",
		NetworkID:     "net-1",
		VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP", RoutingAlgorithm: "round_robin"},
		Backends:      []model.BackendMember{{ComputeID: "compute-1", IP: "10.10.1.145", Port: 30080, Weight: 1}},
	})
	if err != nil {
		t.Fatalf("expected same-VPC cross-subnet backend to succeed, got %v", err)
	}
	if len(stub.lastVSSpec.Nodes) != 1 || stub.lastVSSpec.Nodes[0].BackendPortID != 38135 {
		t.Fatalf("expected backend port from compute subnet, got %#v", stub.lastVSSpec.Nodes)
	}
}

func TestEnsureStackAdoptsProvidedLBSubnetWithoutApplyingDefaultSubnet(t *testing.T) {
	stub := &stubClient{
		lbServices: []LBService{{ID: "lb-provided", Name: "existing", Status: "Created", OperatingStatus: "Active", VPCID: "vpc-1", NetworkID: "lb-subnet", Region: "dev"}},
		vips:       []VIP{{ID: "vip-provided", Address: "10.20.0.10", Status: "Active", NetworkID: "lb-subnet", IPVersion: "IPv4"}},
		computes: map[string]Compute{
			"compute-1": {ID: "compute-1", Name: "worker-1", VPCID: "vpc-1", NetworkID: "compute-subnet", Region: "dev", Ports: []ComputePort{{ID: 38135, FixedIPs: []string{"10.10.1.238"}, NetworkID: "compute-subnet", DeviceID: "compute-1", DeviceType: "compute"}}},
		},
	}
	d := New(stub, "vpc-1")
	result, err := d.EnsureStack(context.Background(), StackEnsureRequest{
		Stack: &model.LoadBalancerStack{
			LBService: model.LBService{Name: "existing", VPCID: "vpc-1", NetworkID: "extension-default-subnet", Region: "dev"},
			VIP:       model.VIP{Name: "vip"},
			VirtualServers: []model.VirtualServer{{Name: "vs", FrontendPort: 8080, BackendNodePort: 30729, Protocol: "TCP", RoutingAlgorithm: "ROUND_ROBIN", DefaultPoolName: "pool"}},
			Pools: []model.Pool{{Name: "pool", Members: []model.BackendMember{{NodeName: "worker-1", ComputeID: "compute-1", IP: "10.10.1.238", Port: 30729, Weight: 1}}}},
		},
		Current:                model.ObservedState{LBServiceID: "lb-provided", VIPPortID: "vip-provided", VIPAddress: "10.20.0.10"},
		StrictLBServiceID:      true,
		StrictVIPPortID:        true,
		AggregateVirtualServer: true,
	})
	if err != nil {
		t.Fatalf("expected provided same-VPC cross-subnet stack to succeed: %v", err)
	}
	if result.Observed.LBServiceID != "lb-provided" || stub.lastVIPSubnet != "lb-subnet" {
		t.Fatalf("expected provided LB subnet adoption, result=%#v vipSubnet=%q", result.Observed, stub.lastVIPSubnet)
	}
	if len(stub.lastVSSpec.Nodes) != 1 || stub.lastVSSpec.Nodes[0].BackendPortID != 38135 {
		t.Fatalf("expected backend port resolved from compute subnet, got %#v", stub.lastVSSpec.Nodes)
	}
}

func TestEnsureStackValidatesExplicitSubnetBelongsToComputeVPC(t *testing.T) {
	stub := &stubClient{networks: map[string]Network{
		"requested-subnet": {ID: "requested-subnet", VPCID: "other-vpc", Status: "Active"},
	}}
	d := New(stub, "vpc-1")
	d.networks = stub
	_, err := d.EnsureStack(context.Background(), StackEnsureRequest{
		Stack: &model.LoadBalancerStack{
			LBService:      model.LBService{Name: "lb", VPCID: "vpc-1", NetworkID: "requested-subnet", Region: "dev"},
			VirtualServers: []model.VirtualServer{{Name: "vs"}},
		},
		NetworkIDExplicit: true,
	})
	if err == nil || !strings.Contains(err.Error(), "belongs to VPC") {
		t.Fatalf("expected explicit subnet/VPC validation failure, got %v", err)
	}
}

func TestVirtualServerManagerRejectsDuplicateFrontendTuple(t *testing.T) {
	stub := &stubClient{vsList: []VirtualServer{{ID: "vs-other", Name: "other-service", VIPPortID: "vip-1", Status: "Active", Protocol: "TCP", Port: 8080}}}
	_, _, _, err := NewVirtualServerManager(stub, "vpc-1").Ensure(context.Background(), VirtualServerEnsureRequest{
		LBServiceID: "lb-1",
		VIPPortID:   "vip-1",
		NetworkID:   "net-1",
		Desired:     model.VirtualServer{Name: "this-service", FrontendPort: 8080, BackendNodePort: 30080, Protocol: "TCP"},
	})
	if err == nil || !strings.Contains(err.Error(), "frontend tuple conflict") {
		t.Fatalf("expected duplicate frontend tuple error, got %v", err)
	}
}

func TestEnsureStackBindsCertificatesToHTTPSVirtualServers(t *testing.T) {
	certClient := &stubCertificateClient{}
	deployer := NewWithResourceManagers(&stubClient{}, "", nil, nil, nil, certClient)
	_, err := deployer.EnsureStack(context.Background(), StackEnsureRequest{Stack: &model.LoadBalancerStack{
		LBService:      model.LBService{Name: "lb"},
		VIP:            model.VIP{Name: "vip"},
		VirtualServers: []model.VirtualServer{{Name: "https-vs", FrontendPort: 443, BackendNodePort: 30443, Protocol: "HTTPS"}},
		Certificates:   []model.Certificate{{Name: "tls", SecretName: "tls-secret"}},
	}})
	if err != nil {
		t.Fatalf("EnsureStack: %v", err)
	}
	if len(certClient.uploaded) != 1 {
		t.Fatalf("expected one certificate upload, got %d", len(certClient.uploaded))
	}
	if len(certClient.bindings) != 1 || certClient.bindings[0].certificateID != "cert-1" || certClient.bindings[0].virtualServerID != "vs-1" {
		t.Fatalf("expected certificate to be bound to the HTTPS virtual server, got %#v", certClient.bindings)
	}
}

func TestEnsureStackRejectsCertificatesUntilCertificateManagerExists(t *testing.T) {
	_, err := New(&stubClient{}, "").EnsureStack(context.Background(), StackEnsureRequest{Stack: &model.LoadBalancerStack{VirtualServers: []model.VirtualServer{{Name: "vs"}}, Certificates: []model.Certificate{{Name: "tls"}}}})
	if err == nil || !strings.Contains(err.Error(), "CertificateManager") {
		t.Fatalf("expected certificate manager error, got %v", err)
	}
}

func TestEnsureStackRecoversVIPByRecoveredVirtualServerAddress(t *testing.T) {
	stub := &stubClient{
		lbServices: []LBService{{ID: "lb-1", Name: "lb", Status: "Active"}},
		vips: []VIP{
			{ID: "28684", Address: "10.1.1.128"},
			{ID: "33672", Address: "10.1.0.17"},
		},
		vsList: []VirtualServer{{
			ID:         "vs-1",
			Name:       "app-vs-a81",
			VIPAddress: "10.1.1.128",
			Status:     "Active",
			Protocol:   "HTTP",
			Port:       8080,
		}},
	}

	stack := &model.LoadBalancerStack{
		LBService: model.LBService{Name: "lb"},
		VIP:       model.VIP{Name: "vip"},
		VirtualServers: []model.VirtualServer{{
			Name:            "app-vs-a81",
			FrontendPort:    8080,
			BackendNodePort: 30080,
			Protocol:        "HTTP",
		}},
	}

	result, err := New(stub, "").EnsureStack(context.Background(), StackEnsureRequest{
		Stack:                       stack,
		Current:                     model.ObservedState{LBServiceID: "lb-1"},
		RecoverVirtualServersByName: true,
	})
	if err != nil {
		t.Fatalf("EnsureStack: %v", err)
	}
	if result.Observed.VIPPortID != "28684" || result.Observed.VIPAddress != "10.1.1.128" {
		t.Fatalf("expected recovered VIP from VS address, got id=%q ip=%q", result.Observed.VIPPortID, result.Observed.VIPAddress)
	}
	if stub.createdVIP != 0 {
		t.Fatalf("expected no VIP create when resolved by recovered VS address, got %d", stub.createdVIP)
	}
}
