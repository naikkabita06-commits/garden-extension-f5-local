package lbaas

import (
	"context"
	"testing"
)

func TestBackendMemberManagerDiffsCreateUpdateDelete(t *testing.T) {
	client := &stubPoolClient{}
	observed := []PoolMemberResource{
		{ID: "keep", ResourceID: "compute-1", ResourceType: "compute", ResourceIP: "10.0.0.1", BackendPortID: 501, Port: 30080, Weight: 10},
		{ID: "update", ResourceID: "compute-2", ResourceType: "compute", ResourceIP: "10.0.0.2", BackendPortID: 502, Port: 30080, Weight: 10},
		{ID: "delete", ResourceID: "compute-old", ResourceType: "compute", ResourceIP: "10.0.0.9", BackendPortID: 509, Port: 30080, Weight: 10},
	}
	desired := []PoolMemberSpec{
		{ResourceID: "compute-1", ResourceType: "compute", ResourceIP: "10.0.0.1", BackendPortID: 501, Port: 30080, Weight: 10},
		{ResourceID: "compute-2", ResourceType: "compute", ResourceIP: "10.0.0.2", BackendPortID: 502, Port: 30080, Weight: 20},
		{ResourceID: "compute-3", ResourceType: "compute", ResourceIP: "10.0.0.3", BackendPortID: 503, Port: 30080, Weight: 10},
	}
	out, changed, err := NewBackendMemberManager(client).Ensure(context.Background(), "lb-1", "vs-1", "pool-1", observed, desired)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if !changed || len(out) != 3 || len(client.createdMembers) != 1 || len(client.updatedMembers) != 1 || len(client.deletedMemberIDs) != 1 {
		t.Fatalf("unexpected diff changed=%t out=%#v created=%#v updated=%#v deleted=%#v", changed, out, client.createdMembers, client.updatedMembers, client.deletedMemberIDs)
	}
}

func TestBackendMemberManagerRejectsIncompleteIdentity(t *testing.T) {
	_, _, err := NewBackendMemberManager(&stubPoolClient{}).Ensure(context.Background(), "lb-1", "vs-1", "pool-1", nil, []PoolMemberSpec{{ResourceIP: "10.0.0.1", Port: 30080}})
	if err == nil {
		t.Fatal("expected incomplete member identity to fail")
	}
}
