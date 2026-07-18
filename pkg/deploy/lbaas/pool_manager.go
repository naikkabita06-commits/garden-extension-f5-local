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
	client PoolClient
}

func NewPoolManager(client PoolClient) *PoolManager { return &PoolManager{client: client} }

func (m *PoolManager) Ensure(ctx context.Context, lbServiceID, virtualServerID string, desired model.Pool, members []PoolMemberSpec, setDefault bool) (PoolResource, bool, error) {
	if strings.TrimSpace(lbServiceID) == "" {
		return PoolResource{}, false, fmt.Errorf("lb service id must not be empty")
	}
	if strings.TrimSpace(virtualServerID) == "" {
		return PoolResource{}, false, fmt.Errorf("virtual server id must not be empty")
	}
	if strings.TrimSpace(desired.Name) == "" {
		return PoolResource{}, false, fmt.Errorf("pool name must not be empty")
	}
	created, err := m.client.CreatePool(ctx, lbServiceID, virtualServerID, PoolSpec{Name: desired.Name, Monitor: desired.Monitor, Members: members})
	if err != nil {
		return PoolResource{}, false, fmt.Errorf("creating pool %s on virtual server %s: %w", desired.Name, virtualServerID, err)
	}
	for _, member := range members {
		if _, err := m.client.CreatePoolMember(ctx, lbServiceID, virtualServerID, created.ID, member); err != nil {
			return created, true, fmt.Errorf("creating pool member %s:%d in pool %s: %w", member.ResourceIP, member.Port, created.ID, err)
		}
	}
	if setDefault {
		if err := m.client.SetDefaultPool(ctx, lbServiceID, virtualServerID, created.ID); err != nil {
			return created, true, fmt.Errorf("setting pool %s as default on virtual server %s: %w", created.ID, virtualServerID, err)
		}
		created.IsDefault = true
	}
	return created, true, nil
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
