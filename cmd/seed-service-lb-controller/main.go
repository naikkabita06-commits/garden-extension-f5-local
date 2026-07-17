// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gardener/gardener/pkg/logger"

	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	f5metrics "github.com/gardener/gardener-extension-f5/pkg/metrics"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

	finalizerName = "f5.extensions.gardener.cloud/seed-service-lb"

	// Annotations stored on the Service to track CMP resource IDs.
	annLBServiceID     = "f5.extensions.gardener.cloud/lb-service-id"
	annVIPPortID       = "f5.extensions.gardener.cloud/vip-port-id"
	annVirtualServerID = "f5.extensions.gardener.cloud/virtual-server-id"
	annVIPAddress      = "f5.extensions.gardener.cloud/vip-address"

	// User-facing input annotations for per-Service LB configuration.
	annProtocol         = "f5.extensions.gardener.cloud/protocol"                    // TCP, UDP, HTTP, HTTPS
	annRoutingAlgorithm = "f5.extensions.gardener.cloud/routing-algorithm"           // round_robin, least_connections, etc.
	annHealthInterval   = "f5.extensions.gardener.cloud/health-check-interval"       // seconds (integer)
	annHealthType       = "f5.extensions.gardener.cloud/health-check-type"           // tcp (default), http
	annHealthPath       = "f5.extensions.gardener.cloud/health-check-path"           // HTTP health check path (e.g. /healthz)
	annSourceRanges     = "f5.extensions.gardener.cloud/source-ranges"               // comma-separated CIDRs (fallback for spec.loadBalancerSourceRanges)
	annDrainingTimeout  = "f5.extensions.gardener.cloud/connection-draining-timeout" // seconds (integer); 0 = disabled
)

// lbServiceConfig holds per-Service LB configuration parsed from annotations.
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

func defaultLBServiceConfig() lbServiceConfig {
	return lbServiceConfig{
		RoutingAlgorithm: "round_robin",
		HealthInterval:   30,
		HealthType:       "tcp",
	}
}

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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 s,
		LeaderElection:         true,
		LeaderElectionID:       "seed-service-lb-controller-leader",
		HealthProbeBindAddress: ":8082",
	})
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
	r.Recorder = mgr.GetEventRecorderFor("seed-service-lb-controller")

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
			// Any Seed node change (scale-up or scale-down) may affect pool members for
			// all managed LoadBalancer Services — re-enqueue all of them.
			return listManagedServiceRequests(ctx, mgr.GetClient())
		})).
		Complete(r); err != nil {
		return fmt.Errorf("creating controller: %w", err)
	}

	ctrl.Log.WithName("setup").Info(
		"starting seed service LB controller (CMP LBaaS)",
		"cmpEndpoint", r.cmpEndpoint,
		"loadBalancerClass", r.loadBalancerClass,
	)

	if err := mgr.Start(ctx); err != nil {
		return fmt.Errorf("starting manager: %w", err)
	}
	return nil
}

// --- Controller ---

type serviceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	cmp               cmpLBaaS
	cmpEndpoint       string
	loadBalancerClass string

	// CMP LBaaS provisioning parameters.
	flavorID  int32
	networkID string
	vpcID     string
	vpcName   string
}

