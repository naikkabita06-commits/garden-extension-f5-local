package lbaas

import "context"

// Client is the typed CMP LBaaS capability required by the deployer.
type Client interface {
	ListLBServices(ctx context.Context) ([]LBService, error)
	CreateLBService(ctx context.Context, spec LBServiceSpec) (LBService, error)
	DeleteLBService(ctx context.Context, id string) error
	ListVIPs(ctx context.Context, lbServiceID string) ([]VIP, error)
	CreateVIP(ctx context.Context, lbServiceID string) (VIP, error)
	DeleteVIP(ctx context.Context, lbServiceID, vipID string) error
	ListVirtualServers(ctx context.Context, lbServiceID string) ([]VirtualServer, error)
	CreateVirtualServer(ctx context.Context, lbServiceID string, spec VirtualServerSpec) (VirtualServer, error)
	DeleteVirtualServer(ctx context.Context, lbServiceID, vsID string) error
	SearchNetworkPortsByIP(ctx context.Context, ip string) ([]NetworkPort, error)
}

type LBService struct{ ID, Name string }
type VIP struct{ ID, Address string }
type VirtualServer struct{ ID, Name string }

type NetworkPort struct {
	ID           int
	ResourceID   string
	ResourceType string
	IP           string
}

type LBServiceSpec struct {
	Name, Description         string
	FlavorID                  int32
	NetworkID, VPCID, VPCName string
}

type VirtualServerSpec struct {
	Name, VIPPortID, Protocol, RoutingAlgorithm string
	Port                                        int32
	MonitorType, MonitorPath                    string
	MonitorInterval                             int32
	PersistenceType                             string
	DrainingTimeout                             int32
	VPCID                                       string
	AllowedCIDRs                                []string
	Nodes                                       []BackendNodeSpec
}

type BackendNodeSpec struct {
	ResourceID    string
	ResourceType  string
	ResourceIP    string
	BackendPortID int
	Port          int32
	Weight        int
}
