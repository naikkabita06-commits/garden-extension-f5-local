# Mechanism B Demo Script — Per-Shoot CP VIP (§4.1)

## What you are showing

> "Beyond the shared Istio VIP, our extension allocates a **dedicated F5 VIP
> per Shoot** that points directly at that Shoot's kube-apiserver NodePort —
> bypassing Istio entirely. Each tenant gets their own IP. In production this
> is done automatically via CMP. Here we show it with a real F5 BIG-IP."

---

## Prerequisites (already done — show state)

```bash
# Show the extension pod is running
kubectl get pods -n extension-gardener-extension-f5

# Show the F5LoadBalancerConfig was reconciled
kubectl -n shoot--local--local get f5loadbalancerconfig f5-loadbalancer -o yaml \
  | grep -A15 "status:"
```


```yaml
status:
  conditions:
  - message: Control-plane VIP/VS marked ready via spec.controlPlaneReady=true
    reason: ExternalProvisioned
    status: "True"
    type: ControlPlaneLoadBalancerReady
  vip: 100.72.200.20
  virtualServerName: cp-apiserver-vs
```


- `ControlPlaneLoadBalancerReady = True` — the gate condition is set
- `vip: 100.72.200.20` — dedicated VIP for THIS Shoot recorded in status
- In production: controller calls CMP, gets real VIP, sets this automatically

---

## Step 1 — Show how the extension was configured (out-of-band path)

```bash
kubectl -n garden-local get shoot local \
  -o jsonpath='{.spec.extensions}' | python3 -m json.tool
```


- `type: f5-loadbalancer` — Gardener extension hook
- `controlPlaneReady: true` — out-of-band flag, tells controller to skip CMP
- `controlPlaneVIP: 100.72.200.20` — we pre-specify the VIP (kind demo only)


> "In production with CMP, these two fields are absent. The controller calls
> CMP LBaaS: POST /lb_service → GET /vip → POST /virtual-servers. Then writes
> the VIP into the F5LoadBalancerConfig status. Same result, fully automated."

---

## Step 2 — Show the BIG-IP VS and pool (what the extension creates in production)

On BIG-IP UI (`https://100.72.44.146`) → Local Traffic → Virtual Servers:

| Field | Value |
|---|---|
| VS Name | `kind-seed-ingress` |
| Destination VIP | `100.72.200.20:443` |
| Pool | `kind istio pool` |
| Pool Member | `100.72.44.199:32031` |


> "This Virtual Server on F5 is what the extension creates via CMP in production.
> The pool member is the Seed node IP + kube-apiserver NodePort. Traffic hits
> the F5 VIP, F5 load balances to this node, iptables DNAT forwards into the
> kind cluster, reaches the kube-apiserver pod directly — no Istio in the path."

---

## Step 3 — Prove live traffic via the real F5 VIP

```bash
# Direct curl through the real F5 BIG-IP VIP
# No port-forward — this goes: VM → F5 100.72.200.20 → pool 100.72.44.199:32031 → kube-apiserver
curl -sk \
  --connect-to api.local.local.external.local.gardener.cloud:443:100.72.200.20:443 \
  https://api.local.local.external.local.gardener.cloud/healthz
```


```json
{
  "kind": "Status",
  "apiVersion": "v1",
  "status": "Failure",
  "message": "Unauthorized",
  "reason": "Unauthorized",
  "code": 401
}
```


- `401 Unauthorized` = **kube-apiserver responded through real F5 hardware**
- This is NOT hitting the kind VIP (`172.18.255.1`) — it's going through `100.72.200.20`
- No port-forward — real network path
- Traffic: curl → F5 VIP `100.72.200.20:443` → pool member `100.72.44.199:32031` → kube-apiserver pod

**Say:**
> "The kube-apiserver of Shoot `local` responded through a real F5 BIG-IP VIP.
> `401` means it received the request and is asking for login credentials —
> identical to what `kubectl` shows before you configure a kubeconfig.
> This VIP is dedicated to this Shoot only. A second Shoot would get `100.72.200.21`."

---

## Step 4 — Show the contrast with Mechanism A

Run both side by side:

```bash
# Mechanism A — shared VIP via Istio (port-forward required to reach 172.18.255.1 from host)
curl -sk \
  --connect-to api.local.local.external.local.gardener.cloud:443:127.0.0.1:9443 \
  https://api.local.local.external.local.gardener.cloud/healthz
# ^^^ needs --connect-to trick because curl can't reach 172.18.255.1 from VM host
# In production: curl → F5 Seed VIP → Istio NodePort → Istio pod → SNI routing → apiserver

# Mechanism B — dedicated VIP, direct to apiserver
curl -sk \
  --connect-to api.local.local.external.local.gardener.cloud:443:100.72.200.20:443 \
  https://api.local.local.external.local.gardener.cloud/healthz
# ^^^ real F5 VIP, no tricks — F5 → NodePort → apiserver, no Istio
```

Both return `401` — both reach kube-apiserver. The difference is the path.

---

## Step 5 — Show the multi-tenant story

**Say:**
> "Now imagine 50 Shoots. With Mechanism A alone, all 50 share `172.18.255.1`.
> Istio must handle all 50 by SNI. One misconfiguration on Istio affects all 50.
>
> With Mechanism B, each Shoot gets its own VIP:
> - Shoot `local`  → `100.72.200.20`
> - Shoot `local2` → `100.72.200.21`
> - Shoot `local3` → `100.72.200.22`
>
> F5 health-checks each Shoot's kube-apiserver independently. A problem with
> one Shoot's apiserver doesn't affect any other. Premium tenants can get
> dedicated F5 profiles with rate limiting or WAF policies."

---


## Traffic path diagrams

**Mechanism A (Shared Istio VIP):**
```
curl/kubectl
    |
    v
F5 Seed VIP: 172.18.255.1:443        ← ONE IP for ALL Shoots
    |
Seed Node: 30443 (Istio NodePort)
    |
Istio ingress gateway pod
    |
SNI routing by TLS hostname           ← api.local.local.*.gardener.cloud
    |
svc/kube-apiserver (ClusterIP)
    |
kube-apiserver pod (shoot--local--local)
```

**Mechanism B (Dedicated F5 VIP):**
```
curl/kubectl
    |
    v
F5 VIP: 100.72.200.20:443            ← DEDICATED IP for THIS Shoot only
    |
F5 pool member: 100.72.44.199:32031
    |
iptables DNAT → 172.18.0.7:32031
    |
NodePort service → kube-apiserver pod 
```
