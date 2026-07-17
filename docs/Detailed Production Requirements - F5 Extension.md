Production Requirements & Operational Expectations for Gardener F5 Extension (LBaaS) in KaaS Platform

## Overview

In our Kubernetes-as-a-Service (KaaS) platform built on Gardener, the F5 extension is a critical infrastructure component responsible for ensuring that Shoot kube-apiservers and tenant application services are reliably reachable from outside the cluster.

The extension provisions and manages load balancing infrastructure through:

- **Seed Ingress LB** → shared VIP for all Shoot kube-apiservers via Istio SNI routing (Mechanism A)
- **Per-Shoot Control-Plane LB** → dedicated VIP per Shoot kube-apiserver bypassing Istio (Mechanism B, optional)
- **Application-Plane LB** → `Service type=LoadBalancer` fulfillment for tenant workloads inside Shoot clusters (optional)

In production, this extension forms the network entry point for both the Gardener control plane and tenant applications, and therefore must meet strict expectations around:

- availability
- observability
- reconciliation safety
- CMP LBaaS compatibility
- operational controls
- recovery readiness

---

## 1. Core Functionalities Supported by the F5 Extension

### 1.1 Seed Ingress Load Balancing — Mechanism A (Shared VIP)

The `seed-service-lb-controller` provisions and manages a VIP for the Seed cluster's Istio ingress gateway Service.

Supported features:

- Detect `Service type=LoadBalancer` with `loadBalancerClass: f5.extensions.gardener.cloud/bigip` on the Seed
- Collect all Seed node Internal IPs and the Istio NodePort
- Call CMP LBaaS API to create `LBService → VIP → VirtualServer`
- Write the allocated VIP into `Service.status.loadBalancer.ingress[0].ip`
- Re-reconcile pool members when Seed nodes are added or removed

Production expectation:

When the Seed is installed, the Istio ingress gateway must receive a stable VIP before any Shoot kube-apiserver DNS is registered. Without this, no Shoot can be reached via its kube-apiserver endpoint.

The single shared VIP routes kube-apiserver traffic to the correct Shoot using Istio SNI inspection on the wildcard domain `api.<shoot>.<project>.<seed>.gardener.cloud`.

### 1.2 Per-Shoot Dedicated Control-Plane VIP — Mechanism B (Optional)

When `enablePerShootControlPlaneVIP: true`, the `gardener-extension-f5` controller provisions a dedicated VIP per Shoot.

Supported features:

- Call CMP: `POST /lb_service → GET /vip → POST /virtual-servers`
- Backends = Seed node IPs + kube-apiserver NodePort
- Store `lbServiceId`, `vipPortId`, `virtualServerId`, `vip` in `F5LoadBalancerConfig.status`
- Set `ControlPlaneLoadBalancerReady=True` after VS is confirmed
- Clean up CMP resources on Shoot deletion (with finalizer-held retry on transient failure)

Production expectation:

Every Shoot using Mechanism B must have its dedicated VS active before gardenlet proceeds with Shoot provisioning. Without it, the Shoot's kube-apiserver is unreachable and the Shoot cannot be used.

### 1.3 Application-Plane Load Balancing

When `enableApplicationLB: true` and `ControlPlaneLoadBalancerReady=True`, the extension deploys `svc-lb-bridge` into the Shoot cluster.

Supported features:

- `svc-lb-bridge` watches `Service type=LoadBalancer` with `loadBalancerClass: f5.extensions.gardener.cloud/bigip` in the Shoot
- For each eligible Service: call CMP to create `LBService → VIP → VirtualServer` with worker Node IPs + NodePort as pool members
- Write allocated VIP into `Service.status.loadBalancer.ingress[].ip`
- Re-enqueue all managed Services when Shoot worker nodes are added or removed
- Finalizer-held cleanup on Service deletion or type change

Production expectation:

For every `Service type=LoadBalancer` created by a tenant:

A valid VIP and Virtual Server must exist in CMP/BIG-IP and be reachable from the internet before the Service is considered ready.

This ensures that tenants can expose applications reliably without managing BIG-IP or CMP directly.

