package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

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
	annotations := map[string]string{annVIPGroup: "shared"}
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

func TestIngressReconcileUploadsAndBindsCertificateFromTLSSecret(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	class := ingressClassName
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web", Annotations: map[string]string{annVIPGroup: "shared"}},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &class,
			DefaultBackend:   &networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: "web", Port: networkingv1.ServiceBackendPort{Number: 80}}},
			TLS:              []networkingv1.IngressTLS{{SecretName: "tls-web", Hosts: []string{"example.test"}}},
		},
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "web"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 80, NodePort: 30080}}}}
	secret := validTLSSecret(t, "app", "tls-web", "example.test")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ing, svc, secret).WithStatusSubresource(&networkingv1.Ingress{}).Build()
	cmp := &stubCMP{}
	r := &ingressReconciler{Client: c, Scheme: scheme, cmp: cmp, Recorder: record.NewFakeRecorder(10)}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ing)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cmp.createCertN != 1 {
		t.Fatalf("expected one certificate upload, got %d", cmp.createCertN)
	}
	if cmp.lastCertQ == nil || strings.TrimSpace(cmp.lastCertQ.Get("sslCert")) == "" || strings.TrimSpace(cmp.lastCertQ.Get("sslPvtKey")) == "" {
		t.Fatalf("expected uploaded TLS material in query, got %#v", cmp.lastCertQ)
	}
	if cmp.attachCertN == 0 || strings.TrimSpace(cmp.attachedCertID) == "" {
		t.Fatalf("expected certificate attach call, got attach=%d certID=%q", cmp.attachCertN, cmp.attachedCertID)
	}
}

func TestListManagedIngressRequestsForSecretEnqueuesWholeGroup(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	class := ingressClassName
	groupAnn := map[string]string{annVIPGroup: "shared"}
	a := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "a", Annotations: groupAnn}, Spec: networkingv1.IngressSpec{IngressClassName: &class, TLS: []networkingv1.IngressTLS{{SecretName: "tls-a"}}}}
	b := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "b", Annotations: groupAnn}, Spec: networkingv1.IngressSpec{IngressClassName: &class}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(a, b).Build()

	reqs := listManagedIngressRequestsForSecret(ctx, c, "app", "tls-a")
	if len(reqs) != 2 {
		t.Fatalf("expected group-wide enqueue of 2 ingresses, got %d: %#v", len(reqs), reqs)
	}
}

func validTLSSecret(t *testing.T, namespace, name, dns string) *corev1.Secret {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: dns}, DNSNames: []string{dns}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	priv := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}, Type: corev1.SecretTypeTLS, Data: map[string][]byte{corev1.TLSCertKey: cert, corev1.TLSPrivateKeyKey: priv}}
}
