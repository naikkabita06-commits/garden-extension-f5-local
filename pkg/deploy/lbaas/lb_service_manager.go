package lbaas

import (
	"context"
	"fmt"
	"strings"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
)

type LBServiceManager struct {
	client Client
	vpcID  string
}

func NewLBServiceManager(client Client, vpcID string) *LBServiceManager {
	return &LBServiceManager{client: client, vpcID: strings.TrimSpace(vpcID)}
}

func (m *LBServiceManager) Ensure(
	ctx context.Context,
	req EnsureRequest,
	currentID string,
	strictExistingID bool,
) (string, bool, error) {
	currentID = strings.TrimSpace(currentID)

	if currentID != "" {
		svc, err := m.client.GetLBService(ctx, currentID)

		switch {
		case err == nil:
			if placementErr := validateLBServicePlacement(svc, req); placementErr != nil {
				return "", false, placementErr
			}
			// The annotation/observed graph contains an ID and CMP confirms
			// that this exact LBService still exists.
			if readyErr := validateLBServiceReadiness(svc); readyErr != nil {
				if terminal, ok := IsTerminalProvisioning(readyErr); ok {
					if strictExistingID {
						return "", false, terminal
					}
					// Managed LB in terminal failed state: delete and recreate.
					if delErr := m.client.DeleteLBService(ctx, currentID); delErr != nil && !f5client.IsNotFound(delErr) {
						return "", false, fmt.Errorf("deleting failed LBService %q before recreate: %w", currentID, delErr)
					}
					return "", false, &ProvisioningPendingError{ResourceType: "LBService", ResourceID: currentID, Status: "deleting", Detail: "failed managed LBService deleted; waiting for CMP deletion before replacement"}
				}
				return "", false, readyErr
			}
			return currentID, false, nil

		case f5client.IsNotFound(err):
			if strictExistingID {
				return "", false, fmt.Errorf(
					"supplied LBService %q was not found in CMP",
					currentID,
				)
			}
			// CMP explicitly confirmed that the recorded resource disappeared.
			// It is safe to recreate it.
			currentID = ""

		default:
			// Do not create another LBService when verification failed because
			// of timeout, 401, 500, network failure, etc.
			return "", false, fmt.Errorf(
				"checking existing LBService %q via CMP: %w",
				currentID,
				err,
			)
		}
	}
	if recoveredID, lookupErr := m.findByName(ctx, req.LBName); lookupErr != nil {
		return "", false, fmt.Errorf("looking up deterministic LBService %q before create: %w", req.LBName, lookupErr)
	} else if recoveredID != "" {
		return m.ensureCreatedReady(ctx, recoveredID, req)
	}

	createdID, err := m.create(ctx, req)
	if err != nil {
		return "", false, err
	}

	return m.ensureCreatedReady(ctx, createdID, req)
}

func (m *LBServiceManager) ensureCreatedReady(ctx context.Context, createdID string, req EnsureRequest) (string, bool, error) {

	created, err := m.client.GetLBService(ctx, createdID)
	if err != nil {
		if f5client.IsNotFound(err) {
			// CMP creation is asynchronous. Right after create/adopt, a read may
			// briefly return 404 until the control-plane index converges.
			return "", false, &ProvisioningPendingError{
				ResourceType: "LBService",
				ResourceID:   createdID,
				Status:       "creating",
				Detail:       "LBService accepted by CMP but not yet readable",
			}
		}
		return "", false, fmt.Errorf("checking LBService %q readiness via CMP: %w", createdID, err)
	}
	if err := validateLBServiceReadiness(created); err != nil {
		return "", false, err
	}
	if err := validateLBServicePlacement(created, req); err != nil {
		return "", false, err
	}

	return createdID, true, nil
}

func validateLBServiceReadiness(svc LBService) error {
	id := strings.TrimSpace(svc.ID)
	status := strings.ToLower(strings.TrimSpace(svc.Status))
	opStatus := strings.ToLower(strings.TrimSpace(svc.OperatingStatus))

	if id == "" {
		return fmt.Errorf("CMP returned LBService without an ID while validating readiness")
	}

	if isFailedLBStatus(status) || isFailedLBStatus(opStatus) {
		return &TerminalProvisioningError{
			ResourceType: "LBService",
			ResourceID:   id,
			Status:       strings.TrimSpace(svc.Status),
			Detail:       strings.TrimSpace(svc.OperatingStatus),
		}
	}

	if isReadyLBStatus(status) || isReadyLBStatus(opStatus) {
		return nil
	}

	return &ProvisioningPendingError{
		ResourceType: "LBService",
		ResourceID:   id,
		Status:       strings.TrimSpace(svc.Status),
		Detail:       strings.TrimSpace(svc.OperatingStatus),
	}
}

