package main

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type stubCMP struct {
	createLBServiceN int
	createVIPN       int
	createVSN        int
	deleteVSN        int
	deleteVIPN       int
	deleteLBN        int
}

func (s *stubCMP) ListLBServices(_ context.Context, _ *f5client.ListLoadBalancersOptions) ([]json.RawMessage, error) {
	return nil, nil
}
func (s *stubCMP) CreateLBService(_ context.Context, _ url.Values) (json.RawMessage, error) {
	s.createLBServiceN++
	return json.RawMessage(`{"id":"lb-001"}`), nil
}
func (s *stubCMP) DeleteLBService(_ context.Context, _ string) error {
	s.deleteLBN++
	return nil
}
func (s *stubCMP) CreateLBServiceVIP(_ context.Context, _ string) (json.RawMessage, error) {
	s.createVIPN++
	return json.RawMessage(`{"id":101}`), nil
}
func (s *stubCMP) GetLBServiceVIPs(_ context.Context, _ string) ([]json.RawMessage, error) {
	return nil, nil
}
func (s *stubCMP) DeleteLBServiceVIP(_ context.Context, _, _ string) error {
	s.deleteVIPN++
	return nil
}
func (s *stubCMP) ListLBVirtualServers(_ context.Context, _ string) ([]json.RawMessage, error) {
	return nil, nil
}
func (s *stubCMP) CreateLBVirtualServer(_ context.Context, _ string, _ url.Values) (json.RawMessage, error) {
	s.createVSN++
	return json.RawMessage(`{"id":"vs-001","name":"test-vs"}`), nil
}
func (s *stubCMP) DeleteLBVirtualServer(_ context.Context, _, _ string) error {
	s.deleteVSN++
	return nil
}
func (s *stubCMP) ListLBVirtualServerPools(context.Context, string, string) ([]json.RawMessage, error) {
	return nil, nil
}
func (s *stubCMP) CreateLBVirtualServerPool(_ context.Context, _, _ string, q url.Values) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"pool-001","pool_name":"` + q.Get("pool_name") + `","members":[]}`), nil
}
func (s *stubCMP) GetLBVirtualServerPool(context.Context, string, string, string) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"pool-001"}`), nil
}
func (s *stubCMP) DeleteLBVirtualServerPool(context.Context, string, string, string) error {
	return nil
}
func (s *stubCMP) SetDefaultLBVirtualServerPool(context.Context, string, string, string) error {
	return nil
}
func (s *stubCMP) CreateLBVirtualServerPoolMember(context.Context, string, string, string, url.Values) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"member-001"}`), nil
}
func (s *stubCMP) UpdateLBVirtualServerPoolMember(context.Context, string, string, string, string, url.Values) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"member-001"}`), nil
}
func (s *stubCMP) DeleteLBVirtualServerPoolMember(context.Context, string, string, string, string) error {
	return nil
}

func (s *stubCMP) SearchNetworkPortsByIP(_ context.Context, ip string) ([]json.RawMessage, error) {
	return []json.RawMessage{json.RawMessage(`{"id":5001,"resource_id":"compute-` + ip + `","resource_type":"compute","fixed_ip":"` + ip + `"}`)}, nil
}

func newTestClient(t *testing.T, objs ...client.Object) (client.Client, *runtime.Scheme) {
	t.Helper()

	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("AddToScheme core: %v", err)
	}

	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}).
		Build()

	return c, s
}

func TestReconcile_ProvisionsCMPAndSetsAnnotations(t *testing.T) {
	ctx := context.Background()

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "istio-system"}}
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.LoadBalancerClass = ptr.To(defaultLBClass)
	svc.Spec.Ports = []corev1.ServicePort{{Name: "https", Port: 443, NodePort: 30443}}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.10"}}

	c, scheme := newTestClient(t, svc, node)
	stub := &stubCMP{}

	r := &serviceReconciler{
		Client:            c,
		Scheme:            scheme,
		cmp:               stub,
		loadBalancerClass: defaultLBClass,
		Recorder:          record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if stub.createLBServiceN != 1 {
		t.Fatalf("expected CreateLBService called once, got %d", stub.createLBServiceN)
	}
	if stub.createVIPN != 1 {
		t.Fatalf("expected CreateLBServiceVIP called once, got %d", stub.createVIPN)
	}
	if stub.createVSN != 1 {
		t.Fatalf("expected CreateLBVirtualServer called once, got %d", stub.createVSN)
	}

	gotSvc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), gotSvc); err != nil {
		t.Fatalf("get svc: %v", err)
	}

	if !controllerutil.ContainsFinalizer(gotSvc, finalizerName) {
		t.Fatalf("expected finalizer to be set")
	}

	if gotSvc.Annotations[annLBServiceID] != "lb-001" {
		t.Fatalf("expected lb-service-id annotation, got %q", gotSvc.Annotations[annLBServiceID])
	}
	if gotSvc.Annotations[annVIPPortID] != "101" {
		t.Fatalf("expected vip-port-id annotation, got %q", gotSvc.Annotations[annVIPPortID])
	}
	if gotSvc.Annotations[annVirtualServerID] != "vs-001" {
		t.Fatalf("expected virtual-server-id annotation, got %q", gotSvc.Annotations[annVirtualServerID])
	}
}

func TestReconcile_SkipsOnClassMismatch(t *testing.T) {
	ctx := context.Background()

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gw", Namespace: "istio-system"}}
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.LoadBalancerClass = ptr.To("some.other/class")
	svc.Spec.Ports = []corev1.ServicePort{{Name: "https", Port: 443, NodePort: 30443}}

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.10"}}

	c, scheme := newTestClient(t, svc, node)
	stub := &stubCMP{}

	r := &serviceReconciler{
		Client:            c,
		Scheme:            scheme,
		cmp:               stub,
		loadBalancerClass: defaultLBClass,
		Recorder:          record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stub.createLBServiceN != 0 {
		t.Fatalf("expected no CMP calls, got %d LB creates", stub.createLBServiceN)
	}
}

func TestReconcile_DeleteCleansCMPResources(t *testing.T) {
	ctx := context.Background()

	now := metav1.NewTime(time.Now())
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:              "gw",
		Namespace:         "istio-system",
		Finalizers:        []string{finalizerName},
		DeletionTimestamp: &now,
		Annotations: map[string]string{
			annLBServiceID:     "lb-001",
			annVIPPortID:       "vip-001",
			annVirtualServerID: "vs-001",
		},
	}}

	c, scheme := newTestClient(t, svc)
	stub := &stubCMP{}

	r := &serviceReconciler{
		Client:            c,
		Scheme:            scheme,
		cmp:               stub,
		loadBalancerClass: defaultLBClass,
		Recorder:          record.NewFakeRecorder(10),
	}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stub.deleteVSN != 1 {
		t.Fatalf("expected DeleteLBVirtualServer called once, got %d", stub.deleteVSN)
	}
	if stub.deleteVIPN != 1 {
		t.Fatalf("expected DeleteLBServiceVIP called once, got %d", stub.deleteVIPN)
	}
	if stub.deleteLBN != 1 {
		t.Fatalf("expected DeleteLBService called once, got %d", stub.deleteLBN)
	}

	gotSvc := &corev1.Service{}
	err = c.Get(ctx, client.ObjectKeyFromObject(svc), gotSvc)
	if err == nil {
		if controllerutil.ContainsFinalizer(gotSvc, finalizerName) {
			t.Fatalf("expected finalizer removed")
		}
		return
	}
	// Fake client may delete the object once finalizers are removed.
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound or updated object, got: %v", err)
	}
}
