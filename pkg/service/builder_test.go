package service

import (
	"strings"
	"testing"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	"github.com/gardener/gardener-extension-f5/pkg/backend"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestBuildLoadBalancerStackBuildsPortsAndBackends(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web", UID: types.UID("uid-1")}}
	svc.Spec.Ports = []corev1.ServicePort{{Name: "web", Protocol: corev1.ProtocolTCP, Port: 80, NodePort: 30080}}

	stack, err := BuildLoadBalancerStack(svc, lbannotations.DefaultLBConfig(), []backend.Node{{IP: "10.0.0.1", Weight: 50}})
	if err != nil {
		t.Fatalf("BuildLoadBalancerStack: %v", err)
	}
	if stack.Owner.Kind != "Service" || stack.Owner.Namespace != "ns" || stack.Owner.Name != "web" || stack.Owner.UID != "uid-1" {
		t.Fatalf("unexpected owner: %#v", stack.Owner)
	}
	if len(stack.Ports) != 1 {
		t.Fatalf("expected one port, got %d", len(stack.Ports))
	}
	port := stack.Ports[0]
	if port.FrontendPort != 80 || port.NodePort != 30080 || port.Protocol != "HTTP" {
		t.Fatalf("unexpected port model: %#v", port)
	}
	if len(port.Backends) != 1 || port.Backends[0].IP != "10.0.0.1" || port.Backends[0].Port != 30080 || port.Backends[0].Weight != 50 {
		t.Fatalf("unexpected backends: %#v", port.Backends)
	}
	if stack.LBService.Name != "app-ns-web" || stack.VIP.Name != "app-vip-ns-web" {
		t.Fatalf("expected deterministic parent resources, got LB=%#v VIP=%#v", stack.LBService, stack.VIP)
	}
	if stack.Pools[0].Monitor == nil || stack.Pools[0].Monitor.Name == "" {
		t.Fatal("expected deterministic pool monitor name")
	}
	if stack.VirtualServers[0].Name != "app-vs-ns-web-80" || stack.Pools[0].Name != "app-pool-ns-web-80" || stack.VirtualServers[0].DefaultPoolName != stack.Pools[0].Name {
		t.Fatalf("expected deterministic listener and pool graph, got VS=%#v pool=%#v", stack.VirtualServers[0], stack.Pools[0])
	}
}

func TestBuildLoadBalancerStackUsesVIPGroupOnlyForSharedParent(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web", Annotations: map[string]string{lbannotations.VIPGroup: "Blue Team"}}}
	svc.Spec.Ports = []corev1.ServicePort{{Port: 443, NodePort: 30443}}

	stack, err := BuildLoadBalancerStack(svc, lbannotations.DefaultLBConfig(), nil)
	if err != nil {
		t.Fatalf("BuildLoadBalancerStack: %v", err)
	}
	if stack.LBService.Name != "app-group-ns-blue-team" || stack.LBService.Ownership.SharedGroup != "Blue Team" {
		t.Fatalf("expected grouped parent LB, got %#v", stack.LBService)
	}
	if stack.VirtualServers[0].Name != "app-vs-ns-web-443" || stack.VirtualServers[0].Ownership.SharedGroup != "" {
		t.Fatalf("expected owner-specific listener, got %#v", stack.VirtualServers[0])
	}
}

func TestBuildLoadBalancerStackHonorsProtocolOverride(t *testing.T) {
	svc := &corev1.Service{}
	svc.Spec.Ports = []corev1.ServicePort{{Protocol: corev1.ProtocolTCP, Port: 8080, NodePort: 30080}}
	cfg := lbannotations.DefaultLBConfig()
	cfg.ProtocolOverride = "TCP"

	stack, err := BuildLoadBalancerStack(svc, cfg, nil)
	if err != nil {
		t.Fatalf("BuildLoadBalancerStack: %v", err)
	}
	if got := stack.Ports[0].Protocol; got != "TCP" {
		t.Fatalf("expected override protocol TCP, got %q", got)
	}
}

func TestBuildLoadBalancerStackRequiresNodePort(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "web", Port: 80}}}}
	_, err := BuildLoadBalancerStack(svc, lbannotations.LBConfig{}, nil)
	if err == nil || !strings.Contains(err.Error(), "BackendNodePortRequired") {
		t.Fatalf("expected BackendNodePortRequired, got %v", err)
	}
}
