# Hyperscaler Load Balancer Parity Analysis

## F5 Gardener Extension vs GKE / EKS

**Date:** 6 May 2026  
**Author:** Platform Architecture Team  
**Scope:** L4/L7 Load Balancing — Feature Parity Assessment

---

## Executive Summary

The F5 Gardener Extension provides **L4 and basic L7 load balancing** via the CMP LBaaS v2.1 API. It handles two core use cases: (1) control-plane HA for Shoot kube-apiservers, and (2) application-plane LoadBalancer Services and Ingress resources in Shoot clusters.

Compared to GKE (Google Cloud) and EKS (Amazon Web Services) native load balancer integrations, the F5 extension covers approximately **~55% of feature surface area**. It supports TCP/UDP/HTTP/HTTPS protocols, multi-port Services, Ingress (IngressClass "f5"), EndpointSlice-based dynamic backends with pod-proportional weights, per-Service annotation-driven configuration (protocol, routing algorithm, health checks, source IP filtering, session affinity, connection draining), K8s event recording, credential rotation, and auto-generated NetworkPolicies. Remaining gaps include Gateway API support, path/host routing, TLS termination, IPv6, multi-zone awareness, and CMP API limitations for observability and metering.

**Key Finding:** The extension is architecturally sound for its current purpose (Gardener control-plane HA + basic Service LB) and now includes a comprehensive annotation model for per-Service customization, matching the configuration patterns used by GKE and EKS controllers. The remaining gaps are primarily CMP API limitations (L7 routing, TLS termination, observability) and infrastructure-level constraints (IPv6, multi-zone, pod-direct targeting).

> **Note:** The extension deploys `svc-lb-bridge` into Shoot clusters for application-plane LoadBalancer Services and Ingress resources. It supports TCP, UDP, HTTP, and HTTPS protocols (auto-detected from port numbers), processes all Service ports, and reconciles Ingress resources with IngressClass "f5" (similar to F5 CIS).

---

## 1. Feature Comparison Matrix

### 1.1 Core Load Balancing

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| Service type=LoadBalancer | External Network LB (L4) + Application LB (L7) | Classic LB, NLB (L4), ALB (L7) | CMP LBaaS VirtualServer — TCP/UDP/HTTP/HTTPS, multi-port supported | **Supported** | — |
| Ingress | GKE Ingress Controller (L7 HTTP/HTTPS) | AWS ALB Ingress Controller | ✅ svc-lb-bridge Ingress controller (IngressClass "f5"); creates HTTP/HTTPS VS via CMP | **Supported** | — |
| Gateway API | GKE Gateway Controller (GA) | AWS Gateway API Controller | ❌ Not implemented | **Not Supported** | Infra gap — CMP has no L7 policy primitives for host/path/header routing |
| L4 TCP | ✅ Full (Network LB) | ✅ Full (NLB) | ✅ Multi-port TCP via CMP (svc-lb-bridge + seed-service-lb-controller) | **Supported** | — |
| L4 UDP | ✅ Full | ✅ Full (NLB) | ✅ UDP protocol supported via CMP | **Supported** | — |
| L7 HTTP/HTTPS | ✅ Full (URL map, path rules, host rules) | ✅ Full (ALB target groups, rules) | ⚠️ CMP supports HTTP/HTTPS VS; protocol auto-detected by port (80→HTTP, 443→HTTPS) or overridable via `f5.extensions.gardener.cloud/protocol` annotation; no path/host routing | **Partially Supported** | Infra gap — CMP VS has no path/host rule configuration |
| L7 gRPC | ✅ Native gRPC health + routing | ✅ Via ALB | ❌ Not implemented | **Not Supported** | Infra gap — CMP/BIG-IP has no gRPC-aware routing or health |
| L7 WebSocket | ✅ Supported | ✅ Supported via ALB | ❌ Not implemented | **Not Supported** | ⚠️ Likely works (BIG-IP allows HTTP Upgrade by default); needs CMP/F5 confirmation |
| Multi-port Services | ✅ All ports forwarded | ✅ All ports (NLB) | ✅ All Service ports processed; one VS per port | **Supported** | — |

