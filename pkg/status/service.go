package status

import (
	"context"
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureServiceVIP patches Service.status.loadBalancer.ingress to the desired
// VIP only when the current status differs.
func EnsureServiceVIP(ctx context.Context, c client.Client, svc *corev1.Service, vip string) error {
	vip = strings.TrimSpace(vip)
	if vip == "" {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(vip), "pending") || strings.EqualFold(strings.TrimSpace(vip), "unknown") {
		return fmt.Errorf("refusing to publish placeholder CMP VIP %q", vip)
	}
	if net.ParseIP(vip) == nil {
		return fmt.Errorf("refusing to publish non-IP CMP VIP %q", vip)
	}
	current := ""
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		current = strings.TrimSpace(svc.Status.LoadBalancer.Ingress[0].IP)
	}
	if current == vip {
		return nil
	}

	base := svc.DeepCopy()
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: vip}}
	return c.Status().Patch(ctx, svc, client.MergeFrom(base))
}
