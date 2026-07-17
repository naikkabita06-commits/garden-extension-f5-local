# Control-Plane Load Balancing: Gardener vs F5 Extension

---

## Part 1 — What Is "Control-Plane Load Balancing"?

The Shoot kube-apiserver pods run on the **Seed** cluster (not on worker nodes).
Control-plane LB = exposing those apiserver pods to the outside world with a
stable IP/VIP so that `kubectl`, worker nodes, and clients can reach them.

---

## Part 2 — Vanilla Gardener Way

### How it works

Gardener uses a **single shared VIP** for all Shoots on a Seed.
There is NO per-Shoot VIP.

```
kubectl
  |
  v
Cloud LB VIP:443          <-- one IP for ALL Shoots on this Seed
  |
Seed Node:NodePort (e.g. 30443)
  |
Istio ingress gateway pod
  |
SNI routing (by TLS hostname)
  e.g. api.project.shoot.external.domain
  |
svc/kube-apiserver in shoot--project--shoot namespace
  |
kube-apiserver pod
```

### Who provisions the VIP?

On AWS/GCP/Azure: the **cloud provider's built-in LB controller** automatically
fulfills `Service type=LoadBalancer` for Istio. It writes the IP into
`Service.status.loadBalancer.ingress` automatically.

gardenlet watches that Service, reads the IP, and writes it to the `Seed` object.
All new Shoots derive their apiserver DNS from that Seed ingress IP.

### Key points

- One VIP shared across all Shoots on a Seed
- Istio multiplexes requests by SNI (TLS hostname)
- Cloud provider does the VIP work automatically — no extra code needed
- Works on AWS, GCP, Azure out of the box

---

## Part 3 — F5 Extension Way (Airtel Cloud / OpenStack)

OpenStack has no built-in LB controller. So the F5 extension fills the gap with
**two separate mechanisms**:

### Mechanism A: Seed Ingress LB (§4.3) — "Gardener's way, done manually"

Same concept as vanilla Gardener (shared Istio VIP), but the VIP is provisioned
manually via CMP LBaaS API by our `seed-service-lb-controller`.

```
kubectl
  |
  v
F5 VIP:443                <-- one IP for ALL Shoots on this Seed
  |                           (provisioned by seed-service-lb-controller via CMP)
Seed Node:NodePort
  |
Istio ingress gateway pod
  |
SNI routing → kube-apiserver pod in shoot--project--shoot
```

**What seed-service-lb-controller does:**
1. Watches `Service istio-ingressgateway` with `loadBalancerClass: f5.extensions.gardener.cloud/bigip`
2. Discovers Seed node IPs from `Node.status.addresses[InternalIP]`
3. Calls CMP API → creates LBService → allocates VIP → creates Virtual Server
   - Pool members: Seed node IPs + Istio NodePort
4. Writes VIP to `Service.status.loadBalancer.ingress[0].ip`
5. gardenlet reads it → updates Seed object → all Shoots get DNS from this VIP

This is **ONE-TIME per Seed**. It is the OpenStack equivalent of what AWS ELB does automatically.

---

### Mechanism B: Per-Shoot CP VIP (§4.1) — "Extra, beyond vanilla Gardener"

An additional **dedicated F5 VIP per Shoot** pointing directly at the
kube-apiserver NodePort — bypassing Istio entirely.

```
kubectl
  |
  v
F5 VIP:443                <-- dedicated IP for THIS Shoot only
  |                           (provisioned by gardener-extension-f5 via CMP)
Seed Node:kube-apiserver-NodePort
  |
kube-apiserver pod in shoot--project--shoot
```

**What gardener-extension-f5 does (per Shoot):**
1. gardenlet creates `Extension/f5-loadbalancer` in Shoot technical namespace
2. Extension controller calls CMP API:
   - POST /lb_service/ → create LBService
   - GET  /lb_service/{id}/vip → allocate VIP
   - POST /virtual-servers with pool members = Seed node IPs + kube-apiserver NodePort
3. Writes VIP to `F5LoadBalancerConfig.status.vip`
4. Sets condition `ControlPlaneLoadBalancerReady=True`
5. provider-airtelcloud wires this VIP into the Shoot's kubeconfig