### 1.2 VIP & IP Management

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| Auto VIP allocation | ✅ Cloud allocates ephemeral IP | ✅ AWS allocates EIP/private IP | ✅ CMP allocates VIP on LB Service | **Supported** | — |
| Static / reserved IP | ✅ `loadBalancerIP` or `spec.loadBalancerSourceRanges` + reserved Regional IP | ✅ EIP allocation + annotation | ⚠️ User provides `controlPlaneVIP` statically in CRD; app-plane VIPs are CMP-allocated (no specific IP can be requested) | **Partially Supported** | Infra gap — CMP `POST .../vip` accepts no IP parameter; VIP allocation is entirely CMP-controlled. `spec.loadBalancerIP` (deprecated K8s 1.24+) cannot be honoured without CMP API enhancement |
| Internal LB | ✅ `cloud.google.com/load-balancer-type: Internal` | ✅ `service.beta.kubernetes.io/aws-load-balancer-internal: "true"` | ❌ All LBs are external (CMP has no internal/external distinction exposed) | **Not Supported** | Infra gap — CMP API has no internal/external flag; needs CMP/F5 confirmation |
| Shared VIP (multiple Services) | ✅ GKE multi-cluster Ingress, shared VIP | ⚠️ Via ALB Ingress grouping | ✅ `f5.extensions.gardener.cloud/vip-group` annotation groups Services/Ingresses onto a shared CMP LBService+VIP with ref-counted cleanup | **Supported** | Fixed — vip-group annotation with ref-counted cleanup for both Service and Ingress controllers |
| IPv4 | ✅ Default | ✅ Default | ✅ Default | **Supported** | — |
| IPv6 | ✅ Dual-stack supported | ✅ Dual-stack NLB/ALB | ❌ No IPv6 handling | **Not Supported** | Infra gap — confirmed with F5 team: CMP/BIG-IP IPv6 not supported |
| Dual-stack (IPv4+IPv6) | ✅ Supported | ✅ Supported | ❌ Not implemented | **Not Supported** | Infra gap — same as IPv6 above |

### 1.3 Backend Targeting

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| NodePort targeting | ✅ Default for external LB | ✅ Default (instance mode) | ✅ All nodes + NodePort (only mode) | **Supported** | — |
| Pod-direct targeting (NEG/IP mode) | ✅ Container-native LB via NEGs | ✅ IP target type (pods directly) | ❌ Always uses NodePort; no pod IP targeting | **Not Supported** | Infra gap — BIG-IP needs L3 routes to pod CIDR (BGP/VXLAN); network team action required |
| Dynamic backend registration | ✅ Auto-scales with pods (NEG) or nodes | ✅ Auto-registers targets | ✅ svc-lb-bridge watches EndpointSlice + Node (only Ready nodes with ready endpoints registered); seed-service-lb-controller uses all-node registration | **Supported** | — |
| Label-based backend selection | ✅ Via NEG + topology (implicit via Service selector) | ✅ Via target group binding | ✅ EndpointSlice reflects Service selector; only nodes with matching pods registered | **Supported** | — |
| Topology-aware routing | ✅ Service topology / topology hints | ✅ AZ-aware routing | ❌ No zone/topology awareness | **Not Supported** | Code gap + Infra gap — code doesn't read topology hints; also single-site BIG-IP has no zone concept |
| External traffic policy (Local) | ✅ Preserves client IP; skips non-local nodes | ✅ Preserves client IP; direct-to-node | ⚠️ EndpointSlice filtering routes only to local-pod nodes; source IP lost due to SNAT automap | **Partially Supported** | Infra gap — SNAT automap on BIG-IP prevents source IP preservation; needs F5 team to evaluate disabling SNAT |
| Weighted backends | ✅ Via traffic splitting (Ingress/Gateway) | ✅ Weighted target groups | ✅ Pod-proportional weights via EndpointSlice count (svc-lb-bridge only; seed-service-lb-controller uses equal-weight all-node backends) | **Supported** | — |

