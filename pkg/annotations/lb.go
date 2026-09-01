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
	HealthPort       = "f5.extensions.gardener.cloud/health-check-port"
	HealthTimeout    = "f5.extensions.gardener.cloud/health-check-timeout"
	SourceRanges     = "f5.extensions.gardener.cloud/source-ranges"
	DrainingTimeout  = "f5.extensions.gardener.cloud/connection-draining-timeout"
	VIPGroup         = "f5.extensions.gardener.cloud/vip-group"
	VPCID            = "f5.extensions.gardener.cloud/vpc-id"
	SubnetID         = "f5.extensions.gardener.cloud/subnet-id"
	NetworkID        = "f5.extensions.gardener.cloud/network-id"
	FlavorID         = "f5.extensions.gardener.cloud/flavor-id"
	CMPComputeID     = "f5.extensions.gardener.cloud/cmp-compute-id"
	BackendIP        = "f5.extensions.gardener.cloud/backend-ip"
)

// LBConfig is the normalized, user-facing load-balancer configuration parsed
// from Kubernetes-native fields and F5 annotations. It is intentionally free of
// CMP transport details so controllers and model builders can share it.
type LBConfig struct {
	RoutingAlgorithm string
	HealthInterval   int32
	HealthType       string
	HealthPath       string
	HealthPort       int32
	HealthTimeout    int32
	ProtocolOverride string
	SourceRanges     []string
	PersistenceType  string
	DrainingTimeout  int32

	// CMP placement
	VPCID     string
	NetworkID string
	FlavorID  int32
}

func DefaultLBConfig() LBConfig {
	return LBConfig{RoutingAlgorithm: "ROUND_ROBIN", HealthInterval: 30, HealthTimeout: 16, HealthType: "tcp"}
}

// NormalizeRoutingAlgorithm converts user-facing spellings to CMP's canonical
// enum representation. CMP UI requests and responses use uppercase values such
// as ROUND_ROBIN and LEAST_CONNECTIONS.
func NormalizeRoutingAlgorithm(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "-", "_")
	value = strings.Join(strings.Fields(value), "_")
	return strings.ToUpper(value)
}

// ParseService reads supported F5 annotations and Kubernetes-native Service
// fields. spec.loadBalancerSourceRanges takes precedence over the annotation.
func ParseService(svc *corev1.Service) LBConfig {
	cfg := DefaultLBConfig()
	if svc == nil {
		return cfg
	}
	if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
		cfg.PersistenceType = "source_ip"
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
		cfg.RoutingAlgorithm = NormalizeRoutingAlgorithm(v)
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
	if v := strings.TrimSpace(ann[HealthPort]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
			cfg.HealthPort = int32(n)
		}
	}
	if v := strings.TrimSpace(ann[HealthTimeout]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.HealthTimeout = int32(n)
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

	if v := strings.TrimSpace(ann[VPCID]); v != "" {
		cfg.VPCID = v
	}

	if v := strings.TrimSpace(ann[NetworkID]); v != "" {
		cfg.NetworkID = v
	}
	if v := strings.TrimSpace(ann[SubnetID]); v != "" {
		cfg.NetworkID = v
	}

	if v := strings.TrimSpace(ann[FlavorID]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.FlavorID = int32(n)
		}
	}
}
