// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gardener/gardener/pkg/logger"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	f5metrics "github.com/gardener/gardener-extension-f5/pkg/metrics"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	runtimelog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager/signals"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	defaultLBClass = "f5.extensions.gardener.cloud/bigip"

	finalizerName = "f5.extensions.gardener.cloud/svc-lb-bridge"

	// Annotations stored on the Service to track CMP resource IDs.
	annLBServiceID     = "f5.extensions.gardener.cloud/lb-service-id"
	annVIPPortID       = "f5.extensions.gardener.cloud/vip-port-id"
	annVirtualServerID = "f5.extensions.gardener.cloud/virtual-server-id"
	annVIPAddress      = "f5.extensions.gardener.cloud/vip-address"
	annBackendHash     = "f5.extensions.gardener.cloud/backend-hash"

	// User-facing input annotations for per-Service LB configuration.
	// These override global defaults from F5LoadBalancerConfig CRD.
	annProtocol         = "f5.extensions.gardener.cloud/protocol"                    // TCP, UDP, HTTP, HTTPS
	annRoutingAlgorithm = "f5.extensions.gardener.cloud/routing-algorithm"           // round_robin, least_connections, etc.
	annHealthInterval   = "f5.extensions.gardener.cloud/health-check-interval"       // seconds (integer)
	annHealthType       = "f5.extensions.gardener.cloud/health-check-type"           // tcp (default), http
	annHealthPath       = "f5.extensions.gardener.cloud/health-check-path"           // HTTP health check path (e.g. /healthz)
	annSourceRanges     = "f5.extensions.gardener.cloud/source-ranges"               // comma-separated CIDRs (fallback for spec.loadBalancerSourceRanges)
	annDrainingTimeout  = "f5.extensions.gardener.cloud/connection-draining-timeout" // seconds (integer); 0 = disabled
	annVIPGroup         = "f5.extensions.gardener.cloud/vip-group"                   // shared VIP group name; Services with the same group share one CMP LBService+VIP
)

// lbServiceConfig holds per-Service LB configuration parsed from annotations,
// with defaults applied. This is the annotation model that enables per-Service
// customization similar to GKE/EKS annotation-driven LB controllers.
type lbServiceConfig struct {
	RoutingAlgorithm string   // CMP routing algorithm (default: round_robin)
	HealthInterval   int32    // health check interval in seconds (default: 30)
	HealthType       string   // health check monitor type: tcp (default), http
	HealthPath       string   // HTTP health check path (e.g. /healthz); only used when HealthType=http
	ProtocolOverride string   // if set, overrides auto-detected protocol
	SourceRanges     []string // allowed source CIDRs (from spec.loadBalancerSourceRanges or annotation)
	PersistenceType  string   // session persistence type (source_addr for ClientIP affinity)
	DrainingTimeout  int32    // connection draining timeout in seconds; 0 = not set
}

// defaultLBServiceConfig returns the default config with standard values.
func defaultLBServiceConfig() lbServiceConfig {
	return lbServiceConfig{
		RoutingAlgorithm: "round_robin",
		HealthInterval:   30,
		HealthType:       "tcp",
	}
}

// parseLBServiceConfig reads user-facing annotations from a Service and returns
// a config with defaults applied for any unset values.
func parseLBServiceConfig(svc *corev1.Service) lbServiceConfig {
	cfg := defaultLBServiceConfig()
	if svc == nil {
		return cfg
	}

	// Session affinity: read from spec.sessionAffinity (Kubernetes-native).
	if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
		cfg.PersistenceType = "source_addr"
	}

	if svc.Annotations == nil {
		return cfg
	}

	if v := strings.TrimSpace(svc.Annotations[annProtocol]); v != "" {
		upper := strings.ToUpper(v)
		switch upper {
		case "TCP", "UDP", "HTTP", "HTTPS":
			cfg.ProtocolOverride = upper
		}
	}

	if v := strings.TrimSpace(svc.Annotations[annRoutingAlgorithm]); v != "" {
		cfg.RoutingAlgorithm = v
	}

	if v := strings.TrimSpace(svc.Annotations[annHealthInterval]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.HealthInterval = int32(n)
		}
	}

	// Health check type and path.
	if v := strings.TrimSpace(svc.Annotations[annHealthType]); v != "" {
		lower := strings.ToLower(v)
		switch lower {
		case "tcp", "http":
			cfg.HealthType = lower
		}
	}
	if v := strings.TrimSpace(svc.Annotations[annHealthPath]); v != "" {
		cfg.HealthPath = v
		if cfg.HealthType == "tcp" {
			cfg.HealthType = "http" // auto-switch to http when path is set
		}
	}

	// Connection draining timeout.
	if v := strings.TrimSpace(svc.Annotations[annDrainingTimeout]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			cfg.DrainingTimeout = int32(n)
		}
	}

	// Source IP filtering: prefer spec.loadBalancerSourceRanges, fall back to annotation.
	if len(svc.Spec.LoadBalancerSourceRanges) > 0 {
		cfg.SourceRanges = svc.Spec.LoadBalancerSourceRanges
	} else if v := strings.TrimSpace(svc.Annotations[annSourceRanges]); v != "" {
		for _, cidr := range strings.Split(v, ",") {
			if c := strings.TrimSpace(cidr); c != "" {
				cfg.SourceRanges = append(cfg.SourceRanges, c)
			}
		}
	}

	return cfg
}

