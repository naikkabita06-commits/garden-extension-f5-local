package lbaas

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
)

type rawAdapterStub struct {
	ports []json.RawMessage
}

func (s rawAdapterStub) ListLBServices(context.Context, *f5client.ListLoadBalancersOptions) ([]json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) CreateLBService(context.Context, url.Values) (json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) DeleteLBService(context.Context, string) error { return nil }
func (s rawAdapterStub) CreateLBServiceVIP(context.Context, string) (json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) GetLBServiceVIPs(context.Context, string) ([]json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) DeleteLBServiceVIP(context.Context, string, string) error { return nil }
func (s rawAdapterStub) ListLBVirtualServers(context.Context, string) ([]json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) CreateLBVirtualServer(context.Context, string, url.Values) (json.RawMessage, error) {
	return nil, nil
}
func (s rawAdapterStub) DeleteLBVirtualServer(context.Context, string, string) error { return nil }
func (s rawAdapterStub) SearchNetworkPortsByIP(context.Context, string) ([]json.RawMessage, error) {
	return append([]json.RawMessage(nil), s.ports...), nil
}

func TestRawAdapterSearchNetworkPortsByIPParsesCMPShapes(t *testing.T) {
	adapter := NewRawClient(rawAdapterStub{ports: []json.RawMessage{
		json.RawMessage(`{"id":5001,"resource_id":"compute-1","resource_type":"compute","fixed_ip":"10.0.0.1"}`),
		json.RawMessage(`{"id_str":"5002","device_id":"compute-2","device_owner":"compute:nova","fixed_ips":[{"ip_address":"10.0.0.2"}]}`),
	}})

	ports, err := adapter.SearchNetworkPortsByIP(context.Background(), "10.0.0.1")
	if err != nil {
		t.Fatalf("SearchNetworkPortsByIP failed: %v", err)
	}
	if len(ports) != 2 {
		t.Fatalf("expected two parsed ports, got %d", len(ports))
	}
	if ports[0].ID != 5001 || ports[0].ResourceID != "compute-1" || ports[0].ResourceType != "compute" || ports[0].IP != "10.0.0.1" {
		t.Fatalf("unexpected first port: %#v", ports[0])
	}
	if ports[1].ID != 5002 || ports[1].ResourceID != "compute-2" || ports[1].ResourceType != "compute" || ports[1].IP != "10.0.0.2" {
		t.Fatalf("unexpected second port: %#v", ports[1])
	}
}
