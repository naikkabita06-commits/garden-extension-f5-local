package ingress

import (
	"testing"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	"github.com/gardener/gardener-extension-f5/pkg/backend"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildLoadBalancerStackBuildsSharedGroupNamesAndMembers(t *testing.T) {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web", Annotations: map[string]string{lbannotations.VIPGroup: "blue"}}}
	stack, err := BuildLoadBalancerStack(ing, lbannotations.DefaultLBConfig(), []backend.Node{{IP: "10.0.0.1", Weight: 50}}, BuildOptions{FrontendPort: 80, BackendPort: 30080, Protocol: "HTTP"})
	if err != nil {
		t.Fatalf("BuildLoadBalancerStack: %v", err)
	}
	if stack.Pools[0].Monitor == nil || stack.Pools[0].Monitor.Name == "" {
		t.Fatal("expected deterministic pool monitor name")
	}
	if stack.LBService.Name != "ing-group-app-blue" || stack.VirtualServers[0].Name != "ing-vs-app-web" || stack.Pools[0].Name != "ing-pool-app-web" {
		t.Fatalf("unexpected names: lb=%q vs=%q pool=%q", stack.LBService.Name, stack.VirtualServers[0].Name, stack.Pools[0].Name)
	}
	if len(stack.Pools[0].Members) != 1 || stack.Pools[0].Members[0].Port != 30080 {
		t.Fatalf("unexpected pool members: %#v", stack.Pools[0].Members)
	}
}

func TestProtocolAndFrontendPortForTLSIngress(t *testing.T) {
	ing := &networkingv1.Ingress{Spec: networkingv1.IngressSpec{TLS: []networkingv1.IngressTLS{{Hosts: []string{"example.test"}}}}}
	if got := ProtocolForIngress(ing); got != "HTTPS" {
		t.Fatalf("expected HTTPS, got %q", got)
	}
	if got := FrontendPortForProtocol("HTTPS"); got != 443 {
		t.Fatalf("expected 443, got %d", got)
	}
}

func TestBuildLoadBalancerStackCapturesRoutingRulesAndCertificates(t *testing.T) {
	pt := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"},
		Spec: networkingv1.IngressSpec{
			TLS:   []networkingv1.IngressTLS{{SecretName: "web-tls", Hosts: []string{"example.test"}}},
			Rules: []networkingv1.IngressRule{{Host: "Example.Test", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/api", PathType: &pt, Backend: *ingressBackend("api", 8080)}}}}}},
		},
	}
	stack, err := BuildLoadBalancerStack(ing, lbannotations.DefaultLBConfig(), []backend.Node{{IP: "10.0.0.1", Weight: 1}}, BuildOptions{FrontendPort: 443, BackendPort: 30080, Protocol: "HTTPS"})
	if err != nil {
		t.Fatalf("BuildLoadBalancerStack: %v", err)
	}
	if len(stack.RoutingRules) != 1 || stack.RoutingRules[0].Host != "example.test" || stack.RoutingRules[0].Path != "/api" || stack.RoutingRules[0].MatchType != "prefix" {
		t.Fatalf("unexpected routing rules: %#v", stack.RoutingRules)
	}
	if len(stack.Certificates) != 1 || stack.Certificates[0].SecretName != "web-tls" || len(stack.Certificates[0].Hosts) != 1 {
		t.Fatalf("unexpected certificates: %#v", stack.Certificates)
	}
}
