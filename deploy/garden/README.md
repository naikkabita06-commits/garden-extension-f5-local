# Gardener Extension for F5 Load Balancer

This document is the operator runbook to register and install this extension on a Gardener landscape using:

- `ControllerRegistration`
- `ControllerDeployment` (Helm-embedded chart)
- `ControllerInstallation`

It also includes a minimal “validate” checklist and troubleshooting notes.

## Prerequisites

- A Gardener management cluster (“Garden”) and at least one Seed.
- `kubectl` context(s) for:
  - the Garden cluster (`--context <garden>`)
  - (optional) the Seed cluster (`--context <seed>`) for verifying pods
- `helm` v3 (required to generate the embedded chart inside the `ControllerDeployment`).
- A controller image built and pushed to a registry reachable by the Seed.

If you plan to validate application-plane load balancing:

- BIG-IP management endpoint reachable from inside the Shoot network (CIS talks to BIG-IP mgmt/AS3).
- BIG-IP data-plane reachability to Shoot backends (node IPs/NodePorts or pod IPs).

## Build and publish the controller image

From repo root:

```bash
make docker-build docker-push
```

The exact registry/tag is controlled by the `Makefile` variables.

## Generate the ControllerDeployment (embeds the Helm chart)

The `ControllerDeployment` is `type: helm` and embeds a base64-encoded chart tarball.

```bash
make gen-controllerdeployment
```

Output:

- `deploy/garden/controllerdeployment-f5.yaml`

Override image repo/tag (example):

```bash
IMAGE_REPOSITORY=<registry>/<project>/gardener-extension-f5 \
IMAGE_TAG=<tag> \
OUTPUT=deploy/garden/controllerdeployment-f5.yaml \
bash scripts/generate-controllerdeployment-f5.sh
```

## Register the extension in the Garden cluster

Apply `ControllerDeployment` and `ControllerRegistration` to the Garden cluster:

```bash
kubectl --context <garden> apply -f deploy/garden/controllerdeployment-f5.yaml
kubectl --context <garden> apply -f deploy/garden/controllerregistration-f5.yaml
```

Verify:

```bash
kubectl --context <garden> get controllerdeployment gardener-extension-f5
kubectl --context <garden> get controllerregistration gardener-extension-f5
```

## Install into a Seed via ControllerInstallation

Edit `deploy/garden/controllerinstallation-f5.yaml` and set:

- `spec.seedRef.name: <seed-name>`

Then apply it to the Garden cluster:

```bash
kubectl --context <garden> apply -f deploy/garden/controllerinstallation-f5.yaml
```

Watch status:

```bash
kubectl --context <garden> get controllerinstallation gardener-extension-f5
kubectl --context <garden> describe controllerinstallation gardener-extension-f5
```

## Verify the controller is running (Seed)

If you have direct Seed cluster access:

```bash
kubectl --context <seed> get pods -A -l app.kubernetes.io/name=gardener-extension-f5
kubectl --context <seed> get deploy -A -l app.kubernetes.io/name=gardener-extension-f5
```

The namespace is chosen by Gardener; labels are the most reliable way to find it.

## Configure a Shoot (what Gardenlet creates)

This extension is triggered by an `Extension` object in the Shoot technical namespace (`shoot--<project>--<shoot>`).

Example (create/patch through your shoot creation flow; shown here for reference):

```yaml
apiVersion: extensions.gardener.cloud/v1alpha1
kind: Extension
metadata:
  name: extension-f5
  namespace: shoot--project--abc
spec:
  type: f5-loadbalancer
  providerConfig:
    spec:
      enableApplicationLB: true
```

Per-shoot configuration is stored in `F5LoadBalancerConfig` plus a referenced credentials `Secret`.
Usage examples live in `docs/usage/usage.md`.

## Kind/provider-local notes (optional demo)

This repo contains helper manifests/scripts for a kind-based Gardener landscape.
For example, the VM demo uses the local registry:

- `registry.local.gardener.cloud:5001`

See `deploy/kind/` and `docs/demo-a-z.md` for the copy/paste demo flow.

## Troubleshooting: VIP created but traffic fails (application plane)

If CIS created the Virtual Server on BIG-IP but `curl` to the VIP hangs/resets, check backend reachability first.

### 1) Backend reachability (BIG-IP → Shoot node NodePort)

With CIS `--pool-member-type=nodeport`, pool members look like:

- `<shoot-node-internal-ip>:<nodePort>`

BIG-IP must be able to route to those addresses and firewalls must allow the NodePort range.

Quick check from a host that should have similar reachability to BIG-IP (adjust IP/port to match your pool member):

```bash
kubectl --kubeconfig <shoot-kubeconfig> get nodes -o wide
curl --max-time 5 http://<shoot-node-internal-ip>:<nodePort>/
```

If this times out, BIG-IP will also time out unless routing/NAT/security-group rules are fixed.

### 2) Shoot → BIG-IP mgmt reachability (CIS control path)

CIS must reach BIG-IP management/AS3 endpoints over HTTPS.
If CIS is CrashLooping with timeouts, verify:

- DNS/routing/firewall allow egress to `spec.cis.bigipUrl` from the Shoot network
- (TLS) if BIG-IP uses a certificate without IP SANs and you configure `bigipUrl` as an IP, you may need proper certs or CIS trusted cert configuration (demo-only workaround: `--insecure=true`)

### 3) Reconcile trigger

If you edit an existing `F5LoadBalancerConfig` and do not see the controller react, annotate the `Extension` object to force a reconcile:

```bash
kubectl -n shoot--<project>--<shoot> annotate extension extension-f5 reconcile-at="$(date -Is)" --overwrite
```

## “Provider not available” troubleshooting (common on OpenStack Seeds)

If the extension type is not available to Shoots, typically one of these is true:

1) No `ControllerInstallation` targets the Seed
- Fix: apply `deploy/garden/controllerinstallation-f5.yaml` with the correct `spec.seedRef.name`.

2) Seed name mismatch
- Fix: ensure `spec.seedRef.name` matches `kubectl --context <garden> get seeds` output.

3) Confusion about `ControllerDeployment.type`
- `ControllerDeployment.type` is always `helm` for embedded chart installs; the Seed provider (OpenStack/etc) is selected by `ControllerInstallation.spec.seedRef.name`.
