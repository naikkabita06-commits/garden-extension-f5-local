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
	Region                  string
	Desired                 model.VirtualServer
	Backends                []model.BackendMember
	Pool                    *model.Pool
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
	for _, listener := range listeners {
		if strings.TrimSpace(listener.ID) == currentID || strings.TrimSpace(listener.Name) == strings.TrimSpace(req.Desired.Name) {
			continue
		}
		if listener.Port != 0 && listener.Port != req.Desired.FrontendPort {
			continue
		}
		if strings.TrimSpace(listener.Protocol) != "" && !strings.EqualFold(listener.Protocol, req.Desired.Protocol) {
			continue
		}
		detail, getErr := m.client.GetVirtualServer(ctx, req.LBServiceID, listener.ID)
		if getErr != nil {
			return "", "", false, fmt.Errorf("checking virtual-server frontend conflict %s: %w", listener.ID, getErr)
		}
		if strings.TrimSpace(detail.VIPPortID) == strings.TrimSpace(req.VIPPortID) && detail.Port == req.Desired.FrontendPort && strings.EqualFold(detail.Protocol, req.Desired.Protocol) {
			return "", "", false, fmt.Errorf("frontend tuple conflict: VIP %s %s/%d is already used by virtual server %s", req.VIPPortID, strings.ToUpper(req.Desired.Protocol), req.Desired.FrontendPort, detail.ID)
		}
	}
	if currentID != "" {
		found := false
		for _, listener := range listeners {
			if strings.TrimSpace(listener.ID) == currentID {
				found = true
				detail, getErr := m.client.GetVirtualServer(ctx, req.LBServiceID, currentID)
				if getErr != nil {
					return "", "", false, fmt.Errorf("checking recorded virtual server %s: %w", currentID, getErr)
				}
				if err := validateVirtualServer(detail, req.VIPPortID, req.Desired); err != nil {
					if _, terminal := IsTerminalProvisioning(err); terminal {
						return m.deleteFailed(ctx, req.LBServiceID, detail, err)
					}
					return "", "", false, err
				}
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
			candidate, getErr := m.client.GetVirtualServer(ctx, req.LBServiceID, adoptedID)
			if getErr != nil {
				return "", "", false, fmt.Errorf("checking recovered virtual server %s: %w", adoptedID, getErr)
			}
			if err := validateVirtualServer(candidate, req.VIPPortID, req.Desired); err != nil {
				if _, terminal := IsTerminalProvisioning(err); terminal {
					return m.deleteFailed(ctx, req.LBServiceID, candidate, err)
				}
				return "", "", false, err
			}
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
		return currentID, req.Desired.Name, true, &ProvisioningPendingError{ResourceType: "VirtualServer", ResourceID: currentID, Status: "deleting", Detail: "waiting for CMP deletion before replacement"}
	}
	createdID, err := m.create(ctx, req.LBServiceID, req.VIPPortID, req.NetworkID, req.Region, req.Desired, req.Pool, req.Backends)
	if err != nil {
		return "", "", changed, err
	}
	created, err := m.client.GetVirtualServer(ctx, req.LBServiceID, createdID)
	if err != nil {
		if f5client.IsNotFound(err) {
			return "", "", true, &ProvisioningPendingError{ResourceType: "VirtualServer", ResourceID: createdID, Status: "creating", Detail: "accepted by CMP but not yet readable"}
		}
		return "", "", true, fmt.Errorf("checking created virtual server %s: %w", createdID, err)
	}
	if err := validateVirtualServer(created, req.VIPPortID, req.Desired); err != nil {
		if _, terminal := IsTerminalProvisioning(err); terminal {
			return m.deleteFailed(ctx, req.LBServiceID, created, err)
		}
		return "", "", true, err
	}
	return createdID, req.Desired.Name, true, nil
}

func (m *VirtualServerManager) deleteFailed(ctx context.Context, lbServiceID string, vs VirtualServer, cause error) (string, string, bool, error) {
	if err := m.client.DeleteVirtualServer(ctx, lbServiceID, vs.ID); err != nil && !f5client.IsNotFound(err) {
		return vs.ID, vs.Name, false, fmt.Errorf("deleting failed virtual server %s after %v: %w", vs.ID, cause, err)
	}
	return vs.ID, vs.Name, true, &ProvisioningPendingError{ResourceType: "VirtualServer", ResourceID: vs.ID, Status: "deleting", Detail: "failed managed virtual server deleted; waiting for CMP deletion before replacement"}
}

func validateVirtualServer(vs VirtualServer, vipPortID string, desired model.VirtualServer) error {
	if strings.TrimSpace(vs.VIPPortID) != "" && strings.TrimSpace(vs.VIPPortID) != strings.TrimSpace(vipPortID) {
		return fmt.Errorf("virtual server %s uses VIP %q, expected %q", vs.ID, vs.VIPPortID, vipPortID)
	}
	if vs.Port != 0 && vs.Port != desired.FrontendPort {
		return fmt.Errorf("virtual server %s uses frontend port %d, expected %d", vs.ID, vs.Port, desired.FrontendPort)
	}
	if strings.TrimSpace(vs.Protocol) != "" && !strings.EqualFold(vs.Protocol, desired.Protocol) {
		return fmt.Errorf("virtual server %s uses protocol %q, expected %q", vs.ID, vs.Protocol, desired.Protocol)
	}
	status := strings.ToLower(strings.TrimSpace(vs.Status))
	switch status {
	case "active", "created", "ready", "available", "online":
		return nil
	case "failed", "error", "errored":
		return &TerminalProvisioningError{ResourceType: "VirtualServer", ResourceID: vs.ID, Status: vs.Status}
	default:
		return &ProvisioningPendingError{ResourceType: "VirtualServer", ResourceID: vs.ID, Status: vs.Status, Detail: "waiting for CMP virtual server readiness"}
	}
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

func (m *VirtualServerManager) create(ctx context.Context, lbServiceID, vipPortID, networkID, region string, vs model.VirtualServer, pool *model.Pool, backends []model.BackendMember) (string, error) {
	spec := VirtualServerSpec{
		Name:               vs.Name,
		VIPPortID:          vipPortID,
		Protocol:           vs.Protocol,
		Port:               vs.FrontendPort,
		RoutingAlgorithm:   vs.RoutingAlgorithm,
		PersistenceType:    vs.PersistenceType,
		DrainingTimeout:    vs.DrainingTimeout,
		VPCID:              m.vpcID,
		AllowedCIDRs:       append([]string(nil), vs.SourceRanges...),
		PoolName:           vs.DefaultPoolName,
		PersistenceEnabled: strings.TrimSpace(vs.PersistenceType) != "",
		XForwardedFor:      true,
	}
	if vs.Monitor != nil {
		spec.MonitorInterval = vs.Monitor.Interval
		spec.MonitorName = vs.Monitor.Name
		spec.MonitorProtocol = vs.Monitor.Type
		spec.MonitorPath = vs.Monitor.Path
		spec.MonitorPort = vs.Monitor.Port
		if spec.MonitorPort == 0 {
			spec.MonitorPort = vs.BackendNodePort
		}
		spec.MonitorTimeout = vs.Monitor.Timeout
	}
	for _, backend := range backends {
		port, err := m.resolveBackendPort(ctx, backend, networkID, region)
		if err != nil {
			return "", err
		}
		spec.Nodes = append(spec.Nodes, BackendNodeSpec{
			ResourceID:     port.ResourceID,
			ResourceType:   port.ResourceType,
			ResourceIP:     port.IP,
			BackendPortID:  port.ID,
			Port:           backend.Port,
			Weight:         backend.Weight,
			MaxConnections: 100,
			InstanceName:   port.InstanceName,
			SourceType:     "vm",
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
		port, err := m.resolveBackendPort(ctx, backend, networkID, "")
		if err != nil {
			return nil, err
		}
		out = append(out, PoolMemberSpec{ResourceID: port.ResourceID, ResourceType: port.ResourceType, ResourceIP: port.IP, BackendPortID: port.ID, Port: backend.Port, Weight: backend.Weight})
	}
	return out, nil
}

func (m *VirtualServerManager) resolveBackendPort(ctx context.Context, backend model.BackendMember, networkID, region string) (NetworkPort, error) {
	ip := strings.TrimSpace(backend.IP)
	computeID := strings.TrimSpace(backend.ComputeID)
	if computeID == "" {
		return NetworkPort{}, fmt.Errorf("node %q has no CMP compute identity; expected spec.providerID=cmp://<uuid>", backend.NodeName)
	}
	return m.resolveComputeBackendPort(ctx, computeID, ip, networkID, region)
}

func (m *VirtualServerManager) resolveComputeBackendPort(ctx context.Context, computeID, ip, networkID, region string) (NetworkPort, error) {
	compute, err := m.client.GetCompute(ctx, computeID)
	if err != nil {
		return NetworkPort{}, fmt.Errorf("fetching CMP compute %s: %w", computeID, err)
	}
	if strings.TrimSpace(m.vpcID) != "" && strings.TrimSpace(compute.VPCID) == "" {
		return NetworkPort{}, fmt.Errorf("CMP compute %s response has no VPC identity", computeID)
	}
	if strings.TrimSpace(m.vpcID) != "" && strings.TrimSpace(compute.VPCID) != strings.TrimSpace(m.vpcID) {
		return NetworkPort{}, fmt.Errorf("CMP compute %s belongs to VPC %q, expected %q", computeID, compute.VPCID, m.vpcID)
	}
	if strings.TrimSpace(networkID) != "" && strings.TrimSpace(compute.NetworkID) == "" {
		return NetworkPort{}, fmt.Errorf("CMP compute %s response has no subnet identity", computeID)
	}
	if strings.TrimSpace(networkID) != "" && strings.TrimSpace(compute.NetworkID) != strings.TrimSpace(networkID) {
		return NetworkPort{}, fmt.Errorf("CMP compute %s belongs to subnet %q, expected %q", computeID, compute.NetworkID, networkID)
	}
	if strings.TrimSpace(region) != "" {
		if strings.TrimSpace(compute.Region) == "" {
			return NetworkPort{}, fmt.Errorf("CMP compute %s response has no region", computeID)
		}
		if !strings.EqualFold(strings.TrimSpace(compute.Region), strings.TrimSpace(region)) {
			return NetworkPort{}, fmt.Errorf("CMP compute %s belongs to region %q, expected %q", computeID, compute.Region, region)
		}
	}

	var match *ComputePort
	for _, port := range compute.Ports {
		if !containsString(port.FixedIPs, ip) || port.ID == 0 {
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
	return NetworkPort{ID: match.ID, ResourceID: computeID, ResourceType: "compute", IP: ip, NetworkID: match.NetworkID, DeviceID: match.DeviceID, DeviceType: match.DeviceType, InstanceName: compute.Name}, nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == strings.TrimSpace(wanted) {
			return true
		}
	}
	return false
}
