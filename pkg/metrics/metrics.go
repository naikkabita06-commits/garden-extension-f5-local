package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// CMPAPICallsTotal counts every outbound CMP LBaaS API call.
	// Labels: controller (which binary), operation (e.g. "CreateVirtualServer"), result ("success"|"error").
	CMPAPICallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "f5_cmp_api_calls_total",
			Help: "Total number of CMP LBaaS API calls made by the F5 extension controllers.",
		},
		[]string{"controller", "operation", "result"},
	)

	// CMPAPICallDuration observes the latency of each CMP LBaaS API call.
	// Labels: controller, operation.
	CMPAPICallDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "f5_cmp_api_call_duration_seconds",
			Help:    "Duration in seconds of CMP LBaaS API calls made by the F5 extension controllers.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"controller", "operation"},
	)

	// VIPAllocationsTotal counts VIP allocation attempts.
	// Labels: controller, result ("success"|"error").
	VIPAllocationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "f5_vip_allocations_total",
			Help: "Total number of CMP VIP allocation attempts by the F5 extension controllers.",
		},
		[]string{"controller", "result"},
	)

	// ReconcileErrorsTotal counts reconcile loop errors by controller.
	// Labels: controller.
	ReconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "f5_reconcile_errors_total",
			Help: "Total number of reconcile errors encountered by the F5 extension controllers.",
		},
		[]string{"controller"},
	)

	// ManagedServicesTotal tracks the current number of Services managed by a controller (gauge).
	// Labels: controller.
	ManagedServicesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "f5_managed_services_total",
			Help: "Number of LoadBalancer Services currently managed by the F5 extension controllers.",
		},
		[]string{"controller"},
	)
)

func init() {
	ctrlmetrics.Registry.MustRegister(
		CMPAPICallsTotal,
		CMPAPICallDuration,
		VIPAllocationsTotal,
		ReconcileErrorsTotal,
		ManagedServicesTotal,
	)
}