func main() {
	runtimelog.SetLogger(logger.MustNewZapLogger(logger.InfoLevel, logger.FormatJSON))

	ctx := signals.SetupSignalHandler()
	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return fmt.Errorf("adding core scheme: %w", err)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{Scheme: s})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding readyz check: %w", err)
	}

	r, err := newReconciler(mgr.GetClient(), mgr.GetScheme())
	if err != nil {
		return err
	}
	r.Recorder = mgr.GetEventRecorderFor("svc-lb-bridge")

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
			// Any node set change can affect which backends should be in each LB.
			// We enqueue all Services that appear to be managed by this controller.
			return listManagedServiceRequests(ctx, mgr.GetClient())
		})).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			// EndpointSlice changes (pod ready/unready) trigger re-sync of owning Service.
			svcName := obj.GetLabels()[discoveryv1.LabelServiceName]
			if svcName == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: client.ObjectKey{
				Namespace: obj.GetNamespace(),
				Name:      svcName,
			}}}
		})).
		Complete(r); err != nil {
		return fmt.Errorf("creating service controller: %w", err)
	}

	// Register the Ingress controller (like CIS, watches Ingress with class "f5").
	ingR := newIngressReconciler(mgr.GetClient(), mgr.GetScheme(), r.cmp, r.vpcID)
	ingR.Recorder = mgr.GetEventRecorderFor("ingress-lb")
	if err := ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1.Ingress{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
			return listManagedIngressRequests(ctx, mgr.GetClient())
		})).
		Complete(ingR); err != nil {
		return fmt.Errorf("creating ingress controller: %w", err)
	}

	ctrl.Log.WithName("setup").Info("starting svc-lb-bridge (CMP LBaaS)",
		"cmpEndpoint", r.cmpEndpoint,
	)
	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("starting manager: %w", err)
	}
	return nil
}

