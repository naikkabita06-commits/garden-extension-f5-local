# F5 Gardener Extension — Fixes & Enhancements Changelog

**Date:** 11 May 2026  
**Scope:** All fixes applied to close gaps identified in [HYPERSCALER-PARITY-ANALYSIS.md](HYPERSCALER-PARITY-ANALYSIS.md)

---

## Code Gap Fixes (10)

### 1. Per-Service Annotation Model

**What:** Added 7 user-facing annotations to `svc-lb-bridge` and `seed-service-lb-controller` enabling per-Service configuration overrides without CRD changes.

**How:** Defined 7 annotation constants in the `const` block of each controller (e.g. `annProtocol = "f5.extensions.gardener.cloud/protocol"`). Created a `lbServiceConfig` struct to hold parsed config and a `parseLBServiceConfig(svc)` function that reads each annotation from the Service's metadata, validates the value (e.g. protocol must be one of TCP/UDP/HTTP/HTTPS, interval must be a positive integer), and returns a config with defaults applied for anything unset. The defaults are: `round_robin` for routing algorithm, `30` seconds for health check interval, `tcp` for health type. This config struct is then passed into `ensureCMPResources()` which sets the corresponding CMP API form parameters (`routing_algorithm`, `monitor_interval`, `monitor_type`, `monitor_path`, `allowed_cidrs`, `connection_draining_timeout`, `persistence_type`) on the VirtualServer create request. Additionally, `spec.sessionAffinity: ClientIP` is read from the Service spec and maps to `persistence_type=source_addr`, and `spec.loadBalancerSourceRanges` is read as a fallback if the annotation is not set.

| Annotation | Purpose | CMP Param |
|------------|---------|-----------|
| `f5.extensions.gardener.cloud/protocol` | Override auto-detected protocol (TCP/UDP/HTTP/HTTPS) | `protocol` |
| `f5.extensions.gardener.cloud/routing-algorithm` | LB algorithm (round_robin, least_connections, etc.) | `routing_algorithm` |
| `f5.extensions.gardener.cloud/health-check-interval` | Health check interval in seconds | `monitor_interval` |
| `f5.extensions.gardener.cloud/health-check-type` | Health check type (tcp/http) | `monitor_type` |
| `f5.extensions.gardener.cloud/health-check-path` | HTTP health check path | `monitor_path` |
| `f5.extensions.gardener.cloud/source-ranges` | Comma-separated allowed CIDRs | `allowed_cidrs` |
| `f5.extensions.gardener.cloud/connection-draining-timeout` | Connection draining timeout in seconds | `connection_draining_timeout` |

**Files:** `cmd/svc-lb-bridge/main.go`, `cmd/seed-service-lb-controller/main.go`

---

### 2. Multi-Port Service Support

**What:** `svc-lb-bridge` now handles all ports on a Service, not just the first one.

**How:** Added a `choosePorts(svc, protocolOverride)` function that iterates over every entry in `svc.Spec.Ports` and returns a `[]portInfo` slice containing the frontend port, NodePort, and detected protocol for each. The main reconcile loop then iterates over this slice and calls `ensureCMPResources()` once per port. Each call creates its own CMP VirtualServer with a deterministic name `app-vs-{namespace}-{name}-{port}` (via `desiredVirtualServerName()`), but all calls share the same LBService and VIP — the `ensureCMPResources` function uses find-or-create logic for LBService and VIP, so the first port creates them and subsequent ports just reuse the same IDs. Each VS gets its own protocol detection (e.g. port 80 → HTTP, port 443 → HTTPS, other ports → TCP), its own backend node list, and its own CMP params.

**Files:** `cmd/svc-lb-bridge/main.go`

---

### 3. Kubernetes Event Recording

**What:** All 4 controllers now write standard Kubernetes Events on the objects they manage, so users can see what happened via `kubectl describe`.

