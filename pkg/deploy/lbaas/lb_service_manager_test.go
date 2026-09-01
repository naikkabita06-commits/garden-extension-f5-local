package lbaas

import (
	"context"
	"strings"
	"testing"
)

func TestLBServiceManagerVerifiesCurrentID(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "app", Status: "Active"}}}
	id, changed, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", false)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if id != "lb-1" || changed || stub.createdLB != 0 {
		t.Fatalf("unexpected result id=%q changed=%t created=%d", id, changed, stub.createdLB)
	}
}

func TestLBServiceManagerTreatsCreatedStatusAsReady(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "app", Status: "Created"}}}
	id, changed, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", false)
	if err != nil || id != "lb-1" || changed {
		t.Fatalf("expected Created LB to be ready, id=%q changed=%t err=%v", id, changed, err)
	}
}

func TestLBServiceManagerKeepsMissingReadinessStatusPending(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "app"}}}
	_, _, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", false)
	pending, ok := IsProvisioningPending(err)
	if !ok {
		t.Fatalf("expected missing readiness fields to remain pending, got %v", err)
	}
	if pending.Status != "unknown" || !strings.Contains(pending.Detail, "omitted status") {
		t.Fatalf("expected explicit missing-status diagnostic, got %#v", pending)
	}
}

func TestLBServiceManagerRecoversUniqueDeterministicNameAfterRecordedIDDisappears(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-other", Name: "app", Status: "Active"}}}
	id, changed, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", false)
	if err != nil || id != "lb-other" || !changed || stub.createdLB != 0 {
		t.Fatalf("expected deterministic recovery, id=%q changed=%t created=%d err=%v", id, changed, stub.createdLB, err)
	}
}

func TestLBServiceManagerCreatesWhenNoObservedMatchExists(t *testing.T) {
	stub := &stubClient{}
	id, changed, err := NewLBServiceManager(stub, "vpc-1").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "", false)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if id != "lb-1" || !changed || stub.createdLB != 1 {
		t.Fatalf("unexpected create result id=%q changed=%t created=%d", id, changed, stub.createdLB)
	}
}

func TestLBServiceManagerRejectsMissingSuppliedIDWhenStrict(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-other", Name: "app", Status: "Active"}}}
	_, _, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", true)
	if err == nil {
		t.Fatal("expected strict supplied LB ID validation to fail")
	}
}

func TestLBServiceManagerProvidedFailedLBDoesNotRecreate(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{
		ID:              "lb-1",
		Name:            "app",
		Status:          "Failed",
		OperatingStatus: "Error",
	}}}
	_, _, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", true)
	if err == nil {
		t.Fatal("expected terminal provisioning error for provided failed LB")
	}
	term, ok := IsTerminalProvisioning(err)
	if !ok {
		t.Fatalf("expected terminal provisioning error, got: %v", err)
	}
	if term.ResourceID != "lb-1" {
		t.Fatalf("expected terminal resource id lb-1, got %q", term.ResourceID)
	}
	if stub.createdLB != 0 {
		t.Fatalf("expected no LB recreation for provided failed LB, created=%d", stub.createdLB)
	}
	if stub.deletedLB != 0 {
		t.Fatalf("expected no LB deletion for provided failed LB, deleted=%d", stub.deletedLB)
	}
}

func TestLBServiceManagerManagedFailedLBWaitsForDeletion(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{
		ID:              "lb-1",
		Name:            "app",
		Status:          "Failed",
		OperatingStatus: "Error",
	}}}
	_, _, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", false)
	if _, ok := IsProvisioningPending(err); !ok {
		t.Fatalf("expected deletion pending state, got %v", err)
	}
	if stub.deletedLB != 1 {
		t.Fatalf("expected one failed LB deletion, got %d", stub.deletedLB)
	}
	if stub.createdLB != 0 {
		t.Fatalf("expected no replacement until deletion is observed, got %d", stub.createdLB)
	}
}

func TestLBServiceManagerValidatesProvidedPlacement(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "app", Status: "Created", OperatingStatus: "Active", VPCID: "vpc-1", NetworkID: "subnet-1", Region: "dev"}}}
	id, changed, err := NewLBServiceManager(stub, "vpc-1").Ensure(context.Background(), EnsureRequest{LBName: "app", VPCID: "vpc-1", NetworkID: "subnet-1", Region: "dev"}, "lb-1", true)
	if err != nil || id != "lb-1" || changed {
		t.Fatalf("expected valid provided LB placement, id=%q changed=%t err=%v", id, changed, err)
	}
}

func TestLBServiceManagerRejectsProvidedSubnetMismatch(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "app", Status: "Active", VPCID: "vpc-1", NetworkID: "other-subnet", Region: "dev"}}}
	_, _, err := NewLBServiceManager(stub, "vpc-1").Ensure(context.Background(), EnsureRequest{LBName: "app", VPCID: "vpc-1", NetworkID: "subnet-1", Region: "dev"}, "lb-1", true)
	if err == nil {
		t.Fatal("expected provided LB subnet mismatch")
	}
}
