# Gardener Extension F5 Demo (Feb 24, 2026)

Scope of this demo:
- Show Seed-side controller is deployed and reconciling.
- Show Seed-side objects (`Extension`, `F5LoadBalancerConfig`) that drive reconciliation.
- Show Shoot-side CIS deployment exists and is configured.
- Prove CIS → BIG-IP mgmt/AS3 connectivity and authentication.


Assumptions in this runbook (edit inline in commands if your setup differs):
- Seed kube context: `kind-gardener-local`
- Shoot technical namespace: `shoot--local--local`
- BIG-IP mgmt IP: `100.65.242.181`

---

## 1) Show the controller is deployed (Seed side)

```bash
# Deployment
kubectl --context kind-gardener-local -n garden-f5-system get deploy gardener-extension-f5 -o wide

# Pods
kubectl --context kind-gardener-local -n garden-f5-system get pod -l app=gardener-extension-f5 -o wide
```

Optional: show a tiny log snippet.

```bash
kubectl --context kind-gardener-local -n garden-f5-system logs deploy/gardener-extension-f5 --tail=80
```


- “This is the Seed-side extension controller (runs in the Seed cluster).”
- “It watches Gardener `Extension` resources and reconciles per-shoot desired state.”
- “Takeaway: controller is running and ready to act when we create/update `Extension` + config.”

---

## 2) Show the objects it reconciles (shoot technical namespace)

```bash
kubectl --context kind-gardener-local -n shoot--local--local get extension/f5 f5loadbalancerconfig/f5 -o wide
```

- “`Extension/f5` is the Gardener reconciliation trigger for this extension type.”
- “`F5LoadBalancerConfig/f5` is our per-shoot input: BIG-IP mgmt URL, partition, and where creds come from.”
- “Takeaway: the Seed already has the two objects that drive CIS deployment into the Shoot.”

---

## 3) Show CIS config inputs (BIG-IP URL / partition / insecure workaround)

```bash
kubectl --context kind-gardener-local -n shoot--local--local get f5loadbalancerconfig/f5 \
  -o jsonpath='bigipUrl={.spec.cis.bigipUrl}{"\n"}partition={.spec.cis.partition}{"\n"}extraArgs={.spec.cis.extraArgs}{"\n"}'
```

Note: the `{...}` above are JSONPath expressions (don’t replace them with literal values).

Expected:
- `bigipUrl` like `https://<mgmt-ip>`
- `partition`
- `extraArgs` contains `--insecure=true` (demo workaround for TLS IP-SAN mismatch)

Example output (this demo environment):

```text
bigipUrl=https://100.65.242.181
partition=k8s-apps
extraArgs=[--insecure=true]
```


- “Here we show the exact BIG-IP endpoint and partition CIS will use.”

---

## 4) Ensure provider-local NetworkPolicy allows Shoot → BIG-IP mgmt/443

In provider-local, shoot traffic is subject to Seed namespace NetworkPolicies.

```bash
kubectl --context kind-gardener-local apply -f /home/ojasvi/gardener-extension-f5/deploy/kind/allow-shoot-to-bigip-mgmt.networkpolicy.yaml
kubectl --context kind-gardener-local -n shoot--local--local get networkpolicy | grep -i bigip || true
```


- “In provider-local, Shoot egress is affected by Seed namespace NetworkPolicies.”
- “Without this allow, CIS cannot reach BIG-IP mgmt/AS3 and you see timeouts/connection errors.”
- “Takeaway: we’ve explicitly allowed Shoot → BIG-IP mgmt/443.”

---

## 5) Access the Shoot API from your VM (port-forward)

Start a port-forward to the shoot kube-apiserver service (keep it running):

```bash
kubectl --context kind-gardener-local -n shoot--local--local port-forward --address 127.0.0.1 svc/kube-apiserver 7443:443
```

If you want an auto-restarting supervisor (recommended for longer demos):

```bash
PF_LOCAL_PORT=7443 PF_NAMESPACE=shoot--local--local /home/ojasvi/gardener-extension-f5/scripts/shoot-apiserver-portforward.sh
```

Quick sanity check (should print `401` if reachable):

```bash
curl -sk -o /dev/null -w '%{http_code}\n' https://127.0.0.1:7443/
```

In a second terminal, create a kubeconfig that talks to localhost (token-based):

