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
	vips, err := m.client.ListVIPs(ctx, lbServiceID)
	if err != nil {
		return currentID, currentAddress, false, err
	}
	if currentID != "" {
		for _, vip := range vips {
			if strings.TrimSpace(vip.ID) == currentID {
				address := strings.TrimSpace(vip.Address)
				if address == "" {
					address = currentAddress
				}
				return currentID, address, address != currentAddress, nil
			}
		}
		// A stale provider ID is drift, not a terminal annotation error. Retain
		// the address only when it can safely identify the desired VIP below.
		currentID = ""
	}
	if currentAddress != "" {
		for _, vip := range vips {
			if strings.TrimSpace(vip.Address) == currentAddress && strings.TrimSpace(vip.ID) != "" {
				return strings.TrimSpace(vip.ID), currentAddress, true, nil
			}
		}
		// The recorded address disappeared along with the provider VIP. Clear it
		// and allocate a replacement only when no ambiguous VIP already exists.
		currentAddress = ""
	}
	if len(vips) > 0 {
		return "", "", false, fmt.Errorf("cannot adopt VIP for LB service %s without a stable VIP id/address; found %d existing VIP(s)", lbServiceID, len(vips))
	}
	vip, err := m.client.CreateVIP(ctx, lbServiceID)
	if err != nil {
		return "", "", false, fmt.Errorf("creating VIP via CMP on LB %s: %w", lbServiceID, err)
	}
	if strings.TrimSpace(vip.ID) == "" {
		return "", "", false, fmt.Errorf("VIP created but no ID returned")
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
	return strings.TrimSpace(vip.ID), address, true, nil
}
