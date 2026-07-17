# Gardener Extension F5 — Progress Snapshot (Feb 18, 2026)

## Demo runbook (Feb 24, 2026) — What we have working today

For a clean, copy/paste A→Z demo script (one-by-one commands): see [docs/demo-a-z.md](demo-a-z.md).

Goal for today’s demo: show the extension working end-to-end in our current dev environment:

- Seed-side controller reconciles an `Extension`.
- Controller deploys CIS into the Shoot (application plane wiring).
- CIS can reach BIG-IP mgmt/AS3 and authenticate (connectivity verified).

### What’s achieved (verified)

- Identified and fixed Shoot→BIG-IP mgmt reachability in provider-local setup (seed namespace NetworkPolicy needed an explicit allow to BIG-IP on 443).
- Restored reconciliation by recreating the missing `Extension` object (when it was absent).
- Stabilized CIS by handling BIG-IP TLS/IP-SAN mismatch with CIS `--insecure=true` (demo workaround).
- Verified from inside the CIS pod that BIG-IP AS3 is reachable and credentials work (`/mgmt/shared/appsvcs/info` returns HTTP 200).
- Added Helm namespace length validation (fails fast when release namespace >63 chars).

### What we will not claim today unless verified live

- “Control-plane LB works end-to-end” (VIP must actually forward to kube-apiserver and `GET /version` must succeed).

### Pre-reqs

- You have:
  - Seed cluster context: `<SEED_CTX>`
  - Shoot technical namespace: `<shoot-tech-ns>` (e.g., `shoot--local--local`)
  - (Optional) Shoot cluster kubeconfig/context for checking CIS resources directly.
- BIG-IP mgmt endpoint and credentials available.

### Live demo steps (copy/paste)

If you want a one-command dry run that prints the key proof points, run:

```bash
./scripts/demo-dry-run.sh
```

The steps below are the same checks, shown explicitly.

#### 1) Show the key objects the controller reconciles

```bash
kubectl --context <SEED_CTX> -n <shoot-tech-ns> get extension
kubectl --context <SEED_CTX> -n <shoot-tech-ns> get f5loadbalancerconfig,f5loadbalancerconfigs.f5.extensions.gardener.cloud || true
```

What to say:
- “The Gardener `Extension` object is the reconciliation trigger, and our per-shoot config is `F5LoadBalancerConfig`.”

#### 2) Ensure BIG-IP mgmt is reachable from the Shoot side (provider-local)

In provider-local, workers are pods in the Seed and are subject to Seed namespace NetworkPolicies.

Apply the explicit allow (BIG-IP /32 on 443):

```bash
kubectl --context <SEED_CTX> apply -f deploy/kind/allow-shoot-to-bigip-mgmt.networkpolicy.yaml
```

Quick proof (from a Shoot pod / hostNetwork pod) should return quickly with HTTP 401 (reachable, auth required).

#### 3) Ensure CIS is configured and running in the Shoot

Check the `F5LoadBalancerConfig` has:

- `enableApplicationLB: true`
- `spec.cis.bigipUrl: https://<BIG-IP-MGMT-IP>`
- credentials secret ref
- for demo workaround: `spec.cis.extraArgs: ["--insecure=true"]`

Then show CIS is present:

```bash
kubectl -n f5-cis-system get deploy,pod
kubectl -n f5-cis-system logs deploy/f5-cis --tail=80
```

#### 4) Prove CIS → BIG-IP mgmt/AS3 works (inside CIS pod)

```bash
kubectl -n f5-cis-system exec deploy/f5-cis -- sh -lc '\
  curl -ksS -u "${BIGIP_USERNAME}:${BIGIP_PASSWORD}" \
    https://<BIG-IP-MGMT-IP>/mgmt/shared/appsvcs/info \
    -o /tmp/as3.json -w "http_code=%{http_code}\n" && head -c 200 /tmp/as3.json && echo'
```

Expected:
- `http_code=200` and JSON containing an AS3 version.

---

## Next steps (post-demo)

1) Replace CIS `--insecure=true` with a production-grade trust model (DNS name + proper cert SANs, or `--trusted-certs-cfgmap`).
2) Implement real control-plane LB provisioning and verification (VIP must serve shoot apiserver `/version`).
3) Production installation: register via `ControllerDeployment`/`ControllerRegistration` (Garden → Seed), remove cluster-admin RBAC, tighten permissions.
4) If AirtelCloud integration is needed: align the exact Extension “output” contract AirtelCloud reads (VIP/DNS keys + readiness semantics) and implement it.

---

## Demo plan (what we will show)

Goal for today’s demo: prove the extension wiring and reconciliation works end-to-end **inside Kubernetes**, and be explicit about what remains **outside Kubernetes** (network/BIG-IP config).

We will show:
1) Seed side: the extension controller is running and reconciling.
2) Shoot tech namespace: the `Extension` + `F5LoadBalancerConfig` objects exist and drive reconciliation.
3) Shoot cluster: CIS resources were created by the controller (strong proof of application-plane wiring).
4) Blocker proof: CIS cannot reach BIG-IP mgmt/AS3 endpoint (`100.65.242.181:443`) → timeouts.
5) Optional: the infra VIP `10.1.1.25` is reachable on port 80 (demonstrates the VIP/VS exists; does **not** prove kube-apiserver LB).

