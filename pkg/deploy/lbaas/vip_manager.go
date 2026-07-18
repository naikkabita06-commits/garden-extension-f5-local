package lbaas

import (
	"context"
	"fmt"
	"strings"
)

type VIPManager struct{ client Client }

func NewVIPManager(client Client) *VIPManager { return &VIPManager{client: client} }

func (m *VIPManager) Ensure(ctx context.Context, lbServiceID, currentID, currentAddress string) (string, string, bool, error) {
	currentID = strings.TrimSpace(currentID)
	currentAddress = strings.TrimSpace(currentAddress)
	if currentID != "" && currentAddress != "" {
		return currentID, currentAddress, false, nil
	}
	vipID, vipAddr, err := m.findOrCreate(ctx, lbServiceID)
	if err != nil {
		return currentID, currentAddress, false, err
	}
	if currentID == "" {
		currentID = vipID
	}
	if currentAddress == "" {
		currentAddress = vipAddr
	}
	return currentID, currentAddress, true, nil
}

func (m *VIPManager) findOrCreate(ctx context.Context, lbServiceID string) (string, string, error) {
	vips, err := m.client.ListVIPs(ctx, lbServiceID)
	if err != nil {
		return "", "", err
	}
	for _, vip := range vips {
		if strings.TrimSpace(vip.ID) != "" {
			return strings.TrimSpace(vip.ID), strings.TrimSpace(vip.Address), nil
		}
	}
	vip, err := m.client.CreateVIP(ctx, lbServiceID)
	if err != nil {
		return "", "", fmt.Errorf("creating VIP via CMP on LB %s: %w", lbServiceID, err)
	}
	if strings.TrimSpace(vip.ID) == "" {
		return "", "", fmt.Errorf("VIP created but no ID returned")
	}
	address := strings.TrimSpace(vip.Address)
	if address == "" {
		if vips, listErr := m.client.ListVIPs(ctx, lbServiceID); listErr == nil {
			for _, found := range vips {
				if strings.TrimSpace(found.ID) == strings.TrimSpace(vip.ID) && strings.TrimSpace(found.Address) != "" {
					address = strings.TrimSpace(found.Address)
					break
				}
			}
		}
	}
	return strings.TrimSpace(vip.ID), address, nil
}
