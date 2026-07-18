package networkpolicy

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	managedByLabel = "app.kubernetes.io/managed-by"
	managedByValue = "svc-lb-bridge"
	purposeLabel   = "f5.extensions.gardener.cloud"
	purposeValue   = "network-policy"
)

// Name returns the deterministic name for the NetworkPolicy generated for a Service.
func Name(svc *corev1.Service) string {
	if svc == nil {
		return "f5-lb-allow"
	}
	return fmt.Sprintf("f5-lb-allow-%s", svc.Name)
}

// Ensure creates or updates a NetworkPolicy that allows ingress traffic to the
// pods backing a LoadBalancer Service on the Service's target ports. Services
// without selectors are intentionally ignored because they do not identify pods.
func Ensure(ctx context.Context, c client.Client, svc *corev1.Service) error {
	if svc == nil || len(svc.Spec.Selector) == 0 {
		return nil
	}

	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: Name(svc), Namespace: svc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, np, func() error {
		np.Labels = map[string]string{
			managedByLabel: managedByValue,
			purposeLabel:   purposeValue,
		}
		np.Spec = SpecForService(svc)
		return nil
	})
	return err
}

// Delete removes the generated NetworkPolicy. It is idempotent and treats a
// missing policy as already deleted.
func Delete(ctx context.Context, c client.Client, svc *corev1.Service) error {
	if svc == nil {
		return nil
	}
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: Name(svc), Namespace: svc.Namespace}}
	if err := c.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// SpecForService builds the desired NetworkPolicy spec for a Service.
func SpecForService(svc *corev1.Service) networkingv1.NetworkPolicySpec {
	ports := make([]networkingv1.NetworkPolicyPort, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		port := intstr.FromInt32(p.Port)
		ports = append(ports, networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &port})
	}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: svc.Spec.Selector},
		Ingress:     []networkingv1.NetworkPolicyIngressRule{{Ports: ports}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
	}
}