### 1.4 Shoot Lifecycle Management

The `gardener-extension-f5` controller handles the full lifecycle of each Shoot's load balancing resources.

Supported features:

- `Reconcile`: create/sync `F5LoadBalancerConfig` from `Extension.spec.providerConfig`; provision or skip CP VIP; deploy `svc-lb-bridge` if enabled
- `Delete`: clean up CMP resources (LBService, VIP, VS) and remove `svc-lb-bridge` from Shoot
- `Migrate` / `Restore`: stubs — **not yet implemented** (see C-1)

Production expectation:

On every Shoot creation, the load balancer must be provisioned and marked Ready before gardenlet proceeds. On every Shoot deletion, all CMP resources must be cleaned up to prevent orphaned VIPs/VSes from accumulating.

### 1.5 Credential Management

The extension and its sub-controllers consume CMP credentials via Kubernetes Secrets.

Supported features:

- Read `Ce-Auth` HMAC token, `project-id`, and `organisation-name` from a Secret in the extension namespace
- Authenticate all CMP API calls with Ce-Auth header set
- Support private CMP CAs via `F5_CA_BUNDLE_PATH` (PEM bundle loaded into HTTP client TLS trust)

Production expectation:

Credentials must allow:

- creation of LBServices, VIPs, and Virtual Servers
- deletion of same
- health/probe calls to validate connectivity

Credentials must be provisioned in the Seed's extension namespace before any Shoot reconciles.

### 1.6 Continuous Reconciliation

All three controllers reconcile desired vs. actual state continuously.

Supported features:

- Verify VIP and VS existence on CMP
- Reconcile pool members against current Node IPs
- Update Extension and CRD status
- Recover from transient failures via controller-manager requeue

Production expectation:

Reconciliation must be idempotent and safe. Repeated reconciliations must never:

- re-provision a VIP that is already allocated
- corrupt status fields
- remove a VIP that is still in use

This guarantees stability under controller restarts or transient CMP outages.

---

## 2. Status Reporting & Observability

This is the most important production requirement after basic functionality.

The F5 extension exposes operational state through:

- `F5LoadBalancerConfig.status` — per-Shoot CMP resource IDs and conditions
- `Extension.status.providerStatus` — VIP address and port written back to Gardener
- Service annotations — per-Service CMP resource IDs managed by `svc-lb-bridge`

These statuses are the primary way the platform can determine:

_Is load balancing infrastructure healthy and is the Shoot control plane / tenant application currently reachable?_

### 2.1 F5LoadBalancerConfig Status Reporting — Where?

Status is reported in: `F5LoadBalancerConfig.status`

This exposes:

- `vip` — the active VIP address
- `virtualServerName` / `virtualServerId` — VS identity on CMP
- `lbServiceId` / `vipPortId` — CMP resource IDs for cleanup tracking
- `conditions[]` — `ControlPlaneLoadBalancerReady` and `ApplicationLoadBalancerReady` with reason and message

Example:

```yaml
status:
  vip: "10.1.2.100"
  virtualServerId: "vs-abc123"
  lbServiceId: "lbs-xyz789"
  conditions:
  - type: ControlPlaneLoadBalancerReady
    status: "True"
    reason: VirtualServerProvisioned
    message: VIP 10.1.2.100 is active on CMP
  - type: ApplicationLoadBalancerReady
    status: "True"
    reason: BridgeDeployed
    message: svc-lb-bridge is running in the Shoot
```

Why this matters in production:

This gives operators immediate visibility into:

- whether the extension successfully provisioned a VIP
- which CMP resources belong to which Shoot (for manual remediation)
- whether the Shoot control plane is load-balancer ready

This becomes the source of truth for load balancer readiness.

### 2.2 Extension providerStatus Reporting

The controller writes back to `Extension.status.providerStatus`:

- `controlPlaneVip` — the active VIP address visible to gardenlet
- `controlPlanePort` — auto-detected port (prefers 443, then 6443)

This confirms:

The Shoot's kube-apiserver endpoint is resolvable via a live VIP.

