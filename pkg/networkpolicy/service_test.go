package networkpolicy

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsureCreatesNetworkPolicyForSelectedService(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "web"}, Ports: []corev1.ServicePort{{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP}}}}

	if err := Ensure(context.Background(), c, svc); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}

	np := &networkingv1.NetworkPolicy{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "f5-lb-allow-web", Namespace: "default"}, np); err != nil {
		t.Fatalf("expected NetworkPolicy: %v", err)
	}
	if np.Labels[purposeLabel] != purposeValue {
		t.Fatalf("expected managed purpose label, got %v", np.Labels)
	}
	if got := np.Spec.PodSelector.MatchLabels["app"]; got != "web" {
		t.Fatalf("expected selector app=web, got %q", got)
	}
	if len(np.Spec.Ingress) != 1 || len(np.Spec.Ingress[0].Ports) != 1 {
		t.Fatalf("expected one ingress port, got %#v", np.Spec.Ingress)
	}
}

func TestEnsureSkipsServiceWithoutSelector(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: "default"}}

	if err := Ensure(context.Background(), c, svc); err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}

	npList := &networkingv1.NetworkPolicyList{}
	if err := c.List(context.Background(), npList); err != nil {
		t.Fatal(err)
	}
	if len(npList.Items) != 0 {
		t.Fatalf("expected no NetworkPolicy for selectorless Service, got %d", len(npList.Items))
	}
}

func TestDeleteIgnoresMissingNetworkPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}}

	if err := Delete(context.Background(), c, svc); err != nil {
		t.Fatalf("Delete should ignore missing NetworkPolicy: %v", err)
	}
}
