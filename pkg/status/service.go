package status

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnsureServiceVIP patches Service.status.loadBalancer.ingress to the desired
// VIP only when the current status differs.
func EnsureServiceVIP(ctx context.Context, c client.Client, svc *corev1.Service, vip string) error {
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
