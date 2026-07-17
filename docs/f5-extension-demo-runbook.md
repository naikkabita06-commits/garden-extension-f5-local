# Demo: Gardener baseline vs F5 extension (exact commands)

Target environment (from your VM):
- Kubernetes context: `kind-gardener-local`
- Shoot: `garden-local/local`
- Shoot technical namespace (on the Seed): `shoot--local--local`
- F5 extension namespace (on the Seed): `extension-gardener-extension-f5-zn8sj`

> Safe-by-default: read-only unless explicitly marked **(MUTATING)**.
> Do **not** print Secret contents or tokens during the demo.

---

## Part 1 — What Gardener does (baseline)

### 1.1 Control-plane endpoint / “control-plane loadbalancing” (Gardener)

Show the Shoot health + advertised API endpoints (these are what clients use):

```bash
kubectl --context kind-gardener-local get shoot -A -o wide

kubectl --context kind-gardener-local -n garden-local get shoot local \
  -o jsonpath='{.status.technicalID}{"\n"}{.status.advertisedAddresses}{"\n"}{.spec.dns}{"\n"}'
```

Show the Shoot’s kube-apiserver Service that exists in the technical namespace (Seed):

```bash
kubectl --context kind-gardener-local -n shoot--local--local get svc kube-apiserver -o wide
kubectl --context kind-gardener-local -n shoot--local--local get endpoints kube-apiserver -o yaml | sed -n '1,160p'
```

Show the Seed ingress gateway Service (this is what fronts the Shoot API DNS names in this kind setup):

```bash
kubectl --context kind-gardener-local -n istio-ingress get svc istio-ingressgateway -o wide
kubectl --context kind-gardener-local -n istio-ingress get svc istio-ingressgateway \
  -o jsonpath='{.status.loadBalancer.ingress}{"\n"}{.spec.ports}{"\n"}'
```

### 1.2 Application-plane loadbalancing (Gardener baseline)

In this kind environment, **without using the F5 loadBalancerClass**, a `Service type=LoadBalancer` typically stays in `EXTERNAL-IP=<pending>`.

(We’ll prove this in Part 3 by creating a test Service without the F5 class.)

---

## Part 2 — Deploy / show deployment of the F5 extension (Seed side)

### 2.1 Show the extension is installed and running

```bash
# Pods anywhere
kubectl --context kind-gardener-local get pods -A -o wide | egrep -i 'gardener-extension-f5|seed-service-lb' || true

# Deployments + pods in the extension namespace
kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj get deploy,pods -o wide

# Image (exact version running)
kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj get deploy gardener-extension-f5 \
  -o jsonpath='{.spec.template.spec.containers[0].image}{"\n"}'

# Logs (tail)
kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj logs deploy/gardener-extension-f5 --tail=200
```

### 2.2 (Optional) “deploy again” to demonstrate rollout (MUTATING: restarts pods)

```bash
kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj rollout restart deploy/gardener-extension-f5
kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj rollout status deploy/gardener-extension-f5

kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj rollout restart deploy/gardener-extension-f5-seed-service-lb
kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj rollout status deploy/gardener-extension-f5-seed-service-lb
```

---

## Part 3 — What the F5 extension adds

### 3.1 Create Shoot kubeconfig at /tmp/shoot-pf.kubeconfig (to show Shoot-side CIS/bridge)

Terminal 1 (keep running):

```bash
kubectl --context kind-gardener-local -n shoot--local--local port-forward --address 127.0.0.1 svc/kube-apiserver 7443:443
```

Terminal 2:

```bash
# Sanity check (should print 401 if reachable)
curl -sk -o /dev/null -w '%{http_code}\n' https://127.0.0.1:7443/

# Create a kubeconfig that talks to the port-forward using a shoot-access token
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
    token: $(kubectl --context kind-gardener-local -n shoot--local--local get secret shoot-access-gardener-resource-manager -o jsonpath='{.data.token}' | base64 -d)
contexts:
- name: shoot
  context:
    cluster: shoot
    user: shoot-access
current-context: shoot
EOF

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig get ns | head
```