## Demo narrative (what to say)

1) Tenancy + VIP model (target design)
- We treat `tenantOrPartition` as the isolation boundary on the provider/BIG-IP side.
- Each shoot has **one control-plane VIP** (for kube-apiserver). Application-plane can have **multiple VIPs** (one per app/Ingress/Service as needed) within the same tenant/partition.
- Whether this maps to “one LB per tenant” depends on the CMP/BIG-IP deployment model; what we enforce in the extension is the **tenant/partition** boundary and the **per-shoot control-plane VIP**.

2) Control-plane LB scope (for this demo)
- We are not demoing real control-plane load balancing today.
- In this repo, `ControlPlaneLoadBalancerReady` becomes `True` in one of these ways:
  - Out-of-band acknowledgement (`spec.controlPlaneReady=true`), or
  - CMP/CCP API provisioning when `spec.ccpApiEndpoint` is set and `spec.controlPlaneReady` is omitted.
- Gardener’s Istio/SNI plumbing helps with shoot API access patterns in a Gardener landscape, but it is not the same as “BIG-IP VIP load balancing kube-apiserver”.

3) CMP/Cloud API integration (current state)
- The controller is now wired to attempt CMP/CCP control-plane provisioning when `spec.ccpApiEndpoint` is provided.
- Next step is to align the request/response shapes and endpoints exactly with the shared Swagger/OpenAPI spec (if they differ from the current client implementation) and expand lifecycle coverage (update/delete).

### CMP/CCP provisioning mode (how to enable)

The controller provisions/updates the control-plane VIP/VS via CMP/CCP when:
- `spec.controlPlaneReady` is **omitted** (`null`), and
- `spec.ccpApiEndpoint` is set.

The CMP HTTP paths/methods are controller-level settings (Helm values → env vars). They are read by `pkg/f5/client.go`:
- `F5_HTTP_API_PREFIX`
- `F5_HTTP_CP_VS_ENSURE_PATH_TEMPLATE`
- `F5_HTTP_CP_VS_DELETE_PATH_TEMPLATE`
- `F5_HTTP_CP_VS_ENSURE_METHOD`
- optional: `F5_INSECURE_SKIP_TLS_VERIFY` (only for trusted/dev environments)

Helm values location:
- `controller.env.*` in [charts/gardener-extension-f5/values.yaml](../charts/gardener-extension-f5/values.yaml)

Example (edit your values.yaml or use `--set-string`):

```bash
# Example only: replace with your CMP/CCP API contract.
helm upgrade --install gardener-extension-f5 charts/gardener-extension-f5 \
  -n garden-f5-system \
  --set-string controller.env.F5_HTTP_API_PREFIX='/ccp' \
  --set-string controller.env.F5_HTTP_CP_VS_ENSURE_PATH_TEMPLATE='/v1/virtual-servers/%s' \
  --set-string controller.env.F5_HTTP_CP_VS_DELETE_PATH_TEMPLATE='/v1/virtual-servers/%s' \
  --set-string controller.env.F5_HTTP_CP_VS_ENSURE_METHOD='PUT'
```

Per-shoot config lives in the `F5LoadBalancerConfig` and its referenced Secret:
- `spec.ccpApiEndpoint`: base URL for the CMP/CCP API
- `spec.controlPlaneVIP`: desired VIP
- `spec.tenantOrPartition`: tenant/partition (also used as `organisation-name` when Ce-Auth is used)
- `spec.credentialsSecretRef`: must contain either:
  - `Ce-Auth` + `project-id` (CMP token auth), or
  - `username` + `password` (basic auth)

4) Installation/ops model
- Manual YAML was used to move fast in dev.
- Target state is Helm-based installation of the controller (and later, any supporting RBAC/config) so it’s repeatable and automated.

Recommended flow (what should be automated vs manual):
- Helm is responsible for installing/upgrading the **seed-side controller** (once per landscape/dev cluster).
- The controller is responsible for **dynamic provider actions** (CMP API calls to create/update LB/VIP/VS) and for deploying CIS into the shoot.
- Per-shoot values like BIG-IP management endpoint(s) should live in **Kubernetes objects** (e.g., `F5LoadBalancerConfig` and a credentials `Secret`), not Helm values.
- The only “manual” step (until we have a real source-of-truth) should be updating the per-shoot CR/Secret with management IP/URL + credentials.

Demo-friendly sequencing:
1) `helm upgrade --install` the controller in `garden-f5-system`.
2) Create the seed-side credentials `Secret` (`cmp-f5-credentials`).
3) Create the shoot (or use an existing shoot).
4) Apply/patch the per-shoot `F5LoadBalancerConfig` with the BIG-IP mgmt URL/IP.
5) Controller reconciles: creates/updates LB/VIP/VS via CMP + deploys CIS into the shoot.

## What we’re trying to do
Deploy and validate the F5 Gardener extension in the local kind-based Gardener dev setup so that:
- Control-plane VIP/VS is provisioned via CMP/CCP APIs (or acknowledged out-of-band via spec).
- Application-plane load balancing is handled by deploying F5 CIS into the shoot cluster (CIS watches K8s Services/Ingresses and programs BIG-IP).

