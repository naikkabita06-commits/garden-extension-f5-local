package main

import (
	"context"
	"strings"
	"testing"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIngressReconcileBuildsCompleteGroupAndRejectsCrossMemberRouteConflict(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	class := ingressClassName
	pathType := networkingv1.PathTypePrefix
	annotations := map[string]string{lbannotations.VIPGroup: "shared"}
	newIngress := func(name, service string) *networkingv1.Ingress {
		return &networkingv1.Ingress{
			ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: name, Annotations: annotations},
			Spec: networkingv1.IngressSpec{
				IngressClassName: &class,
				Rules: []networkingv1.IngressRule{{
					Host: "example.test",
					IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{
								Name: service,
								Port: networkingv1.ServiceBackendPort{Number: 80},
							}},
						}},
					}},
				}},
			},
		}
	}
	service := func(name string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: name}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080}}}}
	}
	first, second := newIngress("first", "web"), newIngress("second", "api")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(first, second, service("web"), service("api")).WithStatusSubresource(&networkingv1.Ingress{}).Build()
	cmp := &stubCMP{}
	r := &ingressReconciler{Client: c, Scheme: scheme, cmp: cmp, Recorder: record.NewFakeRecorder(10)}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(first)})
	if err == nil || !strings.Contains(err.Error(), "RouteConflict") {
		t.Fatalf("expected cross-member RouteConflict from group builder, got %v", err)
	}
	if cmp.createLBN != 0 {
		t.Fatalf("conflicting group must fail before CMP mutation, got %d LB creates", cmp.createLBN)
	}
}
