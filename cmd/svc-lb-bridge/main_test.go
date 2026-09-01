package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	"github.com/gardener/gardener-extension-f5/pkg/model"

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
	createLBN           int
	listLBN             int
	createVN            int
	listVIPN            int
	createVSN           int
	listVSN             int
	deleteVSN           int
	deleteVN            int
	deleteLBN           int
	lastVSQ             url.Values
	createCertN         int
	lastCertQ           url.Values
	attachCertN         int
	attachedCertID      string
	lastVIPListSubnet   string
	lastVIPCreateSubnet string
	vipCreateErr        error

	// Optional canned responses for list calls.
	lbServices []json.RawMessage
	vips       []json.RawMessage
	vsList     []json.RawMessage
	certs      []json.RawMessage
}

func (s *stubCMP) CreateLBService(_ context.Context, _ url.Values) (json.RawMessage, error) {
	s.createLBN++
	created := json.RawMessage(`{"id":"lb-001","name":"app-ns-web"}`)
	s.lbServices = append(s.lbServices, created)
	return created, nil
}
func (s *stubCMP) ListLBServices(_ context.Context, _ *f5client.ListLoadBalancersOptions) ([]json.RawMessage, error) {
	s.listLBN++
	return append([]json.RawMessage(nil), s.lbServices...), nil
}
func (s *stubCMP) DeleteLBService(_ context.Context, _ string) error {
	s.deleteLBN++
	return nil
}

func (s *stubCMP) GetLBService(
	_ context.Context,
	id string,
) (json.RawMessage, error) {
	if s.deleteLBN > 0 {
		return nil, &f5client.HTTPStatusError{StatusCode: 404, Status: "404 Not Found"}
	}
	return json.RawMessage(
		fmt.Sprintf(`{"id":%q,"name":"test-lb","status":"Created","operating_status":"Active","vpc_id":"vpc-1","vpc_name":"vpc-1","network_id":"subnet-service","region":"dev"}`, id),
	), nil
}