func validateLBServicePlacement(svc LBService, req EnsureRequest) error {
	expectedVPC := strings.TrimSpace(req.VPCID)
	expectedNetwork := strings.TrimSpace(req.NetworkID)
	if expectedVPC != "" {
		if strings.TrimSpace(svc.VPCID) == "" {
			return fmt.Errorf("supplied LBService %q response has no VPC identity", svc.ID)
		}
		if strings.TrimSpace(svc.VPCID) != expectedVPC {
			return fmt.Errorf("supplied LBService %q belongs to VPC %q, expected %q", svc.ID, svc.VPCID, expectedVPC)
		}
	}
	if expectedNetwork != "" {
		if strings.TrimSpace(svc.NetworkID) == "" {
			return fmt.Errorf("supplied LBService %q response has no subnet identity", svc.ID)
		}
		if strings.TrimSpace(svc.NetworkID) != expectedNetwork {
			return fmt.Errorf("supplied LBService %q belongs to subnet %q, expected %q", svc.ID, svc.NetworkID, expectedNetwork)
		}
	}
	expectedRegion := strings.TrimSpace(req.Region)
	if expectedRegion != "" {
		if strings.TrimSpace(svc.Region) == "" {
			return fmt.Errorf("supplied LBService %q response has no region", svc.ID)
		}
		if !strings.EqualFold(strings.TrimSpace(svc.Region), expectedRegion) {
			return fmt.Errorf("supplied LBService %q belongs to region %q, expected %q", svc.ID, svc.Region, expectedRegion)
		}
	}
	return nil
}

func isReadyLBStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "active", "ready", "available", "online", "success", "ok":
		return true
	default:
		return false
	}
}

func isFailedLBStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "error", "errored", "degraded", "unavailable":
		return true
	default:
		return false
	}
}

func findUniqueLBServiceByName(items []LBService, name string) (string, error) {
	name = strings.TrimSpace(name)

	var foundID string

	for _, svc := range items {
		svcName := strings.TrimSpace(svc.Name)
		svcID := strings.TrimSpace(svc.ID)

		if svcName != name || svcID == "" {
			continue
		}

		if foundID != "" {
			return "", fmt.Errorf(
				"multiple LB services named %q found; refusing ambiguous adoption",
				name,
			)
		}

		foundID = svcID
	}

	return foundID, nil
}

func (m *LBServiceManager) findByName(ctx context.Context, name string) (string, error) {
	name = strings.TrimSpace(name)

	limit := int32(10)
	offset := int32(0)

	items, err := m.client.ListLBServices(
		ctx,
		&f5client.ListLoadBalancersOptions{
			Limit:  &limit,
			Offset: &offset,
			Search: name,
			Field:  "created",
			Order:  "desc",
		},
	)
	if err != nil {
		return "", err
	}

	return findUniqueLBServiceByName(items, name)
}

func (m *LBServiceManager) create(
	ctx context.Context,
	req EnsureRequest,
) (string, error) {
	vpcID := strings.TrimSpace(req.VPCID)
	if vpcID == "" {
		vpcID = m.vpcID
	}

	lbName := strings.TrimSpace(req.LBName)

	created, err := m.client.CreateLBService(ctx, LBServiceSpec{
		Name:        lbName,
		Description: req.LBDescription,
		FlavorID:    req.FlavorID,
		NetworkID:   req.NetworkID,
		VPCID:       vpcID,
		VPCName:     req.VPCName,
	})
	if err == nil {
		createdID := strings.TrimSpace(created.ID)
		if createdID == "" {
			return "", fmt.Errorf(
				"CMP created LBService %q but returned an empty ID",
				lbName,
			)
		}

		return createdID, nil
	}

	if !f5client.IsAlreadyExists(err) {
		return "", fmt.Errorf("creating LB service via CMP: %w", err)
	}

	// CMP returned 409: find and reuse the already-existing LBService.
	id, lookupErr := m.findByName(ctx, lbName)
	if lookupErr != nil {
		return "", fmt.Errorf(
			"LBService %q returned 409 but lookup failed: %w",
			lbName,
			lookupErr,
		)
	}

	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf(
			"LBService %q returned 409 but was not present in ListLBServices response",
			lbName,
		)
	}

	return id, nil
}
