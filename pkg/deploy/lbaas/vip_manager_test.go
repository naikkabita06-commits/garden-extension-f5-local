package lbaas

import (
	"context"
	"strings"
	"testing"
)

func TestVIPManagerVerifiesCurrentIDAndBackfillsAddress(t *testing.T) {
	stub := &stubClient{vips: []VIP{{ID: "vip-1", Address: "10.0.0.7"}}}
	id, address, changed, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "vip-1", "")
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if id != "vip-1" || address != "10.0.0.7" || !changed || stub.createdVIP != 0 {
		t.Fatalf("unexpected result id=%q address=%q changed=%t created=%d", id, address, changed, stub.createdVIP)
	}
}

func TestVIPManagerRejectsReplacementWhenStaleIdentityIsAmbiguous(t *testing.T) {
	stub := &stubClient{vips: []VIP{{ID: "vip-other", Address: "10.0.0.8"}}}
	_, _, _, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "vip-1", "10.0.0.7")
	if err == nil || !strings.Contains(err.Error(), "cannot adopt VIP") {
		t.Fatalf("expected ambiguous stale VIP error, got %v", err)
	}
}

func TestVIPManagerRecreatesDeletedVIPWhenNoOtherVIPExists(t *testing.T) {
	stub := &stubClient{}
	id, address, changed, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "vip-old", "10.0.0.7")
	if err != nil || id != "7" || address != "10.0.0.7" || !changed || stub.createdVIP != 1 {
		t.Fatalf("expected deleted VIP recreation, id=%q address=%q changed=%t created=%d err=%v", id, address, changed, stub.createdVIP, err)
	}
}

func TestVIPManagerRejectsAmbiguousExistingVIPsWithoutStableIdentity(t *testing.T) {
	stub := &stubClient{vips: []VIP{{ID: "vip-1", Address: "10.0.0.7"}, {ID: "vip-2", Address: "10.0.0.8"}}}
	_, _, _, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "", "")
	if err == nil || !strings.Contains(err.Error(), "cannot adopt VIP") {
		t.Fatalf("expected ambiguous VIP adoption error, got %v", err)
	}
}

func TestVIPManagerCreatesWhenNoObservedVIPExists(t *testing.T) {
	stub := &stubClient{}
	id, address, changed, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "", "")
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if id != "7" || address != "10.0.0.7" || !changed || stub.createdVIP != 1 {
		t.Fatalf("unexpected create result id=%q address=%q changed=%t created=%d", id, address, changed, stub.createdVIP)
	}
}
