# Service type=LoadBalancer – Demo Runbook

## How svc-lb-bridge calls CMP APIs (exact trace)

For every reconcile of a `type: LoadBalancer` Service the bridge makes these CMP calls:

```
Step  CMP API                                              Source of parameters
────  ──────────────────────────────────────────────────── ────────────────────────────────────────────────
1.    GET  /lb_service/                                   check if LBService already exists by stored ID

2.    POST /lb_service/                                   (only if ID not found in annotation)
        name        = "app-<ns>-<svc>" or                ← generated from Service name + vip-group annotation
                      "app-group-<ns>-<vipgroup>"
        description = "App LB for <ns>/<svc>"            ← auto
        vpc_id      = $CMP_VPC_ID                        ← spec.vpcId in F5LoadBalancerConfig
        flavor_id   = spec.flavorId                      ← optional, from Extension providerConfig
        network_id  = spec.networkId                     ← optional, from Extension providerConfig

3.    GET  /lb_service/{lbID}/vip                        check existing VIPs on the LBService

4.    POST /lb_service/{lbID}/vip                        (only if no VIP found, or no shared parent VIP)
        (no body — CMP auto-allocates an IP from its pool)

5.    GET  /{lbID}/virtual-servers                       check if this Service's named VS already exists

6.    GET  /networks/ports/search-by-ip/?fixed_ip=X      FOR EACH ready worker node IP
        → returns: resource_id, resource_type, backend_port_id, IP
        ⚠ REQUIRES worker nodes to be registered CMP compute instances

7.    POST /{lbID}/virtual-servers?                      create the VS listener
        name              = "app-vs-<ns>-<svc>-<port>"  ← auto
        vip_port_id       = <id from step 4>             ← auto
        protocol          = TCP / HTTP / HTTPS           ← derived from spec.ports[*].port number
        port              = 8080                         ← spec.ports[*].port in your Service YAML
        routing_algorithm = round_robin                  ← default or annotation
        interval          = 30                           ← default or annotation
        vpc_id            = $CMP_VPC_ID                 ← spec.vpcId in Extension providerConfig
        nodes[]           = [{resource_id, resource_type,← from step 6
                               resource_ip, backend_port_id,
                               port (NodePort), weight}]
```

## Architecture

```
svc-lb-bridge (running in Shoot namespace f5-cis-system)
  │
  ▼ watches Services of type=LoadBalancer with loadBalancerClass=f5.../bigip
  │
  ▼ calls CMP LBaaS API
  │
  ├─ LBService  (one per Service or shared via vip-group)
  │    └─ VIP  (the allocated IP address; one per LBService)
  │         └─ VirtualServer  (listener: protocol + frontend port → backend pool)
  │              └─ Pool Members  (each = Shoot worker node at NodePort)
  │                 → resolved via SearchNetworkPortsByIP(nodeInternalIP)
```

## Prerequisites — verify ALL before applying

| # | Check | Command |
|---|---|---|
| 1 | Bridge running | `kubectl -n f5-cis-system get pods` |
| 2 | CMP endpoint reachable from bridge | `kubectl -n f5-cis-system logs deploy/f5-svc-lb-bridge \| head` |
| 3 | `CMP_VPC_ID` is set | `kubectl -n f5-cis-system get deploy f5-svc-lb-bridge -o jsonpath='{.spec.template.spec.containers[0].env}'` |
| 4 | Worker nodes are CMP compute VMs | Verify at least one node IP via `GET /networks/ports/search-by-ip/?fixed_ip=<nodeIP>` returns a port |

**Fixing missing `CMP_VPC_ID`** (if blank in step 3):
```bash
kubectl -n shoot--local--local edit extension f5
# Under spec.providerConfig.spec add:
#   vpcId: "your-vpc-id-here"
# Then re-apply; the extension controller will redeploy the bridge with the new env var.
```

**If worker nodes are not CMP compute instances** (e.g. kind/local), VS creation will fail:
```
cannot build VS: no CMP network port found for backend IP 172.18.0.3
```
In that case, the bridge is functional but CMP can't map the node to a compute resource. This is a CMP-side data requirement, not a code bug.

## The Three Sharing Scenarios

| # | What is shared | Annotation | LBService name created |
|---|---|---|---|
| 1 | Nothing – standalone | *(no vip-group)* | `app-demo-svc-lb-app-a-svc` |
| 2 | LBService + VIP (first member creates it) | `vip-group: demo-blue` | `app-group-demo-svc-lb-demo-blue` |
| 3 | LBService + VIP (inherited from sibling) | `vip-group: demo-blue` | reuses `app-group-demo-svc-lb-demo-blue` |

**Shared VIP inheritance mechanism:**  
`sharedParentObservedState()` reads the sibling Service's `f5.extensions.gardener.cloud/observed-graph` annotation (stored after first successful reconcile) to get the LBServiceID and VIPPortID. It passes those to `EnsureStack` which skips create and reuses them.

## Step 1 – Apply namespace and standalone app

```bash
export SHOOT_KC=<path-to-shoot-kubeconfig>

# Apply everything (works if bridge processes objects sequentially)
kubectl --kubeconfig "$SHOOT_KC" apply -f config/samples/demo-svc-lb-scenarios.yaml
```

**OR apply in order for guaranteed sharing** (recommended for shared VIP demo):

