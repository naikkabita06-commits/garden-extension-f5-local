package lifecycle

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	extensionsv1alpha1 "github.com/gardener/gardener/pkg/apis/extensions/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	f5v1alpha1 "github.com/gardener/gardener-extension-f5/pkg/apis/f5/v1alpha1"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
)

func TestReconcile_CreatesConfigWhenMissing_FromProviderConfig(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := f5v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding f5 scheme: %v", err)
	}
	if err := extensionsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding extensions scheme: %v", err)
	}

	const (
		ns   = "test-ns"
		name = "test-ext"
	)

	ex := &extensionsv1alpha1.Extension{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("123")}}
	ex.SetGroupVersionKind(schema.GroupVersionKind{Group: "extensions.gardener.cloud", Version: "v1alpha1", Kind: "Extension"})
	ex.Spec.Type = "f5"
	ex.Spec.ProviderConfig = &runtime.RawExtension{Raw: []byte(`{"spec":{"enableApplicationLB":true}}`)}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(ex).
		Build()

	a := &actuator{client: c}
	if err := a.Reconcile(context.Background(), logr.Discard(), ex); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	got := &f5v1alpha1.F5LoadBalancerConfig{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, got); err != nil {
		t.Fatalf("getting config: %v", err)
	}

	if !got.Spec.EnableApplicationLB {
		t.Fatalf("expected enableApplicationLB true, got false")
	}
	if len(got.OwnerReferences) == 0 {
		t.Fatalf("expected owner reference to be set")
	}
}

func TestReconcile_WritesExtensionProviderStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding k8s scheme: %v", err)
	}
	if err := f5v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding f5 scheme: %v", err)
	}
	if err := extensionsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding extensions scheme: %v", err)
	}

	const (
		ns   = "test-ns"
		name = "test-ext"
	)

	ready := true
	cfg := &f5v1alpha1.F5LoadBalancerConfig{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	cfg.Spec.ControlPlaneVIP = "1.2.3.4"
	cfg.Spec.ControlPlaneReady = &ready
	cfg.Spec.EnableApplicationLB = false
	cfg.Spec.CIS = &f5v1alpha1.CISConfig{BigIPURL: "https://100.65.242.181"}

	ex := &extensionsv1alpha1.Extension{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("123")}}
	ex.SetGroupVersionKind(schema.GroupVersionKind{Group: "extensions.gardener.cloud", Version: "v1alpha1", Kind: "Extension"})
	ex.Spec.Type = "f5"

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cfg, ex).
		WithStatusSubresource(cfg).
		WithStatusSubresource(ex).
		Build()

	a := &actuator{client: c}
	if err := a.Reconcile(context.Background(), logr.Discard(), ex); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	gotEx := &extensionsv1alpha1.Extension{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ex), gotEx); err != nil {
		t.Fatalf("getting extension: %v", err)
	}
	if gotEx.Status.ProviderStatus == nil || len(gotEx.Status.ProviderStatus.Raw) == 0 {
		t.Fatalf("expected providerStatus to be set")
	}

	var ps map[string]any
	if err := json.Unmarshal(gotEx.Status.ProviderStatus.Raw, &ps); err != nil {
		t.Fatalf("unmarshal providerStatus: %v", err)
	}
	if ps["controlPlaneVip"] != "1.2.3.4" {
		t.Fatalf("expected controlPlaneVip 1.2.3.4, got %v", ps["controlPlaneVip"])
	}
	if ps["bigIpManagementIp"] != "100.65.242.181" {
		t.Fatalf("expected bigIpManagementIp 100.65.242.181, got %v", ps["bigIpManagementIp"])
	}
}

func TestBuildCISArgs(t *testing.T) {
	args := buildCISArgs("https://10.0.0.1", "k8s-apps", []string{"--foo=bar"})

	wantContains := []string{
		"--agent=cccl",
		"--bigip-username=$(BIGIP_USERNAME)",
		"--bigip-password=$(BIGIP_PASSWORD)",
		"--bigip-url=https://10.0.0.1",
		"--bigip-partition=k8s-apps",
		"--namespace=all",
		"--pool-member-type=nodeport",
		"--foo=bar",
	}

	for _, w := range wantContains {
		found := false
		for _, a := range args {
			if a == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected args to contain %q, got %v", w, args)
		}
	}
}