## What’s done
### Code & build/deploy plumbing
- Repo on the VM has the updated code.
- Controller can be built into an image `gardener-extension-f5:dev`.
- Controller is deployed inside the kind cluster in namespace `garden-f5-system` using image `gardener-extension-f5:dev`.
- NetworkPolicy exists in the shoot technical namespace to allow traffic needed for the controller to talk to the shoot kube-apiserver.

### Cluster objects created
In the shoot technical namespace `shoot--local--local`:
- CRD installed: `f5loadbalancerconfigs.f5.extensions.gardener.cloud`.
- Extension object created: `extensions.gardener.cloud/v1alpha1 Extension` named `f5`.
- Config object created: `f5.extensions.gardener.cloud/v1alpha1 F5LoadBalancerConfig` named `f5`.

### Config values currently set (from F5LoadBalancerConfig)
- `spec.controlPlaneVIP: 10.1.1.25`  
  In *this extension’s API*, this field is defined as the VIP for the shoot Kubernetes API server (control-plane / kube-apiserver).  
  **Separately:** the CMP VIP/VS you created in the cloud UI on `10.1.1.25` (e.g. VS on ports like `80`/`777` pointing to your VM) is an infra-level object you’re currently using for application-plane testing, but the extension does not treat that as the “application-plane LB” input.
- `spec.controlPlaneReady: true`
- `spec.cis.bigipUrl: https://100.65.242.181`
- `spec.cis.partition: k8s-apps`
- `spec.cis.image: f5networks/k8s-bigip-ctlr:2.14.0`
- `spec.credentialsSecretRef: shoot--local--local/cmp-f5-credentials`

### Current status
- `ControlPlaneLoadBalancerReady=True` (this is driven by `spec.controlPlaneReady=true`).
- `ApplicationLoadBalancerReady=True` (controller reconciled CIS into the shoot).

## What we can confidently claim (and what we cannot)

We can claim (demo-backed):
- The extension controller reconciles and deploys CIS into the shoot.
- The CIS deployment is created with credentials propagated.
- The remaining failure is outside Kubernetes: network reachability to BIG-IP mgmt/AS3.

We cannot claim yet:
- “Control-plane LB works” unless `/version` works through the VIP on `:443` or `:6443`.
- “Application-plane LB works end-to-end” until CIS can reach BIG-IP mgmt and successfully posts AS3 declarations.

## Success criteria (what “done” looks like)

### Control-plane LB (kube-apiserver)
**Success means traffic to the shoot API server works via the VIP.**

Important: in this repo, `ControlPlaneLoadBalancerReady=True` does not automatically prove BIG-IP is correctly configured. It may be set either by:
- Out-of-band acknowledgement (`spec.controlPlaneReady=true`), or
- Controller-driven CMP/CCP provisioning (when `spec.ccpApiEndpoint` is set).

Practical verification (run from a host that can reach the VIP):

```bash
# One of these ports should match your control-plane VIP/VS.
curl -k https://10.1.1.25:443/version || true
curl -k https://10.1.1.25:6443/version || true
```

Current reality in this environment:
- Port `80` on the VIP responds (because a VS exists for app testing).
- Ports `443/6443` are **not** working today (no kube-apiserver LB configured/exposed to BIG-IP).

If you have a kubeconfig that uses the VIP as the cluster endpoint, a stronger check is simply:

```bash
kubectl --kubeconfig <shoot-kubeconfig> get --raw /version
```

### Application-plane LB (CIS-managed)
**Success means the controller deploys CIS into the shoot and CIS can program BIG-IP for app Services/Ingress.**

Minimum success signals:
- `ApplicationLoadBalancerReady=True` on the `F5LoadBalancerConfig`.
- CIS resources exist in the shoot:
  - Namespace: `f5-cis-system`
  - Deployment: `f5-cis`
  - Secret: `f5-cis-credentials`

Verify CIS exists:

```bash
kubectl -n f5-cis-system get deploy f5-cis
kubectl -n f5-cis-system get secret f5-cis-credentials
kubectl -n f5-cis-system get pods -l app=f5-cis
```

End-to-end application LB proof (after CIS is running): create an app `Service`/`Ingress` that CIS watches and verify BIG-IP gets a new app VIP/VS and the app becomes reachable.

## Current state / blocker

We created the required credentials Secret and the controller deployed CIS into the shoot:
- `shoot--local--local/cmp-f5-credentials` (seed-side)
- `f5-cis-system/f5-cis` + `f5-cis-system/f5-cis-credentials` (shoot-side)

However, **CIS is unstable (restarts / may CrashLoop)** because it cannot reach the BIG-IP management endpoint from inside the shoot network.

Proof from inside the shoot (netcheck pod): TCP connect to `100.65.242.181:443` times out.

Important sanity check (VM/host network): from the VM itself, TCP connect to `100.65.242.181:443` succeeds and `curl` returns `HTTP 401 F5 Authorization Required` for `/mgmt/shared/appsvcs/info`.

Note: `401` here is expected *without* credentials. It proves the endpoint is up and reachable at L3/L4/L7, but authentication is required.

