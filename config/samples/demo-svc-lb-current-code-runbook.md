# Service type=LoadBalancer demo runbook

This demo uses four Deployments and four `type: LoadBalancer` Services. It does not create any Ingress resources.

## What the current code can demonstrate

| Comparison | LBService | VIP | Virtual Server |
|---|---|---|---|
| `app-a-svc` vs `app-b-svc` | Different | Different | Different |
| `app-c-svc` vs `app-d-svc` | Same | Same | Different |

The requested case **same LBService, different VIP, different VS** is not available through the current Service annotations/code. The existing `vip-group` mechanism reuses both `LBServiceID` and `VIPPortID`. Supporting that case requires a separate LB-sharing concept, such as `lb-group`, which shares only the LBService ID while allowing each Service to allocate its own VIP.

## 0. Set the Shoot kubeconfig

```bash
export SHOOT_KC=/tmp/shoot-local.kubeconfig
export NS=demo-svc-lb
```

## 1. Pre-check the bridge and configuration

```bash
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system get pod
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system logs deploy/f5-svc-lb-bridge --tail=30
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system get deploy f5-svc-lb-bridge \
  -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep -E '^CMP_(ENDPOINT|VPC_ID|VPC_NAME|NETWORK_ID|LB_FLAVOR_ID)='
```

Confirm the five CMP values are non-empty.

Confirm the Shoot worker node InternalIP values:

```bash
kubectl --kubeconfig "$SHOOT_KC" get nodes \
  -o 'custom-columns=NAME:.metadata.name,INTERNAL-IP:.status.addresses[?(@.type=="InternalIP")].address'
  ```

CMP must be able to resolve these node IPs to compute/network ports. Otherwise LBService/VIP creation may succeed but VS backend creation will fail.

## 2. Save the manifest locally

Use `demo-svc-lb-current-code.yaml`.

## 3. Create the namespace and four applications only

```bash
kubectl --kubeconfig "$SHOOT_KC" apply -f demo-svc-lb-current-code.yaml \
  --prune=false \
  --selector='!demo-step'
```

Because Kubernetes selectors on a multi-document file can be easy to misread, verify what was created:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get deploy,pod
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" wait \
  --for=condition=Available deploy/app-a deploy/app-b deploy/app-c deploy/app-d \
  --timeout=180s
```

If the namespace was skipped by your `kubectl` version, create it once and rerun:

```bash
kubectl --kubeconfig "$SHOOT_KC" create namespace "$NS"
```

## 4. Scenario 1 — different LB, different VIP, different VS

Apply the two standalone Services:

```bash
kubectl --kubeconfig "$SHOOT_KC" apply -f demo-svc-lb-current-code.yaml \
  --selector='demo-step=standalone'
```

Watch both Services:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc app-b-svc -w
```

Expected:

- Both Services leave `<pending>`.
- Their external IPs are different.
- CMP shows two LBServices and two VS objects.

Inspect the bridge:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system \
  logs deploy/f5-svc-lb-bridge -f
```

Inspect Service state and controller annotations:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc app-b-svc -o yaml
```

Quick VIP comparison:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc app-b-svc \
  -o custom-columns=NAME:.metadata.name,VIP:.status.loadBalancer.ingress[0].ip,PORT:.spec.ports[0].port,NODEPORT:.spec.ports[0].nodePort
```

## 5. Scenario 3 parent — create the shared LB/VIP first

Apply only `app-c-svc`:

```bash
kubectl --kubeconfig "$SHOOT_KC" apply -f demo-svc-lb-current-code.yaml \
  --selector='demo-step=shared-parent'
```

Wait until it has an external IP:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-c-svc -w
```

Do not create `app-d-svc` until `app-c-svc` has:

- an external IP, and
- the bridge's observed-state annotation.

Check:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-c-svc \
  -o jsonpath='{.metadata.annotations.f5\.extensions\.gardener\.cloud/observed-graph}{"\n"}'
```

## 6. Scenario 3 child — same LB, same VIP, different VS

Apply `app-d-svc`:

```bash
kubectl --kubeconfig "$SHOOT_KC" apply -f demo-svc-lb-current-code.yaml \
  --selector='demo-step=shared-child'
```

Watch both shared Services:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-c-svc app-d-svc -w
```

Expected:

- `app-c-svc` and `app-d-svc` show the same external IP.
- They use different frontend ports: `9090` and `7070`.
- CMP shows one shared LBService, one shared VIP, and two different VS listeners.

Show all four:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc \
  -o custom-columns=NAME:.metadata.name,VIP:.status.loadBalancer.ingress[0].ip,PORT:.spec.ports[0].port,NODEPORT:.spec.ports[0].nodePort
```

## 7. Functional traffic test

```bash
VIP_A=$(kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
VIP_B=$(kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-b-svc -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
VIP_SHARED=$(kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-c-svc -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

curl -v --connect-timeout 10 "http://${VIP_A}:8080/"
curl -v --connect-timeout 10 "http://${VIP_B}:8081/"
curl -v --connect-timeout 10 "http://${VIP_SHARED}:9090/"
curl -v --connect-timeout 10 "http://${VIP_SHARED}:7070/"
```

Each response should identify the corresponding `whoami` pod.

## 8. Failure diagnosis

Events:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" describe svc app-a-svc
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get events --sort-by=.lastTimestamp
```

Bridge errors:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system \
  logs deploy/f5-svc-lb-bridge --since=10m | grep -iE 'error|fail|forbidden|401|400|network port|backend'
```

Common interpretation:

- `no CMP network port found for backend IP`: worker node is not registered/mappable in CMP.
- `401` or `403`: CMP credential/token problem.
- `400`: inspect CMP response; typically a missing or rejected VPC/network/flavor field.
- `BackendNodePortRequired`: wait for Kubernetes to allocate the NodePort and let reconciliation retry.
- Service remains `<pending>`: inspect Service events and the bridge log together.

## 9. Cleanup

Delete the Services first so the bridge finalizers can remove CMP objects:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" delete svc \
  app-d-svc app-c-svc app-b-svc app-a-svc
```

Wait until all Services are gone:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc
```

Then delete the namespace:

```bash
kubectl --kubeconfig "$SHOOT_KC" delete namespace "$NS"
```
