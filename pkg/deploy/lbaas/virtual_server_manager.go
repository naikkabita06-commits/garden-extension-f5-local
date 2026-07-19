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
	Desired                 model.VirtualServer
	Backends                []model.BackendMember
	CurrentID               string
	CurrentHash             string
	DesiredHash             string
	RecreateWhenHashMissing bool
}

func (m *VirtualServerManager) Ensure(ctx context.Context, req VirtualServerEnsureRequest) (string, string, bool, error) {
	currentID := strings.TrimSpace(req.CurrentID)
	changed := false
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
	// Never adopt a listener merely because its display name matches. The CMP
	// listener response lacks ownership metadata, so only a persisted provider
	// ID can prove this controller owns the resource.
	if currentID != "" {
		if req.CurrentHash != "" {
			if req.CurrentHash == req.DesiredHash {
				return currentID, req.Desired.Name, false, nil
			}
		} else if !req.RecreateWhenHashMissing {
			return currentID, req.Desired.Name, false, nil
		}
		if err := m.client.DeleteVirtualServer(ctx, req.LBServiceID, currentID); err != nil && !f5client.IsNotFound(err) {
			return currentID, req.Desired.Name, false, fmt.Errorf("deleting virtual server %s on LB %s: %w", currentID, req.LBServiceID, err)
		}
		currentID = ""
		changed = true
	}
	createdID, err := m.create(ctx, req.LBServiceID, req.VIPPortID, req.Desired, req.Backends)
	if err != nil {
		return "", "", changed, err
	}
	return createdID, req.Desired.Name, true, nil
}

func (m *VirtualServerManager) findByName(ctx context.Context, lbServiceID, name string) (string, error) {
	list, err := m.client.ListVirtualServers(ctx, lbServiceID)
	if err != nil {
		return "", err
	}
	for _, vs := range list {
		if strings.TrimSpace(vs.Name) == name && strings.TrimSpace(vs.ID) != "" {
			return strings.TrimSpace(vs.ID), nil
		}
	}
	return "", nil
}

func (m *VirtualServerManager) create(ctx context.Context, lbServiceID, vipPortID string, vs model.VirtualServer, backends []model.BackendMember) (string, error) {
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
		port, err := m.resolveBackendPort(ctx, backend.IP)
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
	if strings.TrimSpace(created.Name) != "" {
		return strings.TrimSpace(created.Name), nil
	}
	return vs.Name, nil
}

func (m *VirtualServerManager) poolMemberSpecs(ctx context.Context, backends []model.BackendMember) ([]PoolMemberSpec, error) {
	out := make([]PoolMemberSpec, 0, len(backends))
	for _, backend := range backends {
		port, err := m.resolveBackendPort(ctx, backend.IP)
		if err != nil {
			return nil, err
		}
		out = append(out, PoolMemberSpec{ResourceID: port.ResourceID, ResourceType: port.ResourceType, ResourceIP: port.IP, BackendPortID: port.ID, Port: backend.Port, Weight: backend.Weight})
	}
	return out, nil
}

func (m *VirtualServerManager) resolveBackendPort(ctx context.Context, ip string) (NetworkPort, error) {
	ports, err := m.client.SearchNetworkPortsByIP(ctx, ip)
	if err != nil {
		return NetworkPort{}, fmt.Errorf("searching CMP network port for backend IP %s: %w", ip, err)
	}
	for _, port := range ports {
		if strings.TrimSpace(port.IP) == strings.TrimSpace(ip) && port.ID != 0 {
			if strings.TrimSpace(port.ResourceType) == "" {
				port.ResourceType = "compute"
			}
			if strings.TrimSpace(port.ResourceID) == "" {
				return NetworkPort{}, fmt.Errorf("CMP network port for backend IP %s has no resource_id", ip)
			}
			return port, nil
		}
	}
	return NetworkPort{}, fmt.Errorf("no CMP network port found for backend IP %s", ip)
}
