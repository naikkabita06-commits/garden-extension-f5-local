package lbaas

import (
	"context"
	"fmt"
	"strings"
)

type VIPManager struct{ client Client }

func NewVIPManager(client Client) *VIPManager { return &VIPManager{client: client} }

func (m *VIPManager) Ensure(ctx context.Context, lbServiceID, subnetID, currentID, currentAddress string, strictExistingID bool) (string, string, bool, error) {
	currentID = strings.TrimSpace(currentID)
	currentAddress = strings.TrimSpace(currentAddress)
	if currentID != "" {
		vips, err := m.client.ListVIPs(ctx, lbServiceID, subnetID)
		if err != nil {
			return currentID, currentAddress, false, err
		}
		for _, vip := range vips {
			if strings.TrimSpace(vip.ID) == currentID {
				address := strings.TrimSpace(vip.Address)
				if address == "" {
					address = currentAddress
				}
				return currentID, address, address != currentAddress, nil
			}
		}
		if strictExistingID {
			return "", "", false, fmt.Errorf(
				"supplied VIP %q was not found under LB service %s",
				currentID,
				lbServiceID,
			)
		}
		// A stale provider ID is drift, not a terminal annotation error. Retain
		// the address only when it can safely identify the desired VIP below.
		currentID = ""
	}
	if strictExistingID {
		return "", "", false, fmt.Errorf("supplied VIP ID is required but missing for LB service %s", lbServiceID)
	}
	// When no stable VIP ID is available we always allocate a fresh VIP. This
	// avoids accidental adoption of an unattached VIP that may belong to another
	// owner/reconcile flow.
	currentAddress = ""
	vip, err := m.client.CreateVIP(ctx, lbServiceID, subnetID)
	if err != nil {
		return "", "", false, fmt.Errorf("creating VIP via CMP on LB %s: %w", lbServiceID, err)
	}
	if strings.TrimSpace(vip.ID) == "" {
		return "", "", false, fmt.Errorf("VIP created but no ID returned")
	}
	address := strings.TrimSpace(vip.Address)
	if address == "" {
		if vips, listErr := m.client.ListVIPs(ctx, lbServiceID, subnetID); listErr == nil {
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