**How:** Obtained an `EventRecorder` from the controller-runtime manager during setup (`mgr.GetEventRecorderFor("svc-lb-bridge")`) and stored it on each reconciler struct. Then added `r.Recorder.Eventf()` calls at key points in the reconcile loop:
- After successfully ensuring all CMP resources → `"EnsuredLoadBalancer"` (Normal)
- When starting to delete CMP resources → `"DeletingLoadBalancer"` (Normal)
- When delete fails → `"DeleteFailed"` (Warning)
- When a VIP is allocated → `"AllocatedVIP"` (Normal)
- When CMP returns HTTP 429 → `"RateLimited"` (Warning)
- When ensuring CMP resources fails → `"SyncLoadBalancerFailed"` (Warning)

These events appear on the Service, Ingress, or Extension object respectively, and users can see them with `kubectl describe svc <name>` or `kubectl get events`.

**Files:** `cmd/svc-lb-bridge/main.go`, `cmd/svc-lb-bridge/ingress_controller.go`, `cmd/seed-service-lb-controller/main.go`, `pkg/controller/lifecycle/controller.go`

---

### 4. Credential Rotation / Secret Watch

**What:** When someone updates the CMP credentials Secret (rotates the Ce-Auth token or changes the username/password), the svc-lb-bridge Deployment automatically gets the new credentials without manual restart.

**How:** The credentials are stored in a Kubernetes Secret referenced by `spec.credentialsSecretRef` in the F5LoadBalancerConfig CRD. This Secret can contain either Ce-Auth token-based auth (`Ce-Auth` + `project-id` keys) or basic auth (`username` + `password` keys). During every reconcile, the extension controller's `reconcileCISInShoot()` function re-reads the Secret from the Seed cluster, extracts the current credential values, and calls `controllerutil.CreateOrUpdate()` on the svc-lb-bridge Deployment in the Shoot cluster. The Deployment spec sets environment variables directly from the Secret values (e.g. `CMP_CE_AUTH=<current token value>`). If the Secret value changed since the last reconcile, the Deployment spec changes, which triggers a Kubernetes rolling restart of the svc-lb-bridge pod with fresh credentials. The Gardener extension framework triggers re-reconcile when the Extension CR is updated or periodically, which ensures credential changes are picked up.

**Files:** `pkg/controller/lifecycle/controller.go`

---

### 5. NetworkPolicy Auto-Generation

**What:** Every LoadBalancer Service now gets an auto-generated Kubernetes NetworkPolicy that only allows traffic on the service ports to the correct pods.

**How:** Added two functions: `ensureNetworkPolicy(ctx, svc)` and `cleanupNetworkPolicy(ctx, svc)`. The `ensureNetworkPolicy` function first checks that the Service has a pod selector (`svc.Spec.Selector`). It then builds a list of `NetworkPolicyPort` entries from the Service's ports (one entry per port with the correct protocol). It creates a `NetworkPolicy` object named `f5-lb-allow-{svc-name}` in the Service's namespace using `controllerutil.CreateOrUpdate()`. The policy's `PodSelector` is set to `svc.Spec.Selector` (the same labels the Service uses to find its backing pods), and the ingress rule allows traffic on those ports from any source. The policy is labeled with `app.kubernetes.io/managed-by: svc-lb-bridge` for identification. `ensureNetworkPolicy` is called at the end of every successful reconcile. `cleanupNetworkPolicy` simply deletes the named policy and is called during Service deletion (after CMP cleanup), as well as when a Service is no longer of type LoadBalancer.

**Files:** `cmd/svc-lb-bridge/main.go`

---

### 6. Ingress Controller (IngressClass "f5")

**What:** Users can now create Kubernetes Ingress resources with `ingressClassName: f5` and the controller will set up an HTTP/HTTPS load balancer via CMP.