### 1.4 Health Checks & Monitoring

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| TCP health checks | ✅ Configurable port, interval, threshold | ✅ NLB TCP health checks | ✅ TCP only; interval configurable (monitorInterval) | **Supported** | — |
| HTTP health checks | ✅ Path, host, expected response code | ✅ HTTP/HTTPS health checks | ✅ Configurable via `f5.extensions.gardener.cloud/health-check-type` (tcp/http) and `/health-check-path` annotations; passes `monitor_type` and `monitor_path` to CMP | **Supported** | ~~Code gap~~ — **Fixed**: HTTP health check type and path annotations implemented (effectiveness depends on CMP API support) |
| gRPC health checks | ✅ Native | ✅ Via ALB | ❌ Not implemented | **Not Supported** | Infra gap — BIG-IP needs external monitor script for gRPC; CMP unlikely to expose |
| Custom health check path | ✅ Annotation-configurable | ✅ Annotation-configurable | ✅ Via `f5.extensions.gardener.cloud/health-check-path` annotation | **Supported** | ~~Code gap~~ — **Fixed**: health check path annotation implemented |
| Health check interval | ✅ Configurable per backend | ✅ Configurable | ✅ Global default 30s; overridable per-Service via `f5.extensions.gardener.cloud/health-check-interval` annotation | **Supported** | ~~Code gap~~ — **Fixed**: per-Service annotation override implemented |
| Unhealthy threshold | ✅ Configurable | ✅ Configurable | ❌ Not exposed | **Not Supported** | Code gap — confirm with CMP if threshold param exists |
| Connection draining | ✅ Configurable timeout | ✅ Deregistration delay | ✅ Via `f5.extensions.gardener.cloud/connection-draining-timeout` annotation; passes `connection_draining_timeout` to CMP | **Supported** | ~~Code gap~~ — **Fixed**: connection draining timeout annotation implemented (effectiveness depends on CMP API support) |

### 1.5 Session Affinity & Persistence

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| Client IP affinity | ✅ `service.spec.sessionAffinity: ClientIP` | ✅ `service.spec.sessionAffinity: ClientIP` | ✅ Reads `spec.sessionAffinity: ClientIP`; passes `persistence_type=source_addr` to CMP | **Supported** | ~~Code gap~~ — **Fixed**: session affinity parsed from spec; CMP `persistence_type` param set (effectiveness depends on CMP API support) |
| Cookie-based affinity | ✅ Via Ingress/Gateway BackendConfig | ✅ Via ALB sticky sessions | ❌ Not implemented | **Not Supported** | Infra gap — BIG-IP supports cookie persistence; CMP must expose persistence profile |
| Session timeout | ✅ Configurable | ✅ Configurable | ❌ Not implemented | **Not Supported** | Infra gap — depends on CMP exposing persistence timeout param |

### 1.6 TLS & Certificates

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| TLS termination at LB | ✅ Google-managed certs or custom | ✅ ACM certificates | ❌ Not implemented; svc-lb-bridge is TCP passthrough only | **Not Supported** | Code gap + CMP confirmation — BIG-IP does TLS termination; need CMP to accept cert/key params |
| SSL passthrough | ✅ Supported | ✅ NLB TLS passthrough | ⚠️ TCP passthrough inherently passes TLS through (no termination) | **Partially Supported** | — (works implicitly via TCP VS) |
| Mutual TLS (mTLS) | ✅ Supported | ✅ Supported | ❌ Not implemented | **Not Supported** | Infra gap — requires BIG-IP client-SSL profile with CA bundle; CMP unlikely to expose |
| Auto cert provisioning | ✅ Google-managed SSL certs | ✅ ACM auto-provisioning | ❌ Not implemented | **Not Supported** | Infra gap — no cert-manager/ACME integration; out of scope for LB controller |
| SNI-based routing | ✅ Multiple certs per LB | ✅ Multiple certs per ALB | ⚠️ Mechanism A uses Istio SNI routing (not the F5 extension itself) | **Not Supported** | Infra gap — would need iRule-based SNI routing on BIG-IP; CMP doesn't expose |