// cmpLBaaS abstracts the CMP LBaaS operations for testing.
type cmpLBaaS interface {
	CreateLBService(ctx context.Context, form url.Values) (json.RawMessage, error)
	ListLBServices(ctx context.Context, opts *f5client.ListLoadBalancersOptions) ([]json.RawMessage, error)
	DeleteLBService(ctx context.Context, id string) error
	CreateLBServiceVIP(ctx context.Context, lbServiceID string) (json.RawMessage, error)
	GetLBServiceVIPs(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	DeleteLBServiceVIP(ctx context.Context, lbServiceID, vipID string) error
	CreateLBVirtualServer(ctx context.Context, lbServiceID string, query url.Values) (json.RawMessage, error)
	ListLBVirtualServers(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	DeleteLBVirtualServer(ctx context.Context, lbServiceID, vsID string) error
}

type serviceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	cmp               cmpLBaaS
	cmpEndpoint       string
	loadBalancerClass string
	vpcID             string
}

func newReconciler(c client.Client, scheme *runtime.Scheme) (*serviceReconciler, error) {
	endpoint := strings.TrimSpace(os.Getenv("CMP_ENDPOINT"))
	if endpoint == "" {
		return nil, fmt.Errorf("CMP_ENDPOINT must be set")
	}
	ceAuth := strings.TrimSpace(os.Getenv("CMP_CE_AUTH"))
	if ceAuth == "" {
		return nil, fmt.Errorf("CMP_CE_AUTH must be set")
	}
	orgName := strings.TrimSpace(os.Getenv("CMP_ORGANISATION_NAME"))
	if orgName == "" {
		return nil, fmt.Errorf("CMP_ORGANISATION_NAME must be set")
	}
	projectID := strings.TrimSpace(os.Getenv("CMP_PROJECT_ID"))
	if projectID == "" {
		return nil, fmt.Errorf("CMP_PROJECT_ID must be set")
	}

	log := ctrl.Log.WithName("svc-lb-bridge")
	cmpClient, err := f5client.NewClientWithCeAuth(log, endpoint, orgName, projectID, ceAuth)
	if err != nil {
		return nil, fmt.Errorf("creating CMP client: %w", err)
	}

	return &serviceReconciler{
		Client:            c,
		Scheme:            scheme,
		cmp:               cmpClient,
		cmpEndpoint:       endpoint,
		loadBalancerClass: envOrDefault("F5_SVC_LB_LOADBALANCER_CLASS", defaultLBClass),
		vpcID:             strings.TrimSpace(os.Getenv("CMP_VPC_ID")),
	}, nil
}

// setRecorder sets the event recorder after the manager is started.
func (r *serviceReconciler) setRecorder(rec record.EventRecorder) {
	r.Recorder = rec
}

func (r *serviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("svc-lb-bridge").WithValues("service", req.NamespacedName.String())

	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion.
	if !svc.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(svc, finalizerName) {
			log.Info("cleaning up CMP LBaaS resources")
			r.Recorder.Eventf(svc, corev1.EventTypeNormal, "DeletingLoadBalancer", "Deleting CMP LBaaS resources (LB=%s, VS=%s)", svc.Annotations[annLBServiceID], svc.Annotations[annVirtualServerID])
			if err := r.cleanupCMPResources(ctx, svc); err != nil {
				r.Recorder.Eventf(svc, corev1.EventTypeWarning, "DeleteFailed", "CMP LBaaS cleanup failed: %v", err)
				// CMP is transiently unreachable — keep the finalizer and requeue.
				return ctrl.Result{}, fmt.Errorf("CMP cleanup failed; retrying: %w", err)
			}
			r.Recorder.Event(svc, corev1.EventTypeNormal, "DeletedLoadBalancer", "CMP LBaaS resources deleted successfully")
			r.cleanupNetworkPolicy(ctx, svc)

			base := svc.DeepCopy()
			controllerutil.RemoveFinalizer(svc, finalizerName)
			if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			f5metrics.ManagedServicesTotal.WithLabelValues("svc-lb-bridge").Dec()
		}
		return ctrl.Result{}, nil
	}

	// If the Service is no longer eligible, ensure the managed Ingress is removed.
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		// If we previously provisioned external resources for this Service, clean them up.
		if controllerutil.ContainsFinalizer(svc, finalizerName) {
			log.Info("service is no longer type=LoadBalancer; cleaning up CMP resources")
			if err := r.cleanupCMPResources(ctx, svc); err != nil {
				return ctrl.Result{}, fmt.Errorf("CMP cleanup failed; retrying: %w", err)
			}
			r.cleanupNetworkPolicy(ctx, svc)

			base := svc.DeepCopy()
			controllerutil.RemoveFinalizer(svc, finalizerName)
			if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			f5metrics.ManagedServicesTotal.WithLabelValues("svc-lb-bridge").Dec()
		}
		return ctrl.Result{}, nil
	}

	// Only Services that explicitly opt in via spec.loadBalancerClass are reconciled.
	if strings.TrimSpace(r.loadBalancerClass) != "" {
		if svc.Spec.LoadBalancerClass == nil || strings.TrimSpace(*svc.Spec.LoadBalancerClass) != r.loadBalancerClass {
			// If the Service is not for us, but we previously provisioned it, clean up.
			if controllerutil.ContainsFinalizer(svc, finalizerName) {
				log.Info("loadBalancerClass mismatch; cleaning up CMP resources")
				if err := r.cleanupCMPResources(ctx, svc); err != nil {
					return ctrl.Result{}, fmt.Errorf("CMP cleanup failed; retrying: %w", err)
				}
				r.cleanupNetworkPolicy(ctx, svc)

				base := svc.DeepCopy()
				controllerutil.RemoveFinalizer(svc, finalizerName)
				if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
					return ctrl.Result{}, err
				}
				f5metrics.ManagedServicesTotal.WithLabelValues("svc-lb-bridge").Dec()
			}
			return ctrl.Result{}, nil
		}
	}

	// Parse per-Service LB configuration from annotations (with defaults).
	lbCfg := parseLBServiceConfig(svc)

	ports, ok := choosePorts(svc, lbCfg.ProtocolOverride)
	if !ok {
		log.Info("skipping: service has no usable ports (need port + nodePort)")
		return ctrl.Result{}, nil
	}

	backends, err := listBackendNodes(ctx, r.Client, svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(backends) == 0 {
		log.Info("skipping: no ready nodes with endpoints found")
		return ctrl.Result{}, nil
	}

	// Ensure finalizer so we can cleanup CMP resources.
	if !controllerutil.ContainsFinalizer(svc, finalizerName) {
		base := svc.DeepCopy()
		controllerutil.AddFinalizer(svc, finalizerName)
		if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		f5metrics.ManagedServicesTotal.WithLabelValues("svc-lb-bridge").Inc()
	}

	// Ensure CMP resources for each port. The LBService and VIP are shared across
	// all ports (idempotent find-or-create); each port gets its own VirtualServer.
	var lastIDs *f5client.CMPResourceIDs
	var vip, lastBackendHash string
	cmpStart := time.Now()
	for _, p := range ports {
		ids, portVIP, backendHash, err := r.ensureCMPResources(ctx, svc, p.FrontendPort, p.NodePort, backends, p.Protocol, lbCfg)
		if err != nil {
			f5metrics.CMPAPICallDuration.WithLabelValues("svc-lb-bridge", "EnsureLB").Observe(time.Since(cmpStart).Seconds())
			f5metrics.CMPAPICallsTotal.WithLabelValues("svc-lb-bridge", "EnsureLB", "error").Inc()
			f5metrics.ReconcileErrorsTotal.WithLabelValues("svc-lb-bridge").Inc()
			if rle, ok := f5client.IsRateLimited(err); ok {
				r.Recorder.Eventf(svc, corev1.EventTypeWarning, "RateLimited", "CMP API rate limited; retrying after %s", rle.RetryAfter)
				log.Info("CMP rate limited; requeuing after Retry-After", "retryAfter", rle.RetryAfter)
				return ctrl.Result{RequeueAfter: rle.RetryAfter}, nil
			}
			r.Recorder.Eventf(svc, corev1.EventTypeWarning, "SyncLoadBalancerFailed", "Error ensuring CMP LBaaS resources for port %d: %v", p.FrontendPort, err)
			return ctrl.Result{}, err
		}
		lastIDs = ids
		lastBackendHash = backendHash
		if portVIP != "" {
			vip = portVIP
		}
	}
	f5metrics.CMPAPICallDuration.WithLabelValues("svc-lb-bridge", "EnsureLB").Observe(time.Since(cmpStart).Seconds())
	f5metrics.CMPAPICallsTotal.WithLabelValues("svc-lb-bridge", "EnsureLB", "success").Inc()
	if vip != "" {
		f5metrics.VIPAllocationsTotal.WithLabelValues("svc-lb-bridge", "success").Inc()
		r.Recorder.Eventf(svc, corev1.EventTypeNormal, "AllocatedVIP", "VIP %s allocated via CMP LBaaS", vip)
	}

	base := svc.DeepCopy()
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[annLBServiceID] = lastIDs.LBServiceID
	svc.Annotations[annVIPPortID] = lastIDs.VIPPortID
	svc.Annotations[annVirtualServerID] = lastIDs.VirtualServerID
	if vip != "" {
		svc.Annotations[annVIPAddress] = vip
	}
	if lastBackendHash != "" {
		svc.Annotations[annBackendHash] = lastBackendHash
	}
	if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("CMP LBaaS resources ensured", "lbServiceID", lastIDs.LBServiceID, "vipPortID", lastIDs.VIPPortID, "vsID", lastIDs.VirtualServerID, "vip", vip, "ports", len(ports), "backends", len(backends))
	r.Recorder.Eventf(svc, corev1.EventTypeNormal, "EnsuredLoadBalancer", "CMP LBaaS resources ensured (LB=%s, VIP=%s, ports=%d, backends=%d)", lastIDs.LBServiceID, vip, len(ports), len(backends))

	// Auto-generate NetworkPolicy allowing ingress to backing pods.
	if err := r.ensureNetworkPolicy(ctx, svc); err != nil {
		log.Error(err, "failed to ensure NetworkPolicy for LoadBalancer Service")
	}

	if vip == "" {
		log.Info("VIP not yet allocated from CMP; requeue")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if err := r.ensureServiceStatusVIP(ctx, svc, vip); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *serviceReconciler) ensureCMPResources(ctx context.Context, svc *corev1.Service, frontendPort, nodePort int32, backends []backendNode, protocol string, cfg lbServiceConfig) (*f5client.CMPResourceIDs, string, string, error) {
	ids := &f5client.CMPResourceIDs{}
	if svc != nil && svc.Annotations != nil {
		ids.LBServiceID = strings.TrimSpace(svc.Annotations[annLBServiceID])
		ids.VIPPortID = strings.TrimSpace(svc.Annotations[annVIPPortID])
		ids.VirtualServerID = strings.TrimSpace(svc.Annotations[annVirtualServerID])
	}

	backendHash := desiredBackendHash(frontendPort, nodePort, backends)
	currentHash := ""
	if svc != nil && svc.Annotations != nil {
		currentHash = strings.TrimSpace(svc.Annotations[annBackendHash])
	}

	lbName := desiredLBServiceName(svc)
	vsName := desiredVirtualServerName(svc, frontendPort)

	// Step 1: Ensure LB Service.
	if ids.LBServiceID == "" {
		foundID, err := r.findLBServiceByName(ctx, lbName)
		if err != nil {
			return nil, "", "", err
		}
		ids.LBServiceID = foundID
	}
	if ids.LBServiceID == "" {
		form := url.Values{}
		form.Set("name", lbName)
		form.Set("description", fmt.Sprintf("App LB for %s/%s", svc.Namespace, svc.Name))
		if r.vpcID != "" {
			form.Set("vpc_id", r.vpcID)
		}
		raw, err := r.cmp.CreateLBService(ctx, form)
		if err != nil {
			return nil, "", "", fmt.Errorf("creating LB service via CMP: %w", err)
		}
		var lbCreated struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &lbCreated); err != nil || strings.TrimSpace(lbCreated.ID) == "" {
			return nil, "", "", fmt.Errorf("parsing LB Service response: %s", string(raw))
		}
		ids.LBServiceID = strings.TrimSpace(lbCreated.ID)
	}

	// Step 2: Ensure VIP.
	vip := ""
	if svc != nil && svc.Annotations != nil {
		vip = strings.TrimSpace(svc.Annotations[annVIPAddress])
	}
	if ids.VIPPortID == "" || vip == "" {
		vipID, vipAddr, err := r.findOrCreateVIP(ctx, ids.LBServiceID)
		if err != nil {
			return ids, vip, backendHash, err
		}
		if ids.VIPPortID == "" {
			ids.VIPPortID = vipID
		}
		if vip == "" {
			vip = vipAddr
		}
	}

	// Step 3: Ensure Virtual Server with desired backends.
	// We treat the backend hash as the source of truth for whether the VS needs to be recreated.
	if ids.VirtualServerID == "" {
		foundID, err := r.findVirtualServerByName(ctx, ids.LBServiceID, vsName)
		if err != nil {
			return ids, vip, backendHash, err
		}
		ids.VirtualServerID = foundID
	}

	if ids.VirtualServerID != "" && currentHash != "" && currentHash == backendHash {
		return ids, vip, backendHash, nil
	}

	// Recreate on mismatch or unknown hash.
	if ids.VirtualServerID != "" {
		if err := r.cmp.DeleteLBVirtualServer(ctx, ids.LBServiceID, ids.VirtualServerID); err != nil && !f5client.IsNotFound(err) {
			return ids, vip, backendHash, fmt.Errorf("deleting virtual server %s on LB %s: %w", ids.VirtualServerID, ids.LBServiceID, err)
		}
		ids.VirtualServerID = ""
	}

	vsID, err := r.createVirtualServer(ctx, ids.LBServiceID, ids.VIPPortID, vsName, frontendPort, nodePort, backends, protocol, cfg)
	if err != nil {
		return ids, vip, backendHash, err
	}
	ids.VirtualServerID = vsID
	return ids, vip, backendHash, nil
}

