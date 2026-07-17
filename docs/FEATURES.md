# Gardener Extension for F5 BIG-IP — Features

## Overview

This Gardener Extension integrates **F5 BIG-IP load balancing** (via a CMP/CCP LBaaS API layer) into a Gardener-managed Kubernetes platform targeting OpenStack environments. It fills the role that cloud-native load balancer controllers play automatically on AWS/GCP/Azure, where OpenStack has no built-in `Service type=LoadBalancer` controller.

Load balancing is provided at three distinct tiers:

| Tier | Purpose | Default |
|---|---|---|
| **Seed Ingress LB** | Exposes the Seed's Istio ingress gateway with a single shared VIP for all Shoot kube-apiservers | Enabled |
| **Per-Shoot Control-Plane LB** | Dedicated VIP per Shoot kube-apiserver, bypassing Istio | Disabled (toggle) |
| **Application-Plane LB** | Tenant `Service type=LoadBalancer` inside Shoot clusters | Disabled (toggle) |

---

## Components

All four binaries are compiled into a single multi-stage Docker image (`distroless/static:nonroot`), with `/gardener-extension-f5` as the default `ENTRYPOINT`.

### `gardener-extension-f5` — Primary Extension Controller (Seed-side)

- Registers as a Gardener extension of type **`f5-loadbalancer`** (primary) and legacy **`f5`**.
- Runs in the Seed cluster inside the Gardener-managed namespace.
- Watches `Extension` objects (`extensions.gardener.cloud/v1alpha1`) in each Shoot's technical namespace (`shoot--<project>--<name>`).
- Reconcile loop:
  1. Creates/syncs `F5LoadBalancerConfig` CRD instance from `Extension.spec.providerConfig`.
  2. Provisions (or skips) the control-plane VIP/VS via the CMP LBaaS API.
  3. Deploys `svc-lb-bridge` into the Shoot cluster when app-plane LB is enabled.
  4. Writes status back (`ControlPlaneLoadBalancerReady`, `ApplicationLoadBalancerReady`) and Extension `providerStatus` (VIP, port, BIG-IP management IP).
- Handles the **Gardener handshake**: clears the `gardener.cloud/operation` annotation after each reconcile.
- On deletion: best-effort cleanup of CMP resources (LBService, VIP, VirtualServer) and removal of `svc-lb-bridge` from the Shoot.
- Supports `Reconcile`, `Delete`, `Migrate`, and `Restore` lifecycle operations.
- Resource footprint: 100m CPU / 128Mi request, 256Mi memory limit, 1 replica.

### `svc-lb-bridge` — Application-Plane LB Controller (Shoot-side)

- Deployed **into the Shoot cluster** by the extension controller once the control-plane LB is ready.
- Watches `Service type=LoadBalancer` and `Node` objects in the Shoot.
- Filters by `spec.loadBalancerClass: f5.extensions.gardener.cloud/bigip` (configurable via `F5_SVC_LB_LOADBALANCER_CLASS`).
- For each eligible Service: calls CMP LBaaS API to provision `LBService → VIP → VirtualServer` with backends = worker Node Internal IPs + Service NodePort.
- Mirrors the allocated VIP into `Service.status.loadBalancer.ingress[].ip` so `kubectl get svc` shows `EXTERNAL-IP`.
- On Node churn (add/remove): re-enqueues all managed Services to keep pool members in sync.
- On Service deletion or type change: cleans up CMP resources.
- Tracks CMP resource IDs via Service annotations:
  - `f5.extensions.gardener.cloud/lb-service-id`
  - `f5.extensions.gardener.cloud/vip-port-id`
  - `f5.extensions.gardener.cloud/virtual-server-id`
  - `f5.extensions.gardener.cloud/vip-address`
  - `f5.extensions.gardener.cloud/backend-hash` (detects pool-member drift)
- Uses finalizer `f5.extensions.gardener.cloud/svc-lb-bridge` for safe cleanup.

