# Using the F5 Extension

## Prerequisites

- F5 BIG-IP instance configured and accessible
- Credentials for F5 API access

Notes:

- If BIG-IP is an HA pair, prefer a **stable management endpoint** (floating management IP or DNS name that follows the active device). If you point to a single device IP and failover happens, CIS may stop reconciling until you update the config.
- If you use `spec.cis.bigipUrl` with an **IP address** (e.g. `https://100.65.x.y`) and the BIG-IP certificate does not contain an IP SAN, CIS will fail TLS verification unless you either:
  - use a hostname that matches the certificate, or
  - pass `--insecure` via `spec.cis.extraArgs`.

## Configuration

Create a secret with F5 credentials:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: f5-credentials
  namespace: shoot--project--name
type: Opaque
data:
  username: <base64-encoded-username>
  password: <base64-encoded-password>
```

## Enable the Extension

Add the extension to your shoot specification:

```yaml
spec:
  extensions:
  - type: f5-loadbalancer
    providerConfig:
      spec:
        # Minimal example. This is mapped to F5LoadBalancerConfig.spec.
        enableApplicationLB: false
```

## Application-plane LB (CIS in the Shoot)

The extension can deploy F5 CIS into the Shoot cluster to program BIG-IP for application traffic.

At minimum you need:

- `enableApplicationLB: true`
- `credentialsSecretRef` pointing to a Seed/technical secret that contains `username`/`password` for BIG-IP (these keys are copied into the Shoot)
- `cis.image`, `cis.bigipUrl`, `cis.partition`

Example providerConfig:

```yaml
spec:
  extensions:
  - type: f5-loadbalancer
    providerConfig:
      spec:
        enableApplicationLB: true
        credentialsSecretRef:
          namespace: shoot--project--name
          name: f5-credentials
        cis:
          image: f5networks/k8s-bigip-ctlr:<tag>
          bigipUrl: https://bigip-mgmt.example
          partition: k8s-apps
          extraArgs:
          - --insecure=true
```

## Service type=LoadBalancer bridge (optional)

If you also set `spec.cis.bridgeImage`, the extension deploys a small Shoot-side controller that:

- creates/updates an `Ingress` for each `Service` of type `LoadBalancer` (so CIS picks it up), and
- mirrors the desired VIP into `Service.status.loadBalancer` (so `kubectl get svc` shows `EXTERNAL-IP`).

The bridge acts only when the Service explicitly specifies the desired VIP via the annotation `cis.f5.com/ip` (or `spec.loadBalancerIP`).

Example Service:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: web
  namespace: default
  annotations:
    cis.f5.com/ip: 10.0.0.10
spec:
  type: LoadBalancer
  loadBalancerClass: f5.extensions.gardener.cloud/bigip
  selector:
    app: web
  ports:
  - name: http
    port: 80
    targetPort: 8080
```

## Application-plane demo without AS3 (manual BIG-IP LTM)

If AS3 is not installed on BIG-IP (for example, `GET /mgmt/shared/appsvcs/info` returns `404`), CIS cannot program BIG-IP in AS3 mode.

For a same-day demo you can still expose a Shoot `Service` via BIG-IP by creating a Pool + Virtual Server with NodePort backends.

1) Deploy the demo app Service into the Shoot:

```bash
VIP=100.72.200.30 envsubst < config/samples/app-plane-echo-lb.yaml | kubectl --kubeconfig /tmp/shoot-pf.kubeconfig apply -f -
```

2) Create/update BIG-IP LTM objects (pool members = each node IP + service NodePort):

```bash
KUBECONFIG=/tmp/shoot-pf.kubeconfig \
SERVICE_NS=demo-lb SERVICE_NAME=echo-lb VIP=100.72.200.30 \
BIGIP_HOST=100.72.44.146 BIGIP_USER=admin BIGIP_PASS='***' \
PARTITION=k8s-apps \
./scripts/bigip-ltm-service-vs.sh
```

3) Verify:

```bash
curl -sS http://100.72.200.30:8081/
```

### If BIG-IP can only reach the VM subnet

In some environments BIG-IP cannot route to the Shoot node subnet (e.g. `10.x`) or the pod CIDR (e.g. `10.1.x`).

If your control-plane BIG-IP pool members were on your VM (example: `100.72.44.199:7443`, `100.72.44.199:7444`), you can follow the same approach for application-plane demos:

- run `kubectl port-forward` on the VM to the application pods (binding to `0.0.0.0`), and
- configure BIG-IP pool members as `VM_IP:<localPorts>`.

The helper script supports this as `BACKEND_MODE=vm-portforward`:

```bash
KUBECONFIG=/tmp/shoot-pf.kubeconfig \
SERVICE_NS=demo-lb SERVICE_NAME=echo-lb VIP=100.72.200.21 \
BIGIP_HOST=100.72.44.146 BIGIP_USER=admin BIGIP_PASS='***' \
PARTITION=k8s-apps \
BACKEND_MODE=vm-portforward VM_IP=100.72.44.199 \
PF_BASE_PORT=7445 PF_COUNT=2 \
./scripts/bigip-ltm-service-vs.sh
```

Keep that command running (it keeps the port-forwards alive). In a second terminal:

```bash
curl -sS http://100.72.200.21:8081/
```

## Production-style installation (Garden registration)

To make the extension plug-and-play (Gardenlet installs it into Seeds and creates per-Shoot `Extension` resources automatically), apply the registration objects in the **Garden cluster**.

- Generate `ControllerDeployment` (embeds the Helm chart and sets image values):
  - `make gen-controllerdeployment`
- Apply in Garden cluster:
  - `kubectl --context <garden> apply -f deploy/garden/controllerdeployment-f5.yaml`
  - `kubectl --context <garden> apply -f deploy/garden/controllerregistration-f5.yaml`

See `deploy/garden/README.md` for details.