### 1.7 Routing & Traffic Management

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| Path-based routing | ✅ Ingress/Gateway URL maps | ✅ ALB path-based rules | ❌ Not implemented; L4 only | **Not Supported** | Infra gap — CMP VS has no path-rule configuration; needs iRule or LTM policy |
| Host-based routing | ✅ Ingress/Gateway host rules | ✅ ALB host-based rules | ❌ Not implemented; L4 only | **Not Supported** | Infra gap — CMP VS has no host-rule configuration; needs iRule or LTM policy |
| Header-based routing | ✅ Gateway API HTTPRoute | ✅ ALB header conditions | ❌ Not implemented | **Not Supported** | Infra gap — requires L7 policy engine; CMP doesn't expose |
| Traffic splitting / canary | ✅ Gateway API weight-based | ✅ ALB weighted target groups | ❌ Not implemented | **Not Supported** | Infra gap — needs multiple pools per VS with ratio; CMP doesn't expose |
| URL rewrite | ✅ Supported | ✅ Supported | ❌ Not implemented | **Not Supported** | Infra gap — requires iRule/LTM policy; CMP doesn't expose |
| Redirect | ✅ HTTP→HTTPS redirect | ✅ ALB redirect actions | ❌ Not implemented | **Not Supported** | Infra gap — requires iRule/LTM policy; CMP doesn't expose |
| Load balancing algorithm | ✅ Round-robin (default) + MAGLEV | ✅ Round-robin, least-outstanding | ✅ Configurable globally (`routingAlgorithm`) + per-Service via `f5.extensions.gardener.cloud/routing-algorithm` annotation | **Supported** | — |

### 1.8 Observability & Metrics

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| LB request metrics | ✅ Cloud Monitoring (latency, RPS, errors) | ✅ CloudWatch (connection count, bytes, errors) | ⚠️ CMP API call metrics only; no traffic metrics | **Partially Supported** | Infra gap — BIG-IP has traffic stats; CMP doesn't expose per-VS metrics via API |
| Access logs | ✅ Cloud Logging | ✅ S3 access logs / CloudWatch | ❌ Not implemented | **Not Supported** | Infra gap — BIG-IP has request logging; no CMP API to stream logs |
| K8s Events on Service | ✅ Events for provisioning/errors | ✅ Events for target registration | ✅ EventRecorder on all controllers (EnsuredLoadBalancer, DeletingLoadBalancer, DeleteFailed, AllocatedVIP, RateLimited, SyncLoadBalancerFailed) | **Supported** | ~~Code gap~~ — **Fixed**: event recording added to svc-lb-bridge, seed-service-lb-controller, ingress-lb, and extension controller |
| Prometheus metrics | ✅ Via kube-state-metrics + cloud exporter | ✅ Via CloudWatch exporter | ✅ 5 custom metrics: API call count, API call duration (histogram), reconcile errors, VIP allocations, managed service count (gauge) — all fully instrumented | **Supported** | ~~Code gap~~ — **Fixed**: all 5 metrics fully instrumented including API call duration histogram; traffic-level metrics blocked on CMP API |
| Health status visibility | ✅ Console + API | ✅ Console + API | ❌ No health status exposed back to K8s | **Not Supported** | Code gap + CMP confirmation — need CMP pool member health API to reflect in Service conditions |
| Distributed tracing | ✅ Cloud Trace integration | ✅ X-Ray integration | ❌ Not implemented | **Not Supported** | Infra gap — would need BIG-IP OpenTelemetry integration; out of scope |

### 1.9 Security & Access Control

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| Source IP filtering | ✅ `loadBalancerSourceRanges` | ✅ Security group rules | ✅ Reads `spec.loadBalancerSourceRanges` + annotation fallback (`f5.extensions.gardener.cloud/source-ranges`); passes `allowed_cidrs` to CMP | **Supported** | ~~Code gap~~ — **Fixed**: source range parsing implemented; CMP `allowed_cidrs` param set (effectiveness depends on CMP API support) |
| WAF integration | ✅ Cloud Armor policies | ✅ AWS WAF on ALB | ❌ Not implemented | **Not Supported** | Infra gap — BIG-IP ASM exists but CMP doesn't expose WAF policy attachment |
| DDoS protection | ✅ Cloud Armor + Adaptive Protection | ✅ AWS Shield | ❌ Not implemented (F5 BIG-IP has DDoS capabilities but not exposed) | **Not Supported** | Infra gap — BIG-IP AFM/DDoS exists but CMP doesn't expose; F5 team manages separately |
| Network Policy integration | ✅ Auto-injected firewall rules | ✅ Security group auto-config | ✅ Auto-generates NetworkPolicy per LoadBalancer Service (allows ingress on service ports to backing pods); cleaned up on Service deletion | **Supported** | ~~Code gap~~ — **Fixed**: svc-lb-bridge auto-creates/deletes NetworkPolicy per managed Service |
| Credential rotation | ✅ Auto-rotated via IAM/service accounts | ✅ Auto-rotated via IAM roles | ✅ Extension controller watches credentials Secret; changes trigger re-reconcile → svc-lb-bridge Deployment updated with fresh tokens → rolling restart | **Supported** | ~~Code gap~~ — **Fixed**: Secret watch added to extension controller; credential changes auto-propagate to svc-lb-bridge |

