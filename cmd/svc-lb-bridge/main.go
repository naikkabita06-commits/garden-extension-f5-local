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
	lbnetworkpolicy "github.com/gardener/gardener-extension-f5/pkg/networkpolicy"
	lbservice "github.com/gardener/gardener-extension-f5/pkg/service"
	lbstatus "github.com/gardener/gardener-extension-f5/pkg/status"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	networkingv1 "k8s.io/api/networking/v1"
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

	finalizerName = "f5.extensions.gardener.cloud/svc-lb-bridge"

	// Annotations stored on the Service to track CMP resource IDs.
	annLBServiceID     = "f5.extensions.gardener.cloud/lb-service-id"
	annVIPPortID       = "f5.extensions.gardener.cloud/vip-port-id"
	annVirtualServerID = "f5.extensions.gardener.cloud/virtual-server-id"
	annVIPAddress      = "f5.extensions.gardener.cloud/vip-address"
	annBackendHash     = "f5.extensions.gardener.cloud/backend-hash"
	// annObservedPorts persists independent virtual-server state per Service port.
	// The legacy scalar annotations above mirror the first listener for compatibility.
	annObservedPorts = "f5.extensions.gardener.cloud/observed-ports"
	// annObservedGraph persists the complete provider graph. Scalar annotations
	// remain migration compatibility mirrors for older consumers.
	annObservedGraph = "f5.extensions.gardener.cloud/observed-graph"

	// User-facing input annotations for per-Service LB configuration.
	// These override global defaults from F5LoadBalancerConfig CRD.
	annProtocol         = lbannotations.Protocol
	annRoutingAlgorithm = lbannotations.RoutingAlgorithm
	annHealthInterval   = lbannotations.HealthInterval
	annHealthType       = lbannotations.HealthType
	annHealthPath       = lbannotations.HealthPath
	annSourceRanges     = lbannotations.SourceRanges
	annDrainingTimeout  = lbannotations.DrainingTimeout
	annVIPGroup         = lbannotations.VIPGroup
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