**Out-of-band alternative (used in kind demo):**
Set `controlPlaneReady: true` and `controlPlaneVIP: <ip>` in the Extension
providerConfig. The controller skips CMP calls and marks CP ready immediately.
The Istio VIP is used as the "CP VIP" — traffic still goes through Istio+SNI.

---

## Part 3b — Why Per-Shoot CP VIP? Pros & Cons

### Pros

| Benefit | Explanation |
|---------|-------------|
| **Isolation** | Each Shoot has its own F5 virtual server. One Shoot's traffic problem does not affect others. With shared Istio VIP, all Shoots share one entry point. |
| **Direct path, no Istio dependency** | Traffic goes straight to `kube-apiserver NodePort` — no Istio pod in the middle. If Istio crashes, per-Shoot VIPs still work independently. |
| **Per-Shoot traffic controls** | F5 policies (rate limiting, WAF, persistence, custom health monitors) can be applied per Shoot independently. Not possible on a shared VIP. |
| **Dedicated health monitoring** | F5 health-checks each Shoot's apiserver pool members independently. With Istio, F5 only health-checks Istio pods, not the actual apiservers behind them. |
| **Tenant SLA separation** | A premium tenant can get a dedicated VIP with higher BIG-IP priority or custom LB algorithm. |
| **Cleaner kubeconfig** | The Shoot's kubeconfig contains a unique IP that directly identifies that Shoot — useful for auditing and troubleshooting. |
| **No SNI dependency** | Some clients or tools have issues with SNI-based routing. A direct VIP avoids this entirely. |
| **Required to unlock App-Plane LB** | The extension gates CIS deployment on `ControlPlaneLoadBalancerReady=True`. Without this condition (real or out-of-band), CIS is never deployed and tenants cannot get `Service type=LoadBalancer`. |

### Cons

| Drawback | Explanation |
|----------|-------------|
| **VIP exhaustion** | Each Shoot consumes one VIP from the F5 pool. 100 Shoots = 100 VIPs. VIP pools are finite and must be capacity-planned. |
| **CMP API call per Shoot** | Every new Shoot creation triggers CMP API calls (LBService → VIP → VirtualServer). More moving parts, more failure points. |
| **BIG-IP partition sprawl** | Each Shoot creates objects in the BIG-IP partition. At scale this grows large and complex. |
| **Blocked by DEPENDENCY 1 today** | The CMP API does not return the BIG-IP management IP — so this path requires manual intervention per Shoot until §10 Dependency 1 is resolved. |
| **Cleanup risk** | On Shoot deletion, CMP VS/VIP/LBService must be cleaned up. If cleanup fails, F5 resources are leaked and VIP pool exhausts over time. |

### Bottom Line

> **On Airtel Cloud with F5, you need BOTH mechanisms together:**
>
> - **Mechanism A (Seed Ingress LB §4.3)** is mandatory — it is the OpenStack
>   equivalent of the cloud LB controller. Without it, the Seed has no ingress IP,
>   gardenlet cannot populate Seed DNS, and no Shoots can be created at all.
>
> - **Mechanism B (Per-Shoot CP VIP §4.1)** is what unlocks app-plane LB —
>   the extension uses `ControlPlaneLoadBalancerReady=True` as the gate before
>   deploying CIS into a Shoot. In the kind demo, the out-of-band path
>   (`controlPlaneReady: true`) satisfies this gate without a real CMP call,
>   so Mechanism A (Istio VIP) handles the actual CP traffic.
>
> In production: Mechanism A runs once when the Seed is set up.
> Mechanism B runs automatically for every new Shoot once CMP is reachable
> and Dependency 1 (BIG-IP mgmt IP in API response) is resolved.

---

## Part 4 — Side-by-Side Comparison

| Aspect | Vanilla Gardener | F5 Seed Ingress (§4.3) | F5 Per-Shoot CP (§4.1) |
|--------|-----------------|------------------------|------------------------|
| VIP scope | Shared (all Shoots) | Shared (all Shoots) | Dedicated per Shoot |
| Traffic routing | Istio + SNI | Istio + SNI | Direct to apiserver NodePort |
| Who provides VIP | Cloud LB controller (auto) | seed-service-lb-controller via CMP | gardener-extension-f5 via CMP |
| Exists in vanilla Gardener | Yes | No (OpenStack gap filler) | No (net-new enhancement) |
| Required for app-plane LB | — | — | Yes (gates CIS deployment) |
| Frequency | Once per Seed | Once per Seed | Once per Shoot |

