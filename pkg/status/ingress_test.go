package status

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureIngressVIP(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ing).WithStatusSubresource(&networkingv1.Ingress{}).Build()
	if err := EnsureIngressVIP(context.Background(), c, ing, "10.0.0.10"); err != nil {
		t.Fatalf("EnsureIngressVIP: %v", err)
	}
	got := &networkingv1.Ingress{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ing), got); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Status.LoadBalancer.Ingress) != 1 || got.Status.LoadBalancer.Ingress[0].IP != "10.0.0.10" {
		t.Fatalf("unexpected status: %#v", got.Status.LoadBalancer.Ingress)
	}
}

func TestEnsureIngressVIPRejectsPendingProviderValue(t *testing.T) {
	ing := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}
	if err := EnsureIngressVIP(context.Background(), nil, ing, "pending"); err == nil {
		t.Fatal("expected non-IP provider value to be rejected")
	}
}
