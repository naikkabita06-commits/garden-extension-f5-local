package lbaas

import (
	"context"
	"errors"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
)

// Client is the typed CMP LBaaS capability required by the deployer.
type Client interface {
	ListLBServices(
		ctx context.Context,
		opts *f5client.ListLoadBalancersOptions,
	) ([]LBService, error)
	CreateLBService(ctx context.Context, spec LBServiceSpec) (LBService, error)
	DeleteLBService(ctx context.Context, id string) error
	GetLBService(
		ctx context.Context,
		id string,
	) (LBService, error)
	ListVIPs(ctx context.Context, lbServiceID, subnetID string) ([]VIP, error)
	CreateVIP(ctx context.Context, lbServiceID, subnetID string) (VIP, error)
	DeleteVIP(ctx context.Context, lbServiceID, vipID string) error
	ListVirtualServers(ctx context.Context, lbServiceID string) ([]VirtualServer, error)
	GetVirtualServer(ctx context.Context, lbServiceID, vsID string) (VirtualServer, error)
	CreateVirtualServer(ctx context.Context, lbServiceID string, spec VirtualServerSpec) (VirtualServer, error)
	DeleteVirtualServer(ctx context.Context, lbServiceID, vsID string) error
	GetCompute(ctx context.Context, id string) (Compute, error)
}

type LBService struct {
	ID              string
	Name            string
	Status          string
	OperatingStatus string
	VPCID           string
	VPCName         string
	NetworkID       string
	Region          string
}
type VIP struct {
	ID           string
	Address      string
	Status       string
	NetworkID    string
	IPVersion    string
	AttachedToLB bool
}
type VirtualServer struct {
	ID         string
	Name       string
	VIPPortID  string
	VIPAddress string
	Status     string
	Protocol   string
	Port       int32
}

type NetworkPort struct {
	ID           int
	ResourceID   string
	ResourceType string
	IP           string
	NetworkID    string
	DeviceID     string
	DeviceType   string
	InstanceName string
}

type Compute struct {
	ID        string
	Name      string
	VPCID     string
	NetworkID string
	Region    string
	Status    string
	Ports     []ComputePort
}

type ComputePort struct {
	ID         int
	FixedIPs   []string
	NetworkID  string
	DeviceID   string
	DeviceType string
}

// Network is the CMP subnet/network placement record used to validate an
// explicit Service subnet override before creating provider resources.
type Network struct {
	ID     string
	VPCID  string
	Status string
}

type NetworkClient interface {
	GetNetwork(ctx context.Context, id string) (Network, error)
}

type LBServiceSpec struct {
	Name, Description         string
	FlavorID                  int32
	NetworkID, VPCID, VPCName string
}

type VirtualServerSpec struct {
	Name, VIPPortID, Protocol, RoutingAlgorithm  string
	Port                                         int32
	PoolName                                     string
	MonitorName, MonitorProtocol, MonitorPath    string
	MonitorInterval, MonitorPort, MonitorTimeout int32
	PersistenceType                              string
	PersistenceEnabled                           bool
	XForwardedFor                                bool
	DrainingTimeout                              int32
	VPCID                                        string
	AllowedCIDRs                                 []string
	Nodes                                        []BackendNodeSpec
}

type BackendNodeSpec struct {
	ResourceID     string
	ResourceType   string
	ResourceIP     string
	BackendPortID  int
	Port           int32
	Weight         int
	MaxConnections int
	InstanceName   string
	SourceType     string
}

// ProvisioningPendingError indicates the target resource exists but has not
// reached a ready/active state yet.
type ProvisioningPendingError struct {
	ResourceType string
	ResourceID   string
	Status       string
	Detail       string
}

func (e *ProvisioningPendingError) Error() string {
	return "resource is still provisioning: " + e.ResourceType + " " + e.ResourceID + " status=" + e.Status + " detail=" + e.Detail
}

func IsProvisioningPending(err error) (*ProvisioningPendingError, bool) {
	var pending *ProvisioningPendingError
	if err == nil {
		return nil, false
	}
	if errors.As(err, &pending) {
		return pending, true
	}
	return nil, false
}

// TerminalProvisioningError indicates a provider-reported terminal state.
// Callers should not continue dependent reconciliation until user action or
// an explicit self-heal policy is applied.
type TerminalProvisioningError struct {
	ResourceType string
	ResourceID   string
	Status       string
	Detail       string
}

func (e *TerminalProvisioningError) Error() string {
	return "resource entered terminal state: " + e.ResourceType + " " + e.ResourceID + " status=" + e.Status + " detail=" + e.Detail
}

func IsTerminalProvisioning(err error) (*TerminalProvisioningError, bool) {
	var terminal *TerminalProvisioningError
	if err == nil {
		return nil, false
	}
	if errors.As(err, &terminal) {
		return terminal, true
	}
	return nil, false
}