// cmpLBaaS abstracts the CMP LBaaS operations for testing.
type cmpLBaaS interface {
	ListLBServices(ctx context.Context, opts *f5client.ListLoadBalancersOptions) ([]json.RawMessage, error)
	CreateLBService(ctx context.Context, form url.Values) (json.RawMessage, error)
	DeleteLBService(ctx context.Context, id string) error
	CreateLBServiceVIP(ctx context.Context, lbServiceID string) (json.RawMessage, error)
	GetLBServiceVIPs(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	DeleteLBServiceVIP(ctx context.Context, lbServiceID, vipID string) error
	ListLBVirtualServers(ctx context.Context, lbServiceID string) ([]json.RawMessage, error)
	CreateLBVirtualServer(ctx context.Context, lbServiceID string, query url.Values) (json.RawMessage, error)
	DeleteLBVirtualServer(ctx context.Context, lbServiceID, vsID string) error
}

type config struct {
	CMPEndpoint       string
	OrganisationName  string
	ProjectID         string
	CeAuth            string
	LoadBalancerClass string
	FlavorID          int32
	NetworkID         string
	VPCID             string
	VPCName           string
}

func loadConfigFromEnv() (*config, error) {
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

	lbClass := strings.TrimSpace(os.Getenv("F5_SEED_LB_LOADBALANCER_CLASS"))
	if lbClass == "" {
		lbClass = defaultLBClass
	}

	var flavorID int32
	if v := strings.TrimSpace(os.Getenv("CMP_FLAVOR_ID")); v != "" {
		var f int
		if _, err := fmt.Sscanf(v, "%d", &f); err == nil {
			flavorID = int32(f)
		}
	}

	return &config{
		CMPEndpoint:       endpoint,
		OrganisationName:  orgName,
		ProjectID:         projectID,
		CeAuth:            ceAuth,
		LoadBalancerClass: lbClass,
		FlavorID:          flavorID,
		NetworkID:         strings.TrimSpace(os.Getenv("CMP_NETWORK_ID")),
		VPCID:             strings.TrimSpace(os.Getenv("CMP_VPC_ID")),
		VPCName:           strings.TrimSpace(os.Getenv("CMP_VPC_NAME")),
	}, nil
}

func newReconciler(c client.Client, scheme *runtime.Scheme) (*serviceReconciler, error) {
	cfg, err := loadConfigFromEnv()
	if err != nil {
		return nil, err
	}

	log := ctrl.Log.WithName("seed-service-lb")
	cmpClient, err := f5client.NewClientWithCeAuth(log, cfg.CMPEndpoint, cfg.OrganisationName, cfg.ProjectID, cfg.CeAuth)
	if err != nil {
		return nil, fmt.Errorf("creating CMP client: %w", err)
	}

	return &serviceReconciler{
		Client:            c,
		Scheme:            scheme,
		cmp:               cmpClient,
		cmpEndpoint:       cfg.CMPEndpoint,
		loadBalancerClass: cfg.LoadBalancerClass,
		flavorID:          cfg.FlavorID,
		networkID:         cfg.NetworkID,
		vpcID:             cfg.VPCID,
		vpcName:           cfg.VPCName,
	}, nil
}

func (r *serviceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.Log.WithName("seed-service-lb").WithValues("service", req.NamespacedName.String())

	svc := &corev1.Service{}
	if err := r.Get(ctx, req.NamespacedName, svc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

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

			base := svc.DeepCopy()
			controllerutil.RemoveFinalizer(svc, finalizerName)
			if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
				return ctrl.Result{}, err
			}
			f5metrics.ManagedServicesTotal.WithLabelValues("seed-service-lb").Dec()
		}
		return ctrl.Result{}, nil
	}

	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		return ctrl.Result{}, nil
	}
	if strings.TrimSpace(r.loadBalancerClass) != "" {
		if svc.Spec.LoadBalancerClass == nil || strings.TrimSpace(*svc.Spec.LoadBalancerClass) != r.loadBalancerClass {
			return ctrl.Result{}, nil
		}
	}

	// Parse per-Service LB configuration from annotations (with defaults).
	lbCfg := parseLBServiceConfig(svc)

	ports, ok := choosePorts(svc, lbCfg.ProtocolOverride)
	if !ok {
		log.Info("skipping: no Service port/nodePort")
		return ctrl.Result{}, nil
	}
	// Use first port for LB Service naming and VIP; create a VS per port.
	frontendPort := ports[0].FrontendPort
	nodePort := ports[0].NodePort

	nodeIPs, err := listNodeInternalIPs(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(nodeIPs) == 0 {
		log.Info("skipping: no nodes found")
		return ctrl.Result{}, nil
	}

	// Ensure finalizer so we can cleanup CMP resources.
	if !controllerutil.ContainsFinalizer(svc, finalizerName) {
		base := svc.DeepCopy()
		controllerutil.AddFinalizer(svc, finalizerName)
		if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, err
		}
		f5metrics.ManagedServicesTotal.WithLabelValues("seed-service-lb").Inc()
	}

	// Provision via CMP LBaaS: LBService → VIP → VirtualServer
	protocol := ports[0].Protocol
	cmpStart := time.Now()
	ids, err := r.ensureCMPResources(ctx, svc, frontendPort, nodePort, nodeIPs, protocol, lbCfg)
	f5metrics.CMPAPICallDuration.WithLabelValues("seed-service-lb-controller", "EnsureLB").Observe(time.Since(cmpStart).Seconds())
	if err != nil {
		f5metrics.CMPAPICallsTotal.WithLabelValues("seed-service-lb-controller", "EnsureLB", "error").Inc()
		f5metrics.ReconcileErrorsTotal.WithLabelValues("seed-service-lb-controller").Inc()
		if rle, ok := f5client.IsRateLimited(err); ok {
			r.Recorder.Eventf(svc, corev1.EventTypeWarning, "RateLimited", "CMP API rate limited; retrying after %s", rle.RetryAfter)
			ctrl.Log.WithName("seed-service-lb").Info("CMP rate limited; requeuing after Retry-After", "retryAfter", rle.RetryAfter)
			return ctrl.Result{RequeueAfter: rle.RetryAfter}, nil
		}
		r.Recorder.Eventf(svc, corev1.EventTypeWarning, "SyncLoadBalancerFailed", "Error ensuring CMP LBaaS resources: %v", err)
		return ctrl.Result{}, err
	}
	f5metrics.CMPAPICallsTotal.WithLabelValues("seed-service-lb-controller", "EnsureLB", "success").Inc()

	// Store CMP resource IDs in annotations.
	base := svc.DeepCopy()
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[annLBServiceID] = ids.LBServiceID
	svc.Annotations[annVIPPortID] = ids.VIPPortID
	svc.Annotations[annVirtualServerID] = ids.VirtualServerID
	if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	// Set VIP in the Service status from the VIP annotation (if available).
	vip := svc.Annotations[annVIPAddress]
	if vip == "" {
		vip = "pending"
	}
	log.Info("CMP LBaaS resources provisioned", "lbServiceID", ids.LBServiceID, "vipPortID", ids.VIPPortID, "vsID", ids.VirtualServerID, "vip", vip)
	r.Recorder.Eventf(svc, corev1.EventTypeNormal, "EnsuredLoadBalancer", "CMP LBaaS resources provisioned (LB=%s, VS=%s, VIP=%s, backends=%d)", ids.LBServiceID, ids.VirtualServerID, vip, len(nodeIPs))

	if vip != "" && vip != "pending" {
		if err := ensureServiceStatusVIP(ctx, r.Client, svc, vip); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *serviceReconciler) ensureCMPResources(ctx context.Context, svc *corev1.Service, frontendPort, nodePort int32, nodeIPs []string, protocol string, cfg lbServiceConfig) (*f5client.CMPResourceIDs, error) {
	ids := &f5client.CMPResourceIDs{}

	// Step 1: Create or reuse LB Service.
	existingLBID := svc.Annotations[annLBServiceID]
	if existingLBID != "" {
		ids.LBServiceID = existingLBID
	} else {
		lbName := sanitizeKey(fmt.Sprintf("seed-%s-%s", svc.Namespace, svc.Name))
		form := url.Values{}
		form.Set("name", lbName)
		form.Set("description", fmt.Sprintf("Seed LB for %s/%s", svc.Namespace, svc.Name))
		if r.flavorID != 0 {
			form.Set("flavor_id", fmt.Sprintf("%d", r.flavorID))
		}
		if r.networkID != "" {
			form.Set("network_id", r.networkID)
		}
		if r.vpcID != "" {
			form.Set("vpc_id", r.vpcID)
		}
		if r.vpcName != "" {
			form.Set("vpc_name", r.vpcName)
		}

		raw, err := r.cmp.CreateLBService(ctx, form)
		if err != nil {
			return nil, fmt.Errorf("creating LB service via CMP: %w", err)
		}
		var created struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" {
			return nil, fmt.Errorf("parsing LB Service create response: %s", string(raw))
		}
		ids.LBServiceID = created.ID
	}

	// Step 2: Create or reuse VIP.
	existingVIPID := svc.Annotations[annVIPPortID]
	if existingVIPID != "" {
		ids.VIPPortID = existingVIPID
	} else {
		raw, err := r.cmp.CreateLBServiceVIP(ctx, ids.LBServiceID)
		if err != nil {
			return ids, fmt.Errorf("creating VIP via CMP on LB %s: %w", ids.LBServiceID, err)
		}
		var created struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(raw, &created); err == nil && created.ID != 0 {
			ids.VIPPortID = fmt.Sprintf("%d", created.ID)
		} else {
			var createdStr struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &createdStr) == nil && createdStr.ID != "" {
				ids.VIPPortID = createdStr.ID
			} else {
				return ids, fmt.Errorf("parsing VIP create response: %s", string(raw))
			}
		}
	}

	// Step 3: Create or reuse Virtual Server.
	existingVSID := svc.Annotations[annVirtualServerID]
	if existingVSID != "" {
		ids.VirtualServerID = existingVSID
	} else {
		nodes := make([]map[string]interface{}, 0, len(nodeIPs))
		for i, ip := range nodeIPs {
			nodes = append(nodes, map[string]interface{}{
				"compute_id":      fmt.Sprintf("node-%d", i),
				"compute_ip":      ip,
				"backend_port_id": i + 1,
				"port":            nodePort,
				"weight":          50,
			})
		}

		query := url.Values{}
		query.Set("name", sanitizeKey(fmt.Sprintf("seed-vs-%s-%s-%d", svc.Namespace, svc.Name, frontendPort)))
		query.Set("vip_port_id", ids.VIPPortID)
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

		raw, err := r.cmp.CreateLBVirtualServer(ctx, ids.LBServiceID, query)
		if err != nil {
			return ids, fmt.Errorf("creating virtual server via CMP: %w", err)
		}
		var created struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &created); err == nil && (created.ID != "" || created.Name != "") {
			if created.ID != "" {
				ids.VirtualServerID = created.ID
			} else {
				ids.VirtualServerID = created.Name
			}
		}
	}

	return ids, nil
}

