package lbaas

import (
	"context"
	"strings"
	"testing"
)

func TestLBServiceManagerVerifiesCurrentID(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "app"}}}
	id, changed, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1")
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if id != "lb-1" || changed || stub.createdLB != 0 {
		t.Fatalf("unexpected result id=%q changed=%t created=%d", id, changed, stub.createdLB)
	}
}

func TestLBServiceManagerRediscoversMissingCurrentIDByDesiredName(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-other", Name: "app"}}}
	id, changed, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "lb-1")
	if err != nil || id != "lb-other" || changed {
		t.Fatalf("expected desired LB rediscovery, id=%q changed=%t err=%v", id, changed, err)
	}
}

func TestLBServiceManagerRejectsDuplicateNameAdoption(t *testing.T) {
	stub := &stubClient{lbServices: []LBService{{ID: "lb-1", Name: "app"}, {ID: "lb-2", Name: "app"}}}
	_, _, err := NewLBServiceManager(stub, "").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "")
	if err == nil || !strings.Contains(err.Error(), "multiple LB services") {
		t.Fatalf("expected duplicate name error, got %v", err)
	}
}

func TestLBServiceManagerCreatesWhenNoObservedMatchExists(t *testing.T) {
	stub := &stubClient{}
	id, changed, err := NewLBServiceManager(stub, "vpc-1").Ensure(context.Background(), EnsureRequest{LBName: "app"}, "")
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if id != "lb-1" || !changed || stub.createdLB != 1 {
		t.Fatalf("unexpected create result id=%q changed=%t created=%d", id, changed, stub.createdLB)
	}
}