---

## Part 5 — Starting Fresh: Step-by-Step

All clusters deleted. Starting from zero.

---

### WAY 1: Vanilla Gardener (Cloud provider, e.g. AWS/GCP)

**Prerequisites:** kind or cloud Seed running, gardenlet deployed.

**Step 1 — Seed registration**

gardenlet Helm values — no special LB class needed, just use defaults:

```yaml
seedConfig:
  spec:
    settings:
      loadBalancerServices:
        class: ""   # empty = use cloud provider default
```

**Step 2 — Start gardenlet**

gardenlet creates the Istio ingress Service as `type=LoadBalancer`.
The cloud LB controller (AWS/GCP) automatically assigns an IP.
No action needed from you.

**Step 3 — Verify Seed ingress IP is populated**

```bash
kubectl --context kind-gardener-local \
  -n istio-ingress get svc istio-ingressgateway \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
```

Expected: an external IP (e.g. `10.x.x.x` or an ELB hostname).

**Step 4 — Check Seed object has the ingress IP**

```bash
kubectl --context kind-gardener-local get seed local \
  -o jsonpath='{.status.addresses}'
```

**Step 5 — Create a Shoot**

No F5 extension config needed. Shoot apiserver DNS is derived from Seed ingress IP.
Control-plane LB is done.

---

### WAY 2: F5 Extension (Airtel Cloud / OpenStack)

This covers both §4.3 (Seed Ingress) and §4.1 (Per-Shoot CP VIP) with the
out-of-band path used in the kind demo.

#### Phase 1: Seed Ingress LB (§4.3) — do this ONCE before any Shoots

**Step 1 — Configure gardenlet to use F5 loadBalancerClass**

In gardenlet Helm values:

```yaml
seedConfig:
  spec:
    settings:
      loadBalancerServices:
        class: f5.extensions.gardener.cloud/bigip
```

This tells gardenlet to create the Istio Service with that loadBalancerClass,
which is the trigger for seed-service-lb-controller.

**Step 2 — Deploy seed-service-lb-controller on the Seed**

```bash
kubectl --context kind-gardener-local apply -f deploy/kind/controller.yaml
```

**Step 3 — Verify the Istio Service gets its VIP**

seed-service-lb-controller watches the Service, calls CMP, and writes the IP:

```bash
kubectl --context kind-gardener-local \
  -n istio-ingress get svc istio-ingressgateway \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}'
```

Expected: `100.72.200.20` (or whatever CMP allocated).

In the kind demo, CMP is not available, so the IP is patched manually:

```bash
kubectl --context kind-gardener-local \
  -n istio-ingress patch svc istio-ingressgateway \
  --subresource=status \
  --type=merge \
  -p '{"status":{"loadBalancer":{"ingress":[{"ip":"100.72.200.20"}]}}}'
```

**Step 4 — Verify Seed object has the ingress IP**

```bash
kubectl --context kind-gardener-local get seed local -o wide
```

Look for the IP in the `INGRESS DOMAIN` or `STATUS` column.

---

#### Phase 2: Per-Shoot CP VIP (§4.1) — do this PER SHOOT

**Step 5 — Register the F5 extension with Gardener**

```bash
kubectl --context kind-gardener-local apply -f deploy/garden/controllerregistration-f5.yaml
kubectl --context kind-gardener-local apply -f deploy/garden/controllerdeployment-f5.yaml
kubectl --context kind-gardener-local apply -f deploy/garden/controllerinstallation-f5.yaml
```

**Step 6 — Verify extension pod is running on Seed**

```bash
kubectl --context kind-gardener-local \
  get pods -n extension-gardener-extension-f5-zn8sj
```

Expected: `gardener-extension-f5-xxx   1/1   Running`

**Step 7 — Create credentials Secret in Shoot namespace**

```bash
kubectl --context kind-gardener-local \
  -n shoot--local--local apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: f5-credentials
type: Opaque
stringData:
  username: admin
  password: <bigip-password>
EOF
```

**Step 8 — Create Shoot with F5 extension providerConfig**

