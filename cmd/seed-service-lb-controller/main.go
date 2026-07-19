// SPDX-FileCopyrightText: 2025 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gardener/gardener/pkg/logger"

	lbannotations "github.com/gardener/gardener-extension-f5/pkg/annotations"
	lbbackend "github.com/gardener/gardener-extension-f5/pkg/backend"
	lbaasdeploy "github.com/gardener/gardener-extension-f5/pkg/deploy/lbaas"
	f5client "github.com/gardener/gardener-extension-f5/pkg/f5"
	lbfinalizers "github.com/gardener/gardener-extension-f5/pkg/finalizers"
	f5metrics "github.com/gardener/gardener-extension-f5/pkg/metrics"
	"github.com/gardener/gardener-extension-f5/pkg/model"
	lbservice "github.com/gardener/gardener-extension-f5/pkg/service"
	lbstatus "github.com/gardener/gardener-extension-f5/pkg/status"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	annObservedGraph   = "f5.extensions.gardener.cloud/observed-graph"

	// User-facing input annotations for per-Service LB configuration.
	annProtocol         = lbannotations.Protocol
	annRoutingAlgorithm = lbannotations.RoutingAlgorithm
	annHealthInterval   = lbannotations.HealthInterval
	annHealthType       = lbannotations.HealthType
	annHealthPath       = lbannotations.HealthPath
	annSourceRanges     = lbannotations.SourceRanges
	annDrainingTimeout  = lbannotations.DrainingTimeout
)

type lbServiceConfig = lbannotations.LBConfig