### 1.10 Configuration Model

| Feature | GKE | EKS | F5 Extension | F5 Status | Gap Reason |
|---------|-----|-----|--------------|-----------|------------|
| Annotation-based config | ✅ 50+ annotations for LB customization | ✅ 30+ annotations for NLB/ALB | ✅ 7 input annotations: `f5.extensions.gardener.cloud/protocol`, `/routing-algorithm`, `/health-check-interval`, `/health-check-type`, `/health-check-path`, `/source-ranges`, `/connection-draining-timeout` + `spec.sessionAffinity` and `spec.loadBalancerSourceRanges` | **Supported** | ~~Code gap~~ — **Fixed**: annotation model implemented; 7 annotations + 2 spec fields with per-Service overrides |
| CRD-based config (BackendConfig/Policy) | ✅ BackendConfig, FrontendConfig CRDs | ✅ TargetGroupBinding CRD | ⚠️ F5LoadBalancerConfig CRD (global, not per-Service) | **Partially Supported** | Code gap — CRD exists but is global; per-Service annotations now provide override layer |
| Per-Service customization | ✅ Full annotation model | ✅ Full annotation model | ✅ Annotations override protocol, routing algorithm, and health check interval per Service | **Partially Supported** | ~~Code gap~~ — **Fixed**: per-Service annotation overrides implemented in svc-lb-bridge and seed-service-lb-controller |
| Default values / inheritance | ✅ Project defaults + per-LB override | ✅ Account defaults + per-LB | ✅ Hardcoded defaults + per-Service annotation overrides | **Supported** | ~~Code gap~~ — **Fixed**: annotation values override defaults (round_robin, 30s interval, auto-detected protocol) |

---

## 2. Dependency Matrix

### 2.1 Kubernetes Components

| Dependency | GKE | EKS | F5 Extension | Notes |
|-----------|-----|-----|--------------|-------|
| Service (type=LB) | ✅ Primary trigger | ✅ Primary trigger | ✅ Primary trigger | All three use this as entry point |
| EndpointSlice | ✅ For NEG pod targeting | ✅ For IP-mode targets | ✅ svc-lb-bridge watches EndpointSlice for backend node selection + pod-proportional weights; seed-service-lb-controller uses Node list directly | Used for dynamic backend registration (app-plane only) |
| Ingress | ✅ GKE Ingress controller | ✅ ALB Ingress controller | ✅ svc-lb-bridge Ingress controller (class "f5") | Implemented |
| Gateway/HTTPRoute/TCPRoute | ✅ GKE Gateway controller | ✅ AWS Gateway controller | ❌ Not reconciled | Major gap |
| Custom CRDs | BackendConfig, FrontendConfig, HealthCheckPolicy | TargetGroupBinding, IngressClassParams | F5LoadBalancerConfig | F5 CRD is global, not per-Service |
| ConfigMap | ❌ Not used for LB | ❌ Not used for LB | ❌ Not used | — |

### 2.2 Controller Logic

| Component | GKE | EKS | F5 Extension |
|-----------|-----|-----|--------------|
| Service reconciler | ✅ Cloud controller manager | ✅ AWS Load Balancer Controller | ✅ seed-service-lb-controller + svc-lb-bridge |
| Node watcher | ✅ For instance group updates | ✅ For target registration | ✅ Re-enqueues on Node changes |
| Pod/EndpointSlice watcher | ✅ For NEG updates | ✅ For IP-mode targets | ✅ svc-lb-bridge: EndpointSlice watcher triggers re-reconcile on pod changes; seed-service-lb-controller: Node watcher only |
| Ingress reconciler | ✅ GKE Ingress controller | ✅ ALB controller | ✅ svc-lb-bridge Ingress controller (IngressClass "f5") |
| Gateway reconciler | ✅ GKE Gateway controller | ✅ AWS Gateway controller | ❌ Not implemented |
| Finalizer management | ✅ Prevents premature deletion | ✅ Prevents premature deletion | ✅ Three finalizers (extension, seed-lb, svc-lb-bridge) |
| Leader election | ✅ Built-in | ✅ Built-in | ✅ Built-in |

