package lbaas

import (
	"context"
	"testing"

	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type stubPoolClient struct {
	pools            []PoolResource
	createdPools     []PoolSpec
	createdMembers   []PoolMemberSpec
	updatedMembers   []PoolMemberSpec
	deletedMemberIDs []string
	defaultPoolID    string
	deletedPoolID    string
}

func (s *stubPoolClient) ListPools(context.Context, string, string) ([]PoolResource, error) {
	return append([]PoolResource(nil), s.pools...), nil
}
func (s *stubPoolClient) CreatePool(_ context.Context, _, _ string, spec PoolSpec) (PoolResource, error) {
	s.createdPools = append(s.createdPools, spec)
	pool := PoolResource{ID: "pool-1", Name: spec.Name}
	s.pools = append(s.pools, pool)
	return pool, nil
}
func (s *stubPoolClient) GetPool(context.Context, string, string, string) (PoolResource, error) {
	return PoolResource{}, nil
}
func (s *stubPoolClient) DeletePool(_ context.Context, _, _, poolID string) error {
	s.deletedPoolID = poolID
	return nil
}
func (s *stubPoolClient) SetDefaultPool(_ context.Context, _, _, poolID string) error {
	s.defaultPoolID = poolID
	return nil
}
func (s *stubPoolClient) CreatePoolMember(_ context.Context, _, _, _ string, spec PoolMemberSpec) (PoolMemberResource, error) {
	s.createdMembers = append(s.createdMembers, spec)
	return PoolMemberResource{ID: "member-1", ResourceID: spec.ResourceID, ResourceType: spec.ResourceType, ResourceIP: spec.ResourceIP, BackendPortID: spec.BackendPortID, Port: spec.Port, Weight: spec.Weight}, nil
}
func (s *stubPoolClient) UpdatePoolMember(_ context.Context, _, _, _, memberID string, spec PoolMemberSpec) (PoolMemberResource, error) {
	s.updatedMembers = append(s.updatedMembers, spec)
	return PoolMemberResource{ID: memberID, ResourceID: spec.ResourceID, ResourceType: spec.ResourceType, ResourceIP: spec.ResourceIP, BackendPortID: spec.BackendPortID, Port: spec.Port, Weight: spec.Weight}, nil
}
func (s *stubPoolClient) DeletePoolMember(_ context.Context, _, _, _, memberID string) error {
	s.deletedMemberIDs = append(s.deletedMemberIDs, memberID)
	return nil
}

func TestPoolManagerEnsureCreatesPoolMembersAndDefault(t *testing.T) {
	client := &stubPoolClient{}
	res, changed, err := NewPoolManager(client).Ensure(context.Background(), "lb-1", "vs-1", model.Pool{Name: "pool-web", Monitor: &model.Monitor{Type: "http", Path: "/healthz", Interval: 15}}, []PoolMemberSpec{{ResourceID: "compute-1", ResourceType: "compute", ResourceIP: "10.0.0.1", BackendPortID: 5001, Port: 30080, Weight: 50}}, true)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if !changed || res.ID != "pool-1" || !res.IsDefault {
		t.Fatalf("unexpected result changed=%t res=%#v", changed, res)
	}
	if len(client.createdPools) != 1 || client.createdPools[0].Name != "pool-web" || client.createdPools[0].Monitor.Path != "/healthz" {
		t.Fatalf("unexpected pool spec: %#v", client.createdPools)
	}
	if len(client.createdMembers) != 1 || client.createdMembers[0].BackendPortID != 5001 {
		t.Fatalf("unexpected member specs: %#v", client.createdMembers)
	}
	if client.defaultPoolID != "pool-1" {
		t.Fatalf("expected pool-1 to be default, got %q", client.defaultPoolID)
	}
}

func TestPoolManagerCleanupIgnoresMissingIDs(t *testing.T) {
	client := &stubPoolClient{}
	if err := NewPoolManager(client).Cleanup(context.Background(), "", "vs-1", "pool-1"); err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if client.deletedPoolID != "" {
		t.Fatalf("expected no delete, got %q", client.deletedPoolID)
	}
}

func TestPoolManagerEnsureReusesUniqueObservedPool(t *testing.T) {
	client := &stubPoolClient{pools: []PoolResource{{ID: "pool-1", Name: "pool-web", Members: []PoolMemberResource{{ID: "member-1", ResourceID: "compute-1", ResourceType: "compute", ResourceIP: "10.0.0.1", BackendPortID: 5001, Port: 30080, Weight: 50}}}}}
	res, changed, err := NewPoolManager(client).Ensure(context.Background(), "lb-1", "vs-1", model.Pool{Name: "pool-web"}, []PoolMemberSpec{{ResourceID: "compute-1", ResourceType: "compute", ResourceIP: "10.0.0.1", BackendPortID: 5001, Port: 30080, Weight: 50}}, false)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if changed || res.ID != "pool-1" || len(client.createdPools) != 0 {
		t.Fatalf("expected existing pool reused without mutation, changed=%t result=%#v creates=%#v", changed, res, client.createdPools)
	}
}

func TestPoolManagerEnsureRejectsAmbiguousObservedPool(t *testing.T) {
	client := &stubPoolClient{pools: []PoolResource{{ID: "pool-1", Name: "pool-web"}, {ID: "pool-2", Name: "pool-web"}}}
	_, _, err := NewPoolManager(client).Ensure(context.Background(), "lb-1", "vs-1", model.Pool{Name: "pool-web"}, nil, false)
	if err == nil {
		t.Fatal("expected ambiguous pools to fail")
	}
}
