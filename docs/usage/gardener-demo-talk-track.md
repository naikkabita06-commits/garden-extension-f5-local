# Gardener demo (kind-based)

Scope: **Gardener only** (no extensions).

Known-good values in environment:

- Context: `kind-gardener-local`
- Seed: `local`
- Shoot: `garden-local/local`
- Shoot technical namespace: `shoot--local--local`

## 0) Intro

- “I’ll show Gardener control plane, the Seed, one healthy Shoot, and where the Shoot control plane runs.”

## 1) Gardener control plane is running (Garden cluster API)


- “This is a kind-based Gardener setup. Gardener control plane runs in the `garden` namespace.”

Run:
```sh
kubectl --context kind-gardener-local -n garden get pods
```

Point at:
- `gardener-apiserver`, `gardener-controller-manager`, `gardenlet` are `Running`.

## 2) The Seed is Ready


- “A Seed is the cluster where Shoot control planes run. Here the Seed is `local` and it’s Ready.”

Run:
```sh
kubectl --context kind-gardener-local get seed
kubectl --context kind-gardener-local describe seed local | egrep -i 'Name:|Ready|Conditions|GardenletReady|ExtensionsReady|LastTransitionTime|LastHeartbeatTime' || true
```

Point at:
- `GardenletReady=True` and `ExtensionsReady=True`.

## 3) Show the healthy Shoot + its conditions

- “Now I’ll show the Shoots and drill into one healthy Shoot.”

Run:
```sh
kubectl --context kind-gardener-local get shoot -A
kubectl --context kind-gardener-local -n garden-local describe shoot local
```

Point at:
- `Last Operation: Reconcile Succeeded (100%)`.
- Conditions: `APIServerAvailable=True`, `ControlPlaneHealthy=True`, `EveryNodeReady=True`.

## 4) Connect Shoot → Seed + technical namespace


- “This Shoot is scheduled to Seed `local`. Its control plane runs in the technical namespace `shoot--local--local`.”

Run:
```sh
kubectl --context kind-gardener-local -n garden-local get shoot local -o jsonpath='{.spec.seedName}{"\n"}{.status.technicalID}{"\n"}'
```

## 5) Show the Shoot control plane running on the Seed


- “This namespace is the Seed-side control plane of the Shoot.”

Run:
```sh
kubectl --context kind-gardener-local -n shoot--local--local get pods -o wide | egrep 'kube-apiserver|etcd-(main|events)|kube-controller-manager|kube-scheduler'
```

Point at:
- `kube-apiserver`, `etcd-main/events`, `kube-controller-manager`, `kube-scheduler`.

## 6) (Optional) Worker lifecycle objects (Machines)


- “Machines are MCM’s view of worker node lifecycle.”

Run:
```sh
kubectl --context kind-gardener-local -n shoot--local--local get machinedeployments,machinesets,machines
```

## 7) Access the Shoot cluster API from the VM (DNS workaround)


- “In this local setup, the Shoot API DNS name isn’t resolvable from the VM, so I port-forward the apiserver service to localhost.”

Terminal 1 (keep running):
```sh
kubectl --context kind-gardener-local -n shoot--local--local port-forward svc/kube-apiserver 8443:443
```

Terminal 2:
```sh
kubectl --context kind-gardener-local -n shoot--local--local get secret generic-token-kubeconfig-8a2b75c7 \
  -o jsonpath='{.data.kubeconfig}' | base64 -d > /tmp/shoot-local.kubeconfig

kubectl --context kind-gardener-local -n shoot--local--local get secret shoot-access-gardener-resource-manager \
  -o jsonpath='{.data.token}' | base64 -d > /tmp/shoot-local.token

sed -i 's#/var/run/secrets/gardener.cloud/shoot/generic-kubeconfig/token#/tmp/shoot-local.token#g' /tmp/shoot-local.kubeconfig
sed -i 's#https://api.local.local.internal.local.gardener.cloud#https://127.0.0.1:8443#g' /tmp/shoot-local.kubeconfig
sed -i 's#https://api.local.local.external.local.gardener.cloud#https://127.0.0.1:8443#g' /tmp/shoot-local.kubeconfig

kubectl --kubeconfig /tmp/shoot-local.kubeconfig get nodes -o wide
```

Close:
- “That’s the full path: Garden manages; Seed runs Shoot control plane; Shoot cluster has ready worker nodes.”
