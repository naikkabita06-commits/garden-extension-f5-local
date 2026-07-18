package lbaas

import (
	"context"
	"fmt"
	"strings"
)

// BackendMemberManager reconciles the complete member set for one CMP pool.
// It owns member-level desired-vs-observed diffing; PoolManager owns only the
// parent pool lifecycle and delegates member convergence here.
type BackendMemberManager struct {
	client PoolClient
}

func NewBackendMemberManager(client PoolClient) *BackendMemberManager {
	return &BackendMemberManager{client: client}
}

func (m *BackendMemberManager) Ensure(ctx context.Context, lbServiceID, virtualServerID, poolID string, observed []PoolMemberResource, desired []PoolMemberSpec) ([]PoolMemberResource, bool, error) {
	if strings.TrimSpace(lbServiceID) == "" || strings.TrimSpace(virtualServerID) == "" || strings.TrimSpace(poolID) == "" {
		return nil, false, fmt.Errorf("lb service id, virtual server id and pool id are required for backend member reconciliation")
	}

	byKey := map[string]PoolMemberResource{}
	for _, member := range observed {
		key := poolMemberKey(member.ResourceID, member.ResourceIP, member.BackendPortID, member.Port)
		if strings.TrimSpace(key) != "" {
			byKey[key] = member
		}
	}
	desiredKeys := map[string]struct{}{}
	out := make([]PoolMemberResource, 0, len(desired))
	changed := false

	for _, spec := range desired {
		if strings.TrimSpace(spec.ResourceID) == "" || spec.BackendPortID == 0 || strings.TrimSpace(spec.ResourceIP) == "" || spec.Port == 0 {
			return out, changed, fmt.Errorf("backend member requires resource_id, resource_ip, backend_port_id and port")
		}
		key := poolMemberSpecKey(spec)
		desiredKeys[key] = struct{}{}
		if existing, ok := byKey[key]; ok && strings.TrimSpace(existing.ID) != "" {
			if memberNeedsUpdate(existing, spec) {
				updated, err := m.client.UpdatePoolMember(ctx, lbServiceID, virtualServerID, poolID, existing.ID, spec)
				if err != nil {
					return out, changed, fmt.Errorf("updating backend member %s in pool %s: %w", existing.ID, poolID, err)
				}
				out = append(out, updated)
				changed = true
				continue
			}
			out = append(out, existing)
			continue
		}
		created, err := m.client.CreatePoolMember(ctx, lbServiceID, virtualServerID, poolID, spec)
		if err != nil {
			return out, changed, fmt.Errorf("creating backend member %s:%d in pool %s: %w", spec.ResourceIP, spec.Port, poolID, err)
		}
		out = append(out, created)
		changed = true
	}

	for key, member := range byKey {
		if _, wanted := desiredKeys[key]; wanted {
			continue
		}
		if strings.TrimSpace(member.ID) == "" {
			continue
		}
		if err := m.client.DeletePoolMember(ctx, lbServiceID, virtualServerID, poolID, member.ID); err != nil {
			return out, changed, fmt.Errorf("deleting obsolete backend member %s from pool %s: %w", member.ID, poolID, err)
		}
		changed = true
	}

	return out, changed, nil
}

func poolMemberSpecKey(spec PoolMemberSpec) string {
	return poolMemberKey(spec.ResourceID, spec.ResourceIP, spec.BackendPortID, spec.Port)
}

func poolMemberKey(resourceID, resourceIP string, backendPortID int, port int32) string {
	return strings.TrimSpace(resourceID) + "|" + strings.TrimSpace(resourceIP) + "|" + fmt.Sprintf("%d|%d", backendPortID, port)
}

func memberNeedsUpdate(observed PoolMemberResource, desired PoolMemberSpec) bool {
	return strings.TrimSpace(observed.ResourceType) != strings.TrimSpace(defaultResourceType(desired.ResourceType)) || observed.Weight != desired.Weight
}

func defaultResourceType(resourceType string) string {
	if strings.TrimSpace(resourceType) == "" {
		return "compute"
	}
	return resourceType
}
