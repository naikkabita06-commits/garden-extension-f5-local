package backend

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestListReadyNodeBackendsFiltersByReadyEndpointSlices(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web"}}
	n1 := readyNode("n1", "10.0.0.1")
	n2 := readyNode("n2", "10.0.0.2")
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web-1", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}}}
	slice.Endpoints = []discoveryv1.Endpoint{{NodeName: ptr.To("n2"), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, n1, n2, slice).Build()
	backends, err := ListReadyNodeBackends(context.Background(), c, svc)
	if err != nil {
		t.Fatalf("ListReadyNodeBackends: %v", err)
	}
	if len(backends) != 1 || backends[0].IP != "10.0.0.2" || backends[0].Weight != 50 {
		t.Fatalf("unexpected backends: %#v", backends)
	}
}

func readyNode(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestListReadyNodeBackendsHonorsLocalTrafficPolicyWithoutEndpointSlices(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web"}, Spec: corev1.ServiceSpec{ExternalTrafficPolicy: corev1.ServiceExternalTrafficPolicyTypeLocal}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, readyNode("n1", "10.0.0.1")).Build()
	backends, err := ListReadyNodeBackends(context.Background(), c, svc)
	if err != nil {
		t.Fatalf("ListReadyNodeBackends: %v", err)
	}
	if len(backends) != 0 {
		t.Fatalf("expected no backends without local ready endpoints, got %#v", backends)
	}
}

func TestListReadyNodeBackendsForPortFiltersEndpointSlicePorts(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Name: "http", Port: 80}, {Name: "metrics", Port: 9090}}}}
	n1, n2 := readyNode("n1", "10.0.0.1"), readyNode("n2", "10.0.0.2")
	name := "metrics"
	port := int32(9090)
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "web-metrics", Labels: map[string]string{discoveryv1.LabelServiceName: "web"}}, Ports: []discoveryv1.EndpointPort{{Name: &name, Port: &port}}, Endpoints: []discoveryv1.Endpoint{{NodeName: ptr.To("n2"), Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc, n1, n2, slice).Build()
	backends, err := ListReadyNodeBackendsForPort(context.Background(), c, svc, svc.Spec.Ports[1])
	if err != nil {
		t.Fatal(err)
	}
	if len(backends) != 1 || backends[0].IP != "10.0.0.2" {
		t.Fatalf("unexpected port-aware backends: %#v", backends)
	}
}
