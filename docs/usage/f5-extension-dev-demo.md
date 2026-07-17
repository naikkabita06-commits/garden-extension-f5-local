# F5 extension dev demo (Gardener kind dev env)

Goal: demonstrate the extension controller wiring + status behavior in a local Gardener dev env **without requiring a real F5/CMP Virtual Server (VS)**.

This demo focuses on what you *can* prove today:

- CRD installed
- Controller runs and reconciles an `Extension`
- Controller reads `F5LoadBalancerConfig`
- Controller updates status conditions
- Controller blocks application-plane rollout when control-plane is not ready
- Controller reports permanent config errors without hot-looping

## Assumptions

- Your kube context is `kind-gardener-local`.
- You have one Seed `local` and one Shoot `garden-local/local`.
- The Shoot technical namespace is `shoot--local--local`.

## 0) One-time: install the CRD

```sh
kubectl --context kind-gardener-local apply -f config/crd/f5loadbalancerconfigs.f5.extensions.gardener.cloud.yaml
```

Verify:

```sh
kubectl --context kind-gardener-local get crd f5loadbalancerconfigs.f5.extensions.gardener.cloud
```

## 1) Run the controller (dev)

From the repo root:

```sh
make start
```

This runs the controller using your current kubeconfig/context.

Notes:

- The controller uses controller-runtime's default config loading (`KUBECONFIG`, then in-cluster config).
- If you want to be explicit, you can point it to a kubeconfig file:

```sh
export KUBECONFIG=$PWD/dev/kubeconfig
make start
```

If you are using `kind-gardener-local` on your laptop, relying on your current context is fine.

## 2) Create a minimal credentials Secret (required by the CRD schema)

Even for “blocked-mode” demos, the CRD requires `spec.credentialsSecretRef`, so create a dummy secret:

```sh
kubectl --context kind-gardener-local -n shoot--local--local \
  create secret generic f5-credentials \
  --from-literal=username=dummy \
  --from-literal=password=dummy \
  --dry-run=client -o yaml | kubectl apply -f -
```

## 3) Create the Extension (Seed-side object)

The controller watches `extensions.gardener.cloud/v1alpha1 Extension`.

```sh
kubectl --context kind-gardener-local -n shoot--local--local apply -f - <<'EOF'
apiVersion: extensions.gardener.cloud/v1alpha1
kind: Extension
metadata:
  name: f5
  namespace: shoot--local--local
spec:
  type: f5
EOF
```

## 4) Create the F5LoadBalancerConfig (demo mode: Control-plane NOT ready)

Important: the current controller expects the `F5LoadBalancerConfig` to have the **same name+namespace** as the `Extension`.

This manifest intentionally leaves control-plane not ready, so the controller sets `ApplicationLoadBalancerReady=False` with reason `Blocked` and returns `nil` (no hot-loop).

```sh
kubectl --context kind-gardener-local -n shoot--local--local apply -f - <<'EOF'
apiVersion: f5.extensions.gardener.cloud/v1alpha1
kind: F5LoadBalancerConfig
metadata:
  name: f5
  namespace: shoot--local--local
spec:
  credentialsSecretRef:
    name: f5-credentials
    namespace: shoot--local--local

  enableApplicationLB: true

  # No VS/VIP yet (CMP out-of-band). This keeps us blocked.
  controlPlaneVIP: ""
  controlPlaneReady: false

  # not needed for this blocked-mode demo
  cis: null
EOF
```

Check conditions:

```sh
kubectl --context kind-gardener-local -n shoot--local--local get f5loadbalancerconfig f5 -o yaml
```

What you should see:

- `ControlPlaneLoadBalancerReady=False` (Reason `NotConfigured` when VIP empty, or `NotReady` when `controlPlaneReady=false` and VIP is set)
- `ApplicationLoadBalancerReady=False` with Reason `Blocked`

## 5) Optional: demonstrate “permanent config error” (still no VS required)

If you want to show the controller validates CIS config and marks it as a permanent config error (without looping), mark control-plane ready but keep `cis: null`.

```sh
kubectl --context kind-gardener-local -n shoot--local--local patch f5loadbalancerconfig f5 --type merge -p '
{"spec":{"controlPlaneVIP":"203.0.113.10","controlPlaneReady":true,"cis":null}}
'
```

Then check status:

```sh
kubectl --context kind-gardener-local -n shoot--local--local get f5loadbalancerconfig f5 -o yaml
```

Expected:

- `ControlPlaneLoadBalancerReady=True` (Reason `ExternalProvisioned`)
- `ApplicationLoadBalancerReady=False` with Reason `ConfigError` (message about missing `spec.cis`)

This proves: “controller doesn’t hot-loop on permanent misconfig; it surfaces status clearly”.

## 6) Cleanup

```sh
kubectl --context kind-gardener-local -n shoot--local--local delete f5loadbalancerconfig f5 --ignore-not-found
kubectl --context kind-gardener-local -n shoot--local--local delete extension f5 --ignore-not-found
kubectl --context kind-gardener-local -n shoot--local--local delete secret f5-credentials --ignore-not-found
```

---

## When you DO need a real Virtual Server (VS)

You need a real VIP/VS when you want to validate end-to-end networking, for example:

- Shoot kube-apiserver reachable via the control-plane VIP
- Application `Service type=LoadBalancer` gets an EXTERNAL-IP and traffic flows

If you want the controller to provision the control-plane VIP/VS via CMP/CCP (instead of out-of-band), see the “CMP/CCP provisioning mode” section in [docs/demo-progress.md](../demo-progress.md). That mode requires:
- `spec.ccpApiEndpoint` set (and `spec.controlPlaneReady` omitted)
- CMP auth in the referenced Secret
- controller-level HTTP path/method templates configured via Helm values `controller.env.*`

That requires CMP/F5 to actually provision/configure:

- Control-plane VIP + VS on `:443` pointing to apiserver backends
- For app-plane: CIS must be able to reach BIG-IP/CMP and VIP allocation must be enabled

No amount of mocking inside the extension can prove real dataplane behavior—only real VIP/VS can.

---

## Should you mock anything?

For the demo above: **no**.

Mocking is useful only if you start implementing the CMP LBaaS API integration and want to test request/response handling without real CMP.

---

## Should you commit and use Remote SSH on a VM?

- If your kind-based Gardener cluster runs on a VM and you plan to run `make start` against it, Remote SSH is usually the simplest way to ensure:
  - you are using the right kubeconfig context,
  - network access is consistent.

- Committing your current changes is a good idea if you’ll move between machines (laptop ↔ VM), because it prevents “it worked on my machine” drift.

Rule of thumb:

- If your cluster is on a VM and your laptop must reach it through VPN / SSH tunnels / private DNS, prefer **Remote SSH** and run the controller from the VM.
- If your cluster is local (`kind` on your laptop), prefer running locally.

A typical workflow:

1) On your laptop, check what changed:

```sh
git status
```

2) Commit to a feature branch:

```sh
git checkout -b dev-demo

git add -A

git commit -m "Dev demo: controller conditions + unit tests"
```

3) On the VM (Remote SSH), pull that branch (or copy the repo) and run the same demo commands.
