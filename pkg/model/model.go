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
	Members   []BackendMember
	Monitor   *Monitor
	Ownership Ownership
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

// ObservedState describes provider resources discovered or created by the
// deployment layer and returned to status writers.
type ObservedState struct {
	LBServiceID       string
	VIPPortID         string
	VIPAddress        string
	VirtualServerID   string
	VirtualServerName string
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
	Ports          []ServicePort
}
