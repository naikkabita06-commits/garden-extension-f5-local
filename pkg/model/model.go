package model

import lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"

// Owner identifies the Kubernetes object that owns a desired CMP/F5 stack.
type Owner struct {
	Kind      string
	Namespace string
	Name      string
	UID       string
}

// Ownership carries the labels/identity used to discover, adopt, and safely
// delete provider resources. A matching display name without ownership is not
// sufficient for adoption.
type Ownership struct {
	ManagedBy       string
	ClusterUID      string
	SourceKind      string
	SourceNamespace string
	SourceName      string
	SourceUID       string
	ResourceRole    string
	SharedGroup     string
}

// LBService is the desired parent CMP LB Service.
type LBService struct {
	Name        string
	Description string
	FlavorID    int32
	NetworkID   string
	VPCID       string
	VPCName     string
	Ownership   Ownership
}

// VIP is the desired frontend address allocation under an LBService.
type VIP struct {
	Name      string
	Address   string
	Ownership Ownership
}

// VirtualServer is the desired frontend listener/virtual server.
type VirtualServer struct {
	Name             string
	FrontendPort     int32
	BackendNodePort  int32
	Protocol         string
	RoutingAlgorithm string
	PersistenceType  string
	DrainingTimeout  int32
	SourceRanges     []string
	Monitor          *Monitor
	DefaultPoolName  string
	Ownership        Ownership
}

// Pool is the desired backend pool for one listener or route.
type Pool struct {
	Name      string
	Service   string
	PortName  string
	Port      int32
	Members   []BackendMember
	Monitor   *Monitor
	Ownership Ownership
}

// RoutingRule is a deterministic host/path rule targeting a named backend pool.
type RoutingRule struct {
	Name      string
	Host      string
	Path      string
	MatchType string
	PoolName  string
	Priority  int32
	Ownership Ownership
}

// Certificate is the desired TLS certificate reference for HTTPS listeners.
type Certificate struct {
	Name       string
	SecretName string
	Hosts      []string
	Ownership  Ownership
}

// BackendMember is the normalized backend representation used by model
// builders before a deployer translates it into CMP-specific request fields.
type BackendMember struct {
	IP     string
	Port   int32
	Weight int
}

// Monitor is the desired health-monitor configuration.
type Monitor struct {
	Name     string
	Type     string
	Path     string
	Interval int32
}

// ObservedResource describes one CMP/F5 resource discovered or created by the
// deployment layer. LogicalID is the deterministic desired-state identifier;
// ExternalID is the provider ID returned by CMP.
type ObservedResource struct {
	LogicalID  string
	ExternalID string
	Name       string
	Address    string
	Ownership  Ownership
}

// ObservedGraph is the per-resource provider graph observed during a reconcile.
// It is the durable shape expected by the stack deployer; the legacy scalar
// fields on ObservedState below remain only for compatibility with existing
// status annotations while controllers migrate to graph persistence.
type ObservedGraph struct {
	LBServices     map[string]ObservedResource
	VIPs           map[string]ObservedResource
	VirtualServers map[string]ObservedResource
	Pools          map[string]ObservedResource
	Members        map[string]ObservedResource
	Monitors       map[string]ObservedResource
	RoutingRules   map[string]ObservedResource
	Certificates   map[string]ObservedResource
}

// NewObservedGraph returns an initialized observed provider graph.
func NewObservedGraph() ObservedGraph {
	return ObservedGraph{
		LBServices:     map[string]ObservedResource{},
		VIPs:           map[string]ObservedResource{},
		VirtualServers: map[string]ObservedResource{},
		Pools:          map[string]ObservedResource{},
		Members:        map[string]ObservedResource{},
		Monitors:       map[string]ObservedResource{},
		RoutingRules:   map[string]ObservedResource{},
		Certificates:   map[string]ObservedResource{},
	}
}

// ObservedState describes provider resources discovered or created by the
// deployment layer and returned to status writers. New code should prefer Graph.
type ObservedState struct {
	Graph             ObservedGraph
	LBServiceID       string
	VIPPortID         string
	VIPAddress        string
	VirtualServerID   string
	VirtualServerName string
}

// EnsureGraph initializes the observed graph and mirrors legacy scalar IDs into
// their corresponding graph buckets when present.
func (s *ObservedState) EnsureGraph() {
	// Graphs written by older versions may contain only some buckets.  Preserve
	// every observed entry while making each bucket safe for incremental
	// reconciliation; replacing the whole graph here used to discard children.
	if s.Graph.LBServices == nil {
		s.Graph.LBServices = map[string]ObservedResource{}
	}
	if s.Graph.VIPs == nil {
		s.Graph.VIPs = map[string]ObservedResource{}
	}
	if s.Graph.VirtualServers == nil {
		s.Graph.VirtualServers = map[string]ObservedResource{}
	}
	if s.Graph.Pools == nil {
		s.Graph.Pools = map[string]ObservedResource{}
	}
	if s.Graph.Members == nil {
		s.Graph.Members = map[string]ObservedResource{}
	}
	if s.Graph.Monitors == nil {
		s.Graph.Monitors = map[string]ObservedResource{}
	}
	if s.Graph.RoutingRules == nil {
		s.Graph.RoutingRules = map[string]ObservedResource{}
	}
	if s.Graph.Certificates == nil {
		s.Graph.Certificates = map[string]ObservedResource{}
	}
	if s.LBServiceID != "" {
		if _, ok := s.Graph.LBServices["legacy/lb-service"]; !ok {
			s.Graph.LBServices["legacy/lb-service"] = ObservedResource{LogicalID: "legacy/lb-service", ExternalID: s.LBServiceID}
		}
	}
	if s.VIPPortID != "" {
		if _, ok := s.Graph.VIPs["legacy/vip"]; !ok {
			s.Graph.VIPs["legacy/vip"] = ObservedResource{LogicalID: "legacy/vip", ExternalID: s.VIPPortID, Address: s.VIPAddress}
		}
	}
	if s.VirtualServerID != "" {
		if _, ok := s.Graph.VirtualServers["legacy/virtual-server"]; !ok {
			s.Graph.VirtualServers["legacy/virtual-server"] = ObservedResource{LogicalID: "legacy/virtual-server", ExternalID: s.VirtualServerID, Name: s.VirtualServerName}
		}
	}
}

// DeploymentResult is the typed result returned by a deployer after reconciling
// desired resources into CMP/F5.
type DeploymentResult struct {
	Observed ObservedState
	Changed  bool
}

// ServicePort models one frontend listener/virtual-server desired for a
// Kubernetes Service port.
type ServicePort struct {
	Name         string
	FrontendPort int32
	NodePort     int32
	Protocol     string
	Backends     []BackendMember
}

// LoadBalancerStack is the in-memory desired-state root for one Kubernetes
// object. It intentionally contains no raw CMP JSON or HTTP transport fields.
type LoadBalancerStack struct {
	Owner          Owner
	Ownership      Ownership
	Config         lbannotations.LBConfig
	LBService      LBService
	VIP            VIP
	VirtualServers []VirtualServer
	Pools          []Pool
	RoutingRules   []RoutingRule
	Certificates   []Certificate
	Ports          []ServicePort
}
