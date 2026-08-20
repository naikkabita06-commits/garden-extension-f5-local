package lbaas

import (
	"context"
	"testing"
)

func TestLBServiceManagerVerifiesCurrentID(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "app"}}}
	id, changed, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", false)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if id != "lb-1" || changed || stub.createdLB != 0 {
		t.Fatalf("unexpected result id=%q changed=%t created=%d", id, changed, stub.createdLB)
	}
}

func TestLBServiceManagerDoesNotAdoptSameNameResource(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-other", Name: "app"}}}
	id, changed, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", false)
	if err != nil || id != "lb-1" || !changed || stub.createdLB != 1 {
		t.Fatalf("expected owned replacement creation, id=%q changed=%t created=%d err=%v", id, changed, stub.createdLB, err)
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
	stub := &stubClient{lbServices: []LBService{{ID: "lb-other", Name: "app"}}}
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

func TestLBServiceManagerManagedFailedLBRecreates(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{
		ID:              "lb-1",
		Name:            "app",
		Status:          "Failed",
		OperatingStatus: "Error",
	}}}
	id, changed, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1", false)
	if err != nil {
		t.Fatalf("expected managed failed LB to self-heal, got error: %v", err)
	}
	if id != "lb-1" {
		t.Fatalf("expected recreated stub id lb-1, got %q", id)
	}
	if !changed {
		t.Fatal("expected changed=true after failed LB delete+recreate")
	}
	if stub.deletedLB != 1 {
		t.Fatalf("expected one failed LB deletion, got %d", stub.deletedLB)
	}
	if stub.createdLB != 1 {
		t.Fatalf("expected one LB recreation, got %d", stub.createdLB)
	}
}
