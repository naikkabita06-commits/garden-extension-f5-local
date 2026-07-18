package annotations

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseServicePrefersSpecSourceRangesAndClientIPAffinity(t *testing.T) {
	svc := &corev1.Service{}
	svc.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
	svc.Spec.LoadBalancerSourceRanges = []string{"10.0.0.0/24"}
	svc.Annotations = map[string]string{
		SourceRanges:     "192.168.0.0/24",
		Protocol:         "https",
		HealthPath:       "/healthz",
		HealthInterval:   "15",
		DrainingTimeout:  "30",
		RoutingAlgorithm: "least_connections",
	}

	cfg := ParseService(svc)
	if cfg.PersistenceType != "source_addr" {
		t.Fatalf("expected source_addr persistence, got %q", cfg.PersistenceType)
	}
	if len(cfg.SourceRanges) != 1 || cfg.SourceRanges[0] != "10.0.0.0/24" {
		t.Fatalf("expected spec source ranges to win, got %#v", cfg.SourceRanges)
	}
	if cfg.ProtocolOverride != "HTTPS" {
		t.Fatalf("expected HTTPS override, got %q", cfg.ProtocolOverride)
	}
	if cfg.HealthType != "http" || cfg.HealthPath != "/healthz" {
		t.Fatalf("expected http health path, got type=%q path=%q", cfg.HealthType, cfg.HealthPath)
	}
	if cfg.HealthInterval != 15 || cfg.DrainingTimeout != 30 || cfg.RoutingAlgorithm != "least_connections" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestParseServiceIgnoresInvalidNumericAndProtocolAnnotations(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		Protocol:        "smtp",
		HealthInterval:  "0",
		DrainingTimeout: "-1",
	}}}

	cfg := ParseService(svc)
	if cfg.ProtocolOverride != "" {
		t.Fatalf("expected invalid protocol ignored, got %q", cfg.ProtocolOverride)
	}
	if cfg.HealthInterval != 30 || cfg.DrainingTimeout != 0 {
		t.Fatalf("expected defaults for invalid numeric values, got %#v", cfg)
	}
}
