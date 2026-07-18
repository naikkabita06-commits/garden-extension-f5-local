package model

import "testing"

func TestObservedStateEnsureGraphMirrorsLegacyIDs(t *testing.T) {
	state := ObservedState{LBServiceID: "lb-1", VIPPortID: "vip-1", VIPAddress: "10.0.0.7", VirtualServerID: "vs-1", VirtualServerName: "vs"}
	state.EnsureGraph()
	if got := state.Graph.LBServices["legacy/lb-service"].ExternalID; got != "lb-1" {
		t.Fatalf("expected legacy LB ID mirrored, got %q", got)
	}
	if got := state.Graph.VIPs["legacy/vip"].Address; got != "10.0.0.7" {
		t.Fatalf("expected legacy VIP address mirrored, got %q", got)
	}
	if got := state.Graph.VirtualServers["legacy/virtual-server"].Name; got != "vs" {
		t.Fatalf("expected legacy VS name mirrored, got %q", got)
	}
}
