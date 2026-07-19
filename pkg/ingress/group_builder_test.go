package ingress

import (
	"strings"
	"testing"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	"github.com/gardener/gardener-extension-f5/pkg/backend"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestBuildGroupLoadBalancerStackMergesRoutesAndReusesPools(t *testing.T) {
	prefix, exact := networkingv1.PathTypePrefix, networkingv1.PathTypeExact
	group := map[string]string{lbannotations.VIPGroup: "blue"}
	a := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "a", Annotations: group}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "example.test", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &prefix, Backend: *ingressBackend("web", 80)}}}}}}}}
	b := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "b", Annotations: group}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "example.test", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/api", PathType: &exact, Backend: *ingressBackend("api", 8080)}, {Path: "/health", PathType: &prefix, Backend: *ingressBackend("web", 80)}}}}}}}}
	stack, err := BuildGroupLoadBalancerStack([]*networkingv1.Ingress{b, a}, lbannotations.DefaultLBConfig(), GroupStackBuildOptions{BackendResolver: func(_ string, be networkingv1.IngressServiceBackend) (BackendSet, error) {
		return BackendSet{NodePort: 30000 + be.Port.Number, Nodes: []backend.Node{{IP: "10.0.0.1", Weight: 1}}}, nil
	}})
	if err != nil {
		t.Fatalf("BuildGroupLoadBalancerStack: %v", err)
	}
	if stack.Owner.Kind != "IngressGroup" || stack.LBService.Name != "ing-group-app-blue" {
		t.Fatalf("unexpected group owner or LB: %#v %#v", stack.Owner, stack.LBService)
	}
	if len(stack.VirtualServers) != 1 || len(stack.Pools) != 2 || len(stack.RoutingRules) != 3 {
		t.Fatalf("expected one listener, two pools and three routes: %#v", stack)
	}
	if stack.Pools[0].Members[0].Port != 30080 {
		t.Fatalf("web NodePort was not preserved: %#v", stack.Pools[0].Members)
	}
}

func TestBuildGroupLoadBalancerStackRejectsConflictingRoutes(t *testing.T) {
	pt := networkingv1.PathTypePrefix
	annotations := map[string]string{lbannotations.VIPGroup: "blue"}
	makeIngress := func(name, backendName string) *networkingv1.Ingress {
		return &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: name, Annotations: annotations}, Spec: networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: "example.test", IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pt, Backend: *ingressBackend(backendName, 80)}}}}}}}}
	}
	_, err := BuildGroupLoadBalancerStack([]*networkingv1.Ingress{makeIngress("a", "one"), makeIngress("b", "two")}, lbannotations.DefaultLBConfig(), GroupStackBuildOptions{BackendResolver: func(string, networkingv1.IngressServiceBackend) (BackendSet, error) {
		return BackendSet{NodePort: 30080}, nil
	}})
	if err == nil || !strings.Contains(err.Error(), "RouteConflict") {
		t.Fatalf("expected route conflict, got %v", err)
	}
}

func TestBuildGroupLoadBalancerStackCreatesHTTPSListenerAndCertificates(t *testing.T) {
	pt := networkingv1.PathTypePrefix
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}, Spec: networkingv1.IngressSpec{TLS: []networkingv1.IngressTLS{{SecretName: "web-tls", Hosts: []string{"example.test"}}}, Rules: []networkingv1.IngressRule{{IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: []networkingv1.HTTPIngressPath{{Path: "/", PathType: &pt, Backend: *ingressBackend("web", 80)}}}}}}}}
	stack, err := BuildGroupLoadBalancerStack([]*networkingv1.Ingress{ing}, lbannotations.DefaultLBConfig(), GroupStackBuildOptions{BackendResolver: func(string, networkingv1.IngressServiceBackend) (BackendSet, error) {
		return BackendSet{NodePort: 30080}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stack.VirtualServers) != 2 || stack.VirtualServers[1].Protocol != "HTTPS" || len(stack.Certificates) != 1 {
		t.Fatalf("TLS graph not built: %#v", stack)
	}
}