func (s *stubCMP) CreateLBServiceVIP(_ context.Context, _, subnetID string) (json.RawMessage, error) {
	if s.vipCreateErr != nil {
		return nil, s.vipCreateErr
	}
	s.createVN++
	s.lastVIPCreateSubnet = strings.TrimSpace(subnetID)
	created := json.RawMessage(`{"id":101,"ip_address":"10.0.0.10"}`)
	s.vips = append(s.vips, created)
	return created, nil
}
func (s *stubCMP) GetLBServiceVIPs(_ context.Context, _, subnetID string) ([]json.RawMessage, error) {
	s.listVIPN++
	s.lastVIPListSubnet = strings.TrimSpace(subnetID)
	return append([]json.RawMessage(nil), s.vips...), nil
}
func (s *stubCMP) DeleteLBServiceVIP(_ context.Context, _, _ string) error {
	s.deleteVN++
	return nil
}
func (s *stubCMP) CreateLBVirtualServer(_ context.Context, _ string, q url.Values) (json.RawMessage, error) {
	s.createVSN++
	s.lastVSQ = q
	created := json.RawMessage(fmt.Sprintf(`{"id":"vs-%03d","name":%q,"status":"Active","protocol":%q,"port":%s,"vip_port":{"id":%s}}`, s.createVSN, q.Get("name"), q.Get("protocol"), q.Get("port"), q.Get("vip_port_id")))
	s.vsList = append(s.vsList, created)
	return created, nil
}
func (s *stubCMP) ListLBVirtualServers(_ context.Context, _ string) ([]json.RawMessage, error) {
	s.listVSN++
	return append([]json.RawMessage(nil), s.vsList...), nil
}
func (s *stubCMP) GetLBVirtualServer(_ context.Context, _, id string) (json.RawMessage, error) {
	for _, raw := range s.vsList {
		var item struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &item) == nil && item.ID == id {
			return raw, nil
		}
	}
	return nil, &f5client.HTTPStatusError{StatusCode: 404, Status: "404 Not Found"}
}
func (s *stubCMP) DeleteLBVirtualServer(_ context.Context, _, id string) error {
	s.deleteVSN++
	for i, raw := range s.vsList {
		var item struct{ ID string }
		if json.Unmarshal(raw, &item) == nil && item.ID == id {
			s.vsList = append(s.vsList[:i], s.vsList[i+1:]...)
			break
		}
	}
	return nil
}
func (s *stubCMP) ListLBServiceCertificates(context.Context, string) ([]json.RawMessage, error) {
	return append([]json.RawMessage(nil), s.certs...), nil
}
func (s *stubCMP) CreateLBServiceCertificate(_ context.Context, _ string, q url.Values) (json.RawMessage, error) {
	s.createCertN++
	s.lastCertQ = q
	created := json.RawMessage(`{"id":"cert-001","name":"` + q.Get("name") + `"}`)
	s.certs = append(s.certs, created)
	return created, nil
}
func (s *stubCMP) DeleteLBServiceCertificate(context.Context, string, string) error { return nil }
func (s *stubCMP) AttachLBVirtualServerCertificate(_ context.Context, _, _, certID string) error {
	s.attachCertN++
	s.attachedCertID = certID
	return nil
}
func (s *stubCMP) DetachLBVirtualServerCertificate(context.Context, string, string, string) error {
	return nil
}
func (s *stubCMP) GetCompute(_ context.Context, id string) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"` + id + `","instance_name":"node-` + id + `","vpc_id":"vpc-1","network_id":"subnet-service","region":"dev","ports":[{"id":5001,"fixed_ips":["` + id + `"],"network_id":"subnet-service","device_id":"` + id + `","device_type":"compute"}]}`), nil
}
func (s *stubCMP) GetNetwork(_ context.Context, id string) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"` + id + `","vpc_id":"vpc-1","status":"Active"}`), nil
}
func (s *stubCMP) ListLBVirtualServerPools(_ context.Context, _, _ string) ([]json.RawMessage, error) {
	return nil, nil
}
func (s *stubCMP) CreateLBVirtualServerPool(_ context.Context, _, _ string, q url.Values) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"pool-001","pool_name":"` + q.Get("pool_name") + `","members":[]}`), nil
}
func (s *stubCMP) GetLBVirtualServerPool(_ context.Context, _, _, _ string) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"pool-001","pool_name":"pool"}`), nil
}
func (s *stubCMP) DeleteLBVirtualServerPool(_ context.Context, _, _, _ string) error     { return nil }
func (s *stubCMP) SetDefaultLBVirtualServerPool(_ context.Context, _, _, _ string) error { return nil }
func (s *stubCMP) CreateLBVirtualServerPoolMember(_ context.Context, _, _, _ string, q url.Values) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"member-001"}`), nil
}
func (s *stubCMP) UpdateLBVirtualServerPoolMember(_ context.Context, _, _, _, _ string, q url.Values) (json.RawMessage, error) {
	return json.RawMessage(`{"id":"member-001"}`), nil
}
func (s *stubCMP) DeleteLBVirtualServerPoolMember(_ context.Context, _, _, _, _ string) error {
	return nil
}

func newTestReconciler(t *testing.T, objs ...client.Object) (*serviceReconciler, client.Client, *stubCMP) {
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

	stub := &stubCMP{}
	r := &serviceReconciler{
		Client:            c,
		Scheme:            s,
		cmp:               stub,
		loadBalancerClass: defaultLBClass,
		Recorder:          record.NewFakeRecorder(10),
		shootNamespace:    "shoot-test",
		defaultVPCID:      "vpc-1",
		defaultVPCName:    "vpc-1",
		defaultNetworkID:  "subnet-service",
		defaultRegion:     "dev",
	}
	return r, c, stub
}

func TestReconcile_AllocatesVIPAndProgramsCMPVirtualServer(t *testing.T) {
	ctx := context.Background()

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      "web",
		Namespace: "ns",
		Annotations: map[string]string{
			lbannotations.SubnetID: "subnet-service",
		},
	}}
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.Ports = []corev1.ServicePort{{Port: 8080, NodePort: 30080}}
	svc.Spec.LoadBalancerClass = ptr.To(defaultLBClass)

	n1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "apc:///dev/172.18.0.10"}}
	n1.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.10"}}
	n1.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	n2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}, Spec: corev1.NodeSpec{ProviderID: "apc:///dev/172.18.0.11"}}
	n2.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.11"}}
	n2.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}

	r, c, stub := newTestReconciler(t, svc, n1, n2)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if stub.listLBN != 1 {
		t.Fatalf("expected one deterministic pre-create LB lookup, got %d", stub.listLBN)
	}
	if stub.createLBN != 1 {
		t.Fatalf("expected CreateLBService called once, got %d", stub.createLBN)
	}
	if stub.listVIPN != 0 {
		t.Fatalf("expected no GetLBServiceVIPs call before fresh VIP create, got %d", stub.listVIPN)
	}
	if stub.createVN != 1 {
		t.Fatalf("expected CreateLBServiceVIP called once, got %d", stub.createVN)
	}
	if stub.lastVIPCreateSubnet != "subnet-service" {
		t.Fatalf("expected VIP create subnet %q, got %q", "subnet-service", stub.lastVIPCreateSubnet)
	}
	if stub.listVSN != 2 {
		t.Fatalf("expected ListLBVirtualServers called twice (recovery + ensure), got %d", stub.listVSN)
	}
	if stub.createVSN != 1 {
		t.Fatalf("expected CreateLBVirtualServer called once, got %d", stub.createVSN)
	}

	if stub.lastVSQ == nil {
		t.Fatalf("expected CreateLBVirtualServer to capture query")
	}
	if got := stub.lastVSQ.Get("vip_port_id"); got != "101" {
		t.Fatalf("expected vip_port_id=101, got %q", got)
	}
	if got := stub.lastVSQ.Get("port"); got != "8080" {
		t.Fatalf("expected VS port=8080, got %q", got)
	}
	nodeParams := stub.lastVSQ["nodes"]
	if len(nodeParams) != 2 {
		t.Fatalf("expected 2 nodes params, got %d (%v)", len(nodeParams), nodeParams)
	}
	for _, raw := range nodeParams {
		var n struct {
			ResourceIP   string `json:"resource_ip"`
			ResourceType string `json:"resource_type"`
			Port         int32  `json:"port"`
		}
		if err := json.Unmarshal([]byte(raw), &n); err != nil {
			t.Fatalf("unmarshal node param %q: %v", raw, err)
		}
		if n.Port != 30080 {
			t.Fatalf("expected node port 30080, got %d", n.Port)
		}
		if n.ResourceType != "compute" {
			t.Fatalf("expected resource_type=compute, got %q", n.ResourceType)
		}
		if n.ResourceIP != "172.18.0.10" && n.ResourceIP != "172.18.0.11" {
			t.Fatalf("unexpected resource_ip %q", n.ResourceIP)
		}
	}

	gotSvc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), gotSvc); err != nil {
		t.Fatalf("get svc: %v", err)
	}
	if gotSvc.Annotations[annLBServiceID] != "lb-001" {
		t.Fatalf("expected lb-service-id annotation, got %q", gotSvc.Annotations[annLBServiceID])
	}
	graph, ok := readObservedGraph(gotSvc.Annotations)
	lbFound := false
	for _, lb := range graph.LBServices {
		if lb.ExternalID == "lb-001" {
			lbFound = true
			break
		}
	}
	if !ok || !lbFound || graph.VirtualServers[r.desiredVirtualServerName(svc, 8080)].ExternalID != "vs-001" {
		t.Fatalf("expected complete observed graph annotation, got %#v", graph)
	}
	if got := strings.TrimSpace(stub.lastVSQ.Get("name")); got != r.desiredVirtualServerName(svc, 8080) {
		t.Fatalf("expected shoot-aware VS name %q, got %q", r.desiredVirtualServerName(svc, 8080), got)
	}
	if gotSvc.Annotations[annVIPAddress] != "10.0.0.10" {
		t.Fatalf("expected vip-address annotation, got %q", gotSvc.Annotations[annVIPAddress])
	}
	if strings.TrimSpace(gotSvc.Annotations[annBackendHash]) == "" {
		t.Fatalf("expected backend-hash annotation to be set")
	}
	if gotSvc.Annotations[annObservedGeneration] != "0" {
		t.Fatalf("expected observed generation annotation, got %q", gotSvc.Annotations[annObservedGeneration])
	}
	if len(gotSvc.Status.LoadBalancer.Ingress) != 1 || gotSvc.Status.LoadBalancer.Ingress[0].IP != "10.0.0.10" {
		t.Fatalf("expected service status vip, got %#v", gotSvc.Status.LoadBalancer.Ingress)
	}
}

func TestStrictProvidedResourcePolicies_StickyProvidedModes(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      "web",
		Namespace: "ns",
		UID:       "uid-1",
		Annotations: map[string]string{
			annLBServiceID:     "lb-1",
			annVIPPortID:       "vip-1",
			annLBServiceIDMode: idModeProvided,
			annVIPPortIDMode:   idModeProvided,
		},
	}}
	observed := model.ObservedState{Graph: model.NewObservedGraph()}
	observed.Graph.LBServices["lb"] = model.ObservedResource{ExternalID: "lb-1", Ownership: model.Ownership{SourceKind: "Service", SourceNamespace: "ns", SourceName: "web", SourceUID: "uid-1"}}
	observed.Graph.VIPs["vip"] = model.ObservedResource{ExternalID: "vip-1", Ownership: model.Ownership{SourceKind: "Service", SourceNamespace: "ns", SourceName: "web", SourceUID: "uid-1"}}

	strictLB, strictVIP := strictProvidedResourcePolicies(svc, observed)
	if !strictLB || !strictVIP {
		t.Fatalf("expected sticky strict mode for provided IDs, got lb=%t vip=%t", strictLB, strictVIP)
	}
}

func TestStrictProvidedResourcePolicies_LegacyFallbackWhenOwned(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      "web",
		Namespace: "ns",
		UID:       "uid-1",
		Annotations: map[string]string{
			annLBServiceID: "lb-1",
			annVIPPortID:   "vip-1",
		},
	}}
	observed := model.ObservedState{Graph: model.NewObservedGraph()}
	observed.Graph.LBServices["lb"] = model.ObservedResource{ExternalID: "lb-1", Ownership: model.Ownership{SourceKind: "Service", SourceNamespace: "ns", SourceName: "web", SourceUID: "uid-1"}}
	observed.Graph.VIPs["vip"] = model.ObservedResource{ExternalID: "vip-1", Ownership: model.Ownership{SourceKind: "Service", SourceNamespace: "ns", SourceName: "web", SourceUID: "uid-1"}}

	strictLB, strictVIP := strictProvidedResourcePolicies(svc, observed)
	if strictLB || strictVIP {
		t.Fatalf("expected managed fallback mode when ownership is ours, got lb=%t vip=%t", strictLB, strictVIP)
	}
}

func TestReconcile_RecreatesVirtualServerWhenNodesChange(t *testing.T) {
	ctx := context.Background()

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns"}}
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.Ports = []corev1.ServicePort{{Port: 8080, NodePort: 30080}}
	svc.Spec.LoadBalancerClass = ptr.To(defaultLBClass)

	n1 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "apc:///dev/172.18.0.10"}}
	n1.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.10"}}
	n1.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	n2 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n2"}, Spec: corev1.NodeSpec{ProviderID: "apc:///dev/172.18.0.11"}}
	n2.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.11"}}
	n2.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}

	r, c, stub := newTestReconciler(t, svc, n1, n2)

	// First reconcile provisions VS.
	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile(1): %v", err)
	}
	if stub.createVSN != 1 {
		t.Fatalf("expected CreateLBVirtualServer called once, got %d", stub.createVSN)
	}

	// Add a new node, then reconcile again. CMP's aggregate API owns the pool
	// members, so a backend-set change begins a delete-before-recreate cycle.
	n3 := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n3"}, Spec: corev1.NodeSpec{ProviderID: "apc:///dev/172.18.0.12"}}
	n3.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.12"}}
	n3.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}
	if err := c.Create(ctx, n3); err != nil {
		t.Fatalf("create node n3: %v", err)
	}

	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile(2): %v", err)
	}
	if stub.deleteVSN != 1 {
		t.Fatalf("expected one aggregate listener replacement delete, got %d", stub.deleteVSN)
	}
	if stub.createVSN != 1 {
		t.Fatalf("expected replacement to wait for deletion before a second create, got %d creates", stub.createVSN)
	}
}

func TestReconcile_CleansCMPWhenServiceTypeChanges(t *testing.T) {
	ctx := context.Background()

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:       "web",
		Namespace:  "ns",
		Finalizers: []string{finalizerName},
		Annotations: map[string]string{
			annLBServiceID:     "lb-001",
			annVIPPortID:       "vip-001",
			annVirtualServerID: "vs-001",
		},
	}}
	svc.Spec.Type = corev1.ServiceTypeClusterIP

	r, c, stub := newTestReconciler(t, svc)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if stub.deleteVSN != 1 || stub.deleteVN != 1 || stub.deleteLBN != 1 {
		t.Fatalf("expected CMP deletes (vs,vip,lb) = (1,1,1), got (%d,%d,%d)", stub.deleteVSN, stub.deleteVN, stub.deleteLBN)
	}

	gotSvc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), gotSvc); err != nil {
		if !apierrors.IsNotFound(err) {
			t.Fatalf("get svc: %v", err)
		}
	} else if controllerutil.ContainsFinalizer(gotSvc, finalizerName) {
		t.Fatalf("expected finalizer removed")
	}
}

func TestReconcile_SkipsWhenLoadBalancerClassMismatch(t *testing.T) {
	ctx := context.Background()

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:       "web",
		Namespace:  "ns",
		Finalizers: []string{finalizerName},
		Annotations: map[string]string{
			annLBServiceID:     "lb-001",
			annVIPPortID:       "vip-001",
			annVirtualServerID: "vs-001",
		},
	}}
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.Ports = []corev1.ServicePort{{Port: 8080, NodePort: 30080}}
	svc.Spec.LoadBalancerClass = ptr.To("some.other/class")

	r, c, stub := newTestReconciler(t, svc)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if stub.createLBN != 0 {
		t.Fatalf("expected no CMP creates, got %d", stub.createLBN)
	}
	if stub.deleteVSN != 1 || stub.deleteVN != 1 || stub.deleteLBN != 1 {
		t.Fatalf("expected CMP deletes (vs,vip,lb) = (1,1,1), got (%d,%d,%d)", stub.deleteVSN, stub.deleteVN, stub.deleteLBN)
	}

	gotSvc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), gotSvc); err == nil {
		if controllerutil.ContainsFinalizer(gotSvc, finalizerName) {
			t.Fatalf("expected finalizer removed")
		}
	}
}

func TestReconcile_DeleteCleansCMPResources(t *testing.T) {
	ctx := context.Background()

	now := metav1.NewTime(time.Now())
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:              "web",
		Namespace:         "ns",
		Finalizers:        []string{finalizerName},
		DeletionTimestamp: &now,
		Annotations: map[string]string{
			annLBServiceID:     "lb-001",
			annVIPPortID:       "vip-001",
			annVirtualServerID: "vs-001",
		},
	}}

	r, c, stub := newTestReconciler(t, svc)

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if stub.deleteVSN != 1 {
		t.Fatalf("expected DeleteLBVirtualServer called once, got %d", stub.deleteVSN)
	}
	if stub.deleteVN != 1 {
		t.Fatalf("expected DeleteLBServiceVIP called once, got %d", stub.deleteVN)
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
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected NotFound or updated object, got: %v", err)
	}
}

func TestReconcile_DeleteDoesNotDeleteProvidedLBAndVIP(t *testing.T) {
	ctx := context.Background()
	now := metav1.NewTime(time.Now())
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:              "web",
		Namespace:         "ns",
		Finalizers:        []string{finalizerName},
		DeletionTimestamp: &now,
		Annotations: map[string]string{
			annLBServiceID:     "lb-001",
			annVIPPortID:       "vip-001",
			annVirtualServerID: "vs-001",
			annLBServiceIDMode: idModeProvided,
			annVIPPortIDMode:   idModeProvided,
		},
	}}

	r, _, stub := newTestReconciler(t, svc)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stub.deleteVSN != 1 || stub.deleteVN != 0 || stub.deleteLBN != 0 {
		t.Fatalf("expected only VS delete for provided LB/VIP, got vs=%d vip=%d lb=%d", stub.deleteVSN, stub.deleteVN, stub.deleteLBN)
	}
}

func TestReconcile_DeleteKeepsProvidedLBDeletesManagedVIP(t *testing.T) {
	ctx := context.Background()
	now := metav1.NewTime(time.Now())
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:              "web",
		Namespace:         "ns",
		Finalizers:        []string{finalizerName},
		DeletionTimestamp: &now,
		Annotations: map[string]string{
			annLBServiceID:     "lb-001",
			annVIPPortID:       "vip-001",
			annVirtualServerID: "vs-001",
			annLBServiceIDMode: idModeProvided,
			annVIPPortIDMode:   idModeManaged,
		},
	}}

	r, _, stub := newTestReconciler(t, svc)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stub.deleteVSN != 1 || stub.deleteVN != 1 || stub.deleteLBN != 0 {
		t.Fatalf("expected VS+VIP delete and LB preserve for provided LB, got vs=%d vip=%d lb=%d", stub.deleteVSN, stub.deleteVN, stub.deleteLBN)
	}
}

func TestReconcile_PersistsLBCheckpointBeforeVIPAndVS(t *testing.T) {
	ctx := context.Background()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns"}}
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.Ports = []corev1.ServicePort{{Port: 8080, NodePort: 30080}}
	svc.Spec.LoadBalancerClass = ptr.To(defaultLBClass)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "apc:///dev/172.18.0.10"}}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.10"}}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}

	r, c, stub := newTestReconciler(t, svc, node)
	stub.vipCreateErr = errors.New("vip create failed")

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)}); err != nil {
		t.Fatalf("Reconcile should requeue on ensure error without returning error, got: %v", err)
	}

	got := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), got); err != nil {
		t.Fatalf("get svc: %v", err)
	}
	if got.Annotations[annLBServiceID] != "lb-001" {
		t.Fatalf("expected LB checkpoint annotation to be persisted, got %q", got.Annotations[annLBServiceID])
	}
	if graph, ok := readObservedGraph(got.Annotations); !ok || len(graph.LBServices) == 0 {
		t.Fatalf("expected observed graph with LB checkpoint, got %#v", graph)
	}
}

// Ensure we don't use the f5client import only in non-test code.
var _ = f5client.CMPResourceIDs{}

// Ensure we don't leave unused imports around in this file.
var _ = strings.TrimSpace

func TestReconcile_ProgramsAndTracksEveryServicePortIndependently(t *testing.T) {
	ctx := context.Background()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "multi", Namespace: "ns"}}
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.Ports = []corev1.ServicePort{
		{Name: "http", Port: 80, NodePort: 30080, Protocol: corev1.ProtocolTCP},
		{Name: "https", Port: 443, NodePort: 30443, Protocol: corev1.ProtocolTCP},
	}
	svc.Spec.LoadBalancerClass = ptr.To(defaultLBClass)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "apc:///dev/172.18.0.10"}}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.10"}}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}

	r, c, stub := newTestReconciler(t, svc, node)
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stub.createVSN != 2 {
		t.Fatalf("expected one virtual server per Service port, got %d", stub.createVSN)
	}
	got := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), got); err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	observed := readServicePortObserved(got.Annotations)
	if len(observed) != 2 || observed["http/80/tcp"].VirtualServerID == "" || observed["https/443/tcp"].VirtualServerID == "" {
		t.Fatalf("expected per-port observed virtual servers, got %#v", observed)
	}
}

func TestReconcile_VIPGroupBootstrapsSharedParentWithoutSibling(t *testing.T) {
	ctx := context.Background()

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      "grouped",
		Namespace: "ns",
		Annotations: map[string]string{
			lbannotations.VIPGroup: "shared-apps",
		},
	}}
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	svc.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: 8080, NodePort: 30080, Protocol: corev1.ProtocolTCP}}
	svc.Spec.LoadBalancerClass = ptr.To(defaultLBClass)

	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Spec: corev1.NodeSpec{ProviderID: "apc:///dev/172.18.0.10"}}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "172.18.0.10"}}
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}

	r, c, stub := newTestReconciler(t, svc, node)

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(svc)}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if stub.createLBN != 1 || stub.createVN != 1 || stub.createVSN != 1 {
		t.Fatalf("expected grouped bootstrap to create shared LB/VIP/VS, got lb=%d vip=%d vs=%d", stub.createLBN, stub.createVN, stub.createVSN)
	}

	got := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(svc), got); err != nil {
		t.Fatalf("getting Service: %v", err)
	}
	if got.Annotations[annLBServiceID] == "" || got.Annotations[annVIPPortID] == "" || got.Annotations[annVIPAddress] == "" {
		t.Fatalf("expected grouped bootstrap to persist shared parent annotations, got %#v", got.Annotations)
	}
	if len(got.Status.LoadBalancer.Ingress) != 1 || got.Status.LoadBalancer.Ingress[0].IP == "" {
		t.Fatalf("expected service status ingress VIP, got %#v", got.Status.LoadBalancer.Ingress)
	}
}