func (r *serviceReconciler) cleanupCMPResources(ctx context.Context, svc *corev1.Service) error {
	lbID := svc.Annotations[annLBServiceID]
	vipID := svc.Annotations[annVIPPortID]
	vsID := svc.Annotations[annVirtualServerID]

	log := ctrl.Log.WithName("seed-service-lb").WithValues(
		"service", fmt.Sprintf("%s/%s", svc.Namespace, svc.Name),
		"lbServiceID", lbID, "vipPortID", vipID, "virtualServerID", vsID,
	)

	if vsID != "" && lbID != "" {
		log.Info("deleting CMP Virtual Server")
		if err := r.cmp.DeleteLBVirtualServer(ctx, lbID, vsID); err != nil && !f5client.IsNotFound(err) {
			return fmt.Errorf("deleting virtual server %s on LB %s: %w", vsID, lbID, err)
		}
	}
	if vipID != "" && lbID != "" {
		log.Info("deleting CMP VIP")
		if err := r.cmp.DeleteLBServiceVIP(ctx, lbID, vipID); err != nil && !f5client.IsNotFound(err) {
			return fmt.Errorf("deleting VIP %s on LB %s: %w", vipID, lbID, err)
		}
	}
	if lbID != "" {
		log.Info("deleting CMP LB Service")
		if err := r.cmp.DeleteLBService(ctx, lbID); err != nil && !f5client.IsNotFound(err) {
			return fmt.Errorf("deleting LB service %s: %w", lbID, err)
		}
	}
	return nil
}

