package ingress

import (
	"fmt"
	"sort"
	"strings"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	"github.com/gardener/gardener-extension-f5/pkg/model"

	networkingv1 "k8s.io/api/networking/v1"
)

// ResolveGroup selects the Ingresses that belong to anchor's group and returns
// a stable owner-independent group identity. Ungrouped Ingresses form a group
// of one, while grouped Ingresses must reside in the same namespace.
func ResolveGroup(anchor *networkingv1.Ingress, candidates []networkingv1.Ingress) ([]*networkingv1.Ingress, string, error) {
	if anchor == nil {
		return nil, "", fmt.Errorf("ingress must not be nil")
	}
	group := strings.TrimSpace(anchor.Annotations[lbannotations.VIPGroup])
	members := make([]*networkingv1.Ingress, 0)
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.Namespace != anchor.Namespace {
			continue
		}
		candidateGroup := strings.TrimSpace(candidate.Annotations[lbannotations.VIPGroup])
		if group == "" {
			if candidate.Name == anchor.Name {
				members = append(members, candidate)
			}
			continue
		}
		if candidateGroup == group {
			members = append(members, candidate)
		}
	}
	if len(members) == 0 {
		members = append(members, anchor)
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return members, groupIdentity(anchor.Namespace, group, anchor.Name), nil
}

func groupIdentity(namespace, group, fallback string) string {
	if strings.TrimSpace(group) == "" {
		return "ingress/" + namespace + "/" + fallback
	}
	return "ingress-group/" + namespace + "/" + group
}

// GroupOwner identifies the synthetic owner used by a group stack. It avoids
// assigning shared provider parents to whichever member happened to reconcile.
func GroupOwner(namespace, identity string) model.Owner {
	return model.Owner{Kind: "IngressGroup", Namespace: namespace, Name: identity}
}
