package lbaas

import (
	"context"
	"testing"

	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type stubPoolClient struct {
	createdPools   []PoolSpec
	createdMembers []PoolMemberSpec
	defaultPoolID  string
	deletedPoolID  string
}

func (s *stubPoolClient) CreatePool(_ context.Context, _, _ string, spec PoolSpec) (PoolResource, error) {
	s.createdPools = append(s.createdPools, spec)
	return PoolResource{ID: "pool-1", Name: spec.Name}, nil
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
	return PoolMemberResource{ID: "member-1", ResourceIP: spec.ResourceIP, Port: spec.Port}, nil
}
func (s *stubPoolClient) UpdatePoolMember(context.Context, string, string, string, string, PoolMemberSpec) (PoolMemberResource, error) {
	return PoolMemberResource{}, nil
}
func (s *stubPoolClient) DeletePoolMember(context.Context, string, string, string, string) error {
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
