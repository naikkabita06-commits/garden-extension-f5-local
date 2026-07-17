Gardener F5 Extension – Production Requirements Summary

## Core Features Supported

- Provisions and manages a **Seed Ingress VIP** for the Istio ingress gateway, enabling all Shoot kube-apiservers to share a single stable entry point (Mechanism A).
- Optionally provisions a **dedicated per-Shoot VIP** for kube-apiserver access, bypassing Istio (Mechanism B).
- Deploys `svc-lb-bridge` into each Shoot cluster to fulfil **tenant `Service type=LoadBalancer`** requests via CMP LBaaS.
- Uses Kubernetes Secrets for secure CMP credential handling (Ce-Auth HMAC token, project-id, organisation-name).
- Continuously reconciles VIP and Virtual Server state, keeping BIG-IP pool members in sync with Seed/Shoot node changes.
- Reports health and CMP resource IDs through `F5LoadBalancerConfig.status` and Extension `providerStatus`.
- Enables control-plane reachability by ensuring every Shoot kube-apiserver has a live VIP before gardenlet proceeds.

---

## Production Expectations from the Extension

- Reliable Seed Ingress VIP provisioning so all Shoot kube-apiservers are reachable from day one.
- Valid per-Shoot CP VIP creation for every Shoot using Mechanism B.
- Tenant `Service type=LoadBalancer` fulfilled automatically within the Shoot without tenant interaction.
- Secure credential management with least-privilege CMP access.
- Idempotent reconciliation — repeated reconciles never re-provision an already-live VIP or corrupt status.
- Finalizer-held cleanup — CMP resources are never abandoned; the finalizer is held until CMP confirms deletion.
- Consistent status reporting for operational visibility.
- Recovery readiness — if a controller restarts, all VIPs and VSes are re-confirmed without disruption.

---

## Observability Requirements

`F5LoadBalancerConfig.status` must show:

- `vip` — the active VIP address
- `virtualServerId` / `lbServiceId` / `vipPortId` — CMP resource IDs for traceability and remediation
- `conditions[ControlPlaneLoadBalancerReady]` — readiness, reason, message
- `conditions[ApplicationLoadBalancerReady]` — bridge deployment status, reason, message

Status values must be integrated into:

- Shoot control-plane readiness gates (gate on ControlPlaneLoadBalancerReady=True)
- Prometheus alerts (fire when conditions flip False)
- Cluster health dashboards (LB Healthy / Degraded / Failed)

---

## CMP & Infrastructure Production Requirements (from Network / CMP / Platform Teams)

| Requirement | Target | Owner |
|---|---|---|
| CMP API availability SLA | ≥ 99.95% | CMP team |
| MTTR for provisioning failures | < 30 minutes | CMP team |
| High availability | No single CMP node failure breaks provisioning | CMP team |
| BIG-IP → Shoot worker node L3 reachability | All NodePorts for LoadBalancer Services and control plane | Network team |
| CMP accessible from Seed cluster | Egress HTTPS allowed | Firewall team |
| CMP accessible from all Shoot clusters | Egress HTTPS allowed from each Shoot | Firewall team |
| TLS certificate validity | Valid cert or CA bundle configured | Platform / CMP team |
| VIP and VS quota | Sized for all Shoots + app-plane Services | Platform Architecture |
| CMP tenancy strategy | Per-Shoot or shared project; `vpcId`/`networkId`/`flavorId` documented | Platform Architecture |
| CMP operational APIs | List/delete orphaned VIPs and VSes; inspect VS pool health | CMP team |
| CMP metrics and logs | Request latency, 5xx rate, quota rejections, request IDs | CMP team |
| Ce-Auth credential rotation | Rotation procedure with zero downtime | Platform / Ops team |

---

## External Dependency on CMP Team

The following CMP v2.1 LBaaS API gaps are blockers or significant risks. CMP team engagement is required to address these before production go-live.

| # | Gap | Impact | Severity |
|---|---|---|---|
| A1 | No per-member pool operations (add/remove single member) | Every Node scale requires full VS delete-recreate, causing a traffic outage | Critical / Blocker |
| A2 | No compute lookup by Node IP (`GET /computes?ip=`) | Pool members cannot be registered in a dynamic cluster without manual IP-to-compute-ID mapping | Critical / Blocker |
| A3 | No BIG-IP partition support on LB/VS creation | Multi-Shoot naming collisions; no tenant isolation | Critical |
| A4 | Async status enums (`status`, `operating_status`) undocumented | Reconciliation cannot reliably detect success vs. in-progress vs. failure | Significant |
| A5 | No labels/tags on Virtual Server | Orphaned resources after partial failures are hard to identify; idempotency is fragile | Significant |
| A6 | Multi-port VIP behaviour undefined | Multi-port Services may behave unpredictably | Significant |
| C-3 | No VS update API — delete-recreate required (caused by A1) | Brief traffic drop on every Node scale event | Blocked until A1 resolved |

---

## Production Readiness Criteria

- [ ] Seed Ingress VIP is provisioned and stable
- [ ] First Shoot provisioned end-to-end with reachable kube-apiserver via VIP
- [ ] `ControlPlaneLoadBalancerReady=True` observable in status
- [ ] Tenant LoadBalancer Services receive VIPs (app-plane LB validated)
- [ ] Node scale event keeps pool members in sync without breaking VIP
- [ ] Shoot deletion cleans up all CMP resources (no orphaned VIPs)
- [ ] CMP accessible from Seed and all Shoot networks (I-2, I-3 — Network / Firewall team)
- [ ] BIG-IP → Node network validated (I-1 — Network team)
- [ ] CMP tenancy strategy documented and credentials provisioned (I-4, I-5 — Platform Architecture / Ops team)
- [ ] CMP per-member pool operations API available (A1 — CMP team)
- [ ] CMP compute lookup by Node IP API available (A2 — CMP team)
- [ ] CMP BIG-IP partition support confirmed (A3 — CMP team)
- [ ] CMP async status enums documented (A4 — CMP team)
- [ ] CMP VS update API available (C-3 — CMP team, follows A1)
- [ ] Production design signed off (I-6 — Product team)
- [ ] Node scale drill and Shoot migration drill succeed