Also: when you provide the correct username/password, you should **not** get `401` anymore — you should typically get `200` + JSON. Both outcomes are useful signals:
- `401` (no auth) => reachable, auth required
- `200` (with auth) => reachable, credentials accepted

Once shoot → BIG-IP connectivity is unblocked, validate credentials separately (from a host that can reach BIG-IP mgmt):

```bash
curl -k -u '<F5-USER>:<F5-PASS>' -sS -D- --connect-timeout 5 --max-time 10 \
  https://100.65.242.181/mgmt/shared/appsvcs/info
```

What the result means:
- `200` + JSON response: credentials and AS3 endpoint are OK.
- `401`: wrong credentials or the BIG-IP user/role is not permitted to access iControl REST/AS3 (ask BIG-IP team for a user with sufficient permissions; for demos, admin-level is simplest).
- `404`: AS3 endpoint is not available/enabled on the target BIG-IP.

So the BIG-IP mgmt endpoint is **up and reachable from the VM**, but it is **not reachable from the shoot pod network**. This strongly suggests a shoot egress/NAT/NetworkPolicy/firewall-source-IP mismatch rather than “BIG-IP is down”.

Also note: even when L3/L4 connectivity is fixed, CIS can still CrashLoop if BIG-IP presents a TLS certificate that is not valid for the configured `spec.cis.bigipUrl`.
In our env BIG-IP uses a self-signed cert without IP SANs, and CIS uses the BIG-IP URL as an IP (`https://100.65.242.181`), so CIS fails with:
`x509: cannot validate certificate for 100.65.242.181 because it doesn't contain any IP SANs`.

Demo workaround (not recommended for prod):
set `spec.cis.extraArgs: ["--insecure=true"]` on the `F5LoadBalancerConfig` so CIS skips certificate verification.
Better fix: configure BIG-IP with a proper certificate (IP SAN or use a DNS name) or use CIS `--trusted-certs-cfgmap`.

### Confirmed root cause (this environment)

In the seed cluster namespace `shoot--local--local`, the default egress policy `allow-to-private-networks` allows CGNAT `100.64.0.0/10` but **explicitly excludes `100.64.0.0/13`**.

Our BIG-IP mgmt IP `100.65.242.181` is inside `100.64.0.0/13`, so egress from the provider-local shoot worker (pod `machine-shoot--local--local-worker-*`) was denied and curls timed out.

### Demo fix (allow only BIG-IP mgmt IP:443)

Apply the NetworkPolicy exception manifest:

```bash
kubectl --context kind-gardener-local apply -f deploy/kind/allow-shoot-to-bigip-mgmt.networkpolicy.yaml
```

Validate:

```bash
# From the seed-side provider-local worker pod (must quickly return HTTP 401)
kubectl --context kind-gardener-local -n shoot--local--local exec machine-shoot--local--local-worker-64d67-vsdr7 -- \
  sh -lc 'curl -sSvk --connect-timeout 5 https://100.65.242.181/mgmt/shared/appsvcs/info -o /dev/null'

# From the Shoot (hostNetwork; must quickly return HTTP 401)
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system run hostnetcheck --rm -i --restart=Never \
  --overrides='{"spec":{"hostNetwork":true,"dnsPolicy":"ClusterFirstWithHostNet","tolerations":[{"operator":"Exists"}]}}' \
  --image=curlimages/curl --command -- sh -lc \
  'curl -sSvk --connect-timeout 5 https://100.65.242.181/mgmt/shared/appsvcs/info -o /dev/null'
```

## Flow diagram (for cloud/network handoff)

This is the network flow we are debugging (control-plane provisioning and CIS both need BIG-IP mgmt reachability):

```mermaid
flowchart LR
  subgraph Shoot_Cluster[Shoot cluster]
    CIS[F5 CIS pod\n(pod IP: 100.96.x.x)] --> NODE[Worker node\n(InternalIP: 10.250.130.192)]
  end

  subgraph VM[VM / host]
    VMIP[VM egress\n(src: 10.10.2.171)]
  end

  NODE -->|egress to 100.65.242.181:443| FW[Firewall / routing domain]
  VMIP -->|egress to 100.65.242.181:443| FW

  FW --> BIGIP[BIG-IP mgmt / AS3\n100.65.242.181:443]

  %% Notes
  %% - Pod IPs (100.96.x.x) are overlay IPs and typically not visible outside.
  %% - What the firewall sees is usually node IP or a NAT/SNAT IP, depending on the environment.
```

Key point: `100.96.x.x` is a pod overlay IP; it is typically **not visible** to firewall logs. The meaningful sources to check/allowlist are:
- Worker node egress IPs (e.g. `10.250.130.192` in this env), and/or
- Any NAT/SNAT egress IP(s) used for node traffic.

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system logs pod/netcheck --tail=200
```

If you want a one-liner that proves the timeout directly (rather than reading logs), run:

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system run netcheck \
  --image=curlimages/curl:8.5.0 --restart=Never -it --rm -- \
  sh -lc 'curl -k -sS -D- --connect-timeout 5 --max-time 10 https://100.65.242.181/mgmt/shared/appsvcs/info'
```

### Curl checks (all LB paths)

