package ingress

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
)

func TestValidateSupportedRequiresIngress(t *testing.T) {
	if err := ValidateSupported(nil); err == nil {
		t.Fatal("expected nil ingress to be rejected")
	}
}

func TestValidateSupportedRequiresServiceBackend(t *testing.T) {
	ing := &networkingv1.Ingress{}
	if err := ValidateSupported(ing); err == nil || !strings.Contains(err.Error(), "at least one service backend") {
		t.Fatalf("expected missing backend error, got %v", err)
	}
}

func TestValidateSupportedAcceptsDefaultBackend(t *testing.T) {
	ing := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{DefaultBackend: ingressBackend("web", 80)}}
	if err := ValidateSupported(ing); err != nil {
		t.Fatalf("expected default backend to be accepted: %v", err)
	}
}

func TestValidateSupportedAcceptsMultiplePathsForSameBackend(t *testing.T) {
	ing := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Backend: *ingressBackend("web", 80)}, {Backend: *ingressBackend("web", 80)}}}}}}}}
	if err := ValidateSupported(ing); err != nil {
		t.Fatalf("expected same backend to be accepted: %v", err)
	}
}

func TestValidateSupportedRejectsMultipleBackends(t *testing.T) {
	ing := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Backend: *ingressBackend("web", 80)}, {Backend: *ingressBackend("api", 80)}}}}}}}}
	if err := ValidateSupported(ing); err == nil || !strings.Contains(err.Error(), "multiple backend") {
		t.Fatalf("expected multiple backend rejection, got %v", err)
	}
}

func TestValidateSupportedRejectsResourceBackend(t *testing.T) {
	ing := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{DefaultBackend: &networkingv1.IngressBackend{Resource: &corev1.TypedLocalObjectReference{Kind: "StorageBucket", Name: "static"}}}}
	if err := ValidateSupported(ing); err == nil || !strings.Contains(err.Error(), "resource backends") {
		t.Fatalf("expected resource backend rejection, got %v", err)
	}
}

func ingressBackend(name string, port int32) *networkingv1.IngressBackend {
	return &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: name, Port: networkingv1.ServiceBackendPort{Number: port}}}
}