### 2.3 How Observability Should Be Leveraged in KaaS Production

In a KaaS platform, these statuses should not just exist — they should drive platform automation.

#### A. Platform Readiness Gates

Before marking a Shoot control plane as healthy:

Validate:
- `F5LoadBalancerConfig.status.conditions[ControlPlaneLoadBalancerReady].status = True`
- If `enableApplicationLB: true`: `conditions[ApplicationLoadBalancerReady].status = True`

If not ready:

The Shoot must not be handed to the tenant until load balancing is confirmed. This can be integrated into:

- Shoot readiness conditions
- control-plane reconciliation gates

#### B. Alerting

Prometheus alerts should be generated if:

- `ControlPlaneLoadBalancerReady=False` for more than a threshold duration
- `ApplicationLoadBalancerReady=False` after bridge deployment
- Repeated CMP API call failures
- `svc-lb-bridge` pod not Running in a Shoot

Examples:

- CMP authentication failure (Ce-Auth token expired)
- CMP endpoint unreachable from Seed
- VIP allocated but VS creation failed
- Pool members out of sync after Node add/remove

This allows operators to detect failures before tenant applications are affected.

#### C. Tenant Health Visibility

Expose LB readiness as part of tenant cluster health:

- **LB Healthy**: both conditions True, all Services have VIPs
- **LB Degraded**: bridge running but some Services missing VIPs
- **LB Failed**: ControlPlaneLoadBalancerReady=False

This enables platform teams to surface:

_Load balancer readiness as an SLO component for each Shoot._

This is essential in enterprise KaaS.

#### D. Incident Diagnosis

Status conditions should provide:

- `reason` (machine-readable)
- `message` (human-readable explanation)
- CMP resource IDs (for direct CMP console lookup)

This enables faster MTTR during:

- CMP API outages
- Ce-Auth token rotation issues
- BIG-IP → Node network failures
- VIP exhaustion events

Without actionable status reporting, load balancer diagnosis becomes slow and manual.

---

## 2a. External Dependency on CMP Team

The following gaps in the CMP v2.1 LBaaS API are blockers or significant risks for production. We are requesting CMP team engagement to discuss these enhancements.

| # | Gap | What breaks if not addressed | Severity |
|---|---|---|---|
| A1 | **No per-member pool operations.** There is no API to add or remove individual pool members — only full pool replacement is supported. In Kubernetes, nodes scale dynamically. Replacing the entire pool on each scale event causes disruption to active traffic. **Required: `POST /lb_services/{id}/pools/{pool_id}/members` and `DELETE /lb_services/{id}/pools/{pool_id}/members/{member_id}`** | Every Node add/remove requires full VS delete-recreate, causing a traffic outage each time. | Critical / Blocker |
| A2 | **No compute lookup by Node IP.** CMP requires a `compute_id` for pool members, but Kubernetes only provides Node IPs. There is no API to resolve IP → compute_id. **Required: `GET /computes?ip=<node-ip>`** | Pool members cannot be registered without manual pre-mapping of IPs to compute IDs, which is not viable in a dynamic cluster. | Critical / Blocker |
| A3 | **No BIG-IP partition support.** There is no partition parameter in any LB API. Without partition isolation, resources across Shoots may have naming collisions, there is no logical tenant isolation, and cleanup/ownership tracking is unreliable. **Required: partition selection during LB/VS creation.** | Multi-Shoot deployments risk resource conflicts and cannot be safely isolated per tenant. | Critical |
| A4 | **Async status enums undocumented.** LB operations are asynchronous but valid values for `status` and `operating_status` are not documented. Reconciliation logic cannot reliably detect success vs. in-progress vs. failure. | Reconciliation may incorrectly treat in-progress LB operations as failures, causing unnecessary retries or missed errors. | Significant |
| A5 | **No labels/tags on Virtual Server.** There is no way to attach metadata (e.g., Shoot name, Service UID) to a VS. Without this, idempotency checks and cleanup after partial failures depend on naming conventions alone. | Orphaned resources are hard to identify after partial failures; idempotent VS reconciliation is fragile. | Significant |
| A6 | **Multi-port VIP behaviour undefined.** The behavior of multiple ports (e.g., 80 and 443) sharing a single VIP is not specified in the API. | Multi-port Services may behave unpredictably; the extension cannot safely support them without documented semantics. | Significant |
| C-3 | **No Virtual Server update API.** Because CMP only supports delete + recreate for VSes, there is a gap of a few seconds during which the load balancer is down on every pool member update. This is directly caused by the absence of A1. | Brief traffic drop on every Node scale event. | P1 — blocked until A1 is resolved |