**BIG-IP management / AS3 endpoint (shoot → BIG-IP mgmt)**

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system run netcheck \
  --image=curlimages/curl:8.5.0 --restart=Never -it --rm -- \
  sh -lc 'curl -k -sS -D- --connect-timeout 5 --max-time 10 https://100.65.242.181/mgmt/shared/appsvcs/info'
```

**Control-plane VIP (client → kube-apiserver via VIP)**

```bash
curl -k -sS -D- --connect-timeout 3 --max-time 5 https://10.1.1.25:443/version || true
curl -k -sS -D- --connect-timeout 3 --max-time 5 https://10.1.1.25:6443/version || true
```

**Infra / existing VIP used for app testing (proves VIP/VS exists; not kube-apiserver LB)**

```bash
curl -sS -D- -o /dev/null --connect-timeout 3 --max-time 5 http://10.1.1.25:80/ || true
```

**Application-plane VIP (after CIS programs BIG-IP for a test app)**

Once you create a test `Service`/`Ingress` that CIS watches and BIG-IP allocates/attaches a VIP, curl that VIP:

```bash
# Example patterns (fill in the VIP/hostname you actually get):
curl -sS -D- --connect-timeout 3 --max-time 5 http://<APP_VIP>/ || true
curl -sS -D- --connect-timeout 3 --max-time 5 -H 'Host: echo.demo.local' http://<APP_VIP>/ || true
```

And CIS logs show a timeout to:
- `https://100.65.242.181/mgmt/shared/appsvcs/info`

So, the remaining work is mainly network reachability (shoot -> BIG-IP mgmt) and/or BIG-IP/AS3 endpoint availability.

## Blockers (what prevents end-to-end success)

### Blocker 1 — BIG-IP mgmt reachability (owner: cloud/network/BIG-IP team)
- Symptom: CIS logs show timeout to `https://100.65.242.181/mgmt/shared/appsvcs/info`.
- Proof: from inside the shoot, TCP connect to `100.65.242.181:443` times out.
- Impact: CIS cannot program BIG-IP, so application-plane LB cannot be proven end-to-end.

### Blocker 2 — Control-plane VIP is not backed by kube-apiserver (owner: cloud/BIG-IP team)
- Even if `ControlPlaneLoadBalancerReady=True` (via out-of-band ack or CMP/CCP provisioning), a real control-plane LB demo requires a BIG-IP VS that forwards to shoot kube-apiserver backends that BIG-IP can reach.
- A real control-plane LB demo requires a BIG-IP VS on `10.1.1.25:443` or `:6443` that forwards to shoot kube-apiserver backends that BIG-IP can reach.
- In this kind-based setup, the shoot kube-apiserver pods are not automatically reachable from BIG-IP networks.

### Network-team handoff (exact allowlist)
To unblock CIS, we need egress from the shoot worker network to the BIG-IP **management** endpoint.

Important note on source IPs:
- In this local kind-based setup, shoot pod/node IPs are *internal* to the cluster networking and egress is typically **NAT'd** by the VM/network.
- The cloud firewall will usually see the **VM egress IP** as the source (not the shoot node InternalIP).
- We confirmed the VM egress source towards `100.65.242.181` via `ip route get 100.65.242.181`.

Destination:
- `100.65.242.181:443` (BIG-IP mgmt / AS3 endpoint)

Source (what the firewall is likely to see in this environment):
- VM egress source IP: `10.10.2.171`

Source (shoot-internal context; often NAT'd and not directly usable for firewall rules):
- Worker node InternalIP: `10.250.130.192`
- Pod CIDR: `100.96.0.0/24`

Ask: allow `10.10.2.171` to reach `100.65.242.181:443`.

Verification after the network change (from inside the shoot):

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system run netcheck \
  --image=curlimages/curl:8.5.0 --restart=Never -it --rm -- \
  sh -lc 'curl -k -sS --connect-timeout 5 --max-time 10 https://100.65.242.181/mgmt/shared/appsvcs/info'
```

## What we will do next (step-by-step)
1) Credentials Secret: created (done).

The controller reads:
- `username` from Secret key `username`
- `password` from Secret key `password`

Fastest option (note: this may end up in shell history):

```bash
kubectl -n shoot--local--local create secret generic cmp-f5-credentials \
  --from-literal=username='<F5-USER>' \
  --from-literal=password='<F5-PASS>' \
  --dry-run=client -o yaml | kubectl apply -f -
```

File-based option (avoids putting the secret on the command line):
- Edit the placeholders in [deploy/kind/cmp-f5-credentials.secret.yaml](deploy/kind/cmp-f5-credentials.secret.yaml)
- Apply it:

```bash
kubectl apply -f deploy/kind/cmp-f5-credentials.secret.yaml
```

Verify it exists (does not print the password):

```bash
kubectl -n shoot--local--local get secret cmp-f5-credentials
kubectl -n shoot--local--local get secret cmp-f5-credentials -o jsonpath='{.data.username}' | wc -c
kubectl -n shoot--local--local get secret cmp-f5-credentials -o jsonpath='{.data.password}' | wc -c
echo
```

2) Force a reconcile (optional; usually the controller will reconcile automatically):

```bash
kubectl -n shoot--local--local annotate extension f5 \
  f5.extensions.gardener.cloud/reconcile-at="$(date -Is)" --overwrite
```

3) Watch the config status until Application LB becomes ready:

```bash
kubectl -n shoot--local--local get f5loadbalancerconfig f5 -o yaml | sed -n '1,260p'
```

4) Verify CIS resources appear in the shoot (done; shown in demo via shoot kubeconfig):

```bash
# Namespace / secret / deployment (names depend on controller implementation)
kubectl get ns | grep -E 'f5|cis' || true
kubectl -A get deploy | grep -E 'f5|cis' || true
kubectl -A get secret | grep -E 'f5|cis' || true
```

5) Verify CIS pod health + connectivity to BIG-IP (remaining blocker):

```bash
# This requires access to the shoot cluster. In our demo we use a port-forward
# to the shoot kube-apiserver and a temporary kubeconfig in /tmp.
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system get pods
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system logs deploy/f5-cis --previous --tail=120
```

6) Fix connectivity (what likely needs changing outside Kubernetes):
- Allow egress from shoot worker nodes (CIDR) to BIG-IP mgmt IP `100.65.242.181:443` (routes / firewall / security groups / ACLs).
- Ensure BIG-IP mgmt is reachable from that network and AS3 endpoint is enabled/available.

7) (Optional, if control-plane LB must be demoed) expose kube-apiserver to BIG-IP and create a dedicated VS:
- Expose an endpoint that BIG-IP can actually reach (not a local `kubectl port-forward`).
- Configure BIG-IP VS on `10.1.1.25:443` or `:6443` to that endpoint.
- Verify with `curl -k https://10.1.1.25:443/version` (or `:6443`).

## Demo walkthrough (what to show today)

## Dry run (copy/paste script)

Run this once before the real demo so there are no surprises.

### Terminal A (keep running): shoot kube-apiserver port-forward

1) Find the kube-apiserver pod in the shoot technical namespace:

