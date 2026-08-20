# Service type=LoadBalancer demo runbook (Cases 1-3)

This runbook demonstrates only the finalized provisioning cases:

- Case 1: no LB ID, no VIP ID (controller creates/reconciles all resources).
- Case 2: LB ID supplied, VIP ID not supplied (controller must use that LB, then create/recover VIP/VS).
- Case 3: LB ID and VIP ID supplied (controller must use exactly those resources).

Old vip-group sharing scenarios were removed from this demo on purpose.

echo $IMAGE_TAG                                                                                
v0.1.0-dev-gardener-1.139.2-fix1
 docker build  -t europe-docker.pkg.dev/gardener-project/public/gardener/extensions/f5:${IMAGE_TAG} . 

docker tag \
europe-docker.pkg.dev/gardener-project/public/gardener/extensions/f5:${IMAGE_TAG} \
  registry.local.gardener.cloud:5001/extensions/f5:${IMAGE_TAG}

docker push registry.local.gardener.cloud:5001/extensions/f5:${IMAGE_TAG}

IMAGE_REPOSITORY=registry.local.gardener.cloud:5001/extensions/f5 IMAGE_TAG="${IMAGE_TAG}" OUTPUT=deploy/garden/controllerdeployment-f5.yaml bash scripts/generate-controllerdeployment-f5.sh

ControllerDeployment     ← you apply
ControllerRegistration   ← you apply
        ↓
Gardener controller-manager
        ↓
ControllerInstallation   ← Gardener manages this
        ↓
extension deployed into Seed


./hack/usage/generate-admin-kubeconf.sh > /tmp/shoot-local.kubeconfig
export SHOOT_KC=/tmp/shoot-local.kubeconfig
## 0. Environment

```bash
export SHOOT_KC=/tmp/shoot-local.kubeconfig
export NS=demo-svc-lb
```

## 1. Pre-check bridge and CMP defaults

```bash
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system get pod
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system logs deploy/f5-svc-lb-bridge --tail=30
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system get deploy f5-svc-lb-bridge \
  -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep -E '^CMP_(ENDPOINT|VPC_ID|VPC_NAME|NETWORK_ID|LB_FLAVOR_ID)='
```

Confirm worker InternalIP values are visible and routable in CMP:

```bash
kubectl --kubeconfig "$SHOOT_KC" get nodes \
  -o 'custom-columns=NAME:.metadata.name,INTERNAL-IP:.status.addresses[?(@.type=="InternalIP")].address'
```

## 2. Apply base resources (namespace, deployments, Case 1 service)

Use `demo-svc-lb-current-code.yaml`.

```bash
kubectl --kubeconfig "$SHOOT_KC" apply -f demo-svc-lb-current-code.yaml
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" wait \
  --for=condition=Available deploy/app-a deploy/app-b deploy/app-c \
  --timeout=180s
``` /dc.

## 3. Case 1 verification (no supplied IDs)

