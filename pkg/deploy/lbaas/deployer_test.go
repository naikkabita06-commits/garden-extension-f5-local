package lbaas

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

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
func (s *stubCertificateClient) UnbindCertificate(context.Context, string, string, string) error { return nil }

type stubClient struct {
	lbServices         []LBService
	vips               []VIP
	vsList             []VirtualServer
	lastLBSpec         LBServiceSpec
	lastVSSpec         VirtualServerSpec
	createdLB          int
	createdVIP         int
	createdVS          int
	createdVSResult    *VirtualServer
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
	if s.createdVSResult != nil {
		return *s.createdVSResult, nil
	}
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
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "lb"}}, vips: []VIP{{ID: "7", Address: "10.0.0.7"}}, vsList: []VirtualServer{{ID: "vs-1", Name: "vs"}}}
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
	first := DesiredBackendHash(80, 30080, []model.BackendMember{{IP: "10.0.0.1", Port: 30080, Weight: 10}})
	second := DesiredBackendHash(80, 30080, []model.BackendMember{{IP: "10.0.0.1", Port: 30081, Weight: 10}})
	if first == second {
		t.Fatal("backend hash must change when the CMP member port changes")
	}
}

func TestDesiredVirtualServerHashIncludesReplacementFields(t *testing.T) {
	backends := []model.BackendMember{{IP: "10.0.0.1", Port: 30080, Weight: 10}}
	base := model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080, Protocol: "HTTP", RoutingAlgorithm: "round_robin"}
	changed := base
	changed.PersistenceType = "source_ip"
	if DesiredVirtualServerHash(base, backends) == DesiredVirtualServerHash(changed, backends) {
		t.Fatal("virtual-server hash must change when a replacement field changes")
	}
}

func TestEnsurePreservesExistingVirtualServerWhenHashIsNotManaged(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "lb"}}, vips: []VIP{{ID: "7", Address: "10.0.0.7"}}, vsList: []VirtualServer{{ID: "vs-1", Name: "vs"}}}
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
	if stub.deletedVS != 0 || stub.createdVS != 1 || !res.Changed || res.Observed.VirtualServerID != "vs-1" {
		t.Fatalf("expected deleted VS drift to be recreated, created=%d deleted=%d changed=%t observed=%#v", stub.createdVS, stub.deletedVS, res.Changed, res.Observed)
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

func TestEnsureRejectsAmbiguousBackendNetworkPortIdentity(t *testing.T) {
	stub := &stubClient{searchNetworkPorts: func(ip string) []NetworkPort {
		return []NetworkPort{{ID: 1, ResourceID: "compute-a", ResourceType: "compute", IP: ip}, {ID: 2, ResourceID: "compute-b", ResourceType: "compute", IP: ip}}
	}}
	_, err := New(stub, "").Ensure(context.Background(), EnsureRequest{VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080}, Backends: []model.BackendMember{{IP: "10.0.0.1", Port: 30080}}})
	if err == nil || !strings.Contains(err.Error(), "ambiguous CMP network ports") {
		t.Fatalf("expected ambiguous identity error, got %v", err)
	}
}

func TestEnsureRejectsVirtualServerCreateWithoutProviderID(t *testing.T) {
	stub := &stubClient{createdVSResult: &VirtualServer{Name: "vs"}}
	_, err := New(stub, "").Ensure(context.Background(), EnsureRequest{VirtualServer: model.VirtualServer{Name: "vs", FrontendPort: 80, BackendNodePort: 30080}, Backends: []model.BackendMember{{IP: "10.0.0.1", Port: 30080}}})
	if err == nil || !strings.Contains(err.Error(), "without a provider id") {
		t.Fatalf("expected missing provider ID error, got %v", err)
	}
}

func TestEnsureStackBindsCertificatesToHTTPSVirtualServers(t *testing.T) {
	certClient := &stubCertificateClient{}
	deployer := NewWithResourceManagers(&stubClient{}, "", nil, nil, nil, certClient)
	_, err := deployer.EnsureStack(context.Background(), StackEnsureRequest{Stack: &model.LoadBalancerStack{
		LBService: model.LBService{Name: "lb"},
		VIP:       model.VIP{Name: "vip"},
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