```bash
kubectl -n shoot--local--local get pods -o wide | grep -E 'kube-apiserver|apiserver'
```

2) Start the port-forward (replace the pod name):

```bash
kubectl -n shoot--local--local port-forward pod/<kube-apiserver-pod> 9443:443
```

Expected output:
- `Forwarding from 127.0.0.1:9443 -> 443`

Keep this terminal open for the whole demo.

Important:
- Do **not** run any other commands in Terminal A after starting the port-forward.
- If you accidentally type into Terminal A, or the connection drops, the port-forward can terminate and all `--kubeconfig /tmp/shoot-direct.kubeconfig` commands will start failing with `connection refused`.

If port-forward breaks, recover with:

```bash
# (run in Terminal B) confirm whether 9443 is still listening
ss -lntp | grep ':9443' || echo '9443 not listening'

# (run in Terminal A) restart the port-forward
kubectl -n shoot--local--local port-forward pod/<kube-apiserver-pod> 9443:443
```

### Terminal B: everything else

0) Quick sanity checks:

```bash
kubectl -n garden-f5-system get deploy gardener-extension-f5 -o wide
kubectl -n garden-f5-system get pods -l app=gardener-extension-f5 -o wide
kubectl -n shoot--local--local get extension f5 -o wide
kubectl -n shoot--local--local get f5loadbalancerconfig f5 -o wide
```

1) Show controller logs (seed side):

```bash
# Tip: during the demo, highlight the “Reconciled CIS in Shoot” line.
# You may also see intermittent errors like “deployment f5-cis-system/f5-cis not ready within 1m0s”
# because CIS is restarting due to the BIG-IP mgmt timeout; this does not change the root blocker.
kubectl -n garden-f5-system logs deploy/gardener-extension-f5 --tail=80

# Optional: show only the key reconciliation lines.
kubectl -n garden-f5-system logs deploy/gardener-extension-f5 --since=24h | grep -E 'Reconciling F5 extension|Reconciled CIS in Shoot|deployment f5-cis-system/f5-cis not ready' || true
```

2) Show the config + readiness (shoot technical namespace):

```bash
kubectl -n shoot--local--local get f5loadbalancerconfig f5 -o yaml | sed -n '1,260p'
```

3) Show CIS resources exist (shoot cluster, via port-forward kubeconfig):

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig get ns f5-cis-system
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system get deploy,pods,secret -o wide
```

4) Show blocker proof (CIS logs + direct curl):

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system logs deploy/f5-cis --previous --tail=80

kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system run netcheck \
  --image=curlimages/curl:8.5.0 --restart=Never -it --rm -- \
  sh -lc 'curl -k -sS --connect-timeout 5 --max-time 10 https://100.65.242.181/mgmt/shared/appsvcs/info'
```

5) Optional: show VIP `10.1.1.25` responds on port 80 (infra VS exists):

```bash
curl -sS -D - -o /dev/null --connect-timeout 3 --max-time 5 http://10.1.1.25:80/ || true
```

### What you can confidently demo today (even with the BIG-IP mgmt blocker)
- The extension controller is running in the seed (kind) cluster and reconciling.
- The shoot has the `Extension` + `F5LoadBalancerConfig` objects.
- The controller successfully deployed CIS into the shoot (`f5-cis-system`, `f5-cis`, `f5-cis-credentials`).
- The current blocker is purely **network reachability to BIG-IP mgmt/AS3** (`100.65.242.181:443`) and is independently reproducible from inside the shoot.
- The control-plane “Ready” condition in this repo is an **ack** (`spec.controlPlaneReady=true`) and is not proof of a working kube-apiserver LB.

