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



echo '172.18.255.1 api.local.local.external.local.gardener.cloud' |
sudo tee -a /etc/hosts



For this local-provider test, keep the CMP compute UUID on the Shoot Node as an annotation. Do not replace the existing `kind://...` providerID.

After the Shoot reaches `Create Succeeded`, run:

```bash
NODE_NAME=$(kubectl --kubeconfig "$SHOOT_KC" get nodes -o jsonpath='{.items[0].metadata.name}')

kubectl --kubeconfig "$SHOOT_KC" annotate node "$NODE_NAME" \
  f5.extensions.gardener.cloud/cmp-compute-id="8c6bde46-3dd2-4407-90ff-8c2705427694" \
  f5.extensions.gardener.cloud/backend-ip="10.10.1.238" \
  --overwrite
```

The bridge will then use:

```text
CMP compute UUID = Node annotation cmp-compute-id
Backend IP       = Node annotation backend-ip
Backend port ID  = matching entry from CMP compute.ports[]
Backend app port = Kubernetes Service NodePort
```

Verify it with:

```bash
kubectl --kubeconfig "$SHOOT_KC" get node "$NODE_NAME" \
  -o jsonpath='computeID={.metadata.annotations.f5\.extensions\.gardener\.cloud/cmp-compute-id}{"\n"}backendIP={.metadata.annotations.f5\.extensions\.gardener\.cloud/backend-ip}{"\n"}'
```

For local development, the annotation must be reapplied if the Shoot worker Node is deleted and recreated. In production, the Airtel provider should set:

```yaml
spec:
  providerID: cmp://8c6bde46-3dd2-4407-90ff-8c2705427694
```

automatically, so the manual compute-ID annotation is unnecessary. The `backend-ip` annotation is also only needed locally because the Kind Node InternalIP is `172.18.0.8`, not the CMP VM IP `10.10.1.238`.


docker exec gardener-local-control-plane \
  iptables -I cali-FORWARD 1 -i eth0 -s 172.18.0.1/32 -p tcp -j ACCEPT

nohup bash -c 'while true; do
  docker exec gardener-local-control-plane \
    iptables -C cali-FORWARD -i eth0 -s 172.18.0.1/32 -p tcp -j ACCEPT 2>/dev/null ||
  docker exec gardener-local-control-plane \
    iptables -I cali-FORWARD 1 -i eth0 -s 172.18.0.1/32 -p tcp -j ACCEPT
  sleep 2
done' >/tmp/f5-calico-forward.log 2>&1 &

The path VM host → Kind container → Shoot node → app-b pod
sudo ip route replace 10.0.130.192/32 via 172.18.0.8 dev br-5d738cbda630

Next, forward traffic arriving from CMP/F5 at 10.10.1.238:31514 to the Shoot NodePort:
sudo iptables -t nat -I PREROUTING 1 \
  -i enp3s0 -p tcp -d 10.10.1.238 --dport 31514 \
  -j DNAT --to-destination 10.0.130.192:31514

complete return-path handling so external F5 traffic is translated to the Docker host address and accepted by our Calico rule:

  sudo iptables -t nat -I POSTROUTING 1 \
  -p tcp -d 10.0.130.192 --dport 31514 \
  -j MASQUERADE

for app c nodeport
  sudo iptables -t nat -I PREROUTING 1 \
  -i enp3s0 -p tcp -d 10.10.1.238 --dport 31146 \
  -j DNAT --to-destination 10.0.130.192:31146

  sudo iptables -t nat -I POSTROUTING 1 \
  -p tcp -d 10.0.130.192 --dport 31146 \
  -j MASQUERADE

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



# for garden dev cluster

kubectl --kubeconfig=/home/gardener/admin.conf \
  -n garden get secret airtel-registry-pull \
  -o jsonpath='{.data.\.dockerconfigjson}' \
  | base64 -d > /tmp/airtel-registry-dockerconfig.json
  
kubectl --kubeconfig=/home/gardener/admin.conf \
  -n extension-gardener-extension-f5-7jdgg \
  create secret generic airtel-registry-pull \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson=/tmp/airtel-registry-dockerconfig.json