func desiredLBServiceName(svc *corev1.Service) string {
	if g := strings.TrimSpace(svc.Annotations[annVIPGroup]); g != "" {
		return sanitizeKey(fmt.Sprintf("app-group-%s-%s", svc.Namespace, g))
	}
	return sanitizeKey(fmt.Sprintf("app-%s-%s", svc.Namespace, svc.Name))
}

func desiredVirtualServerName(svc *corev1.Service, frontendPort int32) string {
	return sanitizeKey(fmt.Sprintf("app-vs-%s-%s-%d", svc.Namespace, svc.Name, frontendPort))
}

func desiredBackendHash(frontendPort, nodePort int32, backends []backendNode) string {
	// Sort by IP for deterministic hash.
	sorted := make([]backendNode, len(backends))
	copy(sorted, backends)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].IP < sorted[j].IP })
	b := strings.Builder{}
	b.WriteString(fmt.Sprintf("frontend=%d;nodeport=%d;", frontendPort, nodePort))
	for _, n := range sorted {
		b.WriteString(fmt.Sprintf("%s:%d;", n.IP, n.Weight))
	}
	h := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(h[:])
}

func (r *serviceReconciler) findLBServiceByName(ctx context.Context, name string) (string, error) {
	items, err := r.cmp.ListLBServices(ctx, nil)
	if err != nil {
		return "", err
	}
	for _, raw := range items {
		var svc struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &svc) != nil {
			continue
		}
		if strings.TrimSpace(svc.Name) == name && strings.TrimSpace(svc.ID) != "" {
			return strings.TrimSpace(svc.ID), nil
		}
	}
	return "", nil
}

