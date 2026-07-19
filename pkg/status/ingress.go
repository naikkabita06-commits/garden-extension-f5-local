package status

import (
	"context"
	"fmt"
	"net"
	"strings"

	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureIngressVIP patches Ingress.status.loadBalancer.ingress to the desired
// VIP only when the current status differs.
func EnsureIngressVIP(ctx context.Context, c client.Client, ing *networkingv1.Ingress, vip string) error {
	vip = strings.TrimSpace(vip)
	if vip == "" {
		return nil
	}
	if net.ParseIP(vip) == nil {
		return fmt.Errorf("refusing to publish non-IP CMP VIP %q", vip)
	}
	current := ""
	if len(ing.Status.LoadBalancer.Ingress) > 0 {
		current = strings.TrimSpace(ing.Status.LoadBalancer.Ingress[0].IP)
	}
	if current == vip {
		return nil
	}
	base := ing.DeepCopy()
	ing.Status.LoadBalancer.Ingress = []networkingv1.IngressLoadBalancerIngress{{IP: vip}}
	return c.Status().Patch(ctx, ing, client.MergeFrom(base))
}