func parseLBServiceConfig(svc *corev1.Service) lbServiceConfig {
	return lbannotations.ParseService(svc)
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

type cmpLBaaS = lbaasdeploy.RawClient

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

			if _, err := lbfinalizers.Remove(ctx, r.Client, svc, finalizerName); err != nil {
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

	// Build a typed desired-state snapshot before mutating CMP resources.
	lbCfg := parseLBServiceConfig(svc)
	nodeIPs, err := lbbackend.ListNodeInternalIPs(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(nodeIPs) == 0 {
		log.Info("skipping: no nodes found")
		return ctrl.Result{}, nil
	}
	nodes := make([]lbbackend.Node, 0, len(nodeIPs))
	for _, ip := range nodeIPs {
		nodes = append(nodes, lbbackend.Node{IP: ip, Weight: 50})
	}
	stack, err := lbservice.BuildLoadBalancerStack(svc, lbCfg, nodes)
	if err != nil {
		log.Info("skipping: cannot build desired load-balancer stack", "reason", err.Error())
		return ctrl.Result{}, nil
	}
	// Use first port for the current seed LB flow.
	frontendPort := stack.Ports[0].FrontendPort
	nodePort := stack.Ports[0].NodePort

	// Ensure finalizer so we can cleanup CMP resources.
	if !controllerutil.ContainsFinalizer(svc, finalizerName) {
		if _, err := lbfinalizers.Ensure(ctx, r.Client, svc, finalizerName); err != nil {
			return ctrl.Result{}, err
		}
		f5metrics.ManagedServicesTotal.WithLabelValues("seed-service-lb").Inc()
	}

	// Provision via CMP LBaaS: LBService → VIP → VirtualServer
	protocol := stack.Ports[0].Protocol
	cmpStart := time.Now()
	ids, vip, graph, err := r.ensureCMPResources(ctx, svc, frontendPort, nodePort, nodeIPs, protocol, stack.Config)
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
	if vip != "" {
		svc.Annotations[annVIPAddress] = vip
	}
	if raw, err := json.Marshal(graph); err != nil {
		return ctrl.Result{}, err
	} else {
		svc.Annotations[annObservedGraph] = string(raw)
	}
	if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}

	// Set VIP in the Service status from the VIP annotation (if available).
	if vip == "" {
		vip = "pending"
	}
	log.Info("CMP LBaaS resources provisioned", "lbServiceID", ids.LBServiceID, "vipPortID", ids.VIPPortID, "vsID", ids.VirtualServerID, "vip", vip)
	r.Recorder.Eventf(svc, corev1.EventTypeNormal, "EnsuredLoadBalancer", "CMP LBaaS resources provisioned (LB=%s, VS=%s, VIP=%s, backends=%d)", ids.LBServiceID, ids.VirtualServerID, vip, len(nodeIPs))

	if vip != "" && vip != "pending" {
		if err := lbstatus.EnsureServiceVIP(ctx, r.Client, svc, vip); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *serviceReconciler) ensureCMPResources(ctx context.Context, svc *corev1.Service, frontendPort, nodePort int32, nodeIPs []string, protocol string, cfg lbServiceConfig) (*f5client.CMPResourceIDs, string, model.ObservedGraph, error) {
	current := model.ObservedState{}
	if svc != nil && svc.Annotations != nil {
		current.LBServiceID = strings.TrimSpace(svc.Annotations[annLBServiceID])
		current.VIPPortID = strings.TrimSpace(svc.Annotations[annVIPPortID])
		current.VirtualServerID = strings.TrimSpace(svc.Annotations[annVirtualServerID])
		current.VIPAddress = strings.TrimSpace(svc.Annotations[annVIPAddress])
		if raw := strings.TrimSpace(svc.Annotations[annObservedGraph]); raw != "" {
			_ = json.Unmarshal([]byte(raw), &current.Graph)
		}
	}

	members := make([]model.BackendMember, 0, len(nodeIPs))
	for _, ip := range nodeIPs {
		members = append(members, model.BackendMember{IP: ip, Port: nodePort, Weight: 50})
	}

	vs := model.VirtualServer{
		Name:             sanitizeKey(fmt.Sprintf("seed-vs-%s-%s-%d", svc.Namespace, svc.Name, frontendPort)),
		FrontendPort:     frontendPort,
		BackendNodePort:  nodePort,
		Protocol:         protocol,
		RoutingAlgorithm: cfg.RoutingAlgorithm,
		PersistenceType:  cfg.PersistenceType,
		DrainingTimeout:  cfg.DrainingTimeout,
		SourceRanges:     append([]string(nil), cfg.SourceRanges...),
		Monitor:          &model.Monitor{Type: cfg.HealthType, Path: cfg.HealthPath, Interval: cfg.HealthInterval},
	}
	pool := model.Pool{Name: sanitizeKey(fmt.Sprintf("seed-pool-%s-%s-%d", svc.Namespace, svc.Name, frontendPort)), Members: members, Monitor: vs.Monitor}
	vs.DefaultPoolName = pool.Name
	desired := &model.LoadBalancerStack{LBService: model.LBService{Name: sanitizeKey(fmt.Sprintf("seed-%s-%s", svc.Namespace, svc.Name)), Description: fmt.Sprintf("Seed LB for %s/%s", svc.Namespace, svc.Name), FlavorID: r.flavorID, NetworkID: r.networkID, VPCID: r.vpcID, VPCName: r.vpcName}, VIP: model.VIP{Name: "seed-vip"}, VirtualServers: []model.VirtualServer{vs}, Pools: []model.Pool{pool}}
	result, err := lbaasdeploy.NewFromRaw(r.cmp, r.vpcID).EnsureStack(ctx, lbaasdeploy.StackEnsureRequest{Stack: desired, Current: current})
	if err != nil {
		return nil, current.VIPAddress, current.Graph, err
	}
	observedVS := result.Observed.Graph.VirtualServers[vs.Name]
	return &f5client.CMPResourceIDs{LBServiceID: result.Observed.LBServiceID, VIPPortID: result.Observed.VIPPortID, VirtualServerID: observedVS.ExternalID, VirtualServerName: observedVS.Name}, result.Observed.VIPAddress, result.Observed.Graph, nil

}

func (r *serviceReconciler) cleanupCMPResources(ctx context.Context, svc *corev1.Service) error {
	observed := model.ObservedState{
		LBServiceID:     strings.TrimSpace(svc.Annotations[annLBServiceID]),
		VIPPortID:       strings.TrimSpace(svc.Annotations[annVIPPortID]),
		VirtualServerID: strings.TrimSpace(svc.Annotations[annVirtualServerID]),
	}
	if raw := strings.TrimSpace(svc.Annotations[annObservedGraph]); raw != "" {
		_ = json.Unmarshal([]byte(raw), &observed.Graph)
	}
	observed.EnsureGraph()
	_, err := lbaasdeploy.NewFromRaw(r.cmp, r.vpcID).CleanupStack(ctx, lbaasdeploy.CleanupRequest{Current: observed, DeleteVIP: true, DeleteLBService: true})
	return err
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
