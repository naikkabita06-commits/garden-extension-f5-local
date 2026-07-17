# Seed ingress VIP via F5 (BIG-IP) using `loadBalancerClass` (kind landscape notes)

Date: 2026-04-10

## Goal (what we set out to do)

- Keep Gardener Seed ingress stack as-is (nginx/istio). No Gardener-core changes.
- Make the Seed ingress gateway VIP be provided by BIG-IP by:
  - ensuring the Seed `Service type=LoadBalancer` is created with `spec.loadBalancerClass: f5.extensions.gardener.cloud/bigip` (immutable, must be set at creation time), and
  - providing a VIP via annotation `cis.f5.com/ip`, and
  - running a controller that watches these Services, programs BIG-IP, and (optionally) mirrors VIP into `Service.status.loadBalancer`.

## Summary (is it done?)

**Implementation: Yes.** The repo now contains a Seed-side Service LB class controller and helm wiring to deploy it into the Seed, without any Gardener-core changes.

**End-to-end in THIS kind landscape: Partially demonstrable.**

- BIG-IP programming path: **works** (seed controller is reconciling and AS3 declare no longer errors after the conflicting VS was removed on BIG-IP).
- `Service.status.loadBalancer` path: **cannot be demonstrated reliably here** because `gardener-extension-provider-local` (the local provider extension in this kind setup) continuously overwrites the `status.loadBalancer.ingress[].ip` back to `172.18.255.1` regardless of `loadBalancerClass`.

So the outcome depends on the acceptance criteria:
- If the requirement is **“BIG-IP should own the VIP and route traffic”**: this is effectively done after removing the conflicting BIG-IP VS.
- If the requirement is **“kubectl should show EXTERNAL-IP == VIP”**: this kind seed prevents it due to provider-local status reconciliation.

## What was implemented in the repo (relevant pieces)

- Seed-side controller binary and logic:
  - [cmd/seed-service-lb-controller/main.go](cmd/seed-service-lb-controller/main.go)
  - Watches `Service` objects.
  - Filters to:
    - `spec.type: LoadBalancer`
    - `spec.loadBalancerClass: f5.extensions.gardener.cloud/bigip`
    - VIP provided via `metadata.annotations["cis.f5.com/ip"]` (fallback `spec.loadBalancerIP`).
  - Programs BIG-IP via AS3 and manages cleanup via a finalizer.
  - Attempts to set `Service.status.loadBalancer.ingress[0].ip = <VIP>`.
- AS3 client:
  - [pkg/bigip/as3.go](pkg/bigip/as3.go)
- Helm wiring to deploy the Seed controller:
  - [charts/gardener-extension-f5/values.yaml](charts/gardener-extension-f5/values.yaml) (`seedServiceLBController.*`)
  - [charts/gardener-extension-f5/templates/seed-service-lb-controller-deployment.yaml](charts/gardener-extension-f5/templates/seed-service-lb-controller-deployment.yaml)
  - Includes Gardener networking allow-labels so the pod can reach the runtime apiserver.
- Image build includes the new binary:
  - [Dockerfile](Dockerfile)

## Commands run (chronological) + outcomes

> Notes:
> - Context used: `kind-gardener-local`.
> - BIG-IP mgmt referenced in env/values: `https://100.72.44.146`.
> - VIP targeted for ingress gateway: `100.72.200.20`.
> - Seed-side extension namespace observed: `extension-gardener-extension-f5-zn8sj`.

### 1) Roll out chart bundle + enable seed controller

Regenerate ControllerDeployment manifest from the updated chart and apply it:

```bash
IMAGE_REPOSITORY=registry.local.gardener.cloud:5001/gardener/extensions/f5 \
IMAGE_TAG=$(cat /tmp/f5_new_tag) \
./scripts/generate-controllerdeployment-f5.sh
kubectl --context kind-gardener-local apply -f deploy/garden/controllerdeployment-f5.yaml
```

Patch ControllerDeployment helm values to enable seed controller and pass BIG-IP config:

```bash
kubectl --context kind-gardener-local patch controllerdeployment gardener-extension-f5 --type merge -p '{
  "helm": {
    "values": {
      "seedServiceLBController": {
        "enabled": true,
        "bigipSecretName": "f5-seed-credentials",
        "env": {
          "BIGIP_URL": "https://100.72.44.146",
          "F5_INSECURE_SKIP_TLS_VERIFY": "true"
        }
      }
    }
  }
}'
```

Outcome:
- Deployment `gardener-extension-f5-seed-service-lb` appears in namespace `extension-gardener-extension-f5-zn8sj`.

### 2) Verify deployment has required networking labels

```bash
kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj get deploy gardener-extension-f5-seed-service-lb -o jsonpath='{.spec.template.metadata.labels}'
```

Outcome:
- Pod template includes labels like:
  - `networking.gardener.cloud/to-runtime-apiserver=allowed`
  - `networking.gardener.cloud/to-dns=allowed`
  - `networking.gardener.cloud/to-private-networks=allowed`
  - `networking.gardener.cloud/to-public-networks=allowed`

This fixes the earlier informer startup timeouts to `10.2.0.1:443`.

### 3) Observe controller reconciliation against the Seed ingressgateway Service

Inspect current Service state:

```bash
kubectl --context kind-gardener-local -n istio-ingress get svc istio-ingressgateway -o yaml
```

Outcome:
- Service has:
  - `spec.type: LoadBalancer`
  - `spec.loadBalancerClass: f5.extensions.gardener.cloud/bigip`
  - `metadata.annotations.cis.f5.com/ip: 100.72.200.20`
- Service status remains `172.18.255.1` in this kind setup.

Controller logs show reconciliation:

```bash
kubectl --context kind-gardener-local -n extension-gardener-extension-f5-zn8sj logs deploy/gardener-extension-f5-seed-service-lb --since=10m | tail -n 200
```

Outcome:
- Logs show repeated:
  - `deploying BIG-IP AS3` for `istio-ingress/istio-ingressgateway` with `vip=100.72.200.20`.

### 4) Resolve BIG-IP conflict on :443

Observed error (before BIG-IP cleanup):
- BIG-IP returned HTTP 422 due to a destination conflict with existing `/Common/cp-apiserver-vs`.

After you deleted the conflicting BIG-IP VS on port 443:
- The seed controller no longer logs `AS3 declare failed`.

### 5) Why `Service.status` did not change in kind

We checked `managedFields` to see who writes status:

```bash
kubectl --context kind-gardener-local -n istio-ingress get svc istio-ingressgateway \
  -o jsonpath='{range .metadata.managedFields[?(@.subresource=="status")]}{.manager}{"\t"}{.time}{"\n"}{end}'
```

Outcome:
- The status writer is `gardener-extension-provider-local`.
- That component continuously reconciles status back to `172.18.255.1`.

We also proved manual status patch is immediately overwritten:

```bash
kubectl --context kind-gardener-local -n istio-ingress patch svc istio-ingressgateway \
  --subresource status --type merge \
  -p '{"status":{"loadBalancer":{"ingress":[{"ip":"100.72.200.20"}]}}}'
```

Outcome:
- Patch succeeds, but a subsequent `kubectl get svc` still shows `172.18.255.1`.

## Current state (kind landscape)

- Seed controller is deployed and running.
- Controller can reach apiserver (network labels are present).
- Controller is reconciling `istio-ingressgateway` and declaring AS3 without errors (post BIG-IP cleanup).
- `Service.status.loadBalancer` remains `172.18.255.1` because provider-local overwrites it.

## What to verify outside kind (realistic provider seed)

If you want the Kubernetes Service to show the BIG-IP VIP:
- Ensure there is no other LB/status controller that force-writes the status for that Service, or that it respects `loadBalancerClass`.
- Then the seed controller’s `services/status` patch should be visible as `EXTERNAL-IP=<VIP>`.

If you only need BIG-IP to serve the VIP:
- Verify on BIG-IP (AS3 tenant) that the VS is created for `100.72.200.20:443` and its pool members point at the Seed nodes + nodePort.