```bash
# 1. Create namespace + app-a (standalone)
kubectl --kubeconfig "$SHOOT_KC" apply -f config/samples/demo-svc-lb-scenarios.yaml \
  --selector 'scenario in (1-standalone)'

# 2. Create app-b (first group member, creates the shared LBService)
kubectl --kubeconfig "$SHOOT_KC" apply -f config/samples/demo-svc-lb-scenarios.yaml \
  --selector 'scenario in (2-shared-lb-parent)'

# 3. Wait for app-b to get EXTERNAL-IP (this stores the observed-graph annotation)
kubectl --kubeconfig "$SHOOT_KC" -n demo-svc-lb get svc app-b-svc -w
# Wait until EXTERNAL-IP is set (not <pending>)

# 4. Create app-c and app-d (inherit LBService+VIP from app-b)
kubectl --kubeconfig "$SHOOT_KC" apply -f config/samples/demo-svc-lb-scenarios.yaml \
  --selector 'scenario in (3-shared-lb-vip)'
```

## Step 2 – Watch IP allocation

```bash
kubectl --kubeconfig "$SHOOT_KC" -n demo-svc-lb get svc -w
```

Expected final state:

```
NAME       TYPE           CLUSTER-IP    EXTERNAL-IP   PORT(S)
app-a-svc  LoadBalancer   10.96.x.x     <VIP-A>       8080:3xxxx/TCP   ← own IP
app-b-svc  LoadBalancer   10.96.x.x     <VIP-B>       8080:3xxxx/TCP   ← own IP (group parent)
app-c-svc  LoadBalancer   10.96.x.x     <VIP-B>       9090:3xxxx/TCP   ← SAME IP as app-b
app-d-svc  LoadBalancer   10.96.x.x     <VIP-B>       7070:3xxxx/TCP   ← SAME IP as app-b
```

## Step 3 – Inspect CMP resource allocation

```bash
kubectl --kubeconfig "$SHOOT_KC" -n demo-svc-lb get svc \
  -o custom-columns=\
"NAME:.metadata.name,\
LB-ID:.metadata.annotations.f5\.extensions\.gardener\.cloud/lb-service-id,\
VIP:.metadata.annotations.f5\.extensions\.gardener\.cloud/vip-address,\
VS-ID:.metadata.annotations.f5\.extensions\.gardener\.cloud/virtual-server-id"
```

Expected:
```
NAME       LB-ID        VIP      VS-ID
app-a-svc  lb-aaa...    VIP-A    vs-aaa...   ← own LB, own VIP
app-b-svc  lb-bbb...    VIP-B    vs-bbb...   ← group parent
app-c-svc  lb-bbb...    VIP-B    vs-ccc...   ← SAME lb-id and VIP as app-b
app-d-svc  lb-bbb...    VIP-B    vs-ddd...   ← SAME lb-id and VIP as app-b
```

## Step 4 – Functional test

```bash
VIP_A=$(kubectl --kubeconfig "$SHOOT_KC" -n demo-svc-lb \
  get svc app-a-svc -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
VIP_B=$(kubectl --kubeconfig "$SHOOT_KC" -n demo-svc-lb \
  get svc app-b-svc -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

curl -s http://$VIP_A:8080 | grep Hostname  # app-a backend
curl -s http://$VIP_B:8080 | grep Hostname  # app-b backend
curl -s http://$VIP_B:9090 | grep Hostname  # app-c backend (same IP, different port)
curl -s http://$VIP_B:7070 | grep Hostname  # app-d backend (same IP, different port)
```

## Step 5 – Cleanup

```bash
kubectl --kubeconfig "$SHOOT_KC" delete -f config/samples/demo-svc-lb-scenarios.yaml
```

The bridge finalizer ensures VS → VIP → LBService are deleted from CMP before the Service object is removed.

## Troubleshooting

**Services stuck in `<pending>`** — check bridge logs:
```bash
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system logs deploy/f5-svc-lb-bridge --tail=50
```

| Error message | Cause | Fix |
|---|---|---|
| `no CMP network port found for backend IP <x>` | Worker node IP not in CMP network | Nodes must be CMP compute VMs |
| `cannot adopt VIP for LB service ... found N existing VIP(s)` | Sibling VIP ID was lost; VIPManager refuses to guess | Delete and recreate the Service to get fresh annotation |
| `BackendNodePortRequired` | `spec.ports[*].nodePort` is 0 | Wait — Kubernetes sets it automatically; controller will retry |
| `SyncLoadBalancerFailed: creating LB service via CMP: 401` | Bad CMP credentials | Check `CMP_CE_AUTH` in bridge env |
| `SyncLoadBalancerFailed: creating LB service via CMP: 400` | Missing required CMP field (e.g. `vpc_id`) | Set `spec.vpcId` in Extension providerConfig |

## Annotation Reference

| Annotation | Effect |
|---|---|
| *(none)* | Standalone: own LBService + own VIP + own VS |
| `f5.extensions.gardener.cloud/vip-group: <name>` | Shared: join named CMP LBService group; share VIP; own VS |
| `f5.extensions.gardener.cloud/routing-algorithm: <algo>` | VS pool algorithm (`round_robin`, `least_connections`) |
| `f5.extensions.gardener.cloud/health-check-type: http` | HTTP health monitor (default TCP) |
| `f5.extensions.gardener.cloud/health-check-path: /healthz` | HTTP health check path |
| `f5.extensions.gardener.cloud/health-check-interval: 10` | Monitor interval in seconds |
| `f5.extensions.gardener.cloud/connection-draining-timeout: 30` | Drain timeout in seconds |
| `f5.extensions.gardener.cloud/protocol: TCP` | Force protocol (`TCP`,`UDP`,`HTTP`,`HTTPS`) |
| `f5.extensions.gardener.cloud/source-ranges: 10.0.0.0/8` | Restrict source CIDRs (`allowed_cidrs`) |