Watch Case 1 service:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc -w
```

Expected:

- Service leaves `<pending>`.
- Controller annotations are populated:
  - `f5.extensions.gardener.cloud/lb-service-id`
  - `f5.extensions.gardener.cloud/vip-port-id`
  - `f5.extensions.gardener.cloud/observed-graph`

Capture IDs for Cases 2 and 3:

```bash
export CASE1_LB_ID=$(kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc -o jsonpath='{.metadata.annotations.f5\.extensions\.gardener\.cloud/lb-service-id}')
export CASE1_VIP_ID=$(kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc -o jsonpath='{.metadata.annotations.f5\.extensions\.gardener\.cloud/vip-port-id}')
export CASE1_VIP_IP=$(kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

echo "LB=$CASE1_LB_ID VIP_ID=$CASE1_VIP_ID VIP_IP=$CASE1_VIP_IP"
```

## 4. Case 2 (supplied LB, no supplied VIP)

Create `app-b-svc` with only supplied LB ID. This enforces strict LB identity and allows VIP allocation under that LB.

```bash
cat <<EOF | kubectl --kubeconfig "$SHOOT_KC" apply -f -
apiVersion: v1
kind: Service
metadata:
  name: app-b-svc
  namespace: ${NS}
  annotations:
    f5.extensions.gardener.cloud/lb-service-id: "${CASE1_LB_ID}"
    f5.extensions.gardener.cloud/lb-service-id-mode: "provided"
spec:
  type: LoadBalancer
  loadBalancerClass: f5.extensions.gardener.cloud/bigip
  selector:
    app: app-b
  ports:
    - name: http
      protocol: TCP
      port: 8081
      targetPort: 5678
EOF
```

Watch and verify:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc app-b-svc -w
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-b-svc -o yaml
```

Expected:

- `app-b-svc` succeeds using supplied LB.
- `app-b-svc` gets its own VIP (typically different from `app-a-svc`).

## 5. Case 3 (supplied LB and supplied VIP)

Create `app-c-svc` with both LB ID and VIP ID from Case 1.

```bash
cat <<EOF | kubectl --kubeconfig "$SHOOT_KC" apply -f -
apiVersion: v1
kind: Service
metadata:
  name: app-c-svc
  namespace: ${NS}
  annotations:
    f5.extensions.gardener.cloud/lb-service-id: "${CASE1_LB_ID}"
    f5.extensions.gardener.cloud/lb-service-id-mode: "provided"
    f5.extensions.gardener.cloud/vip-port-id: "${CASE1_VIP_ID}"
    f5.extensions.gardener.cloud/vip-port-id-mode: "provided"
spec:
  type: LoadBalancer
  loadBalancerClass: f5.extensions.gardener.cloud/bigip
  selector:
    app: app-c
  ports:
    - name: http
      protocol: TCP
      port: 9090
      targetPort: 5678
EOF
```

Watch and verify:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc app-c-svc -w
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc \
  -o custom-columns=NAME:.metadata.name,VIP:.status.loadBalancer.ingress[0].ip,PORT:.spec.ports[0].port,NODEPORT:.spec.ports[0].nodePort
```

Expected:

- `app-c-svc` uses the supplied LB and supplied VIP.
- `app-c-svc` VIP equals `app-a-svc` VIP (`CASE1_VIP_IP`).
- Frontend ports differ (`8080` vs `9090`), so VS listeners are distinct.

## 6. Functional traffic test

```bash
VIP_A=$(kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-a-svc -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
VIP_B=$(kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-b-svc -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
VIP_C=$(kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc app-c-svc -o jsonpath='{.status.loadBalancer.ingress[0].ip}')

curl -v --connect-timeout 10 "http://${VIP_A}:8080/"
curl -v --connect-timeout 10 "http://${VIP_B}:8081/"
curl -v --connect-timeout 10 "http://${VIP_C}:9090/"
```

Expected:

- Responses come from app-a/app-b/app-c respectively.
- `VIP_A == VIP_C` for Case 3.

## 7. Failure diagnosis

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get events --sort-by=.lastTimestamp
kubectl --kubeconfig "$SHOOT_KC" -n f5-cis-system logs deploy/f5-svc-lb-bridge --since=10m \
  | grep -iE 'error|fail|forbidden|401|403|400|network port|backend|BackendNodePortRequired'
```

Common interpretation:

- `no CMP network port found for backend IP`: node/network mapping issue in CMP.
- `401` or `403`: CMP credential/token issue.
- `400`: invalid or rejected CMP request field.
- `BackendNodePortRequired`: wait for NodePort assignment and retry.

## 8. Cleanup

Delete services first, then namespace:

```bash
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" delete svc app-c-svc app-b-svc app-a-svc
kubectl --kubeconfig "$SHOOT_KC" -n "$NS" get svc
kubectl --kubeconfig "$SHOOT_KC" delete namespace "$NS"
```
