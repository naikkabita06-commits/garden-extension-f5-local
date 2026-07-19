package lbaas

import (
	"context"
	"fmt"
	"strings"

	"github.com/gardener/gardener-extension-f5/pkg/model"
)

// PoolClient is the typed CMP capability required for decomposed VirtualServer
// pool/member reconciliation. It mirrors the swagger resource hierarchy under
// /load-balancers/{lb_service_id}/virtual-servers/{virtual_server_id}/pools.
type PoolClient interface {
	ListPools(ctx context.Context, lbServiceID, virtualServerID string) ([]PoolResource, error)
	CreatePool(ctx context.Context, lbServiceID, virtualServerID string, spec PoolSpec) (PoolResource, error)
	GetPool(ctx context.Context, lbServiceID, virtualServerID, poolID string) (PoolResource, error)
	DeletePool(ctx context.Context, lbServiceID, virtualServerID, poolID string) error
	SetDefaultPool(ctx context.Context, lbServiceID, virtualServerID, poolID string) error
	CreatePoolMember(ctx context.Context, lbServiceID, virtualServerID, poolID string, spec PoolMemberSpec) (PoolMemberResource, error)
	UpdatePoolMember(ctx context.Context, lbServiceID, virtualServerID, poolID, memberID string, spec PoolMemberSpec) (PoolMemberResource, error)
	DeletePoolMember(ctx context.Context, lbServiceID, virtualServerID, poolID, memberID string) error
}

type PoolResource struct {
	ID        string
	Name      string
	IsDefault bool
	Members   []PoolMemberResource
}

type PoolMemberResource struct {
	ID            string
	ResourceID    string
	ResourceType  string
	ResourceIP    string
	BackendPortID int
	Port          int32
	Weight        int
}

type PoolSpec struct {
	Name             string
	Protocol         string
	RoutingAlgorithm string
	Monitor          *model.Monitor
	Members          []PoolMemberSpec
}

type PoolMemberSpec struct {
	ResourceID    string
	ResourceType  string
	ResourceIP    string
	BackendPortID int
	Port          int32
	Weight        int
}

type PoolManager struct {
	client  PoolClient
	members *BackendMemberManager
}

func NewPoolManager(client PoolClient) *PoolManager {
	return &PoolManager{client: client, members: NewBackendMemberManager(client)}
}

func (m *PoolManager) Ensure(ctx context.Context, lbServiceID, virtualServerID string, desired model.Pool, members []PoolMemberSpec, setDefault bool) (PoolResource, bool, error) {
	return m.ensure(ctx, lbServiceID, virtualServerID, desired, members, setDefault, "", true)
}

// EnsureOwned reconciles a pool only when its provider ID was recorded in this
// stack's observed graph. CMP pool list entries do not carry ownership tags,
// therefore matching a pool name is never sufficient to adopt it. A stale
// graph ID is treated as drift and results in a create only when no foreign
// pool already occupies the desired name.
func (m *PoolManager) EnsureOwned(ctx context.Context, lbServiceID, virtualServerID string, desired model.Pool, members []PoolMemberSpec, setDefault bool, observedPoolID string) (PoolResource, bool, error) {
	return m.ensure(ctx, lbServiceID, virtualServerID, desired, members, setDefault, observedPoolID, false)
}

func (m *PoolManager) ensure(ctx context.Context, lbServiceID, virtualServerID string, desired model.Pool, members []PoolMemberSpec, setDefault bool, observedPoolID string, legacyNameAdoption bool) (PoolResource, bool, error) {
	if strings.TrimSpace(lbServiceID) == "" {
		return PoolResource{}, false, fmt.Errorf("lb service id must not be empty")
	}
	if strings.TrimSpace(virtualServerID) == "" {
		return PoolResource{}, false, fmt.Errorf("virtual server id must not be empty")
	}
	if strings.TrimSpace(desired.Name) == "" {
		return PoolResource{}, false, fmt.Errorf("pool name must not be empty")
	}
	existing, err := m.client.ListPools(ctx, lbServiceID, virtualServerID)
	if err != nil {
		return PoolResource{}, false, fmt.Errorf("listing pools on virtual server %s: %w", virtualServerID, err)
	}
	observedPoolID = strings.TrimSpace(observedPoolID)
	var pool PoolResource
	nameCollision := false
	for _, candidate := range existing {
		if observedPoolID != "" && strings.TrimSpace(candidate.ID) == observedPoolID {
			pool = candidate
			continue
		}
		if strings.EqualFold(strings.TrimSpace(candidate.Name), strings.TrimSpace(desired.Name)) {
			nameCollision = true
			if !legacyNameAdoption {
				continue
			}
			if strings.TrimSpace(pool.ID) != "" {
				return PoolResource{}, false, fmt.Errorf("ambiguous CMP pools named %q on virtual server %s", desired.Name, virtualServerID)
			}
			pool = candidate
		}
	}
	changed := false
	if strings.TrimSpace(pool.ID) == "" {
		if nameCollision && !legacyNameAdoption {
			return PoolResource{}, false, fmt.Errorf("pool %q on virtual server %s is owned by another stack", desired.Name, virtualServerID)
		}
		pool, err = m.client.CreatePool(ctx, lbServiceID, virtualServerID, PoolSpec{Name: desired.Name, Monitor: desired.Monitor, Members: members})
		if err != nil {
			return PoolResource{}, false, fmt.Errorf("creating pool %s on virtual server %s: %w", desired.Name, virtualServerID, err)
		}
		if strings.TrimSpace(pool.ID) == "" {
			return PoolResource{}, false, fmt.Errorf("CMP created pool %q without a provider id", desired.Name)
		}
		changed = true
	}
	convergedMembers, membersChanged, err := m.members.Ensure(ctx, lbServiceID, virtualServerID, pool.ID, pool.Members, members)
	if err != nil {
		return pool, changed, err
	}
	pool.Members = convergedMembers
	changed = changed || membersChanged
	if setDefault && !pool.IsDefault {
		if err := m.client.SetDefaultPool(ctx, lbServiceID, virtualServerID, pool.ID); err != nil {
			return pool, changed, fmt.Errorf("setting pool %s as default on virtual server %s: %w", pool.ID, virtualServerID, err)
		}
		pool.IsDefault = true
		changed = true
	}
	return pool, changed, nil
}

func (m *PoolManager) Cleanup(ctx context.Context, lbServiceID, virtualServerID, poolID string) error {
	lbServiceID = strings.TrimSpace(lbServiceID)
	virtualServerID = strings.TrimSpace(virtualServerID)
	poolID = strings.TrimSpace(poolID)
	if lbServiceID == "" || virtualServerID == "" || poolID == "" {
		return nil
	}
	return m.client.DeletePool(ctx, lbServiceID, virtualServerID, poolID)
}