type cmpLBaaS = lbaasdeploy.RawClient

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

			if _, err := lbfinalizers.Remove(ctx, r.Client, svc, finalizerName); err != nil {
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

			if _, err := lbfinalizers.Remove(ctx, r.Client, svc, finalizerName); err != nil {
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

				if _, err := lbfinalizers.Remove(ctx, r.Client, svc, finalizerName); err != nil {
					return ctrl.Result{}, err
				}
				f5metrics.ManagedServicesTotal.WithLabelValues("svc-lb-bridge").Dec()
			}
			return ctrl.Result{}, nil
		}
	}

	// Build a typed desired-state snapshot before mutating CMP resources.
	lbCfg := parseLBServiceConfig(svc)
	backends, err := lbbackend.ListReadyNodeBackends(ctx, r.Client, svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(backends) == 0 {
		log.Info("skipping: no ready nodes with endpoints found")
		return ctrl.Result{}, nil
	}
	stack, err := lbservice.BuildLoadBalancerStack(svc, lbCfg, backends)
	if err != nil {
		// Validation failures are actionable configuration errors, not a silent
		// no-op. Keep the object unmanaged until it is corrected, but surface a
		// stable reason in the Kubernetes event stream.
		reason := "InvalidLoadBalancerService"
		if strings.Contains(err.Error(), "BackendNodePortRequired") {
			reason = "BackendNodePortRequired"
		}
		r.Recorder.Eventf(svc, corev1.EventTypeWarning, reason, "%v", err)
		log.Info("cannot build desired load-balancer stack", "reason", reason, "error", err)
		return ctrl.Result{}, nil
	}

	// Ensure finalizer so we can cleanup CMP resources.
	if !controllerutil.ContainsFinalizer(svc, finalizerName) {
		if _, err := lbfinalizers.Ensure(ctx, r.Client, svc, finalizerName); err != nil {
			return ctrl.Result{}, err
		}
		f5metrics.ManagedServicesTotal.WithLabelValues("svc-lb-bridge").Inc()
	}

	// Reconcile the complete Service graph in one operation. This replaces the
	// legacy per-listener entry point: pools and members are now converged under
	// their own listener without recreating sibling listeners.
	shared := model.ObservedState{
		LBServiceID: strings.TrimSpace(svc.Annotations[annLBServiceID]),
		VIPPortID:   strings.TrimSpace(svc.Annotations[annVIPPortID]),
		VIPAddress:  strings.TrimSpace(svc.Annotations[annVIPAddress]),
	}
	if graph, ok := readObservedGraph(svc.Annotations); ok {
		shared.Graph = graph
	}
	cmpStart := time.Now()
	stackResult, err := lbaasdeploy.NewFromRaw(r.cmp, r.vpcID).EnsureStack(ctx, lbaasdeploy.StackEnsureRequest{Stack: stack, Current: shared})
	if err != nil {
		f5metrics.CMPAPICallDuration.WithLabelValues("svc-lb-bridge", "EnsureLB").Observe(time.Since(cmpStart).Seconds())
		f5metrics.CMPAPICallsTotal.WithLabelValues("svc-lb-bridge", "EnsureLB", "error").Inc()
		f5metrics.ReconcileErrorsTotal.WithLabelValues("svc-lb-bridge").Inc()
		if rle, ok := f5client.IsRateLimited(err); ok {
			r.Recorder.Eventf(svc, corev1.EventTypeWarning, "RateLimited", "CMP API rate limited; retrying after %s", rle.RetryAfter)
			return ctrl.Result{RequeueAfter: rle.RetryAfter}, nil
		}
		r.Recorder.Eventf(svc, corev1.EventTypeWarning, "SyncLoadBalancerFailed", "Error ensuring CMP LBaaS resource graph: %v", err)
		return ctrl.Result{}, err
	}
	shared = stackResult.Observed
	vip := shared.VIPAddress
	portObserved := make(map[string]servicePortObserved, len(stack.Ports))
	for _, port := range stack.Ports {
		vs := shared.Graph.VirtualServers[desiredVirtualServerName(svc, port.FrontendPort)]
		portObserved[servicePortKey(port)] = servicePortObserved{
			VirtualServerID: vs.ExternalID, VirtualServerName: vs.Name,
			BackendHash: lbaasdeploy.DesiredBackendHash(port.FrontendPort, port.NodePort, port.Backends),
		}
	}
	lastIDs := &f5client.CMPResourceIDs{LBServiceID: shared.LBServiceID, VIPPortID: shared.VIPPortID, VirtualServerID: shared.VirtualServerID, VirtualServerName: shared.VirtualServerName}
	if len(stack.Ports) > 0 && lastIDs.VirtualServerID == "" {
		last := portObserved[servicePortKey(stack.Ports[len(stack.Ports)-1])]
		lastIDs.VirtualServerID, lastIDs.VirtualServerName = last.VirtualServerID, last.VirtualServerName
	}
	f5metrics.CMPAPICallDuration.WithLabelValues("svc-lb-bridge", "EnsureLB").Observe(time.Since(cmpStart).Seconds())
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
	// Preserve legacy values for existing consumers while persisting every listener.
	svc.Annotations[annVirtualServerID] = lastIDs.VirtualServerID
	if vip != "" {
		svc.Annotations[annVIPAddress] = vip
	}
	svc.Annotations[annBackendHash] = portObserved[servicePortKey(stack.Ports[0])].BackendHash
	if err := writeServicePortObserved(svc.Annotations, portObserved); err != nil {
		return ctrl.Result{}, err
	}
	if err := writeObservedGraph(svc.Annotations, shared.Graph); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Patch(ctx, svc, client.MergeFrom(base)); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("CMP LBaaS resources ensured", "lbServiceID", lastIDs.LBServiceID, "vipPortID", lastIDs.VIPPortID, "vsID", lastIDs.VirtualServerID, "vip", vip, "ports", len(stack.Ports), "backends", len(backends))
	r.Recorder.Eventf(svc, corev1.EventTypeNormal, "EnsuredLoadBalancer", "CMP LBaaS resources ensured (LB=%s, VIP=%s, ports=%d, backends=%d)", lastIDs.LBServiceID, vip, len(stack.Ports), len(backends))

	// Auto-generate NetworkPolicy allowing ingress to backing pods.
	if err := lbnetworkpolicy.Ensure(ctx, r.Client, svc); err != nil {
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

func (r *serviceReconciler) ensureCMPResourcesWithCurrent(ctx context.Context, svc *corev1.Service, frontendPort, nodePort int32, backends []backendNode, protocol string, cfg lbServiceConfig, current model.ObservedState, currentHash string) (*f5client.CMPResourceIDs, model.ObservedState, string, error) {

	members := make([]model.BackendMember, 0, len(backends))
	for _, backend := range backends {
		members = append(members, model.BackendMember{IP: backend.IP, Port: nodePort, Weight: backend.Weight})
	}

	vs := model.VirtualServer{
		Name:             desiredVirtualServerName(svc, frontendPort),
		FrontendPort:     frontendPort,
		BackendNodePort:  nodePort,
		Protocol:         protocol,
		RoutingAlgorithm: cfg.RoutingAlgorithm,
		PersistenceType:  cfg.PersistenceType,
		DrainingTimeout:  cfg.DrainingTimeout,
		SourceRanges:     append([]string(nil), cfg.SourceRanges...),
		Monitor:          &model.Monitor{Type: cfg.HealthType, Path: cfg.HealthPath, Interval: cfg.HealthInterval},
	}
	result, err := lbaasdeploy.NewFromRaw(r.cmp, r.vpcID).Ensure(ctx, lbaasdeploy.EnsureRequest{
		LBName:                  desiredLBServiceName(svc),
		LBDescription:           fmt.Sprintf("App LB for %s/%s", svc.Namespace, svc.Name),
		VirtualServer:           vs,
		Backends:                members,
		Current:                 current,
		CurrentHash:             currentHash,
		RecreateWhenHashMissing: true,
	})
	if err != nil {
		return nil, current, "", err
	}
	ids := &f5client.CMPResourceIDs{
		LBServiceID:       result.Observed.LBServiceID,
		VIPPortID:         result.Observed.VIPPortID,
		VirtualServerID:   result.Observed.VirtualServerID,
		VirtualServerName: result.Observed.VirtualServerName,
	}
	return ids, result.Observed, result.BackendHash, nil
}

type servicePortObserved struct {
	VirtualServerID   string `json:"virtualServerID"`
	VirtualServerName string `json:"virtualServerName,omitempty"`
	BackendHash       string `json:"backendHash"`
}

func servicePortKey(port model.ServicePort) string {
	return fmt.Sprintf("%s/%d/%s", strings.TrimSpace(port.Name), port.FrontendPort, strings.ToLower(strings.TrimSpace(port.Protocol)))
}

func readServicePortObserved(annotations map[string]string) map[string]servicePortObserved {
	observed := map[string]servicePortObserved{}
	if annotations == nil || strings.TrimSpace(annotations[annObservedPorts]) == "" {
		return observed
	}
	if err := json.Unmarshal([]byte(annotations[annObservedPorts]), &observed); err != nil {
		return map[string]servicePortObserved{}
	}
	return observed
}

func writeServicePortObserved(annotations map[string]string, observed map[string]servicePortObserved) error {
	b, err := json.Marshal(observed)
	if err != nil {
		return fmt.Errorf("marshalling per-port observed state: %w", err)
	}
	annotations[annObservedPorts] = string(b)
	return nil
}

func readObservedGraph(annotations map[string]string) (model.ObservedGraph, bool) {
	if annotations == nil || strings.TrimSpace(annotations[annObservedGraph]) == "" {
		return model.ObservedGraph{}, false
	}
	var graph model.ObservedGraph
	if err := json.Unmarshal([]byte(annotations[annObservedGraph]), &graph); err != nil {
		return model.ObservedGraph{}, false
	}
	if graph.LBServices == nil {
		return model.ObservedGraph{}, false
	}
	return graph, true
}

func writeObservedGraph(annotations map[string]string, graph model.ObservedGraph) error {
	b, err := json.Marshal(graph)
	if err != nil {
		return fmt.Errorf("marshalling observed graph: %w", err)
	}
	annotations[annObservedGraph] = string(b)
	return nil
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
	observed := model.ObservedState{
		LBServiceID:     strings.TrimSpace(svc.Annotations[annLBServiceID]),
		VIPPortID:       strings.TrimSpace(svc.Annotations[annVIPPortID]),
		VirtualServerID: strings.TrimSpace(svc.Annotations[annVirtualServerID]),
		VIPAddress:      strings.TrimSpace(svc.Annotations[annVIPAddress]),
	}
	if graph, ok := readObservedGraph(svc.Annotations); ok {
		observed.Graph = graph
	}
	// A missing graph is a legacy object. EnsureGraph permits deletion of only
	// its recorded scalar resources; it never falls back to names or all VIPs.
	observed.EnsureGraph()
	shared := r.isLBServiceShared(ctx, svc, observed.LBServiceID)
	_, err := lbaasdeploy.NewFromRaw(r.cmp, r.vpcID).CleanupStack(ctx, lbaasdeploy.CleanupRequest{
		Current: observed, DeleteVIP: !shared, DeleteLBService: !shared,
	})
	return err
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
		if !controllerutil.ContainsFinalizer(&s, finalizerName) {
			continue
		}
		// Only a graph owned by this Service can retain a shared parent. A
		// copied scalar annotation is not sufficient authority to block cleanup.
		if graph, ok := readObservedGraph(s.Annotations); ok {
			for _, parent := range graph.LBServices {
				if parent.ExternalID == lbID && parent.Ownership.SourceKind == "Service" && parent.Ownership.SourceNamespace == s.Namespace && parent.Ownership.SourceName == s.Name && (parent.Ownership.SourceUID == "" || parent.Ownership.SourceUID == string(s.UID)) {
					return true
				}
			}
			continue
		}
		// Legacy objects have no graph ownership metadata; retain the existing
		// scalar compatibility behavior until their next successful reconcile.
		if s.Annotations[annLBServiceID] == lbID {
			return true
		}
	}
	return false
}

func (r *serviceReconciler) cleanupNetworkPolicy(ctx context.Context, svc *corev1.Service) {
	if err := lbnetworkpolicy.Delete(ctx, r.Client, svc); err != nil {
		ctrl.Log.WithName("svc-lb-bridge").Error(err, "failed to delete NetworkPolicy", "networkPolicy", lbnetworkpolicy.Name(svc), "namespace", svc.Namespace)
	}
}

func (r *serviceReconciler) ensureServiceStatusVIP(ctx context.Context, svc *corev1.Service, vip string) error {
	return lbstatus.EnsureServiceVIP(ctx, r.Client, svc, vip)
}

type backendNode = lbbackend.Node

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
