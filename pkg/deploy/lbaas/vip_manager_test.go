package lbaas

import (
	"context"
	"strings"
	"testing"
)

func TestVIPManagerVerifiesCurrentIDAndBackfillsAddress(t *testing.T) {
	stub := &stubClient{vips: []VIP{{ID: "vip-1", Address: "10.0.0.7"}}}
	id, address, changed, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "subnet-1", "vip-1", "", false)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if id != "vip-1" || address != "10.0.0.7" || !changed || stub.createdVIP != 0 {
		t.Fatalf("unexpected result id=%q address=%q changed=%t created=%d", id, address, changed, stub.createdVIP)
	}
}

func TestVIPManagerRecreatesWhenCurrentVIPIsMissing(t *testing.T) {
	stub := &stubClient{vips: []VIP{{ID: "vip-other", Address: "10.0.0.8"}}}
	id, address, changed, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "subnet-1", "vip-1", "10.0.0.7", false)
	if err != nil || id != "7" || address != "10.0.0.7" || !changed || stub.createdVIP != 1 {
		t.Fatalf("expected VIP recreation, id=%q address=%q changed=%t created=%d err=%v", id, address, changed, stub.createdVIP, err)
	}
}

func TestVIPManagerRecreatesDeletedVIPWhenNoOtherVIPExists(t *testing.T) {
	stub := &stubClient{}
	id, address, changed, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "subnet-1", "vip-old", "10.0.0.7", false)
	if err != nil || id != "7" || address != "10.0.0.7" || !changed || stub.createdVIP != 1 {
		t.Fatalf("expected deleted VIP recreation, id=%q address=%q changed=%t created=%d err=%v", id, address, changed, stub.createdVIP, err)
	}
}

func TestVIPManagerCreatesNewVIPEvenWhenOthersExist(t *testing.T) {
	stub := &stubClient{vips: []VIP{{ID: "vip-1", Address: "10.0.0.7"}, {ID: "vip-2", Address: "10.0.0.8"}}}
	id, _, changed, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "subnet-1", "", "", false)
	if err != nil || id != "7" || !changed || stub.createdVIP != 1 {
		t.Fatalf("expected fresh VIP allocation, id=%q changed=%t created=%d err=%v", id, changed, stub.createdVIP, err)
	}
}

func TestVIPManagerCreatesWhenNoObservedVIPExists(t *testing.T) {
	stub := &stubClient{}
	id, address, changed, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "subnet-1", "", "", false)
	if err != nil {
		t.Fatalf("Ensure failed: %v", err)
	}
	if id != "7" || address != "10.0.0.7" || !changed || stub.createdVIP != 1 {
		t.Fatalf("unexpected create result id=%q address=%q changed=%t created=%d", id, address, changed, stub.createdVIP)
	}
}

func TestVIPManagerRejectsMissingSuppliedVIPWhenStrict(t *testing.T) {
	stub := &stubClient{vips: []VIP{{ID: "vip-other", Address: "10.0.0.7"}}}
	_, _, _, err := NewVIPManager(stub).Ensure(context.Background(), "lb-1", "subnet-1", "vip-stale", "10.0.0.7", true)
	if err == nil || !strings.Contains(err.Error(), "supplied VIP") {
		t.Fatalf("expected strict supplied VIP error, got %v", err)
	}
}