### 2.3 Cloud / Infrastructure APIs

| Dependency | GKE | EKS | F5 Extension |
|-----------|-----|-----|--------------|
| LB provisioning API | Compute Engine Forwarding Rules + Backend Services | ELBv2 API (NLB/ALB) | CMP LBaaS v2.1 REST API |
| Health check API | Compute Engine Health Checks | ELBv2 Target Group Health | CMP monitor interval parameter |
| DNS API | Cloud DNS (auto for Ingress) | Route 53 (via external-dns) | ❌ None |
| Certificate API | Certificate Manager / Google-managed certs | ACM (AWS Certificate Manager) | ❌ None |
| VPC/Network API | VPC, Subnets, Firewall Rules | VPC, Subnets, Security Groups | CMP vpc_id, network_id (passed through) |
| IAM/Auth | GCP Service Account (Workload Identity) | IRSA (IAM Roles for Service Accounts) | Ce-Auth token or Basic Auth (Secret-watched; auto-refreshed on change) |

### 2.4 Network & CNI

| Dependency | GKE | EKS | F5 Extension |
|-----------|-----|-----|--------------|
| CNI plugin | GKE dataplane v2 (Cilium) or kubenet | VPC CNI (aws-node) | No CNI requirement (NodePort only) |
| Pod IP reachability | ✅ Required for NEG/IP mode | ✅ Required for IP-mode targets | ❌ Not needed (NodePort) |
| kube-proxy | ⚠️ Optional with NEG | ⚠️ Optional with IP targets | ✅ Required (NodePort depends on kube-proxy) |
| NodePort range | Used as fallback | Used as fallback | **Required** — primary backend path |
| Network plugin | ✅ Must allow cloud LB → Pod traffic | ✅ Must allow NLB → Pod traffic | ⚠️ Must allow F5 VIP → Node:NodePort |

### 2.5 External Hardware / Appliance

| Dependency | GKE | EKS | F5 Extension |
|-----------|-----|-----|--------------|
| Physical/Virtual LB | Cloud-native (no hardware) | Cloud-native (no hardware) | **F5 BIG-IP** (managed by CMP) |
| Appliance management | ❌ Fully managed | ❌ Fully managed | CMP manages BIG-IP; extension manages CMP |
| Partition management | ❌ N/A | ❌ N/A | Per-tenant BIG-IP partition via CMP |
| VIP subnet management | Cloud manages subnet allocation | Cloud manages ENI/subnet | CMP allocates from configured network |
| Firmware/version deps | ❌ None | ❌ None | BIG-IP firmware must support CMP LBaaS features |

---

## 3. Architecture Observations

### 3.1 Strengths of Current F5 Extension

1. **Clean Gardener integration** — properly implements Extension CR contract, lifecycle operations (Migrate/Restore), finalizers, and condition reporting.
2. **Two-mechanism flexibility** — Mechanism A (shared VIP via Istio) requires zero CMP work per Shoot; Mechanism B (dedicated VIP) provides isolation.
3. **Rate limiting** — Client-side rate limiter prevents CMP overload; 429 handling with Retry-After.
4. **Idempotent provisioning** — "ensure-or-find" pattern prevents duplicate resources on retries.
5. **Migration support** — CMP resource IDs preserved during Shoot migration across Seeds.
6. **Metrics** — 5 fully instrumented Prometheus metrics: API call count, API call duration (histogram), reconcile errors, VIP allocations, and managed service count (gauge with proper Inc/Dec lifecycle tracking).
7. **K8s Event recording** — all controllers emit standard Kubernetes Events on Service, Ingress, and Extension objects for provisioning, VIP allocation, deletion, rate limiting, and errors (matches GKE/EKS event patterns).

### 3.2 Architectural Gaps

