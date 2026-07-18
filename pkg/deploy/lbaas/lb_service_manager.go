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
	if strings.TrimSpace(currentID) != "" {
		return strings.TrimSpace(currentID), false, nil
	}
	foundID, err := m.findByName(ctx, req.LBName)
	if err != nil {
		return "", false, err
	}
	if foundID != "" {
		return foundID, false, nil
	}
	createdID, err := m.create(ctx, req)
	if err != nil {
		return "", false, err
	}
	return createdID, true, nil
}

func (m *LBServiceManager) findByName(ctx context.Context, name string) (string, error) {
	items, err := m.client.ListLBServices(ctx)
	if err != nil {
		return "", err
	}
	for _, svc := range items {
		if strings.TrimSpace(svc.Name) == name && strings.TrimSpace(svc.ID) != "" {
			return strings.TrimSpace(svc.ID), nil
		}
	}
	return "", nil
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