Option A — Out-of-band VIP (kind demo path, bypasses CMP):

```yaml
spec:
  extensions:
  - type: f5-loadbalancer
    providerConfig:
      apiVersion: f5.extensions.gardener.cloud/v1alpha1
      kind: F5LoadBalancerConfig
      spec:
        controlPlaneReady: true
        controlPlaneVIP: "100.72.200.20"
        credentialsSecretRef:
          name: f5-credentials
          namespace: shoot--local--local
        enableApplicationLB: true
        cis:
          bigipUrl: "https://100.72.44.146"
          image: "f5networks/k8s-bigip-ctlr:2.14.0"
          bridgeImage: "gardener-extension-f5:latest"
          partition: "k8s-apps"
          extraArgs:
          - "--insecure=true"
```

Option B — Automated via CMP (production path):

```yaml
spec:
  extensions:
  - type: f5-loadbalancer
    providerConfig:
      apiVersion: f5.extensions.gardener.cloud/v1alpha1
      kind: F5LoadBalancerConfig
      spec:
        ccpApiEndpoint: "https://cmp.airtelcloud.internal/api/v1"
        tenantOrPartition: "qa-tenant"
        flavorId: "flavor-uuid"
        networkId: "network-uuid"
        credentialsSecretRef:
          name: f5-credentials
          namespace: shoot--local--local
        enableApplicationLB: true
        cis:
          bigipUrl: "https://100.72.44.146"
          image: "f5networks/k8s-bigip-ctlr:2.14.0"
          bridgeImage: "gardener-extension-f5:latest"
          partition: "k8s-apps"
```

**Step 9 — Watch extension reconcile**

```bash
kubectl --context kind-gardener-local \
  -n shoot--local--local get f5loadbalancerconfig -w
```

Expected conditions:
```
ControlPlaneLoadBalancerReady = True
ApplicationLoadBalancerReady  = True   (after CIS deploys)
```

**Step 10 — Verify CP LB is working**

```bash
# Port-forward to kube-apiserver via the Shoot namespace
kubectl --context kind-gardener-local \
  -n shoot--local--local port-forward svc/kube-apiserver 7443:443 &

# Test CP access
kubectl --server=https://127.0.0.1:7443 \
  --insecure-skip-tls-verify \
  --token=$(kubectl --context kind-gardener-local \
    -n shoot--local--local get secret \
    -o jsonpath='{.items[0].data.token}' | base64 -d) \
  get nodes
```

**Step 11 — Test via F5 VIP directly**

```bash
curl -sk --resolve api.local.local.external.local.gardener.cloud:443:100.72.200.20 \
  https://api.local.local.external.local.gardener.cloud/healthz
```

Expected: `ok`

---

## Part 6 — Quick Reference

| Need | Command |
|------|---------|
| Check Seed ingress IP | `kubectl get seed local -o wide` |
| Check Istio Service status | `kubectl -n istio-ingress get svc istio-ingressgateway` |
| Check F5 extension pod | `kubectl get pods -n extension-gardener-extension-f5-zn8sj` |
| Check per-Shoot CP status | `kubectl -n shoot--local--local get f5loadbalancerconfig -o yaml` |
| Check CP conditions | `kubectl -n shoot--local--local get f5loadbalancerconfig -o jsonpath='{.status.conditions}'` |
| Check CIS in Shoot | `kubectl --kubeconfig /tmp/shoot-pf.kubeconfig get pods -n f5-cis-system` |
| Test CP VIP | `curl -sk --resolve api.local.local.external.local.gardener.cloud:443:100.72.200.20 https://api.local.local.external.local.gardener.cloud/healthz` |


1) multiple tenant architecture 

Vanilla Gardener does not give per-Shoot VIPs 
What vanilla Gardener does
Only one VIP exists — the Seed Ingress VIP shared by all Shoots. The per-Shoot "address" is just a different DNS hostname (not a different IP), routed by Istio+SNI:

Gardener's provider-local / provider-airtelcloud writes the Shoot's apiserver address into the Shoot's kubeconfig. That address is api.<shoot>.<project>.<seed-ingress-domain> — which resolves to the shared Seed VIP.

There is no mechanism in upstream Gardener to allocate a per-Shoot IP.