func (r *serviceReconciler) findOrCreateVIP(ctx context.Context, lbServiceID string) (vipID string, vipAddress string, err error) {
	vips, err := r.cmp.GetLBServiceVIPs(ctx, lbServiceID)
	if err != nil {
		return "", "", err
	}
	for _, raw := range vips {
		// Some CMP responses encode id as int, some as string.
		var vip struct {
			ID      string `json:"id"`
			Address string `json:"ip_address"`
		}
		if json.Unmarshal(raw, &vip) == nil {
			if strings.TrimSpace(vip.ID) != "" {
				return strings.TrimSpace(vip.ID), strings.TrimSpace(vip.Address), nil
			}
		}
		var vipNumeric struct {
			ID      int    `json:"id"`
			Address string `json:"ip_address"`
		}
		if json.Unmarshal(raw, &vipNumeric) == nil && vipNumeric.ID != 0 {
			return fmt.Sprintf("%d", vipNumeric.ID), strings.TrimSpace(vipNumeric.Address), nil
		}
	}

	vipRaw, err := r.cmp.CreateLBServiceVIP(ctx, lbServiceID)
	if err != nil {
		return "", "", fmt.Errorf("creating VIP via CMP on LB %s: %w", lbServiceID, err)
	}
	var created struct {
		ID      int    `json:"id"`
		IDStr   string `json:"id_str"`
		Address string `json:"ip_address"`
	}
	if json.Unmarshal(vipRaw, &created) != nil {
		return "", "", fmt.Errorf("parsing VIP create response: %s", string(vipRaw))
	}
	if created.ID != 0 {
		return fmt.Sprintf("%d", created.ID), strings.TrimSpace(created.Address), nil
	}
	if strings.TrimSpace(created.IDStr) != "" {
		return strings.TrimSpace(created.IDStr), strings.TrimSpace(created.Address), nil
	}
	return "", "", fmt.Errorf("VIP created but no ID returned: %s", string(vipRaw))
}

