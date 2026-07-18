package status

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureServiceVIP(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "svc"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).WithStatusSubresource(&corev1.Service{}).Build()

	if err := EnsureServiceVIP(ctx, c, svc, "10.0.0.10"); err != nil {
		t.Fatalf("EnsureServiceVIP: %v", err)
	}
	stored := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), stored); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(stored.Status.LoadBalancer.Ingress) != 1 || stored.Status.LoadBalancer.Ingress[0].IP != "10.0.0.10" {
		t.Fatalf("unexpected status: %#v", stored.Status.LoadBalancer.Ingress)
	}
}