### `seed-service-lb-controller` — Seed Ingress LB Controller (Seed-side)

- Runs on the Seed cluster itself (deployed separately alongside Seed setup).
- Watches `Service type=LoadBalancer` on the Seed with `loadBalancerClass: f5.extensions.gardener.cloud/bigip` (e.g. the Istio `ingressgateway` Service).
- Discovers Seed node Internal IPs (`Node.status.addresses[InternalIP]`) and the Istio NodePort.
- Calls CMP LBaaS API to create `LBService → VIP → VirtualServer`.
- Writes the allocated VIP into `Service.status.loadBalancer.ingress[0].ip`.
- gardenlet picks up this IP, populates the Seed object's ingress, and all Shoots derive their kube-apiserver DNS from it — the OpenStack equivalent of what AWS ELB does automatically.
- Configured entirely via environment variables (`CMP_ENDPOINT`, `CMP_CE_AUTH`, `CMP_ORGANISATION_NAME`, `CMP_PROJECT_ID`, `CMP_FLAVOR_ID`, `CMP_NETWORK_ID`, `CMP_VPC_ID`, `CMP_VPC_NAME`).
- Uses finalizer `f5.extensions.gardener.cloud/seed-service-lb`.

### `cmpctl` — Operator CLI

A self-contained CLI for operators to interact with the CMP LBaaS API directly.

| Subcommand | Purpose |
|---|---|
| `ce-auth` | Generate a Ce-Auth HMAC token from API ID + secret |
| `lb-list` / `lb-create` | List/create load balancers (older CMP API) |
| `lbsvc-create` / `lbsvc-list` / `lbsvc-get` | Create/list/get LBServices (CMP v2.1) |
| `lbsvc-vip` / `vip-create` | List VIPs or create a VIP under an LBService |
| `vs-list` / `vs-create` | List or create Virtual Servers |
| `health` | Check CMP endpoint health |
| `request` | Generic raw HTTP request (GET/POST/etc.) to any CMP path |

---

## CRD: `F5LoadBalancerConfig` (`f5.extensions.gardener.cloud/v1alpha1`)

Short name: `f5lbc`. Namespaced resource — one instance per Shoot, created in the Shoot's technical namespace.

### Spec Fields

| Field | Type | Purpose |
|---|---|---|
| `ccpApiEndpoint` | string | CMP/CCP base URL for control-plane LB provisioning |
| `tenantOrPartition` | string | CMP tenant or BIG-IP partition |
| `credentialsSecretRef` | SecretReference | Points to Secret with CMP credentials |
| `controlPlaneVIP` | string | Pre-assigned or desired VIP for Shoot kube-apiserver |
| `controlPlaneReady` | \*bool | Out-of-band override: skip CMP calls and mark CP ready immediately |
| `enablePerShootControlPlaneVIP` | bool | Toggle Mechanism B (dedicated per-Shoot CP VIP via CMP) |
| `enableApplicationLB` | bool | Toggle deployment of `svc-lb-bridge` into the Shoot |
| `flavorId` | int32 | CMP LB flavor ID |
| `networkId` | string | CMP network/subnet ID |
| `vpcId` / `vpcName` | string | CMP VPC identifiers |
| `routingAlgorithm` | string | Load-balancing algorithm (default: `round_robin`) |
| `monitorInterval` | int32 | Health-check interval in seconds (default: 30) |

### Status Fields

| Field | Purpose |
|---|---|
| `vip` | Actual VIP configured on CMP/F5 |
| `virtualServerName` | VS name/ID on CMP/F5 |
| `lbServiceId` | CMP LBService ID |
| `vipPortId` | CMP VIP port ID |
| `virtualServerId` | CMP Virtual Server ID |
| `conditions[]` | `ControlPlaneLoadBalancerReady`, `ApplicationLoadBalancerReady` with reason/message |

Extension `providerStatus` (written back to the `Extension` object):

| Field | Purpose |
|---|---|
| `controlPlaneVip` | The active VIP address |
| `controlPlanePort` | Auto-detected port (prefers 443, then 6443) |

