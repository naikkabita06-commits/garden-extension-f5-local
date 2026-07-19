package lbaas

import (
	"context"
	"fmt"
	"strings"
)

type LBServiceManager struct {
	client Client
	vpcID  string
}

func NewLBServiceManager(client Client, vpcID string) *LBServiceManager {
	return &LBServiceManager{client: client, vpcID: strings.TrimSpace(vpcID)}
}

func (m *LBServiceManager) Ensure(ctx context.Context, req EnsureRequest, currentID string) (string, bool, error) {
	currentID = strings.TrimSpace(currentID)
	items, err := m.client.ListLBServices(ctx)
	if err != nil {
		return "", false, err
	}
	if currentID != "" {
		for _, svc := range items {
			if strings.TrimSpace(svc.ID) == currentID {
				return currentID, false, nil
			}
		}
		// The graph is an observation, not a source of truth. A provider-side
		// deletion must converge back to the desired named LBService.
		currentID = ""
	}
	// A display-name match is never ownership proof. CMP's currently used
	// LBService API does not expose a stable ownership/tag field, so adoption
	// is deliberately forbidden until that contract exists. This avoids
	// mutating an unrelated same-name resource.
	createdID, err := m.create(ctx, req)
	if err != nil {
		return "", false, err
	}
	return createdID, true, nil
}

func findUniqueLBServiceByName(items []LBService, name string) (string, error) {
	var foundID string
	for _, svc := range items {
		if strings.TrimSpace(svc.Name) != name || strings.TrimSpace(svc.ID) == "" {
			continue
		}
		if foundID != "" {
			return "", fmt.Errorf("multiple LB services named %q found; refusing name-only adoption", name)
		}
		foundID = strings.TrimSpace(svc.ID)
	}
	return foundID, nil
}

func (m *LBServiceManager) findByName(ctx context.Context, name string) (string, error) {
	items, err := m.client.ListLBServices(ctx)
	if err != nil {
		return "", err
	}
	return findUniqueLBServiceByName(items, name)
}

func (m *LBServiceManager) create(ctx context.Context, req EnsureRequest) (string, error) {
	vpcID := strings.TrimSpace(req.VPCID)
	if vpcID == "" {
		vpcID = m.vpcID
	}
	created, err := m.client.CreateLBService(ctx, LBServiceSpec{
		Name:        req.LBName,
		Description: req.LBDescription,
		FlavorID:    req.FlavorID,
		NetworkID:   req.NetworkID,
		VPCID:       vpcID,
		VPCName:     req.VPCName,
	})
	if err != nil {
		return "", fmt.Errorf("creating LB service via CMP: %w", err)
	}
	if strings.TrimSpace(created.ID) == "" {
		return "", fmt.Errorf("LB Service created but no ID returned")
	}
	return strings.TrimSpace(created.ID), nil
}