func (r *serviceReconciler) findVirtualServerByName(ctx context.Context, lbServiceID, name string) (string, error) {
	list, err := r.cmp.ListLBVirtualServers(ctx, lbServiceID)
	if err != nil {
		return "", err
	}
	for _, raw := range list {
		var vs struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &vs) != nil {
			continue
		}
		if strings.TrimSpace(vs.Name) == name && strings.TrimSpace(vs.ID) != "" {
			return strings.TrimSpace(vs.ID), nil
		}
	}
	return "", nil
}

func (r *serviceReconciler) createVirtualServer(ctx context.Context, lbServiceID, vipPortID, name string, frontendPort, nodePort int32, backends []backendNode, protocol string, cfg lbServiceConfig) (string, error) {
	nodes := make([]map[string]interface{}, 0, len(backends))
	for _, bn := range backends {
		nodes = append(nodes, map[string]interface{}{
			"compute_id":      bn.IP,
			"compute_ip":      bn.IP,
			"backend_port_id": 1,
			"port":            nodePort,
			"weight":          bn.Weight,
		})
	}

	query := url.Values{}
	query.Set("name", name)
	query.Set("vip_port_id", vipPortID)
	query.Set("protocol", protocol)
	query.Set("port", fmt.Sprintf("%d", frontendPort))
	query.Set("routing_algorithm", cfg.RoutingAlgorithm)
	query.Set("interval", fmt.Sprintf("%d", cfg.HealthInterval))
	if cfg.HealthType != "" && cfg.HealthType != "tcp" {
		query.Set("monitor_type", cfg.HealthType)
	}
	if cfg.HealthPath != "" {
		query.Set("monitor_path", cfg.HealthPath)
	}
	if cfg.PersistenceType != "" {
		query.Set("persistence_type", cfg.PersistenceType)
	}
	if cfg.DrainingTimeout > 0 {
		query.Set("connection_draining_timeout", fmt.Sprintf("%d", cfg.DrainingTimeout))
	}
	if r.vpcID != "" {
		query.Set("vpc_id", r.vpcID)
	}
	if len(cfg.SourceRanges) > 0 {
		query.Set("allowed_cidrs", strings.Join(cfg.SourceRanges, ","))
	}
	for _, n := range nodes {
		nodeJSON, _ := json.Marshal(n)
		query.Add("nodes", string(nodeJSON))
	}

	vsRaw, err := r.cmp.CreateLBVirtualServer(ctx, lbServiceID, query)
	if err != nil {
		return "", fmt.Errorf("creating virtual server via CMP: %w", err)
	}
	var created struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if json.Unmarshal(vsRaw, &created) == nil {
		if strings.TrimSpace(created.ID) != "" {
			return strings.TrimSpace(created.ID), nil
		}
		if strings.TrimSpace(created.Name) != "" {
			return strings.TrimSpace(created.Name), nil
		}
	}
	return name, nil
}

func listManagedServiceRequests(ctx context.Context, c client.Client) []reconcile.Request {
	svcs := &corev1.ServiceList{}
	if err := c.List(ctx, svcs); err != nil {
		// If we can't list, don't enqueue anything.
		return nil
	}
	out := make([]reconcile.Request, 0, len(svcs.Items))
	for _, svc := range svcs.Items {
		if !controllerutil.ContainsFinalizer(&svc, finalizerName) {
			continue
		}
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&svc)})
	}
	return out
}

