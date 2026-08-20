package lbaas

import (
	"context"
	"fmt"
	"strings"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type VirtualServerManager struct {
	client Client
	vpcID  string
}

func NewVirtualServerManager(client Client, vpcID string) *VirtualServerManager {
	return &VirtualServerManager{client: client, vpcID: strings.TrimSpace(vpcID)}
}

type VirtualServerEnsureRequest struct {
	LBServiceID             string
	VIPPortID               string
	NetworkID               string
	Desired                 model.VirtualServer
	Backends                []model.BackendMember
	CurrentID               string
	CurrentHash             string
	DesiredHash             string
	RecreateWhenHashMissing bool
	RecoverByName           bool
}

func (m *VirtualServerManager) Ensure(ctx context.Context, req VirtualServerEnsureRequest) (string, string, bool, error) {
	currentID := strings.TrimSpace(req.CurrentID)
	changed := false
	adoptedByName := false
	// Re-list listeners on every reconcile. Recorded IDs may have been deleted
	// outside the extension; a stale graph must never be trusted as existence.
	listeners, err := m.client.ListVirtualServers(ctx, req.LBServiceID)
	if err != nil {
		return "", "", false, err
	}
	if currentID != "" {
		found := false
		for _, listener := range listeners {
			if strings.TrimSpace(listener.ID) == currentID {
				found = true
				break
			}
		}
		if !found {
			currentID = ""
		}
	}
	if currentID == "" && req.RecoverByName {
		adoptedID, err := findUniqueVirtualServerByName(listeners, req.Desired.Name)
		if err != nil {
			return "", "", false, err
		}
		if adoptedID != "" {
			currentID = adoptedID
			adoptedByName = true
		}
	}
	if currentID != "" {
		if req.CurrentHash != "" {
			if req.CurrentHash == req.DesiredHash {
				return currentID, req.Desired.Name, false, nil
			}
		} else if !req.RecreateWhenHashMissing || adoptedByName {
			return currentID, req.Desired.Name, false, nil
		}
		if err := m.client.DeleteVirtualServer(ctx, req.LBServiceID, currentID); err != nil && !f5client.IsNotFound(err) {
			return currentID, req.Desired.Name, false, fmt.Errorf("deleting virtual server %s on LB %s: %w", currentID, req.LBServiceID, err)
		}
		currentID = ""
		changed = true
	}
	createdID, err := m.create(ctx, req.LBServiceID, req.VIPPortID, req.NetworkID, req.Desired, req.Backends)
	if err != nil {
		return "", "", changed, err
	}
	return createdID, req.Desired.Name, true, nil
}

func findUniqueVirtualServerByName(list []VirtualServer, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	foundID := ""
	for _, vs := range list {
		if strings.TrimSpace(vs.Name) != name || strings.TrimSpace(vs.ID) == "" {
			continue
		}
		if foundID != "" {
			return "", fmt.Errorf("multiple virtual servers named %q found; refusing ambiguous adoption", name)
		}
		foundID = strings.TrimSpace(vs.ID)
	}
	return foundID, nil
}

func (m *VirtualServerManager) create(ctx context.Context, lbServiceID, vipPortID, networkID string, vs model.VirtualServer, backends []model.BackendMember) (string, error) {
	spec := VirtualServerSpec{
		Name:             vs.Name,
		VIPPortID:        vipPortID,
		Protocol:         vs.Protocol,
		Port:             vs.FrontendPort,
		RoutingAlgorithm: vs.RoutingAlgorithm,
		PersistenceType:  vs.PersistenceType,
		DrainingTimeout:  vs.DrainingTimeout,
		VPCID:            m.vpcID,
		AllowedCIDRs:     append([]string(nil), vs.SourceRanges...),
	}
	if vs.Monitor != nil {
		spec.MonitorInterval = vs.Monitor.Interval
		spec.MonitorType = vs.Monitor.Type
		spec.MonitorPath = vs.Monitor.Path
	}
	for _, backend := range backends {
		port, err := m.resolveBackendPort(ctx, backend, networkID)
		if err != nil {
			return "", err
		}
		spec.Nodes = append(spec.Nodes, BackendNodeSpec{
			ResourceID:    port.ResourceID,
			ResourceType:  port.ResourceType,
			ResourceIP:    port.IP,
			BackendPortID: port.ID,
			Port:          backend.Port,
			Weight:        backend.Weight,
		})
	}
	created, err := m.client.CreateVirtualServer(ctx, lbServiceID, spec)
	if err != nil {
		return "", fmt.Errorf("creating virtual server via CMP: %w", err)
	}
	if strings.TrimSpace(created.ID) != "" {
		return strings.TrimSpace(created.ID), nil
	}
	// A successful asynchronous response without a provider ID cannot be
	// represented as an observed resource. Do not fabricate an ID from a name;
	// callers must requeue and rediscover through a documented provider ID.
	return "", fmt.Errorf("CMP created virtual server %q without a provider id", vs.Name)
}

func (m *VirtualServerManager) poolMemberSpecs(ctx context.Context, backends []model.BackendMember, networkID string) ([]PoolMemberSpec, error) {
	out := make([]PoolMemberSpec, 0, len(backends))
	for _, backend := range backends {
		port, err := m.resolveBackendPort(ctx, backend, networkID)
		if err != nil {
			return nil, err
		}
		out = append(out, PoolMemberSpec{ResourceID: port.ResourceID, ResourceType: port.ResourceType, ResourceIP: port.IP, BackendPortID: port.ID, Port: backend.Port, Weight: backend.Weight})
	}
	return out, nil
}

func (m *VirtualServerManager) resolveBackendPort(ctx context.Context, backend model.BackendMember, networkID string) (NetworkPort, error) {
	ip := strings.TrimSpace(backend.IP)
	computeID := strings.TrimSpace(backend.ComputeID)
	if computeID != "" {
		return m.resolveComputeBackendPort(ctx, computeID, ip, networkID)
	}

	ports, err := m.client.SearchNetworkPortsByIP(ctx, ip)
	if err != nil {
		return NetworkPort{}, fmt.Errorf("searching CMP network port for backend IP %s: %w", ip, err)
	}
	var match *NetworkPort
	for _, port := range ports {
		if strings.TrimSpace(port.IP) == strings.TrimSpace(ip) && port.ID != 0 {
			if strings.TrimSpace(port.ResourceType) == "" {
				port.ResourceType = "compute"
			}
			if strings.TrimSpace(networkID) != "" && strings.TrimSpace(port.NetworkID) != "" && strings.TrimSpace(port.NetworkID) != strings.TrimSpace(networkID) {
				continue
			}
			if strings.TrimSpace(port.ResourceID) == "" {
				return NetworkPort{}, fmt.Errorf("CMP network port for backend IP %s has no resource_id", ip)
			}
			if match != nil {
				return NetworkPort{}, fmt.Errorf("ambiguous CMP network ports for backend IP %s; refusing backend identity adoption", ip)
			}
			candidate := port
			match = &candidate
		}
	}
	if match != nil {
		return *match, nil
	}
	return NetworkPort{}, fmt.Errorf("no CMP network port found for backend IP %s", ip)
}

func (m *VirtualServerManager) resolveComputeBackendPort(ctx context.Context, computeID, ip, networkID string) (NetworkPort, error) {
	compute, err := m.client.GetCompute(ctx, computeID)
	if err != nil {
		return NetworkPort{}, fmt.Errorf("fetching CMP compute %s: %w", computeID, err)
	}
	if strings.TrimSpace(m.vpcID) != "" && strings.TrimSpace(compute.VPCID) != "" && strings.TrimSpace(compute.VPCID) != strings.TrimSpace(m.vpcID) {
		return NetworkPort{}, fmt.Errorf("CMP compute %s belongs to VPC %q, expected %q", computeID, compute.VPCID, m.vpcID)
	}
	if strings.TrimSpace(networkID) != "" && strings.TrimSpace(compute.NetworkID) != "" && strings.TrimSpace(compute.NetworkID) != strings.TrimSpace(networkID) {
		return NetworkPort{}, fmt.Errorf("CMP compute %s belongs to subnet %q, expected %q", computeID, compute.NetworkID, networkID)
	}

	var match *ComputePort
	for _, port := range compute.Ports {
		if strings.TrimSpace(port.IP) != ip || port.ID == 0 {
			continue
		}
		if strings.TrimSpace(port.DeviceID) != "" && strings.TrimSpace(port.DeviceID) != computeID {
			return NetworkPort{}, fmt.Errorf("CMP compute %s port %d has device_id %q", computeID, port.ID, port.DeviceID)
		}
		if strings.TrimSpace(port.DeviceType) != "" && !strings.EqualFold(strings.TrimSpace(port.DeviceType), "compute") {
			return NetworkPort{}, fmt.Errorf("CMP compute %s port %d has device_type %q", computeID, port.ID, port.DeviceType)
		}
		if strings.TrimSpace(networkID) != "" && strings.TrimSpace(port.NetworkID) != "" && strings.TrimSpace(port.NetworkID) != strings.TrimSpace(networkID) {
			return NetworkPort{}, fmt.Errorf("CMP compute %s port %d belongs to subnet %q, expected %q", computeID, port.ID, port.NetworkID, networkID)
		}
		if match != nil {
			return NetworkPort{}, fmt.Errorf("multiple CMP compute ports match backend IP %s on compute %s", ip, computeID)
		}
		candidate := port
		match = &candidate
	}
	if match == nil {
		return NetworkPort{}, fmt.Errorf("backend IP %s is absent from CMP compute %s ports", ip, computeID)
	}
	return NetworkPort{ID: match.ID, ResourceID: computeID, ResourceType: "compute", IP: ip, NetworkID: match.NetworkID, DeviceID: match.DeviceID, DeviceType: match.DeviceType}, nil
}
