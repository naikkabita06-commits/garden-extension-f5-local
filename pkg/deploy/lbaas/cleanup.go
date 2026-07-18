package lbaas

import (
	"context"
	"fmt"
	"strings"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type CleanupRequest struct {
	Current         model.ObservedState
	DeleteVIP       bool
	DeleteLBService bool
}

type CleanupResult struct {
	DeletedVirtualServer bool
	DeletedVIP           bool
	DeletedLBService     bool
}

func (d *Deployer) Cleanup(ctx context.Context, req CleanupRequest) (*CleanupResult, error) {
	result := &CleanupResult{}
	current := req.Current
	if current.LBServiceID == "" {
		return result, nil
	}
	if current.VirtualServerID != "" {
		if err := d.client.DeleteVirtualServer(ctx, current.LBServiceID, current.VirtualServerID); err != nil && !f5client.IsNotFound(err) {
			return result, fmt.Errorf("deleting virtual server %s on LB %s: %w", current.VirtualServerID, current.LBServiceID, err)
		}
		result.DeletedVirtualServer = true
	}
	if req.DeleteVIP && current.VIPPortID != "" {
		if err := d.client.DeleteVIP(ctx, current.LBServiceID, current.VIPPortID); err != nil && !f5client.IsNotFound(err) {
			return result, fmt.Errorf("deleting VIP %s on LB %s: %w", current.VIPPortID, current.LBServiceID, err)
		}
		result.DeletedVIP = true
	}
	if req.DeleteLBService {
		if err := d.client.DeleteLBService(ctx, current.LBServiceID); err != nil && !f5client.IsNotFound(err) {
			return result, fmt.Errorf("deleting LB service %s: %w", current.LBServiceID, err)
		}
		result.DeletedLBService = true
	}
	return result, nil
}

type CleanupDiscoveryRequest struct {
	LBServiceID             string
	VirtualServerNamePrefix string
	DeleteAllVIPs           bool
	DeleteLBService         bool
}

func (d *Deployer) FindLBServiceIDByName(ctx context.Context, name string) (string, error) {
	return d.lbServices.findByName(ctx, name)
}

func (d *Deployer) CleanupDiscovered(ctx context.Context, req CleanupDiscoveryRequest) (*CleanupResult, error) {
	result := &CleanupResult{}
	lbID := strings.TrimSpace(req.LBServiceID)
	if lbID == "" {
		return result, nil
	}
	if strings.TrimSpace(req.VirtualServerNamePrefix) != "" {
		items, err := d.client.ListVirtualServers(ctx, lbID)
		if err != nil {
			return result, err
		}
		for _, vs := range items {
			if strings.HasPrefix(strings.TrimSpace(vs.Name), req.VirtualServerNamePrefix) && strings.TrimSpace(vs.ID) != "" {
				if err := d.client.DeleteVirtualServer(ctx, lbID, strings.TrimSpace(vs.ID)); err != nil && !f5client.IsNotFound(err) {
					return result, fmt.Errorf("deleting virtual server %s on LB %s: %w", strings.TrimSpace(vs.ID), lbID, err)
				}
				result.DeletedVirtualServer = true
			}
		}
	}
	if req.DeleteAllVIPs {
		items, err := d.client.ListVIPs(ctx, lbID)
		if err != nil {
			return result, err
		}
		for _, vip := range items {
			if strings.TrimSpace(vip.ID) == "" {
				continue
			}
			if err := d.client.DeleteVIP(ctx, lbID, strings.TrimSpace(vip.ID)); err != nil && !f5client.IsNotFound(err) {
				return result, fmt.Errorf("deleting VIP %s on LB %s: %w", strings.TrimSpace(vip.ID), lbID, err)
			}
			result.DeletedVIP = true
		}
	}
	if req.DeleteLBService {
		if err := d.client.DeleteLBService(ctx, lbID); err != nil && !f5client.IsNotFound(err) {
			return result, fmt.Errorf("deleting LB service %s: %w", lbID, err)
		}
		result.DeletedLBService = true
	}
	return result, nil
}