func (r *serviceReconciler) cleanupCMPResources(ctx context.Context, svc *corev1.Service) error {
	lbID := svc.Annotations[annLBServiceID]
	vipID := svc.Annotations[annVIPPortID]
	vsID := svc.Annotations[annVirtualServerID]

	// Best-effort lookup by deterministic name if IDs are missing.
	if strings.TrimSpace(lbID) == "" {
		if found, err := r.findLBServiceByName(ctx, desiredLBServiceName(svc)); err == nil {
			lbID = found
		}
	}

	log := ctrl.Log.WithName("svc-lb-bridge").WithValues(
		"service", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name),
		"lbServiceID", lbID, "vipPortID", vipID, "virtualServerID", vsID,
	)

	if lbID != "" {
		// Delete VS: use ID if present; otherwise delete any VS matching our naming convention.
		if vsID != "" {
			log.Info("deleting CMP Virtual Server")
			if err := r.cmp.DeleteLBVirtualServer(ctx, lbID, vsID); err != nil && !f5client.IsNotFound(err) {
				return fmt.Errorf("deleting virtual server %s on LB %s: %w", vsID, lbID, err)
			}
		} else {
			// Best-effort: list and delete by name prefix (no ID stored).
			if list, err := r.cmp.ListLBVirtualServers(ctx, lbID); err == nil {
				prefix := sanitizeKey(fmt.Sprintf("app-vs-%s-%s-", svc.Namespace, svc.Name))
				for _, raw := range list {
					var vs struct {
						ID   string `json:"id"`
						Name string `json:"name"`
					}
					if json.Unmarshal(raw, &vs) != nil {
						continue
					}
					if strings.HasPrefix(strings.TrimSpace(vs.Name), prefix) && strings.TrimSpace(vs.ID) != "" {
						_ = r.cmp.DeleteLBVirtualServer(ctx, lbID, strings.TrimSpace(vs.ID))
					}
				}
			}
		}

		// If this LBService is shared (vip-group), only delete VIP+LBService when no other
		// Services still reference the same LBService ID. This prevents destroying resources
		// that are still in use by other group members.
		shared := r.isLBServiceShared(ctx, svc, lbID)
		if shared {
			log.Info("LBService is shared with other Services; skipping VIP and LBService deletion")
		}

		if !shared {
			// Delete VIP: use ID if present; otherwise delete all VIPs on this LB (dedicated LB per Service).
			if vipID != "" {
				log.Info("deleting CMP VIP")
				if err := r.cmp.DeleteLBServiceVIP(ctx, lbID, vipID); err != nil && !f5client.IsNotFound(err) {
					return fmt.Errorf("deleting VIP %s on LB %s: %w", vipID, lbID, err)
				}
			} else {
				// Best-effort: delete all VIPs found on the LB.
				if vips, err := r.cmp.GetLBServiceVIPs(ctx, lbID); err == nil {
					for _, raw := range vips {
						var vip struct {
							ID string `json:"id"`
						}
						if json.Unmarshal(raw, &vip) == nil && strings.TrimSpace(vip.ID) != "" {
							_ = r.cmp.DeleteLBServiceVIP(ctx, lbID, strings.TrimSpace(vip.ID))
							continue
						}
						var vipN struct {
							ID int `json:"id"`
						}
						if json.Unmarshal(raw, &vipN) == nil && vipN.ID != 0 {
							_ = r.cmp.DeleteLBServiceVIP(ctx, lbID, fmt.Sprintf("%d", vipN.ID))
						}
					}
				}
			}
		}
		if !shared && lbID != "" {
			log.Info("deleting CMP LB Service")
			if err := r.cmp.DeleteLBService(ctx, lbID); err != nil && !f5client.IsNotFound(err) {
				return fmt.Errorf("deleting LB service %s: %w", lbID, err)
			}
		}
	}

	return nil
}

// isLBServiceShared checks whether any other Service with a finalizer still references
// the same CMP LBService ID. Used for vip-group ref-counting.
func (r *serviceReconciler) isLBServiceShared(ctx context.Context, self *corev1.Service, lbID string) bool {
	if strings.TrimSpace(lbID) == "" {
		return false
	}
	svcs := &corev1.ServiceList{}
	if err := r.List(ctx, svcs, client.InNamespace(self.Namespace)); err != nil {
		return false
	}
	for _, s := range svcs.Items {
		if s.Name == self.Name && s.Namespace == self.Namespace {
			continue
		}
		if s.Annotations[annLBServiceID] == lbID && controllerutil.ContainsFinalizer(&s, finalizerName) {
			return true
		}
	}
	return false
}

// networkPolicyName returns the deterministic name for the auto-generated NetworkPolicy.
func networkPolicyName(svc *corev1.Service) string {
	return fmt.Sprintf("f5-lb-allow-%s", svc.Name)
}

// ensureNetworkPolicy creates or updates a NetworkPolicy that allows ingress traffic
// to the pods backing this LoadBalancer Service on the Service's target ports.
// This mirrors GKE/EKS behavior of auto-creating firewall rules for LB Services.
func (r *serviceReconciler) ensureNetworkPolicy(ctx context.Context, svc *corev1.Service) error {
	if svc.Spec.Selector == nil || len(svc.Spec.Selector) == 0 {
		return nil // No pod selector, can't create policy
	}

	// Build ingress rules for each Service port.
	var ingressPorts []networkingv1.NetworkPolicyPort
	for _, p := range svc.Spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		port := intstr.FromInt32(p.Port)
		ingressPorts = append(ingressPorts, networkingv1.NetworkPolicyPort{
			Protocol: &proto,
			Port:     &port,
		})
	}

	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkPolicyName(svc),
			Namespace: svc.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, np, func() error {
		np.Labels = map[string]string{
			"app.kubernetes.io/managed-by": "svc-lb-bridge",
			"f5.extensions.gardener.cloud": "network-policy",
		}
		np.Spec = networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: svc.Spec.Selector,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{Ports: ingressPorts},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		}
		return nil
	})
	return err
}

