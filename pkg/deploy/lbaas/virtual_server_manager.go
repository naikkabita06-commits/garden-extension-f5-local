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
	if currentID == "" {
		foundID, err := m.findByName(ctx, req.LBServiceID, req.Desired.Name)
		if err != nil {
			return "", "", false, err
		}
		currentID = foundID
	}
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
	for i, backend := range backends {
		spec.Nodes = append(spec.Nodes, BackendNodeSpec{
			ResourceID:    backend.IP,
			ResourceType:  "compute",
			ResourceIP:    backend.IP,
			BackendPortID: i + 1,
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