---

## 3. Production Requirements from Infrastructure & CMP Team

For production readiness in our KaaS platform, the CMP LBaaS backend and network infrastructure must satisfy the following requirements.

### 3.1 Availability & SLA

**Requirement:**

- ≥99.95% availability for CMP LBaaS API
- MTTR < 30 min for any VIP/VS provisioning failure

**Why:**

If CMP is unavailable, new Shoots cannot provision their control-plane VIPs and new tenant LoadBalancer Services cannot get VIPs. Existing VIPs remain up (served by BIG-IP), but any Shoot creation, deletion, or Node scale event will fail.

**Production expectation:**

CMP API availability directly impacts KaaS control-plane provisioning SLA.

### 3.2 High Availability

**Requirement:**

- HA CMP API topology
- automatic failover
- no single CMP node dependency

**Why:**

A single CMP instance outage must not make all VIP provisioning unavailable. BIG-IP itself can continue forwarding traffic, but the control path (provisioning/updates) becomes blocked.

**Production expectation:**

No single CMP failure should block Shoot provisioning or Node scale events.

### 3.3 BIG-IP → Shoot Worker Node Network Reachability

**Requirement:**

- BIG-IP pool members must be able to reach every Shoot worker node's Internal IP on the configured NodePorts
- Also required: BIG-IP → Seed node IPs on the Istio NodePort (for Mechanism A)

**Why:**

Even if a VIP and VS are successfully provisioned in CMP/BIG-IP, traffic will never reach pods if there is no L3 path from BIG-IP to the worker nodes.

**Production expectation:**

Network team must validate and document that all required BIG-IP → Node IP routes exist before any Shoot goes live.

### 3.4 CMP API Accessibility from Seed and Shoot Clusters

**Requirement:**

- CMP HTTPS endpoint reachable from the Seed cluster (for `gardener-extension-f5` and `seed-service-lb-controller`)
- CMP HTTPS endpoint reachable from every Shoot cluster (for `svc-lb-bridge`)

**Why:**

Both the Seed-side controllers and the Shoot-side bridge make outbound HTTPS calls to CMP. Firewall rules must allow this traffic.

**Production expectation:**

Firewall/network team must provision and validate egress rules before extension deployment.

### 3.5 TLS & Networking

**Requirement:**

- Valid TLS certificate on CMP endpoint (or CA bundle provided via `F5_CA_BUNDLE_PATH`)
- Stable long-lived HTTPS connections
- Tuned keepalives

**Why:**

CMP API calls are synchronous in the reconcile loop. Connection instability causes reconcile failures that requeue and retry, putting extra load on CMP and the controller.

**Production expectation:**

TLS certificate expiry must be monitored and renewed before expiry. `F5_CA_BUNDLE_PATH` must be pre-populated in values if a private CA is used.

### 3.6 CMP Rate Limits and Quota

**Requirement:**

- CMP must either not rate limit at operational load, or document per-project rate limits
- VIP quota must be sized for: 1 VIP per Seed (Mechanism A) + up to N VIPs per Shoot (Mechanism B + app LB Services)

**Why:**

At controller restart, all Shoots are reconciled simultaneously. Without rate limit awareness, simultaneous 429 responses from CMP will cause all Shoots to fail reconciliation at once (C-9 in code gaps).

**Production expectation:**

Platform architecture team must plan CMP tenancy, project allocation, and VIP quota before the first multi-Shoot deployment.

### 3.7 Observability Requirements

**Requirement:**

CMP should provide:

