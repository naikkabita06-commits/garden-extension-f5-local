# F5 Gardener Extension — Full Engineering Walkthrough

## Table of Contents

1. [Architecture Overview](#1-architecture-overview)
2. [Repository Structure](#2-repository-structure)
3. [The Three Binaries](#3-the-three-binaries)
4. [CRD: F5LoadBalancerConfig](#4-crd-f5loadbalancerconfig)
5. [Extension Entry Point](#5-extension-entry-point)
6. [Extension Controller (Reconciler + Actuator)](#6-extension-controller-reconciler--actuator)
7. [CMP HTTP Client](#7-cmp-http-client)
8. [Seed Service LB Controller](#8-seed-service-lb-controller)
9. [Shoot svc-lb-bridge](#9-shoot-svc-lb-bridge)
10. [Metrics & Observability](#10-metrics--observability)
11. [Mechanism A vs Mechanism B](#11-mechanism-a-vs-mechanism-b)
12. [Lifecycle Operations (Migrate / Restore / Delete)](#12-lifecycle-operations-migrate--restore--delete)
13. [Partitioning Strategy](#13-partitioning-strategy)
14. [Comparison with Hyperscaler Extensions](#14-comparison-with-hyperscaler-extensions)
15. [Production Gaps & Roadmap](#15-production-gaps--roadmap)

---

## 1. Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              SEED CLUSTER                                       │
│                                                                                 │
│  ┌────────────────────────┐       ┌─────────────────────────────┐               │
│  │ gardener-extension-f5  │       │  seed-service-lb-controller │               │
│  │ (main extension)       │       │  (Seed LB for Services)     │               │
│  │                        │       │                             │               │
│  │ Watches: Extension CR  │       │ Watches: Services + Nodes   │               │
│  │ Creates: F5LBConfig CR │       │ Provisions: LBService→VIP→VS│               │
│  │ Deploys: svc-lb-bridge │       └──────────────┬──────────────┘               │
│  └────────────┬───────────┘                      │                              │
│               │                                  │                              │
│               │  (Shoot kubeconfig)              │                              │
│               ▼                                  ▼                              │
│  ┌─────────────────────────────────────────────────────────────────┐            │
│  │                     CMP LBaaS v2.1 API                          │            │
│  │  POST /load-balancers/lb_service/        → Create LB Service    │            │
│  │  POST /load-balancers/lb_service/{id}/vip → Allocate VIP        │            │
│  │  POST /load-balancers/{id}/virtual-servers → Create VS          │            │
│  └─────────────────────────────────────────────────────────────────┘            │
└─────────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────────────────┐
│                             SHOOT CLUSTER                                       │
│                                                                                 │
│  ┌───────────────────────────────┐                                              │
│  │  svc-lb-bridge                │                                              │
│  │  (App-plane LB controller)    │                                              │
│  │                               │                                              │
│  │  Watches: Services type=LB    │                                              │
│  │  + Nodes (pool members)       │                                              │
│  │  Provisions: LBService→VIP→VS │                                              │
│  │  Writes: Service.status.LB    │                                              │
│  └───────────────────────────────┘                                              │
└─────────────────────────────────────────────────────────────────────────────────┘
```

**Key Design Decisions:**
- **No direct BIG-IP communication** — all provisioning goes through CMP LBaaS v2.1 API
- **No AS3** — CMP handles BIG-IP configuration internally
- **NodePort pool members** — backends are Seed/Shoot node IPs with NodePort (no CNI requirement)
- **Generic Gardener Extension** — implements the `extensions.gardener.cloud/v1alpha1 Extension` contract

---

## 2. Repository Structure

```
gardener-extension-f5/
├── cmd/
│   ├── gardener-extension-f5/main.go    # Main extension binary
│   ├── seed-service-lb-controller/main.go  # Seed Service LB
│   ├── svc-lb-bridge/main.go           # Shoot app-plane LB
│   └── cmpctl/main.go                  # CLI tool for CMP debugging
├── pkg/
│   ├── apis/f5/v1alpha1/types.go       # CRD types (F5LoadBalancerConfig)
│   ├── controller/lifecycle/
│   │   ├── extension_controller.go     # Reconciler registration + finalizer
│   │   └── controller.go              # Actuator (core business logic)
│   ├── f5/client.go                    # CMP HTTP client
│   └── metrics/metrics.go             # Prometheus metrics
├── charts/gardener-extension-f5/       # Helm chart for deployment
├── config/crd/                         # CRD YAML definitions
├── deploy/                             # Garden + Kind deployment manifests
└── scripts/                            # Dev/debug scripts
```

---

## 3. The Three Binaries

| Binary                       | Runs In | Purpose                                                                                                                    | Watches                                          |
|------------------------------|---------|----------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------|
| `gardener-extension-f5`      | Seed    | Main Gardener extension. Reconciles Extension CRs, provisions control-plane VIP via CMP, deploys svc-lb-bridge into Shoots | `Extension` CRs (type `f5-loadbalancer` or `f5`) |
| `seed-service-lb-controller` | Seed    | LB controller for Seed-level Services (e.g., Istio ingress gateway). Provisions LBService→VIP→VirtualServer via CMP        | `Service` (type=LB) + `Node`                     |
| `svc-lb-bridge`              | Shoot   | App-plane LB controller for Shoot workloads. Same CMP flow but inside the Shoot cluster                                    | `Service` (type=LB, class=f5) + `Node`           |

---

## 4. CRD: F5LoadBalancerConfig

**API Group:** `f5.extensions.gardener.cloud/v1alpha1`  
**Kind:** `F5LoadBalancerConfig`

### Spec (desired state)

| Field                           | Type            | Purpose                                         |
|---------------------------------|-----------------|-------------------------------------------------|
| `ccpApiEndpoint`                | string          | CMP LBaaS base URL                              |
| `tenantOrPartition`             | string          | CMP tenant / BIG-IP partition                   |
| `credentialsSecretRef`          | SecretReference | Points to Secret with Ce-Auth or Basic Auth     |
| `controlPlaneVIP`               | string          | VIP for kube-apiserver                          |
| `controlPlaneReady`             | *bool           | Manual override for dev/demo                    |
| `enablePerShootControlPlaneVIP` | bool            | Mechanism B toggle (dedicated VIP per Shoot)    |
| `enableApplicationLB`           | bool            | Deploy svc-lb-bridge into Shoot                 |
| `cis`                           | CISConfig       | Bridge image config, BIG-IP URL, partition      |
| `flavorId`                      | int32           | CMP LB flavor                                   |
| `networkId`                     | string          | CMP network/subnet                              |
| `vpcId`                         | string          | CMP VPC ID                                      |
| `vpcName`                       | string          | CMP VPC name                                    |
| `routingAlgorithm`              | string          | Load-balancing algorithm (default: round_robin) |
| `monitorInterval`               | int32           | Health-check interval seconds (default: 30)     |

### Status (observed state)

| Field               | Type      | Purpose                                                         |
|---------------------|-----------|-----------------------------------------------------------------|
| `vip`               | string    | Allocated VIP address                                           |
| `virtualServerName` | string    | VS name on CMP                                                  |
| `lbServiceId`       | string    | CMP LB Service ID                                               |
| `vipPortId`         | string    | CMP VIP port ID                                                 |
| `virtualServerId`   | string    | CMP Virtual Server ID                                           |
| `conditions[]`      | Condition | `ControlPlaneLoadBalancerReady`, `ApplicationLoadBalancerReady` |

### How it's created

The F5LoadBalancerConfig is **not created by the user directly**. It is auto-created by the extension from `Extension.spec.providerConfig` JSON:

```json
{
  "ccpApiEndpoint": "https://cmp.example.com",
  "tenantOrPartition": "my-tenant",
  "credentialsSecretRef": {"name": "f5-creds", "namespace": "shoot--proj--name"},
  "enablePerShootControlPlaneVIP": true,
  "enableApplicationLB": true,
  "cis": {
    "bridgeImage": "registry.example.com/svc-lb-bridge:latest",
    "bigipUrl": "https://bigip.example.com",
    "partition": "my-partition"
  }
}
```

---

## 5. Extension Entry Point

**File:** `cmd/gardener-extension-f5/main.go`

```go
func main() {
    // 1. Setup structured JSON logger (Gardener convention)
    runtimelog.SetLogger(logger.MustNewZapLogger(logger.InfoLevel, logger.FormatJSON))

    // 2. Register schemes: client-go + Gardener Extensions + F5 CRD
    s := runtime.NewScheme()
    clientgoscheme.AddToScheme(s)
    extensionsv1alpha1.AddToScheme(s)
    f5v1alpha1.AddToScheme(s)

    // 3. Create controller-runtime Manager with leader election
    mgr := ctrl.NewManager(cfg, ctrl.Options{
        Scheme:           s,
        LeaderElection:   true,
        LeaderElectionID: "gardener-extension-f5-leader",
    })

    // 4. Register the lifecycle controller
    lifecycle.AddToManager(mgr, ctrl.Log)

    // 5. Optionally register CRD conversion webhook (if TLS certs present)
    if webhookEnabled {
        mgr.GetWebhookServer().Register("/convert", ...)
    }

    // 6. Start (blocking)
    mgr.Start(ctx)
}
```

**Key points:**
- Leader election ensures only one replica reconciles at a time
- Health/readiness probes on `:8081`
- Webhook server optional (only needed for multi-version CRD conversion)

---

## 6. Extension Controller (Reconciler + Actuator)

### 6.1 Reconciler (`extension_controller.go`)

The reconciler is the entry gate. It:

1. **Watches** all `Extension` CRs across namespaces
2. **Filters** by type (`f5-loadbalancer` or legacy `f5`)
3. **Manages finalizer** `extensions.gardener.cloud/f5`
4. **Delegates** to `Actuator.Reconcile()` or `Actuator.Delete()`
5. **Clears** the `gardener.cloud/operation` annotation (Gardener handshake)

```go
func (r *ExtensionReconciler) Reconcile(ctx, req) (Result, error) {
    ex := get Extension CR
    if !isSupportedExtensionType(ex.Spec.Type) → skip

    if not being deleted:
        add finalizer
        call Actuator.Reconcile(ctx, log, ex)
        clear gardener.cloud/operation annotation  // ← CRITICAL: gardenlet waits for this
    else:
        call Actuator.Delete(ctx, log, ex)
        clear operation annotation
        remove finalizer
}
```

### 6.2 Actuator (`controller.go`)

The actuator implements the real business logic. It satisfies the Gardener `extensioncontroller.Actuator` interface:

```go
type Actuator interface {
    Reconcile(ctx, log, Extension) error
    Delete(ctx, log, Extension) error
    ForceDelete(ctx, log, Extension) error
    Migrate(ctx, log, Extension) error
    Restore(ctx, log, Extension) error
}
```

#### Reconcile Flow

```
Reconcile()
├── ensureF5LoadBalancerConfig()          // Create/sync CRD from providerConfig
│
├── if !enablePerShootControlPlaneVIP:
│   └── reconcileControlPlaneStatusSharedSeedIngress()  // Mechanism A: mark Ready immediately
│
├── elif ccpApiEndpoint != "":
│   └── provisionControlPlaneViaCMP()     // Mechanism B: full CMP provisioning
│       ├── Read credentials from Secret
│       ├── Discover kube-apiserver backends (Endpoints)
│       ├── Create CMP client (Ce-Auth or Basic Auth)
│       ├── Probe CMP endpoint (connectivity check)
│       ├── SetCMPLBaaSConfig (flavor, network, vpc, algorithm)
│       └── EnsureControlPlaneVirtualServer()
│           ├── ensureOrFindLBService()
│           ├── ensureOrFindVIP()
│           └── ensureOrFindVirtualServer()
│
├── elif controlPlaneReady != nil:
│   └── reconcileControlPlaneStatus()     // Dev stub: trust user override
│
├── if enableApplicationLB AND ControlPlaneReady:
│   └── reconcileCISInShoot()             // Deploy svc-lb-bridge into Shoot
│       ├── getShootClient() (via Seed kubeconfig Secret)
│       ├── Ensure namespace f5-cis-system
│       ├── Ensure ServiceAccount + RBAC
│       └── Ensure svc-lb-bridge Deployment
│
└── updateExtensionOutput()               // Write VIP/port into Extension.status.providerStatus
```

#### Error Handling

- **`permanentError`** — config/credential errors that should NOT be retried (wrapped with `permanent()`)
- **Transient errors** — network/CMP failures that ARE retried by controller-runtime's exponential backoff
- **Rate limiting** — CMP HTTP 429 responses are caught and requeued with `RequeueAfter`

---

## 7. CMP HTTP Client

**File:** `pkg/f5/client.go`

### Authentication Modes

| Mode           | Headers                                                            | Use Case                      |
|----------------|--------------------------------------------------------------------|-------------------------------|
| **Ce-Auth**    | `Ce-Auth: <token>`, `organisation-name: <org>`, `project-id: <id>` | CMP platform API (production) |
| **Basic Auth** | `Authorization: Basic <base64>`                                    | Legacy/direct BIG-IP (dev)    |

### Rate Limiting

```go
rateLimiter: rate.NewLimiter(rate.Limit(10), 20)  // 10 req/s, burst 20
```

Client-side rate limiting prevents triggering CMP's server-side 429 when many Shoots reconcile simultaneously.

### TLS Configuration

| Env Var                       | Purpose                                    |
|-------------------------------|--------------------------------------------|
| `F5_INSECURE_SKIP_TLS_VERIFY` | Skip TLS cert verification (dev only)      |
| `F5_CA_BUNDLE_PATH`           | Custom CA bundle for private CMP endpoints |

### CMP LBaaS v2.1 Flow

```
┌──────────────────┐      ┌───────────────┐      ┌────────────────────┐
│ 1. Create/Find   │ ───► │ 2. Create/Find│ ───► │ 3. Create/Find     │
│    LB Service    │      │    VIP Port   │      │    Virtual Server  │
│                  │      │               │      │    (with backends) │
└──────────────────┘      └───────────────┘      └────────────────────┘
POST /lb_service/         POST /lb_service/      POST /{id}/virtual-servers
                          {id}/vip               ?nodes=[{ip,port,weight}]
```

### Key Methods

```go
type Client interface {
    Probe(ctx) (*ProbeResult, error)                                    // Connectivity check
    EnsureControlPlaneVirtualServer(ctx, vip, port, backends) (*IDs, error)  // High-level CP provisioning
    DeleteControlPlaneVirtualServer(ctx, ids) error                      // Reverse-order cleanup

    // Low-level CMP LBaaS:
    ListLBServices / CreateLBService / DeleteLBService
    CreateLBServiceVIP / GetLBServiceVIPs / DeleteLBServiceVIP
    ListLBVirtualServers / CreateLBVirtualServer / DeleteLBVirtualServer
}
```

---

## 8. Seed Service LB Controller

**File:** `cmd/seed-service-lb-controller/main.go`  
**Purpose:** Manages `Service.type=LoadBalancer` in the **Seed** cluster (e.g., Istio ingress gateway)

### How it works

1. Watches all Services in the Seed cluster
2. Filters by `spec.loadBalancerClass: f5.extensions.gardener.cloud/bigip`
3. On eligible Service:
   - Adds finalizer `f5.extensions.gardener.cloud/seed-service-lb`
   - Provisions CMP resources: LBService → VIP → VirtualServer
   - Pool members = **all Seed Node InternalIPs** with the Service's NodePort
   - Stores CMP resource IDs in Service annotations
   - Writes VIP into `Service.status.loadBalancer.ingress[0].ip`
4. On Node change: re-enqueues all managed Services to update pool members

### Annotations

| Annotation                                       | Value                 |
|--------------------------------------------------|-----------------------|
| `f5.extensions.gardener.cloud/lb-service-id`     | CMP LB Service ID     |
| `f5.extensions.gardener.cloud/vip-port-id`       | CMP VIP port ID       |
| `f5.extensions.gardener.cloud/virtual-server-id` | CMP Virtual Server ID |
| `f5.extensions.gardener.cloud/vip-address`       | Allocated VIP IP      |

### Configuration (env vars)

| Env Var                         | Required | Purpose                  |
|---------------------------------|----------|--------------------------|
| `CMP_ENDPOINT`                  | Yes      | CMP LBaaS base URL       |
| `CMP_CE_AUTH`                   | Yes      | Ce-Auth token            |
| `CMP_ORGANISATION_NAME`         | Yes      | CMP org/tenant           |
| `CMP_PROJECT_ID`                | Yes      | CMP project              |
| `CMP_FLAVOR_ID`                 | No       | LB flavor                |
| `CMP_NETWORK_ID`                | No       | Network/subnet           |
| `CMP_VPC_ID`                    | No       | VPC ID                   |
| `CMP_VPC_NAME`                  | No       | VPC name                 |
| `F5_SEED_LB_LOADBALANCER_CLASS` | No       | Override LB class filter |

---

## 9. Shoot svc-lb-bridge

**File:** `cmd/svc-lb-bridge/main.go`  
**Purpose:** Manages `Service.type=LoadBalancer` in **Shoot** clusters (application workloads)

### Deployment

The svc-lb-bridge is **deployed into the Shoot** by the main extension controller (`reconcileCISInShoot()`):
- Namespace: `f5-cis-system`
- ServiceAccount: `f5-cis` with ClusterRole (get/list/watch Services+Nodes, update/patch Services/status)
- Deployment: `f5-svc-lb-bridge` with CMP credentials in env vars

### Backend Hash (Change Detection)

The bridge uses a **SHA-256 hash** of `(frontendPort, nodePort, nodeIPs)` to detect when pool members change:

```go
func desiredBackendHash(frontendPort, nodePort int32, nodeIPs []string) string {
    sort.Strings(ips)
    h := sha256.Sum256("frontend=443;nodeport=31234;10.0.0.1;10.0.0.2;")
    return hex.EncodeToString(h[:])
}
```

If the hash changes (node scale-up/down, port change), the VS is **deleted and recreated** with new backends.

### Configuration (env vars)

| Env Var                        | Required | Purpose                                                         |
|--------------------------------|----------|-----------------------------------------------------------------|
| `CMP_ENDPOINT`                 | Yes      | CMP LBaaS base URL                                              |
| `CMP_CE_AUTH`                  | Yes      | Ce-Auth token                                                   |
| `CMP_ORGANISATION_NAME`        | Yes      | CMP org/tenant                                                  |
| `CMP_PROJECT_ID`               | Yes      | CMP project                                                     |
| `CMP_VPC_ID`                   | No       | VPC ID for VS creation                                          |
| `F5_SVC_LB_LOADBALANCER_CLASS` | No       | LB class filter (default: `f5.extensions.gardener.cloud/bigip`) |

---

## 10. Metrics & Observability

**File:** `pkg/metrics/metrics.go`

| Metric                             | Type      | Labels                        | Purpose                    |
|------------------------------------|-----------|-------------------------------|----------------------------|
| `f5_cmp_api_calls_total`           | Counter   | controller, operation, result | Every CMP API call         |
| `f5_cmp_api_call_duration_seconds` | Histogram | controller, operation         | CMP call latency           |
| `f5_vip_allocations_total`         | Counter   | controller, result            | VIP allocation attempts    |
| `f5_reconcile_errors_total`        | Counter   | controller                    | Reconcile loop errors      |
| `f5_managed_services_total`        | Gauge     | controller                    | Currently managed Services |

All metrics are registered with controller-runtime's default Prometheus registry.

---

## 11. Mechanism A vs Mechanism B

### Mechanism A — Shared Seed Ingress VIP (Default)

```
Client → Shared Seed VIP → Istio → SNI routing → kube-apiserver Pod
```

- `enablePerShootControlPlaneVIP: false`
- No per-Shoot CMP provisioning needed
- Control-plane LB condition is set to `Ready` immediately
- Same as vanilla Gardener behavior
- The **seed-service-lb-controller** manages the shared VIP

### Mechanism B — Dedicated Per-Shoot VIP

```
Client → Dedicated VIP → F5 VS → NodePort → kube-apiserver Pod
```

- `enablePerShootControlPlaneVIP: true`
- Extension calls CMP to provision LBService→VIP→VirtualServer per Shoot
- Pool members = kube-apiserver pod IPs (from Endpoints)
- Frontend port = 443 or 6443 (auto-detected)
- Bypasses Istio entirely

### When to use which

| Criteria    | Mechanism A                    | Mechanism B                 |
|-------------|--------------------------------|-----------------------------|
| Simplicity  | ✅ Zero F5 per-shoot work       | ❌ More moving parts         |
| Performance | ✅ Shared infra                 | ✅ Direct path, no Istio hop |
| Isolation   | ❌ Shared VIP                   | ✅ Dedicated VIP per Shoot   |
| Scalability | ⚠️ Single VIP, many SNI routes | ✅ Linear VIP scaling        |
| Compliance  | ⚠️ Shared IP in DNS            | ✅ Unique IP per Shoot       |

---

## 12. Lifecycle Operations (Migrate / Restore / Delete)

### Delete (Shoot deletion)

```
Delete()
├── cleanupControlPlaneViaCMP()    // Delete VS → VIP → LBService (reverse order, best-effort)
└── cleanupCISInShoot()            // Delete bridge Deployment + RBAC from Shoot (best-effort)
```

Best-effort: deletion is **never blocked** by CMP failures.

### Migrate (source Seed, before Shoot moves)

```
Migrate()
├── Read F5LoadBalancerConfig status (CMP resource IDs)
├── Marshal into Extension.status.providerStatus
│   {lbServiceId, vipPortId, virtualServerId, virtualServerName, controlPlaneVip}
├── Patch Extension status
└── cleanupCISInShoot() (best-effort; destination Seed will re-deploy)
```

**Critical:** CMP resources are NOT deleted — the VIP must stay active during migration.

### Restore (destination Seed, after Shoot arrives)

```
Restore()
├── ensureF5LoadBalancerConfig()   // Re-create CRD from providerConfig
├── Re-hydrate CMP IDs from Extension.status.providerStatus
│   cfg.Status = {lbServiceId, vipPortId, virtualServerId, vip}
├── Set ControlPlaneLoadBalancerReady = True  // VIP still exists on CMP
├── reconcileCISInShoot()          // Re-deploy bridge if app LB enabled
└── updateExtensionOutput()
```

---

## 13. Partitioning Strategy

### Current Implementation: **Per-Tenant/Environment**

The extension uses a **per-tenant (per-environment)** partitioning strategy, not per-seed or per-shoot:

| Level              | What gets its own partition?                                         | Evidence                                                               |
|--------------------|----------------------------------------------------------------------|------------------------------------------------------------------------|
| **Tenant/Org**     | `spec.tenantOrPartition` in F5LoadBalancerConfig                     | All Shoots in the same org share the same CMP tenant                   |
| **LB Service**     | Named `cp-lb-{partition}` (control-plane) or `app-{ns}-{name}` (app) | One CMP LB Service per partition for CP; one per Shoot Service for app |
| **VIP**            | One VIP per LB Service                                               | Each VIP is scoped to its LB Service                                   |
| **Virtual Server** | Named `cp-apiserver-vs` (CP) or `app-vs-{ns}-{name}-{port}` (app)    | One VS per purpose per LB Service                                      |

### Naming conventions on CMP:

```
Control-plane:
  LB Service:      cp-lb-{tenantOrPartition}
  Virtual Server:  cp-apiserver-vs

Seed Services:
  LB Service:      seed-{namespace}-{svcName}
  Virtual Server:  seed-vs-{namespace}-{svcName}-{port}

Shoot App Services:
  LB Service:      app-{namespace}-{svcName}
  Virtual Server:  app-vs-{namespace}-{svcName}-{port}
```

### What this means:

- **Per-Seed:** The `seed-service-lb-controller` creates per-Service resources on CMP. All Seeds sharing the same CMP tenant/org can see each other's LB Services (no Seed-level isolation on CMP).
- **Per-Shoot:** Each Shoot gets its own Virtual Server (Mechanism B) or shares the Seed Ingress VIP (Mechanism A). App-plane Services get individual LB Services on CMP.
- **Per-Environment:** The CMP `organisationName` + `projectID` headers scope API calls to a specific environment. Cross-tenant visibility is prevented by CMP's IAM, not by the extension.

### Hyperscaler comparison:

| Provider   | Partition Boundary           | Isolation Mechanism                         |
|------------|------------------------------|---------------------------------------------|
| AWS        | Per-Shoot (VPC)              | Each Shoot = separate cloud account or VPC  |
| GCP        | Per-Shoot (Project)          | Each Shoot = GCP project-level isolation    |
| Azure      | Per-Shoot (Resource Group)   | Each Shoot = dedicated resource group       |
| **F5/CMP** | **Per-Tenant + per-Service** | CMP org/project headers + LB Service naming |

---

## 14. Comparison with Hyperscaler Extensions

| Feature                     | AWS/GCP/Azure CCM                 | This F5 Extension               |
|-----------------------------|-----------------------------------|---------------------------------|
| **Protocol**                | Cloud API (AWS SDK, GCP client)   | CMP LBaaS v2.1 REST             |
| **VIP allocation**          | Cloud allocates IPs automatically | CMP allocates VIP on LB Service |
| **Health checks**           | Built into cloud LB               | CMP `interval` parameter        |
| **Pool members**            | Instance IDs or Pod IPs (ENI)     | Node InternalIPs + NodePort     |
| **Multi-zone**              | Automatic cross-AZ distribution   | Single pool, no zone awareness  |
| **Credential rotation**     | IAM roles, auto-rotated           | Manual Secret update (gap)      |
| **Quota pre-check**         | Cloud APIs expose quota limits    | Not implemented (gap)           |
| **UDP/SCTP**                | Full protocol support             | TCP only (gap)                  |
| **Event recording**         | K8s Events on Service objects     | Structured logs only (gap)      |
| **Network Policy**          | Auto-injected allow-from-LB       | Not implemented (gap)           |
| **Per-Service annotations** | Rich annotation model             | loadBalancerClass only          |

---

## 15. Production Gaps & Roadmap

### High Priority

| # | Gap                                                     | Impact                                     | Effort                     |
|---|---------------------------------------------------------|--------------------------------------------|----------------------------|
| 1 | **Credential rotation** — no watch on Secret changes    | Stale Ce-Auth tokens cause silent failures | Medium                     |
| 2 | **Quota pre-check** — no pre-flight capacity validation | Half-provisioned state on CMP exhaustion   | Low (if CMP has quota API) |
| 3 | **Network Policy injection** — no allow-from-VIP rules  | Shoot pods unreachable from F5 VIP         | Medium                     |

### Medium Priority

| # | Gap                                                           | Impact                                  | Effort |
|---|---------------------------------------------------------------|-----------------------------------------|--------|
| 4 | **UDP protocol** — only TCP Virtual Servers                   | DNS/VoIP workloads can't use F5 LB      | Medium |
| 5 | **Event recording** — no K8s Events on Services               | Shoot users can't debug LB provisioning | Low    |
| 6 | **Exponential backoff with jitter** — fixed requeue intervals | Thundering herd on CMP recovery         | Low    |

### Lower Priority

| # | Gap                                                     | Impact                                    | Effort |
|---|---------------------------------------------------------|-------------------------------------------|--------|
| 7 | **Per-Service annotations** — no fine-grained LB tuning | Users can't customize routing per Service | Medium |
| 8 | **Multi-AZ weighting** — equal-weight pool members      | Uneven traffic distribution across zones  | High   |

---

## Appendix: Quick Reference

### Build & Run

```bash
# Build all binaries
make build

# Run extension locally (against kind cluster)
./bin/gardener-extension-f5

# Run seed-service-lb-controller
CMP_ENDPOINT=https://cmp.example.com \
CMP_CE_AUTH=<token> \
CMP_ORGANISATION_NAME=<org> \
CMP_PROJECT_ID=<project> \
./bin/seed-service-lb-controller

# Run svc-lb-bridge (in Shoot kubeconfig context)
CMP_ENDPOINT=https://cmp.example.com \
CMP_CE_AUTH=<token> \
CMP_ORGANISATION_NAME=<org> \
CMP_PROJECT_ID=<project> \
./bin/svc-lb-bridge
```

### Key Finalizers

| Finalizer                                      | Owner                      | Purpose                                      |
|------------------------------------------------|----------------------------|----------------------------------------------|
| `extensions.gardener.cloud/f5`                 | gardener-extension-f5      | Ensure CMP cleanup on Extension deletion     |
| `f5.extensions.gardener.cloud/seed-service-lb` | seed-service-lb-controller | Ensure CMP cleanup on Seed Service deletion  |
| `f5.extensions.gardener.cloud/svc-lb-bridge`   | svc-lb-bridge              | Ensure CMP cleanup on Shoot Service deletion |

### Key Conditions

| Condition                       | Set By                | True When                         |
|---------------------------------|-----------------------|-----------------------------------|
| `ControlPlaneLoadBalancerReady` | gardener-extension-f5 | VIP/VS provisioned or Mechanism A |
| `ApplicationLoadBalancerReady`  | gardener-extension-f5 | svc-lb-bridge running in Shoot    |

### Error Classification

| Error Type         | Behavior                           | Example                             |
|--------------------|------------------------------------|-------------------------------------|
| `permanentError`   | No retry, set lastOperation=Failed | Missing credentials, invalid config |
| Transient error    | Retry with backoff                 | CMP timeout, network unreachable    |
| `RateLimitedError` | Requeue after Retry-After          | CMP HTTP 429                        |
