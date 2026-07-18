package lbaas

import (
	"context"
	"fmt"
	"strings"
)

type RoutingRuleClient interface {
	ListRoutingRules(ctx context.Context, lbServiceID, virtualServerID string) ([]RoutingRuleResource, error)
	CreateRoutingRule(ctx context.Context, lbServiceID, virtualServerID string, spec RoutingRuleSpec) (RoutingRuleResource, error)
	DeleteRoutingRule(ctx context.Context, lbServiceID, virtualServerID, ruleID string) error
}

type RoutingRuleResource struct {
	ID     string
	Host   string
	Path   string
	PoolID string
}

type RoutingRuleSpec struct {
	Host   string
	Path   string
	PoolID string
}

type RoutingRuleManager struct{ client RoutingRuleClient }

func NewRoutingRuleManager(client RoutingRuleClient) *RoutingRuleManager {
	return &RoutingRuleManager{client: client}
}

func (m *RoutingRuleManager) Ensure(ctx context.Context, lbServiceID, virtualServerID string, desired []RoutingRuleSpec) ([]RoutingRuleResource, bool, error) {
	if len(desired) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(lbServiceID) == "" || strings.TrimSpace(virtualServerID) == "" {
		return nil, false, fmt.Errorf("lb service id and virtual server id are required for routing rule reconciliation")
	}
	existing, err := m.client.ListRoutingRules(ctx, lbServiceID, virtualServerID)
	if err != nil {
		return nil, false, fmt.Errorf("listing routing rules for virtual server %s: %w", virtualServerID, err)
	}
	byKey := map[string]RoutingRuleResource{}
	for _, rule := range existing {
		byKey[routingRuleKey(rule.Host, rule.Path, rule.PoolID)] = rule
	}
	out := make([]RoutingRuleResource, 0, len(desired))
	changed := false
	for _, spec := range desired {
		if strings.TrimSpace(spec.PoolID) == "" {
			return out, changed, fmt.Errorf("routing rule pool id must not be empty")
		}
		key := routingRuleKey(spec.Host, spec.Path, spec.PoolID)
		if existing, ok := byKey[key]; ok && strings.TrimSpace(existing.ID) != "" {
			out = append(out, existing)
			continue
		}
		created, err := m.client.CreateRoutingRule(ctx, lbServiceID, virtualServerID, spec)
		if err != nil {
			return out, changed, fmt.Errorf("creating routing rule host=%s path=%s pool=%s: %w", spec.Host, spec.Path, spec.PoolID, err)
		}
		out = append(out, created)
		changed = true
	}
	return out, changed, nil
}

func (m *RoutingRuleManager) Cleanup(ctx context.Context, lbServiceID, virtualServerID, ruleID string) error {
	if strings.TrimSpace(lbServiceID) == "" || strings.TrimSpace(virtualServerID) == "" || strings.TrimSpace(ruleID) == "" {
		return nil
	}
	return m.client.DeleteRoutingRule(ctx, lbServiceID, virtualServerID, ruleID)
}

func routingRuleKey(host, path, poolID string) string {
	return strings.TrimSpace(host) + "|" + strings.TrimSpace(path) + "|" + strings.TrimSpace(poolID)
}
