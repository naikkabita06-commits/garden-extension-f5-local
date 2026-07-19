package lbaas

import (
	"context"
	"fmt"
	"sort"
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

// CleanupStack deletes exactly the provider resources recorded in an observed
// graph. It intentionally never discovers resources by a display-name prefix
// and never enumerates all VIPs: both approaches can delete another owner's
// resources on a shared LBService.
//
// Child resources are deleted before their parents in provider dependency
// order: routing rules, members, monitors, pools, virtual servers, VIP, then
// the LBService. Parent deletion is opt-in so the caller can retain shared
// parents after it has validated references in its Kubernetes scope.
func (d *Deployer) CleanupStack(ctx context.Context, req CleanupRequest) (*CleanupResult, error) {
	current := req.Current
	current.EnsureGraph()
	result := &CleanupResult{}
	lbID := current.LBServiceID
	if lbID == "" {
		for _, resource := range current.Graph.LBServices {
			lbID = resource.ExternalID
			break
		}
	}
	if lbID == "" {
		return result, nil
	}

	for _, rule := range sortedResources(current.Graph.RoutingRules) {
		if d.routingRules == nil {
			return result, fmt.Errorf("routing-rule cleanup requires a RoutingRuleManager")
		}
		vsID := virtualServerIDForGraphKey(current.Graph, rule.LogicalID)
		if err := d.routingRules.Cleanup(ctx, lbID, vsID, rule.ExternalID); err != nil && !f5client.IsNotFound(err) {
			return result, fmt.Errorf("deleting routing rule %s: %w", rule.ExternalID, err)
		}
	}
	for _, member := range sortedResources(current.Graph.Members) {
		if d.pools == nil {
			return result, fmt.Errorf("member cleanup requires a PoolManager")
		}
		vsID, poolID := poolParentIDsForGraphKey(current.Graph, member.LogicalID)
		if err := d.pools.members.client.DeletePoolMember(ctx, lbID, vsID, poolID, member.ExternalID); err != nil && !f5client.IsNotFound(err) {
			return result, fmt.Errorf("deleting pool member %s: %w", member.ExternalID, err)
		}
	}
	for _, monitor := range sortedResources(current.Graph.Monitors) {
		if d.monitors == nil {
			return result, fmt.Errorf("monitor cleanup requires a MonitorManager")
		}
		vsID, poolID := poolParentIDsForGraphKey(current.Graph, monitor.LogicalID)
		if err := d.monitors.Cleanup(ctx, lbID, vsID, poolID, monitor.ExternalID); err != nil && !f5client.IsNotFound(err) {
			return result, fmt.Errorf("deleting monitor %s: %w", monitor.ExternalID, err)
		}
	}
	for _, pool := range sortedResources(current.Graph.Pools) {
		if d.pools == nil {
			return result, fmt.Errorf("pool cleanup requires a PoolManager")
		}
		vsID := virtualServerIDForGraphKey(current.Graph, pool.LogicalID)
		if err := d.pools.Cleanup(ctx, lbID, vsID, pool.ExternalID); err != nil && !f5client.IsNotFound(err) {
			return result, fmt.Errorf("deleting pool %s: %w", pool.ExternalID, err)
		}
	}
	for _, vs := range sortedResources(current.Graph.VirtualServers) {
		if err := d.client.DeleteVirtualServer(ctx, lbID, vs.ExternalID); err != nil && !f5client.IsNotFound(err) {
			return result, fmt.Errorf("deleting virtual server %s: %w", vs.ExternalID, err)
		}
		result.DeletedVirtualServer = true
	}
	if req.DeleteVIP {
		for _, vip := range sortedResources(current.Graph.VIPs) {
			if err := d.client.DeleteVIP(ctx, lbID, vip.ExternalID); err != nil && !f5client.IsNotFound(err) {
				return result, fmt.Errorf("deleting VIP %s: %w", vip.ExternalID, err)
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

func sortedResources(resources map[string]model.ObservedResource) []model.ObservedResource {
	keys := make([]string, 0, len(resources))
	for key := range resources {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]model.ObservedResource, 0, len(keys))
	for _, key := range keys {
		if resources[key].ExternalID != "" {
			result = append(result, resources[key])
		}
	}
	return result
}

func virtualServerIDForGraphKey(graph model.ObservedGraph, key string) string {
	// Graph keys are structured as virtual-server/pool[/child]. Do not use a
	// display-name prefix here: names such as "web" and "web-admin" must never
	// be allowed to select each other's parent during deletion.
	name, _, ok := strings.Cut(key, "/")
	if !ok {
		name = key
	}
	return graph.VirtualServers[name].ExternalID
}

func poolParentIDsForGraphKey(graph model.ObservedGraph, key string) (string, string) {
	parts := strings.SplitN(key, "/", 3)
	if len(parts) < 2 {
		return "", ""
	}
	poolKey := parts[0] + "/" + parts[1]
	return graph.VirtualServers[parts[0]].ExternalID, graph.Pools[poolKey].ExternalID
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

func (d *Deployer) FindLBServiceIDByName(ctx context.Context, name string) (string, error) {
	return d.lbServices.findByName(ctx, name)
}
