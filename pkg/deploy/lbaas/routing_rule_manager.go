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
	ID        string
	Host      string
	Path      string
	MatchType string
	PoolID    string
}

type RoutingRuleSpec struct {
	Host      string
	Path      string
	MatchType string
	PoolID    string
}

type RoutingRuleManager struct{ client RoutingRuleClient }

func NewRoutingRuleManager(client RoutingRuleClient) *RoutingRuleManager {
	return &RoutingRuleManager{client: client}
}

func (m *RoutingRuleManager) Ensure(ctx context.Context, lbServiceID, virtualServerID string, desired []RoutingRuleSpec) ([]RoutingRuleResource, bool, error) {
	// This compatibility method predates the observed graph.  New controller
	// code must use EnsureOwned: a list response has no ownership metadata, so
	// treating every returned rule as ours is unsafe.
	return m.ensure(ctx, lbServiceID, virtualServerID, desired, nil, true)
}

// EnsureOwned reconciles rules owned by this stack only. ownedRuleIDs must be
// populated from the persisted observed graph. Rules returned by CMP that are
// not in that set are deliberately ignored: CMP's routing-rule response does
// not expose labels or another stable ownership field, and a host/path match
// is not ownership proof.
func (m *RoutingRuleManager) EnsureOwned(ctx context.Context, lbServiceID, virtualServerID string, desired []RoutingRuleSpec, ownedRuleIDs map[string]struct{}) ([]RoutingRuleResource, bool, error) {
	return m.ensure(ctx, lbServiceID, virtualServerID, desired, ownedRuleIDs, false)
}

func (m *RoutingRuleManager) ensure(ctx context.Context, lbServiceID, virtualServerID string, desired []RoutingRuleSpec, ownedRuleIDs map[string]struct{}, legacyAllOwned bool) ([]RoutingRuleResource, bool, error) {
	if strings.TrimSpace(lbServiceID) == "" || strings.TrimSpace(virtualServerID) == "" {
		return nil, false, fmt.Errorf("lb service id and virtual server id are required for routing rule reconciliation")
	}
	existing, err := m.client.ListRoutingRules(ctx, lbServiceID, virtualServerID)
	if err != nil {
		return nil, false, fmt.Errorf("listing routing rules for virtual server %s: %w", virtualServerID, err)
	}
	byKey := map[string]RoutingRuleResource{}
	foreignKeys := map[string]struct{}{}
	for _, rule := range existing {
		if !legacyAllOwned {
			if _, owned := ownedRuleIDs[strings.TrimSpace(rule.ID)]; !owned {
				foreignKeys[routingRuleKey(rule.Host, rule.Path, rule.MatchType, rule.PoolID)] = struct{}{}
				continue
			}
		}
		byKey[routingRuleKey(rule.Host, rule.Path, rule.MatchType, rule.PoolID)] = rule
	}
	desiredKeys := map[string]struct{}{}
	out := make([]RoutingRuleResource, 0, len(desired))
	changed := false
	for _, spec := range desired {
		if strings.TrimSpace(spec.PoolID) == "" {
			return out, changed, fmt.Errorf("routing rule pool id must not be empty")
		}
		key := routingRuleKey(spec.Host, spec.Path, spec.MatchType, spec.PoolID)
		desiredKeys[key] = struct{}{}
		if _, foreign := foreignKeys[key]; foreign {
			return out, changed, fmt.Errorf("routing rule host=%s path=%s pool=%s is owned by another stack", spec.Host, spec.Path, spec.PoolID)
		}
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
	for key, rule := range byKey {
		if _, wanted := desiredKeys[key]; wanted {
			continue
		}
		if strings.TrimSpace(rule.ID) == "" {
			continue
		}
		if err := m.client.DeleteRoutingRule(ctx, lbServiceID, virtualServerID, rule.ID); err != nil {
			return out, changed, fmt.Errorf("deleting obsolete routing rule %s: %w", rule.ID, err)
		}
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

func routingRuleKey(host, path, matchType, poolID string) string {
	mt := strings.ToLower(strings.TrimSpace(matchType))
	if mt == "" {
		mt = "prefix"
	}
	return strings.ToLower(strings.TrimSpace(host)) + "|" + strings.TrimSpace(path) + "|" + mt + "|" + strings.TrimSpace(poolID)
}