1. **No Gateway API controller** — CMP supports HTTP/HTTPS Virtual Servers and the extension now has an Ingress controller (IngressClass "f5"), but there is no Gateway API controller for HTTPRoute/TCPRoute resources.
2. ~~**No annotation model**~~ — **Fixed**: Three per-Service annotations defined (`f5.extensions.gardener.cloud/protocol`, `/routing-algorithm`, `/health-check-interval`) with defaults applied.
3. ~~**Port-based protocol detection**~~ — **Fixed**: Protocol annotation (`f5.extensions.gardener.cloud/protocol`) overrides auto-detection heuristic.
4. ~~**Static credentials**~~ — **Fixed**: Extension controller now watches the credentials Secret. Changes trigger re-reconcile, which re-reads credentials and updates the svc-lb-bridge Deployment (rolling restart with fresh tokens). Seed-service-lb-controller still uses static env vars (deployed externally).
5. **No DNS integration** — VIP allocation doesn't trigger DNS record creation. External-dns or manual configuration required.
6. ~~**No event recording**~~ — **Fixed**: All controllers now emit K8s Events on Service/Ingress/Extension objects.

### 3.3 CMP API Capabilities vs Code Usage

The F5 extension **underutilizes** the CMP LBaaS v2.1 API. The code hardcodes TCP, but CMP supports more:

| CMP Capability | Available? | Used by Extension? | Impact |
|---------------|-----------|-------------------|--------|
| TCP Virtual Server | ✅ Yes | ✅ Yes | Core functionality works |
| HTTP Virtual Server (L7) | ✅ Yes | ✅ Yes | Protocol auto-detected (port 80/8080→HTTP, 443/8443→HTTPS) |
| UDP Virtual Server | ✅ Yes | ✅ Yes | Supported via K8s Service protocol=UDP |
| Health check types (HTTP, custom) | ❗ Unknown | ✅ Yes (monitor_type/monitor_path params sent) | Annotations defined; CMP support needs verification |
| Session persistence | ❗ Unknown | ✅ Yes (persistence_type=source_addr sent for ClientIP) | spec.sessionAffinity read; CMP support needs verification |
| TLS offload | ❓ Unknown | ❌ No | Needs CMP confirmation |
| Weighted pools | ✅ Yes | ✅ Yes | Pod-proportional weights via EndpointSlice count |
| IPv6 VIP | ❌ No | ❌ No | Confirmed not supported by F5 team |
| Quota/capacity API | ❓ Unknown | ❌ No | Needs verification |

**Key Insight:** The extension now supports all four CMP protocols (TCP, UDP, HTTP, HTTPS), multi-port Services, EndpointSlice-based dynamic backend selection with pod-proportional weights, an Ingress controller, and a comprehensive annotation model (7 annotations + 2 spec fields) for per-Service customization of protocol, routing algorithm, health checks (type, path, interval), session affinity, source IP filtering, and connection draining. The remaining gaps are primarily L7 routing (Gateway API), TLS termination, and CMP API limitations for observability — most requiring CMP API enhancements.

### 3.4 CMP API Limitations (Infra Gaps)

The following are **CMP LBaaS v2.1 API limitations** — features that hyperscalers provide natively but CMP does not expose, regardless of extension code:

| # | CMP API Gap | Impact on Extension | Hyperscaler Equivalent |
|---|------------|--------------------|-----------------------|
| 1 | **No individual pool member CRUD** — Cannot add/remove/update a single member on an existing pool; must replace the full member list | Forces full pool member replacement on every backend change; increases API payload size and latency for large pools | GKE NEG: per-endpoint add/detach; EKS: per-target register/deregister |
| 2 | **No async task tracking APIs** — LB provisioning is synchronous; no task ID or status polling endpoint | Extension cannot track long-running operations; must rely on retries and timeouts to confirm completion | GKE: Operation resource with polling; EKS: waiter APIs for LB active state |
| 3 | **No health monitor standalone CRUD** — Monitors are embedded in pool creation; cannot update monitor independently | Changing health check config requires pool recreation or full update; no per-Service health customization possible | GKE: independent HealthCheck resource; EKS: independent target group health check config |
| 4 | **No LB observability APIs** — No endpoint for traffic metrics (RPS, latency, throughput, error rates) | Extension cannot surface real-time traffic data; Prometheus metrics are limited to controller-side API call stats | GKE: Cloud Monitoring per-LB metrics; EKS: CloudWatch per-LB metrics |
| 5 | **No metering / quota APIs** — No endpoint to query resource consumption, connection counts, or enforce quotas | Cannot implement usage-based billing, capacity planning, or resource limit enforcement | GKE: Quota API + billing export; EKS: Service Quotas + CloudWatch usage metrics |
| 6 | **No resource quota check API** — Cannot pre-validate whether a tenant has capacity before provisioning | Provisioning may fail at CMP level with no graceful pre-flight check; users see errors only after the fact | GKE: project quota check; EKS: service quota limit check |
| 7 | **No LB metrics API (RPS, latency, throughput)** — BIG-IP collects these internally but CMP does not expose them | Cannot fulfil FRD OBS-08 (networking metrics) or provide SLA dashboards | GKE: per-backend latency/RPS in Cloud Monitoring; EKS: per-target connection/byte metrics |
| 8 | **No LB logs endpoint** — No access log or request log streaming from CMP/BIG-IP | Cannot fulfil FRD OBS-11 (ingress/egress traffic logs); no audit trail for requests | GKE: Cloud Logging; EKS: S3 access logs / CloudWatch Logs |