```bash
TOKEN=$(kubectl --context kind-gardener-local -n shoot--local--local get secret shoot-access-gardener-resource-manager -o jsonpath='{.data.token}' | base64 -d)

cat > /tmp/shoot-pf.kubeconfig <<EOF
apiVersion: v1
kind: Config
clusters:
- name: shoot
  cluster:
    server: https://127.0.0.1:7443
    insecure-skip-tls-verify: true
users:
- name: shoot-access
  user:
    token: ${TOKEN}
contexts:
- name: shoot
  context:
    cluster: shoot
    user: shoot-access
current-context: shoot
EOF

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig get ns | head
```


- “We’re accessing the Shoot API from the VM by port-forwarding the Shoot’s `kube-apiserver` service.”
- “We generate a kubeconfig using a Shoot-access token so we don’t hit RBAC issues (some kubeconfigs are intentionally low-privilege).”
- “Takeaway: we can now query real Shoot resources with `--kubeconfig /tmp/shoot-pf.kubeconfig`.”

---

## 6) Show CIS is deployed in the Shoot

```bash
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig get ns f5-cis-system
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system get deploy,pod -o wide
```

If `deploy/f5-cis` is missing, recreate the Extension trigger:

```bash
kubectl --context kind-gardener-local apply -f /home/ojasvi/gardener-extension-f5/deploy/kind/extension-f5.yaml
```

Wait ~20–40s and rerun the CIS check.


- “Now we’re inside the Shoot cluster: CIS should be deployed in namespace `f5-cis-system`.”
- “If the Deployment is `1/1` and the CIS pod is `Running`, the controller successfully pushed Shoot-side resources.”
- “Takeaway: extension reconciliation resulted in CIS being present in the Shoot.”

---

## 7) Show CIS runtime args (how it connects)

```bash
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system get deploy f5-cis \
  -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}'
```

What to point out:
- `--bigip-url=https://...` comes from `F5LoadBalancerConfig.spec.cis.bigipUrl`
- `--bigip-partition=...` comes from `F5LoadBalancerConfig.spec.cis.partition`
- `--bigip-username=$(BIGIP_USERNAME)` / `--bigip-password=$(BIGIP_PASSWORD)` are env vars sourced from a Shoot secret created by the controller
- `--insecure=true` is the demo workaround (TLS IP-SAN mismatch)


- “This is the ‘wiring proof’: CIS args show it is configured from our `F5LoadBalancerConfig`.”
- “You can point to the exact values: BIG-IP URL, partition, and that creds are injected via env.”
- “Takeaway: runtime configuration matches the intended per-shoot config.”

---

## 8) Prove CIS → BIG-IP mgmt/AS3 works (real proof)

Run from inside the CIS pod:

```bash
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system exec deploy/f5-cis -- sh -lc '
  curl -ksS -u "${BIGIP_USERNAME}:${BIGIP_PASSWORD}" \
    https://100.65.242.181/mgmt/shared/appsvcs/info \
    -o /tmp/as3.json -w "http_code=%{http_code}\n" && head -c 200 /tmp/as3.json && echo'
```

Expected:
- `http_code=200` and JSON snippet that includes AS3/version information.

If you see `http_code=404`, AS3 is not installed on BIG-IP. In that case CIS (AS3 agent) cannot program BIG-IP; use the manual LTM demo path instead:

```bash
# Deploy a demo Service in the Shoot
VIP=100.72.200.30 envsubst < config/samples/app-plane-echo-lb.yaml | kubectl --kubeconfig /tmp/shoot-pf.kubeconfig apply -f -

# Create/update BIG-IP LTM objects via iControl REST from an in-cluster curl pod
KUBECONFIG=/tmp/shoot-pf.kubeconfig \
SERVICE_NS=demo-lb SERVICE_NAME=echo-lb VIP=100.72.200.30 \
BIGIP_HOST=100.72.44.146 BIGIP_USER=admin BIGIP_PASS='***' \
PARTITION=k8s-apps \
./scripts/bigip-ltm-service-vs.sh
```


- “This is the strongest proof point: from inside the CIS pod we hit BIG-IP AS3 over HTTPS and authenticate.”
- “`http_code=200` confirms network reachability + credentials correctness (not just DNS/port open).”
- “Takeaway: Shoot-side CIS → BIG-IP mgmt/AS3 is working end-to-end.”

---

## Optional: Application-plane load balancing (what users create)

Important note about this demo setup:
- CIS is running with the default `--agent=as3` and is watching `Ingress` + `ConfigMap`.
- By default, a plain `Service type=LoadBalancer` will NOT program BIG-IP here.
- If you enable the optional **ServiceType=LB bridge** (`spec.cis.bridgeImage`), then a `Service type=LoadBalancer` becomes a user-facing trigger:
  - the bridge creates an equivalent annotated `Ingress` (so CIS programs BIG-IP), and
  - the bridge mirrors the VIP into the Service status (so `kubectl get svc` shows an `EXTERNAL-IP`).