### 0) Pre-req: make sure shoot access is set up (port-forward)

We use a port-forward to the shoot kube-apiserver on localhost `:9443` and a kubeconfig at `/tmp/shoot-direct.kubeconfig`.

Keep the port-forward terminal running (do not stop it during the demo).

### 1) Show controller is running (seed/garden side)

```bash
kubectl -n garden-f5-system get deploy,pods -l app=gardener-extension-f5 -o wide
kubectl -n garden-f5-system logs deploy/gardener-extension-f5 --tail=40
```

What to say:
- “The extension controller is running inside the kind-based Gardener dev cluster and is reconciling the `Extension` resource.”

### 2) Show the Gardener extension trigger object

```bash
kubectl -n shoot--local--local get extension f5 -o yaml | sed -n '1,120p'
```

What to say:
- “This `Extension` object is what tells Gardener to run the F5 extension reconciliation for this shoot.”

### 3) Show the config and current readiness

```bash
kubectl -n shoot--local--local get f5loadbalancerconfig f5 -o yaml | sed -n '1,260p'
```

What to call out:
- `ControlPlaneLoadBalancerReady=True` can be either out-of-band ack (`spec.controlPlaneReady=true`) or controller-driven CMP/CCP provisioning (when `spec.ccpApiEndpoint` is set).
- `ApplicationLoadBalancerReady=True` (the controller created CIS resources in the shoot).

### 3.1) Show CIS resources in the shoot (strong proof)

We use a port-forward to the shoot kube-apiserver on localhost `:9443` (you’ll see “Forwarding from 127.0.0.1:9443”). Keep that terminal running.

Note about VS Code “Forwarded Ports”:
- `9443` is our **temporary** `kubectl port-forward` to the shoot kube-apiserver (`9443:443`). This is what `/tmp/shoot-direct.kubeconfig` talks to.
- `5001` is the local Docker registry used by the kind-based environment (for pushing/pulling images).
- `8443` is a published kind-node port mapping to the `istio-ingressgateway` NodePort (it is not related to BIG-IP or CIS).
- `32379` is a published kind-node port mapping but typically not needed for this demo.

```bash
kubectl -n shoot--local--local port-forward pod/<kube-apiserver-pod> 9443:443
```

Then, with a demo kubeconfig (already prepared in this environment as `/tmp/shoot-direct.kubeconfig`), show:

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig get ns f5-cis-system
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system get deploy,pods,secret
```

Call out what you see:
- Namespace `f5-cis-system` exists.
- Deployment `f5-cis` exists.
- Secret `f5-cis-credentials` exists (copied from seed-side credentials).

### 3.2) Be transparent about what’s left

Right now, CIS is crashing because it can’t reach BIG-IP mgmt from the shoot network.

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system logs deploy/f5-cis --previous --tail=80
```

What to say:
- “CIS is deployed, but it cannot reach BIG-IP mgmt/AS3, so it cannot program BIG-IP yet. This is a networking/allowlist/routing issue outside the cluster.”

Optional hard proof (direct connectivity check from inside shoot):

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system run netcheck \
  --image=curlimages/curl:8.5.0 --restart=Never -it --rm -- \
  sh -lc 'curl -k -sS --connect-timeout 5 --max-time 10 https://100.65.242.181/mgmt/shared/appsvcs/info'
```

### 4) (Optional) Show the VIP is alive at infra level

This does not prove kube-apiserver LB (ports depend on CMP VS config), but it proves the VIP is reachable.

```bash
curl -v --connect-timeout 3 --max-time 5 http://10.1.1.25:80/ || true
nc -vz -w 3 10.1.1.25 777 || true
```

What to say:
- “The VIP/VS exists (port 80 returns 200), but this is not the kube-apiserver LB.”

### Can we show *control-plane LB* today with the current VIP/VS?

You can only demo **real** control-plane LB if the BIG-IP Virtual Server on `10.1.1.25` is configured to forward **Kubernetes API traffic** to the shoot kube-apiserver backends.

In practice that means:
- VS listens on `10.1.1.25:443` or `10.1.1.25:6443`.
- Pool members are the shoot kube-apiserver endpoints (whatever is correct in your setup) and the health monitor is passing.

If your current CMP VS on `10.1.1.25` is instead pointing to the VM on app ports like `80`/`777`, then it’s **not** a control-plane LB and you should not present it as such.

Fast verification (run from a host that can reach the VIP):

```bash
# A working control-plane LB returns Kubernetes version JSON.
curl -k -sS --connect-timeout 3 --max-time 5 https://10.1.1.25:443/version || true
curl -k -sS --connect-timeout 3 --max-time 5 https://10.1.1.25:6443/version || true
```

What “good” looks like:
- HTTP 200 and JSON like `{ "major": "1", "minor": "..." }`.

If it times out / refuses / or returns something unrelated to Kubernetes:
- The VIP may be up, but the control-plane LB path is not configured correctly yet.

## One message to send after the demo (blocker handoff)

We need network reachability for BIG-IP mgmt/AS3.

- Destination: `100.65.242.181:443` (BIG-IP mgmt)
- Source (firewall-visible in this environment): `10.10.2.171` (VM egress IP)
- Proof: from inside the shoot, connect to `100.65.242.181:443` times out; CIS logs show timeouts to `/mgmt/shared/appsvcs/info`.

### 5) Explain what’s left / how we finish

What’s left to complete the application-plane story end-to-end:
1) Fix network reachability from the shoot cluster to BIG-IP mgmt (`https://100.65.242.181:443`).
2) Confirm BIG-IP has AS3 endpoint available (`/mgmt/shared/appsvcs/info`).
3) Once CIS stays Running, create a test app Service/Ingress in the shoot and confirm BIG-IP gets a new app VIP/VS and traffic works.