**How:** Created a new `ingressReconciler` struct in `ingress_controller.go` that implements `reconcile.Reconciler`. It is registered with the controller-runtime manager to watch `networkingv1.Ingress` resources filtered by `ingressClassName == "f5"`. The reconcile flow mirrors the Service controller:
1. If the Ingress is being deleted and has a finalizer → call `cleanupCMPResources()` (cascade delete: VS → VIP → LBService) → remove finalizer.
2. If the Ingress is active → add finalizer → call `ensureCMPResources()` which creates a CMP LBService (named `ing-{namespace}-{name}`), allocates a VIP on it, and creates an HTTP or HTTPS VirtualServer with backend nodes. The VS protocol is chosen based on whether the Ingress rule uses port 443/8443 (HTTPS) or not (HTTP).
3. Store the CMP resource IDs as annotations on the Ingress (`annIngressLBServiceID`, `annIngressVIPPortID`, `annIngressVSID`, `annIngressVIPAddress`).
4. Write the allocated VIP into `ingress.status.loadBalancer.ingress[].ip` so external DNS or users can find it.

Backend nodes are discovered the same way as for Services — using EndpointSlice to find nodes with ready pods, with pod-proportional weights.

**Files:** `cmd/svc-lb-bridge/ingress_controller.go`

---

### 7. EndpointSlice-Based Dynamic Backends

**What:** Instead of registering all cluster nodes as load balancer backends, the controller now only registers nodes that actually have running pods for the Service, and gives more weight to nodes with more pods.

**How:** Added a `getNodesWithReadyEndpoints(ctx, client, svc)` function that lists EndpointSlices matching the Service (using the standard `discovery.kubernetes.io/service-name` label). For each EndpointSlice, it iterates over endpoints, checks `ep.Conditions.Ready`, and builds a `map[nodeName]→endpointCount`. The `listBackendNodes()` function then lists all cluster nodes, filters for Ready nodes, and cross-references with the EndpointSlice map. If EndpointSlice data is available, only nodes present in the map are included. Each node's weight is set proportionally: `endpointCount × 50` (so a node with 3 pods gets weight 150, a node with 1 pod gets weight 50). This weight is passed to CMP as the `weight` field in the backend node JSON. The controller also registers a `Watches(&discoveryv1.EndpointSlice{})` on the manager with a custom `MapFunc` that maps EndpointSlice changes back to the owning Service, so any pod scale-up/down or readiness change triggers a Service re-reconcile.

**Files:** `cmd/svc-lb-bridge/main.go`

---

### 8. RBAC Expansion for Extension Controller

**What:** The svc-lb-bridge Deployment in the Shoot cluster needed additional Kubernetes permissions to work with all the new features.

**How:** The extension controller's `reconcileCISInShoot()` function creates a ClusterRole and ClusterRoleBinding in the Shoot cluster for the svc-lb-bridge service account. The ClusterRole's `Rules` were expanded to include additional API groups and resources:
- `discovery.k8s.io` group → `endpointslices` (get, list, watch) — needed for EndpointSlice-based backend discovery
- `networking.k8s.io` group → `ingresses`, `ingresses/status` (get, list, watch, update, patch) — needed for the Ingress controller
- `networking.k8s.io` group → `networkpolicies` (get, list, create, update, patch, delete) — needed for auto-generated NetworkPolicies
- Core group → `events` (create, patch) — needed for the EventRecorder

The ClusterRole is applied via `controllerutil.CreateOrUpdate()` so it is updated in-place whenever the extension is reconciled.

**Files:** `pkg/controller/lifecycle/controller.go`

---

### 9. Shared VIP (vip-group Annotation)

**What:** Multiple Services (or Ingresses) can now share a single VIP by adding the same `f5.extensions.gardener.cloud/vip-group` annotation. Instead of each Service getting its own IP address, all Services in a group share one.

**How — creation side:** Modified the `desiredLBServiceName(svc)` function to check for the `annVIPGroup` annotation. If the annotation is set (e.g. `vip-group: "my-app"`), the LBService is named `app-group-{namespace}-{group}` instead of the normal `app-{namespace}-{service-name}`. This means multiple Services with the same group name in the same namespace produce the same LBService name. Since `ensureCMPResources()` uses find-or-create logic (`findLBServiceByName()` → if not found, `CreateLBService()`), the first Service in the group creates the LBService and VIP, and subsequent Services find the existing one by name and reuse it. Each Service still creates its own VirtualServer(s) on the shared LBService, so traffic for each port is routed independently. All Services in the group end up with the same VIP in their `status.loadBalancer.ingress[].ip`.