### 3.2 Control-plane LB (F5 extension control-plane status + IDs)

```bash
kubectl --context kind-gardener-local -n shoot--local--local get extension -o wide
kubectl --context kind-gardener-local -n shoot--local--local get f5loadbalancerconfig -o wide

kubectl --context kind-gardener-local -n shoot--local--local describe \
  $(kubectl --context kind-gardener-local -n shoot--local--local get f5loadbalancerconfig -o name | head -n 1) \
  | sed -n '1,220p'

kubectl --context kind-gardener-local -n shoot--local--local get \
  $(kubectl --context kind-gardener-local -n shoot--local--local get f5loadbalancerconfig -o name | head -n 1) \
  -o jsonpath='{range .status.conditions[*]}{.type}{"="}{.status}{"  reason="}{.reason}{"\n"}{end}'

kubectl --context kind-gardener-local -n shoot--local--local get \
  $(kubectl --context kind-gardener-local -n shoot--local--local get f5loadbalancerconfig -o name | head -n 1) \
  -o jsonpath=$'VIP={.status.vip}{"\n"}LBServiceID={.status.lbServiceID}{"\n"}VIPPortID={.status.vipPortID}{"\n"}VirtualServerID={.status.virtualServerID}{"\n"}'
```

### 3.3 Seed ingress LB (F5 seed-service-lb-controller sets Service.status → gardenlet updates Seed)

```bash
# Seed-side LB controller logs
kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj logs deploy/gardener-extension-f5-seed-service-lb --tail=200

# Service status shows VIP (this is what gardenlet watches)
kubectl --context kind-gardener-local -n istio-ingress get svc istio-ingressgateway -o wide
kubectl --context kind-gardener-local -n istio-ingress get svc istio-ingressgateway \
  -o jsonpath='{.spec.loadBalancerClass}{"\n"}{.status.loadBalancer.ingress}{"\n"}'

# Seed object reflects ingress addresses
kubectl --context kind-gardener-local get seed -o wide
kubectl --context kind-gardener-local get seed -o yaml | egrep -n 'ingress|loadBalancer|loadbalancer|addresses|ip' | head -n 120
```

### 3.4 Application-plane LB (F5 CIS + bridge in Shoot)

Show CIS and the bridge are present in the Shoot:

```bash
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig get ns f5-cis-system
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system get deploy,pods -o wide

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system logs deploy/f5-cis --tail=200
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system logs deploy/f5-svc-lb-bridge --tail=200 2>/dev/null || true

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n f5-cis-system get deploy f5-cis \
  -o jsonpath='{.spec.template.spec.containers[0].args}{"\n"}'
```

### 3.5 (MUTATING) Baseline app-plane test: Service type=LoadBalancer without F5 class stays pending

```bash
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig create ns demo-baseline --dry-run=client -o yaml | kubectl --kubeconfig /tmp/shoot-pf.kubeconfig apply -f -

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-baseline create deployment whoami --image=traefik/whoami:v1.10.2 --dry-run=client -o yaml | kubectl --kubeconfig /tmp/shoot-pf.kubeconfig apply -f -

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-baseline expose deployment whoami --port 80 --target-port 80 --type LoadBalancer --name whoami-lb

kubectl --kubeconfig /tmp/shoot-pf.kubeconfig -n demo-baseline get svc whoami-lb -o wide
```

### 3.6 End-to-end traffic proof (Application-plane LB)

```bash
# End-to-end application-plane loadbalancing proof
for i in $(seq 1 10); do
  curl -sS http://100.72.200.20:8085/
  sleep 1
done
```

---

## Cleanup (only if you created demo resources)

```bash
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig delete ns demo-baseline
kubectl --kubeconfig /tmp/shoot-pf.kubeconfig delete ns demo-lb 2>/dev/null || true
```