### 0) (One-time) Enable Service type=LoadBalancer support

Set `spec.cis.bridgeImage` to an image that contains the `/svc-lb-bridge` binary (typically the same image as the extension controller after rebuilding/publishing).

Example (this repo’s default extension image repo/tag):

```bash
kubectl --context kind-gardener-local -n shoot--local--local patch f5loadbalancerconfig/f5 --type merge -p \
  '{"spec":{"cis":{"bridgeImage":"europe-docker.pkg.dev/gardener-project/public/gardener/extensions/f5:latest"}}}'

# Note: the Seed-side controller reconciles `Extension/f5`. If the bridge Deployment
# does not appear, (re-)apply the Extension trigger or force a reconcile:
kubectl --context kind-gardener-local -n shoot--local--local apply -f /home/ojasvi/gardener-extension-f5/deploy/kind/extension-f5.yaml

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system get deploy f5-svc-lb-bridge -o wide
```

### A) Create a small demo app + Ingress (Shoot)

```bash
# Demo app
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig create ns demo-lb --dry-run=client -o yaml | kubectl --kubeconfig /tmp/shoot-pf.kubeconfig apply -f -

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
spec:
  replicas: 2
  selector:
    matchLabels:
      app: echo
  template:
    metadata:
      labels:
        app: echo
    spec:
      containers:
      - name: echo
        image: hashicorp/http-echo:0.2.3
        args:
        - "-text=hello-from-shoot"
        - "-listen=:5678"
        ports:
        - containerPort: 5678
EOF

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb rollout status deploy/echo

# ClusterIP service (Ingress backend)
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb apply -f - <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: echo-svc
spec:
  selector:
    app: echo
  ports:
  - name: http
    port: 80
    targetPort: 5678
EOF

# Ingress (this is what CIS watches)
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: echo
  annotations:
    kubernetes.io/ingress.class: f5
    virtual-server.f5.com/ip: "10.1.1.25"
spec:
  rules:
  - host: echo.demo.local
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: echo-svc
            port:
              number: 80
EOF

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb get pod,svc,ingress -o wide
```

### A2) Alternative: create `Service type=LoadBalancer` (bridge-enabled flow)

This is the “what app teams expect”: they create a `Service type=LoadBalancer` and specify a VIP.

Shortcut (uses the repo sample manifest):

```bash
VIP=10.1.1.25 envsubst < config/samples/app-plane-echo-lb.yaml | kubectl --kubeconfig /tmp/shoot-pf.kubeconfig apply -f -
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb get svc echo-lb -o wide
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb get ingress -o wide | grep -i echo-lb || true
```

```bash
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb apply -f - <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: echo-lb
  annotations:
    cis.f5.com/ip: "10.1.1.25"
spec:
  selector:
    app: echo
  type: LoadBalancer
  ports:
  - name: http
    port: 8081
    targetPort: 5678
EOF

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb get svc echo-lb -o wide
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-lb get ingress -o wide | grep -i echo-lb || true
```

Talk track:
- “User creates an Ingress for their app; CIS translates that into BIG-IP config.”
- “This is the application-plane load balancing flow for this demo setup (AS3 + Ingress).”

### B) Optional: traffic test (if VIP is reachable from your VM)

```bash
curl -sS -H 'Host: echo.demo.local' http://10.1.1.25/ | head
```

### C) Proof on BIG-IP (works even if VIP isn’t directly reachable)

This queries BIG-IP iControl REST from inside the CIS pod and filters for the VIP.

```bash
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system exec deploy/f5-cis -c cis -- \
  sh -lc 'curl -sk -u "$BIGIP_USERNAME:$BIGIP_PASSWORD" "https://100.65.242.181/mgmt/tm/ltm/virtual?\$select=fullPath,destination" | tr "," "\n" | grep -E "destination|fullPath" | grep -A1 "10.1.1.25" || true'
```

---

## Closing line

- “Today we demonstrated Seed-side reconciliation + Shoot-side CIS deployment + verified BIG-IP mgmt/AS3 connectivity.”


Seed kube context (name): kind-gardener-local

Seed extension namespace: garden-f5-system

Shoot (name): local--local (you see it inside the technical namespace name)

Shoot technical namespace (in Seed): shoot--local--local

CIS namespace (in Shoot cluster): f5-cis-system