**How — deletion side:** Modified `cleanupCMPResources()` to be ref-counted. After deleting this Service's VirtualServer(s), the function calls `isLBServiceShared(ctx, self, lbID)`. This function lists all Services in the same namespace, and checks if any *other* Service (with a different name) has the same `annLBServiceID` annotation value and still has the finalizer. If yes, it means other Services are still using the shared LBService — so the function skips VIP and LBService deletion. If no other Service references the same LB ID, it's the last one in the group, and the full cascade delete (VIP → LBService) proceeds.

**How — Ingress controller:** Same pattern applied. LBService is named `ing-group-{namespace}-{group}` when annotation is set. A `findLBServiceByName()` method was added to the ingress reconciler (lists all CMP LBServices and matches by name). VIP step was changed to first list existing VIPs on the LBService before creating a new one (for group members joining an already-created LBService). `isLBServiceShared()` for Ingress lists all Ingresses in the namespace and checks for matching `annIngressLBServiceID`.

**Files:** `cmd/svc-lb-bridge/main.go`, `cmd/svc-lb-bridge/ingress_controller.go`

---

### 10. Automatic Ce-Auth Token Rotation

**What:** The CMP Ce-Auth token is a short-lived HMAC-signed token (default 299 seconds / ~5 minutes). Previously, operators had to manually regenerate it using a Python script or copy it from the CMP UI. If no one rotated it, the token expired and all CMP API calls failed. Now the extension controller automatically generates fresh tokens from long-lived API key credentials stored in the Secret.

**How:** Added a `GenerateCeAuthToken(apiKeyID, secretKey, validity)` function in `pkg/f5/client.go` — a Go port of the existing `scripts/cmp-ceauth.py` Python script. It produces tokens in the format `{apiKeyID}.{expiryTimestamp}.{hmacSha256Hex}` using the standard `crypto/hmac` + `crypto/sha256` packages.

Added a `refreshCeAuthIfNeeded(ctx, log, secret, existingCeAuth, existingErr)` method on the extension controller actuator. This method is called in all 3 credential-reading paths (control-plane provisioning, control-plane cleanup, app-plane svc-lb-bridge deployment). It checks the Secret for `api-key-id` + `api-secret` keys. If both are present, it generates a fresh 299-second Ce-Auth token and writes it back to the Secret's `Ce-Auth` key via `a.client.Update()`. If only a manual `Ce-Auth` token exists (no API keys), it passes through unchanged (backward compatible).

The Secret update triggers the existing Secret watch (`mapSecretToExtensions`) → Extension re-reconcile → svc-lb-bridge Deployment gets updated `CMP_CE_AUTH` env var → Kubernetes rolling restart with fresh token.

Operators now store `api-key-id` + `api-secret` (permanent, never expire) instead of a pre-generated `Ce-Auth` token. The controller keeps the token fresh on every reconcile cycle automatically.

**Files:** `pkg/f5/client.go`, `pkg/controller/lifecycle/controller.go`, `deploy/kind/cmp-f5-credentials.secret.yaml`

---

## Bug Fixes (5)

### Bug Fix 1: RBAC Missing Permissions

**What:** The extension actuator was hitting "forbidden" errors when trying to manage certain Kubernetes resources.

**How:** The ClusterRole created by `reconcileCISInShoot()` was missing rules for some resources that the controller needed. Added the missing API groups, resources, and verbs to the `PolicyRule` list in the ClusterRole spec.

**Files:** `pkg/controller/lifecycle/controller.go`

---

### Bug Fix 2: Multi-Port VS Cleanup

**What:** When a multi-port Service was deleted, some VirtualServers were left behind on CMP because the controller only tried to delete the single VS ID stored in the annotation.

