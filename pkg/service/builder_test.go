package service

import (
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