> **Action Required:** These gaps must be raised with the CMP / F5 platform team as API enhancement requests. They cannot be solved by extension code alone.

---

## 4. FRD Requirements vs. Implementation Status

Source: `Kubernetes_FRD_new.xlsx`. Of 105 total FRD requirements across 9 sheets, **9 are directly owned or materially influenced by the F5 Gardener Extension**. All others belong to Gardener core, cloud platform, or other extensions. GitHub issue numbers reference open tracking issues.

| FRD ID | Requirement | Status | Gap / Note | Issue |
|--------|-------------|--------|------------|-------|
| SR-17 | Managed Ingress controller — watch Kind:Ingress, provision L7 LB, update Ingress status | ✅ Implemented | Core Ingress reconciliation works, but the extension only writes back the allocated VIP. It does not create or manage DNS names, and backend registration is still NodePort-based rather than direct pod-IP mode. | #1 |
| INT-01 | Auto-provision LBs when user creates Service type=LoadBalancer | ✅ Implemented | Service reconciliation, VIP lifecycle, and per-Service annotation-driven configuration are working. Annotations support protocol, routing algorithm, health checks (type/path/interval), session affinity, source IP filtering, and connection draining. | #3 |
| INT-05 | Native TLS/SSL cert integration via annotations on HTTPS LB listener | ⚠️ Partial | HTTPS virtual servers can be created, but certificate handling is not Kubernetes-native yet. Users cannot reference a TLS Secret and have the controller sync and attach that cert/key to BIG-IP automatically. | #4 |
| INT-06 | Integration with monitoring/metering tools | ⚠️ Partial | The extension exposes a small Prometheus metrics set for controller/API activity, but not the service-level traffic and billing data usually needed by external monitoring or chargeback systems. | — |
| PERF-05 | LB for services with auto traffic distribution across pods | ✅ Implemented | Traffic distribution is implemented through EndpointSlice-driven backend updates and pod-proportional weights. This covers the core requirement, though it still depends on NodePort forwarding rather than pod-direct load balancing. | — |
| OBS-08 | Networking metrics — packet loss, DNS latency, egress/ingress bandwidth | ⚠️ Partial | Some controller-side metrics exist, but network traffic telemetry is missing. The current blocker is that CMP does not appear to expose BIG-IP bandwidth, packet loss, or latency statistics needed to satisfy this FRD fully. | #5 |
| OBS-11 | Ingress/Egress traffic logs from LB | ❌ Not implemented | The extension does not currently surface request or flow logs from BIG-IP back into Kubernetes or a central logging system. This looks blocked on CMP support for exposing or exporting LB access logs. | #6 |
| MTR-05 | LB metering — number of rules, new connections/second | ⚠️ Partial | The repo can infer static counts such as managed VIPs or rules, but dynamic usage signals like new connections per second are not available from the current CMP integration. That leaves this only partially covered for billing use cases. | #7 |
| MTR-10 | Managed add-ons metering (ingress controller, service mesh) | ❌ Not applicable | The extension does deploy and manage `svc-lb-bridge`, but platform-level add-on inventory and billing aggregation are outside this controller's responsibility. This requirement is better owned by a central metering layer. | #8 |

**Key gaps to close:**
- **DNS** — VIP→FQDN mapping needed for SR-17 full compliance (external-dns or Gardener DNS provider)
- **CMP API** — traffic stats, request logs, and certificate attachment must be confirmed with F5 team
- **~~Annotation model~~** — ~~per-Service overrides for health checks, routing algorithm, and persistence (INT-01)~~ **Fixed**
- **Cert sync** — K8s Secret→BIG-IP sync needed for INT-05 full compliance


