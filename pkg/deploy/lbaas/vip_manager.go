package lbaas

import (
	"context"
	"fmt"
	"strings"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
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
				if err := validateVIP(vip, subnetID, strictExistingID); err != nil {
					if _, terminal := IsTerminalProvisioning(err); terminal && !strictExistingID {
						if deleteErr := m.client.DeleteVIP(ctx, lbServiceID, currentID); deleteErr != nil && !f5client.IsNotFound(deleteErr) {
							return currentID, currentAddress, false, fmt.Errorf("deleting failed managed VIP %q: %w", currentID, deleteErr)
						}
						return currentID, currentAddress, true, &ProvisioningPendingError{ResourceType: "VIP", ResourceID: currentID, Status: "deleting", Detail: "failed managed VIP deleted; waiting before replacement"}
					}
					return currentID, currentAddress, false, err
				}
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

func validateVIP(vip VIP, subnetID string, strict bool) error {
	if strings.TrimSpace(vip.NetworkID) != "" && strings.TrimSpace(subnetID) != "" && strings.TrimSpace(vip.NetworkID) != strings.TrimSpace(subnetID) {
		return fmt.Errorf("VIP %q belongs to subnet %q, expected %q", vip.ID, vip.NetworkID, subnetID)
	}
	if version := strings.TrimSpace(vip.IPVersion); version != "" && !strings.EqualFold(version, "IPv4") {
		return fmt.Errorf("VIP %q uses unsupported IP version %q", vip.ID, version)
	}
	status := strings.ToLower(strings.TrimSpace(vip.Status))
	if strict && status == "" {
		return fmt.Errorf("supplied VIP %q response has no status", vip.ID)
	}
	switch status {
	case "", "active", "created", "ready", "available":
		return nil
	case "failed", "error", "errored":
		return &TerminalProvisioningError{ResourceType: "VIP", ResourceID: vip.ID, Status: vip.Status}
	default:
		return &ProvisioningPendingError{ResourceType: "VIP", ResourceID: vip.ID, Status: vip.Status, Detail: "waiting for CMP VIP readiness"}
	}
}