kubectl --kubeconfig=/home/gardener/admin.conf \
  -n extension-gardener-extension-f5-7jdgg \
  label secret airtel-registry-pull \
  gardener.cloud/role=helm-pull-secret

# To access shoot

kubectl --kubeconfig="$VG" \
  -n garden-local \
  create --raw \
  "/apis/core.gardener.cloud/v1beta1/namespaces/garden-local/shoots/f5-test/adminkubeconfig" \
  -f - <<'EOF' > /tmp/shoot--local--f5-test-kubeconfig-response.json
{
  "apiVersion": "authentication.gardener.cloud/v1alpha1",
  "kind": "AdminKubeconfigRequest",
  "spec": {
    "expirationSeconds": 3600
  }
}
EOF


cat /tmp/shoot--local--f5-test-kubeconfig-response.json \
  | python3 -c 'import sys,json,base64; print(base64.b64decode(json.load(sys.stdin)["status"]["kubeconfig"]).decode())' \
  > /tmp/shoot--local--f5-test.conf


env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy -u ALL_PROXY -u all_proxy \
kubectl --kubeconfig=/tmp/shoot--local--f5-test.conf get nodes
No resources found
[gardener@gardener-k8s-master-1 ~]$ 

  env -u HTTP_PROXY -u HTTPS_PROXY -u http_proxy -u https_proxy -u ALL_PROXY -u all_proxy \
kubectl --kubeconfig=/tmp/shoot--local--f5-test.conf \
  --context garden-local--smoke-local-internal \
  api-resources --api-group=apps



# workerless shoot 

[gardener@gardener-k8s-master-1 ~]$ kubectl --kubeconfig="$VG" \
  -n garden-local \
  get shoot smoke-local \
  -o jsonpath='{.spec.provider.workers}{"\n"}'

[gardener@gardener-k8s-master-1 ~]$ 
 
 # Extract VG admin kubeconfig
kubectl --kubeconfig=/home/gardener/admin.conf -n garden get secret gardener -o jsonpath='{.data.kubeconfig}' | base64 -d > /tmp/vg.conf

export VG=/tmp/vg.conf
 
export NO_PROXY="${NO_PROXY},api.virtual-garden.100.65.239.241.nip.io,.virtual-garden.100.65.239.241.nip.io"
export no_proxy="${NO_PROXY}"

# Talk to Virtual Garden
kubectl --kubeconfig="$VG" get ns
kubectl --kubeconfig="$VG" get seeds
kubectl --kubeconfig="$VG" get shoots -A
kubectl --kubeconfig="$VG" get shoot smoke-local -n garden-local -o wide

# worker shoot for f5 test

cp /home/gardener/src/gardener-v1.139.2/example/provider-local/shoot.yaml \
   /tmp/f5-test-shoot.yaml

sed '0,/^  name: local$/s//  name: f5-test/' \
  /home/gardener/src/gardener-v1.139.2/example/provider-local/shoot.yaml \


sed -i '/^[[:space:]]*type: calico$/d' /tmp/f5-test-shoot.yaml



python3 - <<'PY'
from pathlib import Path

p = Path("/tmp/f5-test-shoot.yaml")
s = p.read_text()

s = s.replace(
"""  networking:
    ipFamilies:
    - IPv4
    nodes: 10.0.0.0/16
    services: 10.201.0.0/16
""",
"""  networking:
    type: calico
    ipFamilies:
    - IPv4
    nodes: 10.0.0.0/16
    services: 10.201.0.0/16
""",
1,
)

p.write_text(s)
PY

sed -i '/authentication\.gardener\.cloud\/issuer: "managed"/d' /tmp/f5-test-shoot.yaml


 kubectl --kubeconfig="$VG" apply -f /tmp/f5-test-shoot.yaml


 kubectl --kubeconfig="$VG" -n garden-local create secret generic cmp-f5-credentials \
  --from-literal=api-key-id='fa5ee955-0b3a-412a-8a68-d7f39978840b' \
  --from-literal=api-secret='hxy00x7HVC8a/B5vJ9lTh94pX46sc9iy-wM1oPzH' \
  --from-literal=project-id='qa-first-cell' \
  --from-literal=organisation-name='qa-tenant'