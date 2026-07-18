package lbaas

import (
	"context"
	"testing"

	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type stubMonitorClient struct {
	monitors []MonitorResource
	created  []MonitorSpec
	updated  []MonitorSpec
	deleted  string
}

func (s *stubMonitorClient) ListMonitors(context.Context, string, string, string) ([]MonitorResource, error) {
	return append([]MonitorResource(nil), s.monitors...), nil
}
func (s *stubMonitorClient) CreateMonitor(_ context.Context, _, _, _ string, spec MonitorSpec) (MonitorResource, error) {
	s.created = append(s.created, spec)
	return MonitorResource{ID: "mon-1", Name: spec.Name, Protocol: spec.Protocol, Path: spec.Path, Interval: spec.Interval}, nil
}
func (s *stubMonitorClient) UpdateMonitor(_ context.Context, _, _, _, id string, spec MonitorSpec) (MonitorResource, error) {
	s.updated = append(s.updated, spec)
	return MonitorResource{ID: id, Name: spec.Name, Protocol: spec.Protocol, Path: spec.Path, Interval: spec.Interval}, nil
}
func (s *stubMonitorClient) DeleteMonitor(_ context.Context, _, _, _, id string) error {
	s.deleted = id
	return nil
}

func TestMonitorManagerEnsureCreatesOrUpdates(t *testing.T) {
	client := &stubMonitorClient{}
	res, changed, err := NewMonitorManager(client).Ensure(context.Background(), "lb-1", "vs-1", "pool-1", &model.Monitor{Name: "web", Type: "http", Path: "/healthz", Interval: 15})
	if err != nil {
		t.Fatalf("Ensure create failed: %v", err)
	}
	if !changed || res.ID != "mon-1" || len(client.created) != 1 {
		t.Fatalf("expected create, changed=%t res=%#v created=%#v", changed, res, client.created)
	}

	client.monitors = []MonitorResource{{ID: "mon-1", Name: "web"}}
	_, changed, err = NewMonitorManager(client).Ensure(context.Background(), "lb-1", "vs-1", "pool-1", &model.Monitor{Name: "web", Type: "http", Path: "/ready", Interval: 30})
	if err != nil {
		t.Fatalf("Ensure update failed: %v", err)
	}
	if !changed || len(client.updated) != 1 || client.updated[0].Path != "/ready" {
		t.Fatalf("expected update, changed=%t updated=%#v", changed, client.updated)
	}
}

func TestMonitorManagerCleanupIgnoresMissingIDs(t *testing.T) {
	client := &stubMonitorClient{}
	if err := NewMonitorManager(client).Cleanup(context.Background(), "lb-1", "", "pool-1", "mon-1"); err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if client.deleted != "" {
		t.Fatalf("expected no delete, got %q", client.deleted)
	}
}