## Post-unblock: end-to-end application LB test checklist

Do this after Blocker 1 is fixed (CIS can reach BIG-IP mgmt) to get an actual “works end-to-end” proof.

### 1) Confirm CIS is healthy and talking to BIG-IP

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system get pods -l app=f5-cis -o wide
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system logs deploy/f5-cis --tail=120
```

What you want to see:
- No CrashLoopBackOff.
- No repeated timeouts to `/mgmt/shared/appsvcs/info`.

### 2) Deploy a tiny demo app in the shoot

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig create ns demo || true

kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n demo create deploy echo \
  --image=hashicorp/http-echo:1.0 \
  -- /http-echo -text='hello from shoot'

kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n demo expose deploy echo \
  --port 5678 --target-port 5678 --type ClusterIP

kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n demo get deploy,svc,pods -o wide
```

### 3) Create an Ingress that CIS should watch

```bash
cat <<'YAML' | kubectl --kubeconfig /tmp/shoot-direct.kubeconfig apply -f -
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo
  namespace: demo
spec:
  rules:
  - host: echo.demo.local
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: echo
            port:
              number: 5678
YAML

kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n demo get ingress echo -o yaml | sed -n '1,180p'
```

### 4) Validate CIS reacted

```bash
kubectl --kubeconfig /tmp/shoot-direct.kubeconfig -n f5-cis-system logs deploy/f5-cis --tail=200
```

Expected: log lines that indicate it processed the `demo/echo` Service/Ingress and attempted to post/update config on BIG-IP (exact strings vary by CIS version).

### 5) Final proof (requires BIG-IP visibility)

One of these must be demonstrated:
- BIG-IP UI/AS3 shows a new application VS/VIP created for the Ingress, OR
- You can curl the resulting VIP and get `hello from shoot`.

## Action items (do later)

### Remove the need for `kubectl port-forward` in this dev environment

Today we use a local port-forward (`127.0.0.1:9443`) to reach the shoot kube-apiserver for demo commands.

Action: explore whether the shoot kube-apiserver can be exposed in a stable way (e.g., NodePort / published kind port / ingress) so that `kubectl` to the shoot works without a port-forward.

Suggested investigation commands:

```bash
kubectl -n shoot--local--local get svc | grep -E 'apiserver|kube-apiserver' || true
kubectl -n shoot--local--local get svc kube-apiserver -o yaml | sed -n '1,220p'

# If kind is used, check published ports on the kind control-plane container
kind get nodes --name gardener-local
docker port gardener-local-control-plane || true
```

### Replace manual controller install with Helm (target)

This repo now has a minimal Helm chart for the controller under `charts/gardener-extension-f5`.

Kind/dev install example:

```bash
helm upgrade --install gardener-extension-f5 ./charts/gardener-extension-f5 \
  -n garden-f5-system --create-namespace \
  --set image.repository=gardener-extension-f5 \
  --set image.tag=dev

Notes on sequencing:
- In a real Gardener landscape, install the extension controller **before** creating shoots (it’s a shared component, not per-shoot).
- For this local demo, installing it after the shoot exists is still OK; it will reconcile once it sees the `Extension`/`F5LoadBalancerConfig` objects.
```

Note:
- The chart currently binds the controller ServiceAccount to `cluster-admin` for the demo environment. Production should replace this with least-privilege RBAC.

## FAQ

### “Should we use the default seed or create a separate seed for integrations (e.g., external-dns-management)?”

For local dev and a single shoot scheduled to the default seed (e.g., `local`), use the existing default seed. Creating a separate seed is only necessary if you specifically need isolation, different infrastructure/network reachability, or you want to test multi-seed behavior.

For this F5 extension demo, using the default seed is fine because:
- The extension controller runs in the (kind) cluster and reconciles shoots scheduled to the seed.
- CIS is deployed into the shoot cluster once prerequisites (credentials) exist.

## Notes
- Earlier, we hit a Kubernetes version compatibility problem (shoot Kubernetes v1.33.7) and a missing credentials Secret; those are resolved.
- Current remaining blocker for end-to-end app LB: shoot pods cannot reach BIG-IP mgmt `100.65.242.181:443` (TCP connect timeout), so CIS cannot program BIG-IP yet.

VM to bIG IP 
curl -k -u admin -sS -D- --connect-timeout 5 --max-time 10 \
  https://100.65.242.181/mgmt/shared/appsvcs/info