- per-request logs with request IDs
- Prometheus metrics (or compatible scrape endpoint)

Required metrics:

- 5xx rate
- API request latency
- VIP/VS creation success rate
- rate of quota rejection (429)

**Why:**

This enables:

- alerting on CMP degradation
- incident triage
- SLO monitoring of provisioning P99

Without backend observability, CMP failures are only discovered when operators check status manually.

### 3.8 Operational Controls

**Requirement:**

Admin APIs or runbooks to:

- list all VIPs and VSes in a given project
- delete orphaned VIPs/VSes after Shoot migration failures
- inspect VS pool member health
- rotate Ce-Auth credentials without downtime

Also required:

- documented runbooks for common failure modes (CMP unreachable, VIP allocation failed, pool members out of sync)

**Why:**

Platform teams need deterministic remediation steps. This is required for:

- operational support
- disaster recovery drills
- incident response

### 3.9 CMP Tenancy Strategy

**Requirement:**

Platform architecture must decide, per Shoot or per cluster-group:

- whether each Shoot gets its own CMP project/VPC or shares one
- which CMP `flavorId`, `networkId`, `vpcId` are used per Seed
- how CMP credentials are scoped (per-Shoot vs. shared)

**Why:**

The `F5LoadBalancerConfig.spec` fields `vpcId`, `networkId`, `flavorId`, `projectId` must be populated before the extension can provision any resources. Without a tenancy strategy, these values are undefined and provisioning is blocked.

**Production expectation:**

These values must be documented in a per-Seed or per-cluster-type configuration profile and loaded into values.yaml before the first Shoot is created.

---

## 4. Final Production Readiness Expectation

For production in our KaaS platform, the F5 extension must guarantee:

_At any point in time, every Shoot cluster's kube-apiserver is reachable via a live, healthy VIP, and every tenant-requested LoadBalancer Service has a valid VIP backed by healthy BIG-IP pool members._

This means:

**Extension guarantees:**

- `seed-service-lb-controller` maintains a live Seed Ingress VIP
- Every Shoot with Mechanism B has a live per-Shoot CP VIP
- Every `Service type=LoadBalancer` has a live app-plane VIP
- CMP resource IDs are tracked via finalizers — no orphaned resources on deletion
- Status is observable — conditions, VIP addresses, CMP IDs are visible in the CRD
- Failures are alertable — conditions flip to False with reason/message

**CMP / Network infrastructure guarantees:**

- HA CMP API availability
- BIG-IP → Node network connectivity
- VIP/VS quota headroom
- TLS validity
- Rate limit awareness
- Admin remediation APIs

---

## 5. Success Criteria for Production Go-Live

The F5 extension is considered production-ready only when:

- [ ] Seed Ingress VIP is provisioned and stable (`seed-service-lb-controller` running and healthy)
- [ ] First Shoot provisioned end-to-end with reachable kube-apiserver via VIP
- [ ] `ControlPlaneLoadBalancerReady=True` observable in `F5LoadBalancerConfig.status`
- [ ] `ApplicationLoadBalancerReady=True` observable when `enableApplicationLB: true`
- [ ] Node scale event does not break existing VIP connectivity (pool member re-sync validated)
- [ ] Shoot deletion cleans up all CMP resources (no orphaned VIPs)
- [ ] CMP accessible from Seed and Shoot networks (I-2, I-3 — Network / Firewall team)
- [ ] BIG-IP → Node network validated (I-1 — Network team)
- [ ] CMP tenancy strategy documented and CMP credentials provisioned (I-4, I-5 — Platform Architecture / Ops team)
- [ ] CMP VS update API available to eliminate delete-recreate outage (C-3 — CMP team)
- [ ] CMP per-member pool operations API available (A1 — CMP team)
- [ ] CMP compute lookup by Node IP API available (A2 — CMP team)
- [ ] CMP BIG-IP partition support confirmed (A3 — CMP team)
- [ ] CMP async status enums documented (A4 — CMP team)
- [ ] Production design signed off (I-6 — Product team)
- [ ] Node scale drill and Shoot migration drill succeed