---

## Control-Plane Load Balancing

### Mechanism A — Shared Seed Ingress VIP (default)

When `enablePerShootControlPlaneVIP: false`:

- **No per-Shoot VIP is allocated.**
- `seed-service-lb-controller` provisions a single VIP for the Seed's Istio ingress gateway Service.
- All Shoot kube-apiservers share this VIP; Istio routes TLS by SNI hostname (`api.<shoot>.<project>.<seed>.gardener.cloud`).
- Extension marks `ControlPlaneLoadBalancerReady=True` immediately (reason: `SharedSeedIngressVIP`).

### Mechanism B — Per-Shoot Dedicated Control-Plane VIP

When `enablePerShootControlPlaneVIP: true`:

- Extension calls CMP LBaaS API per Shoot: `POST /lb_service → GET /vip → POST /virtual-servers`.
- Backends = Seed node IPs + kube-apiserver `NodePort` (discovered via the Shoot's Service).
- VIP/VS IDs stored in `F5LoadBalancerConfig.status`.
- **Out-of-band shortcut:** set `controlPlaneReady: true` + `controlPlaneVIP: <ip>` to skip CMP calls (useful for kind/demo environments).
- Enables per-Shoot F5 policies, dedicated health monitors, rate limiting, WAF, and tenant SLA separation.
- On Shoot deletion: best-effort CMP cleanup (non-blocking if CMP is unreachable).

---

## Application-Plane Load Balancing

Gated by `ControlPlaneLoadBalancerReady=True` **and** `enableApplicationLB: true`.

### `svc-lb-bridge` (CMP LBaaS)

- `svc-lb-bridge` deployed as a `Deployment` in namespace `f5-svc-lb-bridge` in the Shoot cluster.
- Uses CMP LBaaS API to provision load balancers — no direct BIG-IP access required (CMP abstracts BIG-IP).
- `Service type=LoadBalancer` with `loadBalancerClass: f5.extensions.gardener.cloud/bigip` triggers provisioning.
- Pool members derived from Node Internal IPs + NodePort (NodePort mode, not pod-direct).
- VIP can be pinned via `spec.loadBalancerIP`.

---

## Kubernetes Objects Created in Shoot

When `enableApplicationLB: true`, the extension creates the following objects in the Shoot cluster:

| Object | Name | Namespace |
|---|---|---|
| `Namespace` | `f5-svc-lb-bridge` | — |
| `ServiceAccount` | `f5-svc-lb-bridge` | `f5-svc-lb-bridge` |
| `ClusterRole` | `f5-svc-lb-bridge` | — |
| `ClusterRoleBinding` | `f5-svc-lb-bridge` | — |
| `Secret` | `f5-svc-lb-credentials` | `f5-svc-lb-bridge` |
| `Deployment` | `f5-svc-lb-bridge` | `f5-svc-lb-bridge` |

All objects are removed on `enableApplicationLB: false` or Shoot deletion.

---

## F5 / CMP Client

The `pkg/f5` package provides a typed Go client with the following capabilities.

### Authentication Methods

| Method | Headers Sent | When Used |
|---|---|---|
| **Ce-Auth (HMAC token)** | `Ce-Auth: <token>`, `organisation-name`, `project-id` | Production CMP |
| **Env-var fallback** | Reads `F5_USERNAME` / `F5_PASSWORD` from environment | Local development |

Ce-Auth token format: `HMAC-SHA256(apiID.expiryTimestamp, secret)` hex-encoded → `apiID.expiry.signature`.

### CMP LBaaS Operations (v2.1 API)

| Level | Operations |
|---|---|
| LB (older API) | List, Create, Get, Update, Delete, Options |
| LBService | List, Create, Get, Delete |
| VIP | Create (auto-allocate), GetAll, Delete |
| Virtual Server | List, Create, Get, Delete |
| Flavors | List |
| High-level | `EnsureControlPlaneVirtualServer` (idempotent upsert), `DeleteControlPlaneVirtualServer` |
| Connectivity | `Probe` (validates reachability + auth without mutations) |

### HTTP Client Behaviour

- 15-second request timeout.
- TLS verification skippable via `F5_INSECURE_SKIP_TLS_VERIFY=true`.
- Redirect-following stopped at the first redirect (prevents resolution of internal CMP hostnames).
- Configurable API path prefix via `F5_HTTP_API_PREFIX`.
- VS ensure/delete path templates via `F5_HTTP_CP_VS_ENSURE_PATH_TEMPLATE` / `F5_HTTP_CP_VS_DELETE_PATH_TEMPLATE`.
- Probe path and method configurable via `F5_HTTP_PROBE_PATH` / `F5_HTTP_PROBE_METHOD`.

---

## kube-apiserver Backend Discovery

The extension automatically discovers kube-apiserver backends for pool-member configuration:

- Looks up the `kube-apiserver` `Service` in the Shoot's technical namespace on the Seed.
- Reads `Node.status.addresses[InternalIP]` for each Seed node.
- Port selection: prefers 443 → 6443 → first available NodePort.
- Used to populate the Virtual Server pool when creating a per-Shoot control-plane VIP.

---

## Architecture Overview

```
Garden Cluster
  ├── ControllerRegistration: gardener-extension-f5
  ├── ControllerDeployment:   gardener-extension-f5 (Helm chart)
  └── ControllerInstallation: → targets Seed

Seed Cluster (namespace: extension-gardener-extension-f5)
  ├── gardener-extension-f5 pod        (1 replica)
  ├── seed-service-lb-controller pod   (separate Deployment)
  │
  └── Per-Shoot namespace: shoot--<project>--<name>
        ├── Extension/f5-loadbalancer
        ├── F5LoadBalancerConfig/<name>  (spec + status)
        └── Secret:  <credentials>

Shoot Cluster (namespace: f5-svc-lb-bridge)
  └── Deployment: f5-svc-lb-bridge  (CMP LBaaS bridge)
```

Network policies provisioned by the Helm chart allow:
- Extension pod → DNS, Garden/Seed API server, private/public networks, Shoot kube-apiserver port 443.
- `svc-lb-bridge` → CMP LBaaS API endpoint (HTTPS).

---

## Configuration Reference

### Helm Values (Extension Controller Pod)

| Environment Variable | Purpose |
|---|---|
| `F5_INSECURE_SKIP_TLS_VERIFY` | Skip TLS certificate verification |
| `F5_HTTP_API_PREFIX` | Prefix prepended to all CMP API paths |
| `F5_HTTP_PROBE_PATH` | Health probe endpoint path (default: `/`) |
| `F5_HTTP_PROBE_METHOD` | Health probe HTTP method (default: `GET`) |
| `F5_HTTP_CP_VS_ENSURE_PATH_TEMPLATE` | Path for idempotent VS upsert |
| `F5_HTTP_CP_VS_DELETE_PATH_TEMPLATE` | Path for VS deletion |
| `F5_HTTP_CP_VS_ENSURE_METHOD` | HTTP method for VS upsert |

### `svc-lb-bridge` / `seed-service-lb-controller`

| Environment Variable | Purpose |
|---|---|
| `CMP_ENDPOINT` | CMP LBaaS API base URL |
| `CMP_CE_AUTH` | Ce-Auth token (or API ID:secret for token generation) |
| `CMP_ORGANISATION_NAME` | CMP organisation name |
| `CMP_PROJECT_ID` | CMP project ID |
| `CMP_FLAVOR_ID` | LB flavor ID |
| `CMP_NETWORK_ID` | Network/subnet ID for VIP allocation |
| `CMP_VPC_ID` / `CMP_VPC_NAME` | VPC identifiers |
| `F5_SVC_LB_LOADBALANCER_CLASS` | `loadBalancerClass` value to watch (default: `f5.extensions.gardener.cloud/bigip`) |
