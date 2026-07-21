package annotations

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	Protocol         = "f5.extensions.gardener.cloud/protocol"
	RoutingAlgorithm = "f5.extensions.gardener.cloud/routing-algorithm"
	HealthInterval   = "f5.extensions.gardener.cloud/health-check-interval"
	HealthType       = "f5.extensions.gardener.cloud/health-check-type"
	HealthPath       = "f5.extensions.gardener.cloud/health-check-path"
	SourceRanges     = "f5.extensions.gardener.cloud/source-ranges"
	DrainingTimeout  = "f5.extensions.gardener.cloud/connection-draining-timeout"
	VIPGroup         = "f5.extensions.gardener.cloud/vip-group"
	NetworkID        = "f5.extensions.gardener.cloud/network-id"
	FlavorID         = "f5.extensions.gardener.cloud/flavor-id"
)

// LBConfig is the normalized, user-facing load-balancer configuration parsed
// from Kubernetes-native fields and F5 annotations. It is intentionally free of
// CMP transport details so controllers and model builders can share it.
type LBConfig struct {
	RoutingAlgorithm string
	HealthInterval   int32
	HealthType       string
	HealthPath       string
	ProtocolOverride string
	SourceRanges     []string
	PersistenceType  string
	DrainingTimeout  int32

	// CMP placement
	NetworkID string
	FlavorID  int32
}

func DefaultLBConfig() LBConfig {
	return LBConfig{RoutingAlgorithm: "round_robin", HealthInterval: 30, HealthType: "tcp"}
}

// ParseService reads supported F5 annotations and Kubernetes-native Service
// fields. spec.loadBalancerSourceRanges takes precedence over the annotation.
func ParseService(svc *corev1.Service) LBConfig {
	cfg := DefaultLBConfig()
	if svc == nil {
		return cfg
	}
	if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
		cfg.PersistenceType = "source_addr"
	}
	parseAnnotations(&cfg, svc.Annotations)
	if len(svc.Spec.LoadBalancerSourceRanges) > 0 {
		cfg.SourceRanges = append([]string(nil), svc.Spec.LoadBalancerSourceRanges...)
	}
	return cfg
}

// ParseObject reads annotations from non-Service objects such as Ingress. It is
// deliberately limited to annotation-backed fields because those resources do
// not carry Kubernetes Service-specific source-range/session-affinity fields.
func ParseObject(obj metav1.Object) LBConfig {
	cfg := DefaultLBConfig()
	if obj == nil {
		return cfg
	}
	parseAnnotations(&cfg, obj.GetAnnotations())
	return cfg
}

func parseAnnotations(cfg *LBConfig, ann map[string]string) {
	if ann == nil {
		return
	}
	if v := strings.TrimSpace(ann[Protocol]); v != "" {
		switch upper := strings.ToUpper(v); upper {
		case "TCP", "UDP", "HTTP", "HTTPS":
			cfg.ProtocolOverride = upper
		}
	}
	if v := strings.TrimSpace(ann[RoutingAlgorithm]); v != "" {
		cfg.RoutingAlgorithm = v
	}
	if v := strings.TrimSpace(ann[HealthInterval]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.HealthInterval = int32(n)
		}
	}
	if v := strings.TrimSpace(ann[HealthType]); v != "" {
		switch lower := strings.ToLower(v); lower {
		case "tcp", "http":
			cfg.HealthType = lower
		}
	}
	if v := strings.TrimSpace(ann[HealthPath]); v != "" {
		cfg.HealthPath = v
		if cfg.HealthType == "tcp" {
			cfg.HealthType = "http"
		}
	}
	if v := strings.TrimSpace(ann[DrainingTimeout]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.DrainingTimeout = int32(n)
		}
	}
	if len(cfg.SourceRanges) == 0 {
		if v := strings.TrimSpace(ann[SourceRanges]); v != "" {
			for _, cidr := range strings.Split(v, ",") {
				if c := strings.TrimSpace(cidr); c != "" {
					cfg.SourceRanges = append(cfg.SourceRanges, c)
				}
			}
		}
	}

	if v := strings.TrimSpace(ann[NetworkID]); v != "" {
		cfg.NetworkID = v
	}

	if v := strings.TrimSpace(ann[FlavorID]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.FlavorID = int32(n)
		}
	}
}
