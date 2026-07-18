package lbaas

import (
	"context"
	"fmt"
	"strings"

	"github.com/gardener/gardener-extension-f5/pkg/model"
)

type MonitorClient interface {
	ListMonitors(ctx context.Context, lbServiceID, virtualServerID, poolID string) ([]MonitorResource, error)
	CreateMonitor(ctx context.Context, lbServiceID, virtualServerID, poolID string, spec MonitorSpec) (MonitorResource, error)
	UpdateMonitor(ctx context.Context, lbServiceID, virtualServerID, poolID, monitorID string, spec MonitorSpec) (MonitorResource, error)
	DeleteMonitor(ctx context.Context, lbServiceID, virtualServerID, poolID, monitorID string) error
}

type MonitorResource struct {
	ID       string
	Name     string
	Protocol string
	Path     string
	Interval int32
}

type MonitorSpec struct {
	Name     string
	Protocol string
	Path     string
	Interval int32
}

type MonitorManager struct{ client MonitorClient }

func NewMonitorManager(client MonitorClient) *MonitorManager { return &MonitorManager{client: client} }

func (m *MonitorManager) Ensure(ctx context.Context, lbServiceID, virtualServerID, poolID string, desired *model.Monitor) (MonitorResource, bool, error) {
	if desired == nil {
		return MonitorResource{}, false, nil
	}
	if strings.TrimSpace(lbServiceID) == "" || strings.TrimSpace(virtualServerID) == "" || strings.TrimSpace(poolID) == "" {
		return MonitorResource{}, false, fmt.Errorf("lb service id, virtual server id and pool id are required for monitor reconciliation")
	}
	spec := MonitorSpec{Name: desired.Name, Protocol: desired.Type, Path: desired.Path, Interval: desired.Interval}
	if strings.TrimSpace(spec.Name) == "" {
		spec.Name = "default-monitor"
	}
	monitors, err := m.client.ListMonitors(ctx, lbServiceID, virtualServerID, poolID)
	if err != nil {
		return MonitorResource{}, false, fmt.Errorf("listing monitors for pool %s: %w", poolID, err)
	}
	for _, monitor := range monitors {
		if strings.TrimSpace(monitor.Name) == strings.TrimSpace(spec.Name) && strings.TrimSpace(monitor.ID) != "" {
			updated, err := m.client.UpdateMonitor(ctx, lbServiceID, virtualServerID, poolID, monitor.ID, spec)
			if err != nil {
				return monitor, false, fmt.Errorf("updating monitor %s on pool %s: %w", monitor.ID, poolID, err)
			}
			return updated, true, nil
		}
	}
	created, err := m.client.CreateMonitor(ctx, lbServiceID, virtualServerID, poolID, spec)
	if err != nil {
		return MonitorResource{}, false, fmt.Errorf("creating monitor %s on pool %s: %w", spec.Name, poolID, err)
	}
	return created, true, nil
}

func (m *MonitorManager) Cleanup(ctx context.Context, lbServiceID, virtualServerID, poolID, monitorID string) error {
	if strings.TrimSpace(lbServiceID) == "" || strings.TrimSpace(virtualServerID) == "" || strings.TrimSpace(poolID) == "" || strings.TrimSpace(monitorID) == "" {
		return nil
	}
	return m.client.DeleteMonitor(ctx, lbServiceID, virtualServerID, poolID, monitorID)
}