// cleanupNetworkPolicy removes the auto-generated NetworkPolicy for this Service.
func (r *serviceReconciler) cleanupNetworkPolicy(ctx context.Context, svc *corev1.Service) {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      networkPolicyName(svc),
			Namespace: svc.Namespace,
		},
	}
	if err := r.Delete(ctx, np); err != nil && !apierrors.IsNotFound(err) {
		ctrl.Log.WithName("svc-lb-bridge").Error(err, "failed to delete NetworkPolicy", "networkPolicy", np.Name, "namespace", np.Namespace)
	}
}

// portInfo holds the details extracted from a single Service port.
type portInfo struct {
	FrontendPort int32
	NodePort     int32
	Protocol     string // TCP, UDP, HTTP, or HTTPS
}

// mapK8sProtocolToCMP maps a Kubernetes Service port protocol to the CMP protocol string.
// CMP supports: TCP, UDP, HTTP, HTTPS.
func mapK8sProtocolToCMP(p corev1.Protocol, port int32) string {
	switch p {
	case corev1.ProtocolUDP:
		return "UDP"
	case corev1.ProtocolTCP:
		switch port {
		case 80, 8080:
			return "HTTP"
		case 443, 8443:
			return "HTTPS"
		default:
			return "TCP"
		}
	default:
		return "TCP"
	}
}

func choosePorts(svc *corev1.Service, protocolOverride string) ([]portInfo, bool) {
	if svc == nil || len(svc.Spec.Ports) == 0 {
		return nil, false
	}
	var ports []portInfo
	for _, p := range svc.Spec.Ports {
		if p.Port == 0 || p.NodePort == 0 {
			continue
		}
		proto := mapK8sProtocolToCMP(p.Protocol, p.Port)
		if protocolOverride != "" {
			proto = protocolOverride
		}
		ports = append(ports, portInfo{
			FrontendPort: p.Port,
			NodePort:     p.NodePort,
			Protocol:     proto,
		})
	}
	if len(ports) == 0 {
		return nil, false
	}
	return ports, true
}

func (r *serviceReconciler) ensureServiceStatusVIP(ctx context.Context, svc *corev1.Service, vip string) error {
	currentIP := ""
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		currentIP = strings.TrimSpace(svc.Status.LoadBalancer.Ingress[0].IP)
	}
	if currentIP == vip {
		return nil
	}

	base := svc.DeepCopy()
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: vip}}
	return r.Status().Patch(ctx, svc, client.MergeFrom(base))
}

// listBackendNodeIPs returns the InternalIPs of nodes that:
// 1. Are in Ready condition
// 2. Have ready endpoints for the given Service (via EndpointSlice)
// Falls back to all Ready nodes if no EndpointSlice is found.
// backendNode represents a node to include in the CMP pool with a proportional weight.
type backendNode struct {
	IP     string
	Weight int
}

func listBackendNodes(ctx context.Context, c client.Client, svc *corev1.Service) ([]backendNode, error) {
	// Get nodes that have ready endpoints via EndpointSlice (node name → endpoint count).
	targetNodes := getNodesWithReadyEndpoints(ctx, c, svc)

	nl := &corev1.NodeList{}
	if err := c.List(ctx, nl); err != nil {
		return nil, err
	}

	out := make([]backendNode, 0, len(nl.Items))
	for _, n := range nl.Items {
		if !isNodeReady(&n) {
			continue
		}
		// If we found EndpointSlice data, only include nodes with ready endpoints.
		epCount := 0
		if len(targetNodes) > 0 {
			count, ok := targetNodes[n.Name]
			if !ok {
				continue
			}
			epCount = count
		}
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP && strings.TrimSpace(a.Address) != "" {
				weight := 50
				if epCount > 0 {
					weight = epCount * 50 // proportional: 1 pod=50, 3 pods=150
				}
				out = append(out, backendNode{IP: strings.TrimSpace(a.Address), Weight: weight})
				break
			}
		}
	}
	return out, nil
}

// getNodesWithReadyEndpoints finds EndpointSlices for the Service and returns
// a map of node names to the count of ready endpoints on that node.
func getNodesWithReadyEndpoints(ctx context.Context, c client.Client, svc *corev1.Service) map[string]int {
	epsList := &discoveryv1.EndpointSliceList{}
	sel := labels.SelectorFromSet(labels.Set{
		discoveryv1.LabelServiceName: svc.Name,
	})
	if err := c.List(ctx, epsList, &client.ListOptions{
		Namespace:     svc.Namespace,
		LabelSelector: sel,
	}); err != nil {
		return nil
	}

	nodes := make(map[string]int)
	for i := range epsList.Items {
		for j := range epsList.Items[i].Endpoints {
			ep := &epsList.Items[i].Endpoints[j]
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if ep.NodeName != nil && *ep.NodeName != "" {
				nodes[*ep.NodeName]++
			}
		}
	}
	return nodes
}

// isNodeReady returns true if the node has condition Ready=True.
func isNodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func sanitizeKey(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "x"
	}
	b := strings.Builder{}
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

func envOrDefault(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}