**How:** With multi-port support, each port creates its own VS, but only the last VS ID gets stored in the annotation (they overwrite each other). The cleanup function was updated: if no VS ID is stored in the annotation, it calls `r.cmp.ListLBVirtualServers(ctx, lbID)` to list all VirtualServers on the LBService, then iterates through them looking for names matching the prefix `app-vs-{namespace}-{name}-`. Any VS with a matching prefix is deleted. This ensures all port-specific VSs are cleaned up even without stored IDs.

**Files:** `cmd/svc-lb-bridge/main.go`

---

### Bug Fix 3: Ingress Annotation Constants

**What:** The Ingress controller was reading/writing the wrong annotation keys — it was using Service-level annotation constants instead of its own Ingress-specific ones.

**How:** The Ingress controller has its own set of annotation constants (`annIngressLBServiceID`, `annIngressVIPPortID`, `annIngressVSID`, `annIngressVIPAddress`) that are separate from the Service controller's (`annLBServiceID`, `annVIPPortID`, etc.). The code was referencing the Service-level constants in some places. Fixed all annotation reads and writes in the Ingress reconciler to consistently use the `annIngress*` constants.

**Files:** `cmd/svc-lb-bridge/ingress_controller.go`

---

### Bug Fix 4: ManagedServicesTotal Gauge Lifecycle

**What:** The `ManagedServicesTotal` Prometheus gauge only went up, never down. After running for a while it would show 50 managed Services even if only 2 were still active.

**How:** The gauge's `.Inc()` was already being called when a finalizer was first added (Service creation). Added a corresponding `.Dec()` call in the cleanup path — specifically, after the finalizer is removed during deletion. This ensures the gauge accurately reflects the current number of Services the controller is actively managing.

**Files:** `cmd/svc-lb-bridge/main.go`

---

### Bug Fix 5: CMPAPICallDuration Histogram Instrumentation

**What:** The `CMPAPICallDuration` Prometheus histogram metric existed but was never being recorded. Duration was always zero or absent.

**How:** Added `time.Now()` captures before CMP API call blocks and `time.Since(start).Seconds()` observations after. Specifically: a `cmpStart := time.Now()` is captured before the multi-port loop that calls `ensureCMPResources()`, and after the loop completes (or on error), `f5metrics.CMPAPICallDuration.WithLabelValues("svc-lb-bridge", "EnsureLB").Observe(time.Since(cmpStart).Seconds())` records the total duration. Same pattern applied to delete operations in `cleanupCMPResources()` and to all CMP calls in the seed-service-lb-controller and ingress controller. Labels include the controller name and operation type (EnsureLB, DeleteLB, etc.) for per-operation dashboarding.

**Files:** `cmd/svc-lb-bridge/main.go`, `cmd/svc-lb-bridge/ingress_controller.go`, `cmd/seed-service-lb-controller/main.go`

---

## Documentation Fixes (4)

1. **Annotation count** — Corrected from 6 → 7 in the parity matrix. The `connection-draining-timeout` annotation was implemented but not counted in the doc.
2. **EndpointSlice row** — Clarified that `svc-lb-bridge` uses EndpointSlice for backend selection while `seed-service-lb-controller` uses the Node list directly. The previous wording was ambiguous.
3. **Static/reserved IP** — Reclassified from "code gap" to "infra gap". Investigation confirmed CMP's `POST .../vip` endpoint accepts no IP parameter — the extension cannot honour `spec.loadBalancerIP` without a CMP API change.
4. **Shared VIP row** — Updated from "Code gap — no VIP grouping logic" to "Supported" after implementing the `vip-group` annotation with ref-counted cleanup.

---

## Verification

All 3 binaries compile cleanly and all unit tests pass:

```
go build ./cmd/gardener-extension-f5/...      ✅
go build ./cmd/seed-service-lb-controller/...  ✅
go build ./cmd/svc-lb-bridge/...               ✅
go test  ./cmd/svc-lb-bridge/...               ✅
go test  ./cmd/seed-service-lb-controller/...  ✅
go test  ./pkg/...                             ✅
```
