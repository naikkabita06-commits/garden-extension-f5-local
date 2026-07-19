package lbaas

import (
	"context"
	"testing"
)

type stubRoutingRuleClient struct {
	rules   []RoutingRuleResource
	created []RoutingRuleSpec
	deleted []string
}

func (s *stubRoutingRuleClient) ListRoutingRules(context.Context, string, string) ([]RoutingRuleResource, error) {
	return append([]RoutingRuleResource(nil), s.rules...), nil
}
func (s *stubRoutingRuleClient) CreateRoutingRule(_ context.Context, _, _ string, spec RoutingRuleSpec) (RoutingRuleResource, error) {
	s.created = append(s.created, spec)
	return RoutingRuleResource{ID: "rule-1", Host: spec.Host, Path: spec.Path, MatchType: spec.MatchType, PoolID: spec.PoolID}, nil
}
func (s *stubRoutingRuleClient) DeleteRoutingRule(_ context.Context, _, _, id string) error {
	s.deleted = append(s.deleted, id)
	return nil
}

func TestRoutingRuleManagerEnsureCreatesMissingRules(t *testing.T) {
	client := &stubRoutingRuleClient{rules: []RoutingRuleResource{{ID: "rule-existing", Host: "example.com", Path: "/", MatchType: "prefix", PoolID: "pool-1"}}}
	rules, changed, err := NewRoutingRuleManager(client).Ensure(context.Background(), "lb-1", "vs-1", []RoutingRuleSpec{{Host: "example.com", Path: "/", MatchType: "prefix", PoolID: "pool-1"}, {Host: "api.example.com", Path: "/v1", MatchType: "exact", PoolID: "pool-2"}})
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if !changed || len(rules) != 2 || len(client.created) != 1 || client.created[0].PoolID != "pool-2" {
		t.Fatalf("unexpected rules changed=%t rules=%#v created=%#v", changed, rules, client.created)
	}
}

func TestRoutingRuleManagerRejectsRuleWithoutPool(t *testing.T) {
	_, _, err := NewRoutingRuleManager(&stubRoutingRuleClient{}).Ensure(context.Background(), "lb-1", "vs-1", []RoutingRuleSpec{{Host: "example.com", Path: "/"}})
	if err == nil {
		t.Fatal("expected missing pool id to fail")
	}
}

func TestRoutingRuleManagerDeletesObsoleteRulesWhenDesiredChanges(t *testing.T) {
	client := &stubRoutingRuleClient{rules: []RoutingRuleResource{{ID: "rule-old", Host: "old.example.com", Path: "/", MatchType: "prefix", PoolID: "pool-old"}, {ID: "rule-keep", Host: "example.com", Path: "/", MatchType: "exact", PoolID: "pool-1"}}}
	rules, changed, err := NewRoutingRuleManager(client).Ensure(context.Background(), "lb-1", "vs-1", []RoutingRuleSpec{{Host: "example.com", Path: "/", MatchType: "exact", PoolID: "pool-1"}})
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if !changed || len(rules) != 1 || len(client.deleted) != 1 || client.deleted[0] != "rule-old" {
		t.Fatalf("expected obsolete rule deletion, changed=%t rules=%#v deleted=%#v", changed, rules, client.deleted)
	}
}

func TestRoutingRuleManagerDeletesAllRulesWhenDesiredIsEmpty(t *testing.T) {
	client := &stubRoutingRuleClient{rules: []RoutingRuleResource{{ID: "rule-old", Host: "old.example.com", Path: "/", MatchType: "prefix", PoolID: "pool-old"}}}
	rules, changed, err := NewRoutingRuleManager(client).Ensure(context.Background(), "lb-1", "vs-1", nil)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if !changed || len(rules) != 0 || len(client.deleted) != 1 || client.deleted[0] != "rule-old" {
		t.Fatalf("expected all rules deleted, changed=%t rules=%#v deleted=%#v", changed, rules, client.deleted)
	}
}

func TestRoutingRuleManagerEnsureOwnedDoesNotDeleteForeignRules(t *testing.T) {
	client := &stubRoutingRuleClient{rules: []RoutingRuleResource{
		{ID: "rule-owned", Host: "owned.example.com", Path: "/", MatchType: "prefix", PoolID: "pool-1"},
		{ID: "rule-foreign", Host: "foreign.example.com", Path: "/", MatchType: "prefix", PoolID: "pool-2"},
	}}
	rules, changed, err := NewRoutingRuleManager(client).EnsureOwned(context.Background(), "lb-1", "vs-1", nil, map[string]struct{}{"rule-owned": {}})
	if err != nil {
		t.Fatalf("EnsureOwned failed: %v", err)
	}
	if !changed || len(rules) != 0 || len(client.deleted) != 1 || client.deleted[0] != "rule-owned" {
		t.Fatalf("expected only owned rule deletion, changed=%t rules=%#v deleted=%#v", changed, rules, client.deleted)
	}
}

func TestRoutingRuleManagerEnsureOwnedRejectsForeignRouteCollision(t *testing.T) {
	client := &stubRoutingRuleClient{rules: []RoutingRuleResource{{ID: "rule-foreign", Host: "example.com", Path: "/", MatchType: "prefix", PoolID: "pool-1"}}}
	_, _, err := NewRoutingRuleManager(client).EnsureOwned(context.Background(), "lb-1", "vs-1", []RoutingRuleSpec{{Host: "example.com", Path: "/", MatchType: "prefix", PoolID: "pool-1"}}, map[string]struct{}{})
	if err == nil {
		t.Fatal("expected foreign rule collision to fail")
	}
	if len(client.created) != 0 || len(client.deleted) != 0 {
		t.Fatalf("foreign rule must not be mutated, created=%#v deleted=%#v", client.created, client.deleted)
	}
}
