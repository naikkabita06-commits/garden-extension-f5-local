package lbaas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type Deployer struct {
	client Client
	vpcID  string

	lbServices     *LBServiceManager
	vips           *VIPManager
	virtualServers *VirtualServerManager
}

func New(client Client, vpcID string) *Deployer {
	vpcID = strings.TrimSpace(vpcID)
	return &Deployer{
		client:         client,
		vpcID:          vpcID,
		lbServices:     NewLBServiceManager(client, vpcID),
		vips:           NewVIPManager(client),
		virtualServers: NewVirtualServerManager(client, vpcID),
	}
}

type EnsureRequest struct {
	LBName        string
	LBDescription string
	FlavorID      int32
	NetworkID     string
	VPCID         string
	VPCName       string

	VirtualServer model.VirtualServer
	Backends      []model.BackendMember
	Current       model.ObservedState
	CurrentHash   string

	// RecreateWhenHashMissing enables controller-specific convergence for mutable
	// backend membership. Service LB reconciliation sets this when backend changes
	// must recreate a CMP virtual server. Seed and Ingress paths keep this false so
	// annotated existing virtual servers are reused, matching their historical
	// finalizer-safe behavior until a richer update API exists.
	RecreateWhenHashMissing bool
}

type EnsureResult struct {
	Observed    model.ObservedState
	BackendHash string
	Changed     bool
}

func (d *Deployer) Ensure(ctx context.Context, req EnsureRequest) (*EnsureResult, error) {
	result := &EnsureResult{Observed: req.Current}
	result.Observed.EnsureGraph()
	result.BackendHash = DesiredBackendHash(req.VirtualServer.FrontendPort, req.VirtualServer.BackendNodePort, req.Backends)

	lbID, changed, err := d.lbServices.Ensure(ctx, req, result.Observed.LBServiceID)
	if err != nil {
		return nil, err
	}
	result.Observed.LBServiceID = lbID
	result.Observed.Graph.LBServices[req.LBName] = model.ObservedResource{LogicalID: req.LBName, ExternalID: lbID, Name: req.LBName, Ownership: req.VirtualServer.Ownership}
	result.Changed = result.Changed || changed

	vipID, vipAddress, changed, err := d.vips.Ensure(ctx, result.Observed.LBServiceID, result.Observed.VIPPortID, result.Observed.VIPAddress)
	if err != nil {
		return result, err
	}
	result.Observed.VIPPortID = vipID
	result.Observed.VIPAddress = vipAddress
	result.Observed.Graph.VIPs["vip/"+vipID] = model.ObservedResource{LogicalID: "vip/" + vipID, ExternalID: vipID, Address: vipAddress, Ownership: req.VirtualServer.Ownership}
	result.Changed = result.Changed || changed

	vsID, vsName, changed, err := d.virtualServers.Ensure(ctx, VirtualServerEnsureRequest{
		LBServiceID:             result.Observed.LBServiceID,
		VIPPortID:               result.Observed.VIPPortID,
		Desired:                 req.VirtualServer,
		Backends:                req.Backends,
		CurrentID:               result.Observed.VirtualServerID,
		CurrentHash:             req.CurrentHash,
		DesiredHash:             result.BackendHash,
		RecreateWhenHashMissing: req.RecreateWhenHashMissing,
	})
	if err != nil {
		return result, err
	}
	result.Observed.VirtualServerID = vsID
	result.Observed.VirtualServerName = vsName
	result.Observed.Graph.VirtualServers[req.VirtualServer.Name] = model.ObservedResource{LogicalID: req.VirtualServer.Name, ExternalID: vsID, Name: vsName, Ownership: req.VirtualServer.Ownership}
	result.Changed = result.Changed || changed
	return result, nil
}

func DesiredBackendHash(frontendPort, nodePort int32, backends []model.BackendMember) string {
	sorted := make([]model.BackendMember, len(backends))
	copy(sorted, backends)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].IP < sorted[j].IP })
	b := strings.Builder{}
	b.WriteString(fmt.Sprintf("frontend=%d;nodeport=%d;", frontendPort, nodePort))
	for _, n := range sorted {
		b.WriteString(fmt.Sprintf("%s:%d;", n.IP, n.Weight))
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])
}