func TestReconcileControlPlaneStatus_UsesControlPlaneReadyOverride(t *testing.T) {
	cfg := &f5v1alpha1.F5LoadBalancerConfig{}
	cfg.Spec.ControlPlaneVIP = "1.2.3.4"

	readyFalse := false
	cfg.Spec.ControlPlaneReady = &readyFalse
	(&actuator{}).reconcileControlPlaneStatus(logr.Discard(), cfg)
	cond := meta.FindStatusCondition(cfg.Status.Conditions, "ControlPlaneLoadBalancerReady")
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("expected CP Ready False, got %#v", cond)
	}

	readyTrue := true
	cfg.Spec.ControlPlaneReady = &readyTrue
	(&actuator{}).reconcileControlPlaneStatus(logr.Discard(), cfg)
	cond = meta.FindStatusCondition(cfg.Status.Conditions, "ControlPlaneLoadBalancerReady")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("expected CP Ready True, got %#v", cond)
	}
	if cfg.Status.VIP != "1.2.3.4" {
		t.Fatalf("expected status.vip to be set, got %q", cfg.Status.VIP)
	}
}

func TestReconcileControlPlaneStatus_NotConfiguredWhenVIPMissing(t *testing.T) {
	cfg := &f5v1alpha1.F5LoadBalancerConfig{}
	(&actuator{}).reconcileControlPlaneStatus(logr.Discard(), cfg)
	cond := meta.FindStatusCondition(cfg.Status.Conditions, "ControlPlaneLoadBalancerReady")
	if cond == nil {
		t.Fatalf("expected condition to be set")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "NotConfigured" {
		t.Fatalf("expected NotConfigured/False, got %#v", cond)
	}
}

func TestIsConditionTrue(t *testing.T) {
	conds := []metav1.Condition{{Type: "X", Status: metav1.ConditionTrue}}
	if !isConditionTrue(conds, "X") {
		t.Fatalf("expected condition true")
	}
	if isConditionTrue(conds, "Y") {
		t.Fatalf("expected condition false")
	}
}

func TestReconcile_BlockedWhenControlPlaneNotReady_NoError(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := f5v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding f5 scheme: %v", err)
	}
	if err := extensionsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding extensions scheme: %v", err)
	}

	const (
		ns   = "test-ns"
		name = "test-ext"
	)

	cfg := &f5v1alpha1.F5LoadBalancerConfig{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	cfg.Spec.EnableApplicationLB = true
	// Force the controller down the per-shoot control-plane VIP path, which requires
	// an explicitly configured VIP. Without it, ControlPlaneLoadBalancerReady remains
	// NotConfigured and app-plane is blocked.
	cfg.Spec.EnablePerShootControlPlaneVIP = true

	ex := &extensionsv1alpha1.Extension{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cfg, ex).
		WithStatusSubresource(cfg).
		Build()

	a := &actuator{client: c}
	if err := a.Reconcile(context.Background(), logr.Discard(), ex); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	got := &f5v1alpha1.F5LoadBalancerConfig{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cfg), got); err != nil {
		t.Fatalf("getting config: %v", err)
	}

	cpCond := meta.FindStatusCondition(got.Status.Conditions, "ControlPlaneLoadBalancerReady")
	if cpCond == nil || cpCond.Status != metav1.ConditionFalse || cpCond.Reason != "NotConfigured" {
		t.Fatalf("expected CP NotConfigured/False, got %#v", cpCond)
	}

	appCond := meta.FindStatusCondition(got.Status.Conditions, "ApplicationLoadBalancerReady")
	if appCond == nil || appCond.Status != metav1.ConditionFalse || appCond.Reason != "Blocked" {
		t.Fatalf("expected Application Blocked/False, got %#v", appCond)
	}
}