// portInfo holds the details extracted from a single Service port.
type portInfo struct {
	FrontendPort int32
	NodePort     int32
	Protocol     string // TCP, UDP, HTTP, or HTTPS
}

// mapK8sProtocolToCMP maps a Kubernetes Service port protocol to the CMP protocol string.
// CMP supports: TCP, UDP, HTTP, HTTPS.
// For SCTP or unrecognised protocols, it defaults to TCP.
func mapK8sProtocolToCMP(p corev1.Protocol, port int32) string {
	switch p {
	case corev1.ProtocolUDP:
		return "UDP"
	case corev1.ProtocolTCP:
		// Detect HTTP/HTTPS by well-known ports; default to TCP.
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

func listNodeInternalIPs(ctx context.Context, c client.Client) ([]string, error) {
	nl := &corev1.NodeList{}
	if err := c.List(ctx, nl); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(nl.Items))
	for _, n := range nl.Items {
		for _, a := range n.Status.Addresses {
			if a.Type == corev1.NodeInternalIP && strings.TrimSpace(a.Address) != "" {
				out = append(out, strings.TrimSpace(a.Address))
				break
			}
		}
	}
	return out, nil
}

func ensureServiceStatusVIP(ctx context.Context, c client.Client, svc *corev1.Service, vip string) error {
	current := ""
	if len(svc.Status.LoadBalancer.Ingress) > 0 {
		current = strings.TrimSpace(svc.Status.LoadBalancer.Ingress[0].IP)
	}
	if current == vip {
		return nil
	}

	base := svc.DeepCopy()
	svc.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: vip}}
	return c.Status().Patch(ctx, svc, client.MergeFrom(base))
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

// silence unused import warning in case build tags change
var _ metav1.Time

// listManagedServiceRequests returns reconcile requests for all Seed Services that carry
// the controller's finalizer. Called on Node events to keep pool members in sync.
func listManagedServiceRequests(ctx context.Context, c client.Client) []reconcile.Request {
	svcs := &corev1.ServiceList{}
	if err := c.List(ctx, svcs); err != nil {
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
