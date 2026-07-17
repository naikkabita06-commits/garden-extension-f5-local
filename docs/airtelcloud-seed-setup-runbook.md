# Airtelcloud Seed Setup Runbook

Setting up a real Airtelcloud Seed on a second kind cluster, connected to an existing
Gardener Garden cluster (kind-gardener-local), using three custom extensions:

| Extension | Purpose |
|---|---|
| `gardener-extension-provider-airtelcloud` | Infrastructure, ControlPlane, Worker (OpenStack) |
| `gardener-extension-tcpwave` | DNS for Seed ingress + Shoot API records |
| `gardener-extension-f5` | Control-plane LB (Seed VIP) + App-plane LB (CIS) |

**Assumption:** Garden cluster (`kind-gardener-local`) is already up and running via
`make kind-up && make gardener-up`. Your kubeconfig context is `kind-gardener-local`.

---

## Part 1 — Second kind cluster (Seed)

### 1.1 Free disk space first

The seed cluster image builds eat ~6 GB. Always check before starting.

```bash
df -h /var /home /tmp

# If /var is low (needs ~6 GB free):
sudo lvextend -L +6G /dev/mapper/rhel-var
sudo xfs_growfs /var

# If /home is low:
sudo lvextend -L +20G /dev/mapper/rhel-home
sudo xfs_growfs /home

# If /tmp is low:
sudo lvextend -L +10G /dev/mapper/rhel-tmp
sudo xfs_growfs /tmp

# Prune stale Docker data (safe — only removes unused):
docker system prune -af
docker volume prune -f
```

### 1.2 Bring up the second kind cluster

```bash
cd ~/gardener
make kind2-up
```

Expected output includes:
- Cluster `gardener-local2` created
- Calico, metrics-server, local-path-provisioner deployed
- Node: `gardener-local2-control-plane`

### 1.3 Merge kubeconfigs

Run this once; gives you both contexts under a single `~/.kube/config`.

```bash
kind get kubeconfig --name gardener-local  > ~/.kube/config
kind get kubeconfig --name gardener-local2 > ~/.kube/config2

export KUBECONFIG=~/.kube/config:~/.kube/config2
kubectl config view --merge --flatten > ~/.kube/config.merged
mv ~/.kube/config.merged ~/.kube/config
unset KUBECONFIG

kubectl config get-contexts
# Should show:
#   kind-gardener-local    ← garden
#   kind-gardener-local2   ← seed
```

### 1.4 Note key addresses

You will need these throughout:

```bash
# Garden cluster node IP (gardenlet connects garden → seed via this)
docker inspect gardener-local-control-plane | grep '"IPAddress"'
# e.g. 172.18.0.8

# Seed cluster node IP
kubectl --context kind-gardener-local2 get nodes -o wide
# INTERNAL-IP e.g. 172.18.0.9

# Seed pod CIDR
kubectl --context kind-gardener-local2 get pods -A -o wide | grep "10\."
# e.g. 10.1.134.0/16

# Seed service CIDR
kubectl --context kind-gardener-local2 cluster-info dump | grep service-cluster-ip-range
# e.g. 10.2.0.0/16
```

---

## Part 2 — Bootstrap token (gardenlet self-registration)

The gardenlet uses a bootstrap token to authenticate to the garden API server for
the first time, then stores a proper kubeconfig in a Secret.

### 2.1 Generate token values

```bash
TOKEN_ID=$(openssl rand -hex 3)      # e.g. f0f23d
TOKEN_SECRET=$(openssl rand -hex 8)  # e.g. ed343cbcc087fa99
echo "token-id:     $TOKEN_ID"
echo "token-secret: $TOKEN_SECRET"
echo "full token:   ${TOKEN_ID}.${TOKEN_SECRET}"
```

### 2.2 Create bootstrap token YAML

```bash
cat > ~/bootstrap-token.yaml << EOF
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${TOKEN_ID}
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  description: "Token for gardenlet on Seed ojasvi-seed"
  token-id: "${TOKEN_ID}"
  token-secret: "${TOKEN_SECRET}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
EOF
```

### 2.3 Apply on both clusters

```bash
kubectl --context kind-gardener-local  apply -f ~/bootstrap-token.yaml
kubectl --context kind-gardener-local2 apply -f ~/bootstrap-token.yaml
```

### 2.4 Verify node-bootstrapper ClusterRoleBinding exists

```bash
kubectl --context kind-gardener-local get clusterrolebinding | grep node-bootstrapper
# If missing:
kubectl --context kind-gardener-local create clusterrolebinding bootstrap-token-csr \
  --clusterrole=system:node-bootstrapper \
  --group=system:bootstrappers
```

### 2.5 Note the garden cluster CA

```bash
kubectl --context kind-gardener-local config view --raw \
  -o jsonpath='{.clusters[?(@.name=="kind-gardener-local")].cluster.certificate-authority-data}'
# Copy this base64 value — needed in gardenlet values
```

---

## Part 3 — Credentials and RBAC (both clusters)

### 3.1 TCPWave credentials secret

Apply in the **garden** namespace on **both** clusters.

```bash
cat > ~/tcpwave-creds-secret.yaml << 'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: tcpwave-credentials
  namespace: garden
type: Opaque
stringData:
  endpoint:         "https://n1devcmp-user.airteldev.com"
  apiId:            "f54a9387-0432-40b2-8508-8ecfd30be52a"
  secret:           "0iU2PlFfE+7iMcZL/0zswAjKfeAwC+okYtkg"
  organisationName: "qa-tenant"
  organisationId:   "c62e0e9d-011f-4dae-9f98-35e816de6cde"
  projectId:        "943258ef-ea73-40e1-8fbd-8d9dadc60d02"
  projectName:      "qa-first-cell"
  region:           "dev"
EOF

kubectl --context kind-gardener-local  apply -f ~/tcpwave-creds-secret.yaml
kubectl --context kind-gardener-local2 create ns garden --dry-run=client -o yaml | kubectl --context kind-gardener-local2 apply -f -
kubectl --context kind-gardener-local2 apply -f ~/tcpwave-creds-secret.yaml
```

### 3.2 Leader election RBAC for airtelcloud provider

The provider-airtelcloud pod needs a Role + RoleBinding in the `garden` namespace
to manage lease objects (leader election).

```bash
cat > ~/role-leader-election.yaml << 'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: gardener-extension-provider-airtelcloud-leader-election
  namespace: garden
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
EOF

cat > ~/rolebinding-leader-election.yaml << 'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: gardener-extension-provider-airtelcloud-leader-election
  namespace: garden
subjects:
- kind: ServiceAccount
  name: gardener-extension-provider-airtelcloud
  namespace: garden
roleRef:
  kind: Role
  name: gardener-extension-provider-airtelcloud-leader-election
  apiGroup: rbac.authorization.k8s.io
EOF

# Apply on seed cluster (where the provider pod runs)
kubectl --context kind-gardener-local2 apply -f ~/role-leader-election.yaml
kubectl --context kind-gardener-local2 apply -f ~/rolebinding-leader-election.yaml

# Also on garden cluster (where gardenlet service account lives)
kubectl --context kind-gardener-local apply -f ~/role-leader-election.yaml
kubectl --context kind-gardener-local apply -f ~/rolebinding-leader-election.yaml
```

---

## Part 4 — Build and load extension images

All images are loaded directly into the seed's containerd using `ctr import`.
The `registry.local.gardener.cloud:5001` registry is only used as a consistent
image name — you push there AND load into containerd so the seed can pull locally.

> Ensure `/etc/docker/daemon.json` has the insecure registry entry:
> ```json
> { "insecure-registries": ["registry.local.gardener.cloud:5001"] }
> ```
> Then `sudo systemctl restart docker`.

### 4.1 OS extensions

```bash
# gardenlinux
cd ~/extension_repos/gardener-extension-os-gardenlinux
make docker-images
docker tag europe-docker.pkg.dev/gardener-project/public/gardener/extensions/os-gardenlinux:v0.37.0-dev \
  registry.local.gardener.cloud:5001/gardener-extension-os-gardenlinux:v0.1.0
docker push registry.local.gardener.cloud:5001/gardener-extension-os-gardenlinux:v0.1.0
docker save registry.local.gardener.cloud:5001/gardener-extension-os-gardenlinux:v0.1.0 \
  | docker exec -i gardener-local2-control-plane ctr -n k8s.io images import -
./generate-controller-registration.sh
kubectl --context kind-gardener-local apply -f controller-registration.yaml

# ubuntu (if using ubuntu images)
cd ~/extension_repos/gardener-extension-os-ubuntu
make docker-images
docker tag europe-docker.pkg.dev/gardener-project/public/gardener/extensions/os-ubuntu:v1.39.0-dev \
  registry.local.gardener.cloud:5001/gardener-extension-os-ubuntu:v0.1.0
docker push registry.local.gardener.cloud:5001/gardener-extension-os-ubuntu:v0.1.0
docker save registry.local.gardener.cloud:5001/gardener-extension-os-ubuntu:v0.1.0 \
  | docker exec -i gardener-local2-control-plane ctr -n k8s.io images import -
./generate-controller-registration.sh
kubectl --context kind-gardener-local apply -f controller-registration.yaml
```

### 4.2 Airtelcloud provider

```bash
cd ~/extension_repos/gardener-extension-provider-airtelcloud
make docker-images
docker tag europe-docker.pkg.dev/gardener-project/public/gardener/extensions/provider-airtelcloud:v0.1.0-dev \
  registry.local.gardener.cloud:5001/gardener-extension-provider-airtelcloud:v0.1.0
docker push registry.local.gardener.cloud:5001/gardener-extension-provider-airtelcloud:v0.1.0
docker save registry.local.gardener.cloud:5001/gardener-extension-provider-airtelcloud:v0.1.0 \
  | docker exec -i gardener-local2-control-plane ctr -n k8s.io images import -

sh example/generate_controllerregistration.sh
kubectl --context kind-gardener-local apply -f example/controller-registration.yaml
```

### 4.3 F5 extension

```bash
cd ~/extension_repos/gardener-extension-f5
docker build -t registry.local.gardener.cloud:5001/gardener-extension-f5:v0.1.1-dev .
docker push registry.local.gardener.cloud:5001/gardener-extension-f5:v0.1.1-dev
docker save registry.local.gardener.cloud:5001/gardener-extension-f5:v0.1.1-dev \
  | docker exec -i gardener-local2-control-plane ctr -n k8s.io images import -

# CRD must exist on SEED cluster before the extension pod starts
kubectl --context kind-gardener-local2 apply \
  -f charts/gardener-extension-f5/crds/f5loadbalancerconfigs.f5.extensions.gardener.cloud.yaml

# Register on GARDEN cluster
kubectl --context kind-gardener-local apply -f deploy/garden/controllerdeployment-f5.yaml
kubectl --context kind-gardener-local apply -f deploy/garden/controllerregistration-f5.yaml

# NOTE: The `ControllerRegistration` must register `f5-loadbalancer` for Seed usage.
# If your Seed spec includes `extensions: - type: f5-loadbalancer`, ensure the registration has
# `autoEnable: [seed, shoot]` for the `f5-loadbalancer` resource.
```

### 4.4 TCPWave extension

```bash
cd ~/extension_repos/gardener-extension-tcpwave
./deploy.sh   # builds image, loads into seed containerd, generates + applies registration
```

### 4.5 Verify registrations

```bash
kubectl --context kind-gardener-local get controllerregistration
# Expected:
#   gardener-extension-f5
#   provider-dns-tcpwave
#   gardener-extension-provider-airtelcloud
#   os-gardenlinux (or os-ubuntu)
#   networking-calico / networking-cilium  ← from make gardener-up

kubectl --context kind-gardener-local get controllerdeployment
```

---

## Part 5 — Gardenlet values and deployment

### 5.1 Prepare values file

Replace the placeholder values with yours (CA from Step 2.5, IPs from Step 1.4,
token from Step 2.1).

This repo already contains a template you can start from:

```bash
cd ~/extension_repos/gardener-extension-f5

# The repo file uses hyphens; the helm command below expects the underscore name in ~/
cp -f ./gardenlet-airtelcloud-values.yaml ~/gardenlet_airtelcloud_values.yaml
```

```yaml
# ~/gardenlet_airtelcloud_values.yaml
replicaCount: 1

config:
  gardenClientConnection:
    bootstrapKubeconfig:
      name: gardenlet-kubeconfig-bootstrap
      namespace: garden
      kubeconfig: |
        apiVersion: v1
        kind: Config
        current-context: gardenlet-bootstrap@default
        clusters:
        - cluster:
            certificate-authority-data: <GARDEN_CA_BASE64>   # from step 2.5
            server: https://<GARDEN_NODE_IP>:6443             # e.g. 172.18.0.8
          name: default
        contexts:
        - context:
            cluster: default
            user: gardenlet-bootstrap
          name: gardenlet-bootstrap@default
        users:
        - name: gardenlet-bootstrap
          user:
            token: <TOKEN_ID>.<TOKEN_SECRET>   # e.g. f0f23d.ed343cbcc087fa99
    kubeconfigSecret:
      name: gardenlet-kubeconfig
      namespace: garden

  seedConfig:
    metadata:
      name: ojasvi-seed
      labels:
        environment: evaluation
    spec:
      provider:
        type: airtelcloud
        region: regionOne

      dns:
        provider:
          type: tcpwave
          # NOTE: Gardener v1.139+ requires credentialsRef (full ObjectReference).
          # secretRef is NOT valid here — gardenlet will panic at startup if this is missing.
          credentialsRef:
            apiVersion: v1
            kind: Secret
            name: tcpwave-credentials
            namespace: garden
        internal:
          type: tcpwave
          domain: internal.ojasvi-seed.airtel.local
          credentialsRef:
            apiVersion: v1
            kind: Secret
            name: tcpwave-credentials
            namespace: garden

      ingress:
        domain: ingress.ojasvi-seed.airtel.local
        controller:
          kind: nginx

      extensions:
      - type: f5-loadbalancer

      networks:
        nodes: 172.18.0.0/16        # seed node CIDR (from step 1.4)
        pods: 10.1.134.0/16         # seed pod CIDR  (from step 1.4)
        services: 10.2.0.0/16       # seed svc CIDR  (from step 1.4)
```

### 5.2 Deploy gardenlet

```bash
kubectl config use-context kind-gardener-local2

# NOTE: The gardenlet Helm chart location differs between Gardener checkouts.
# Find it in your local Gardener repo first.
cd ~/gardener/gardener

GARDENLET_CHART_DIR=$(find ./charts -maxdepth 6 -type f -name Chart.yaml \
  | grep -i '/gardenlet/Chart.yaml$' \
  | head -n 1 \
  | xargs -I {} dirname {})

if [[ -z "${GARDENLET_CHART_DIR}" ]]; then
  echo "ERROR: Could not find gardenlet Helm chart under ./charts"
  echo "Try: find ./charts -maxdepth 6 -type f -name Chart.yaml | sort"
  exit 1
fi

echo "Using gardenlet chart: ${GARDENLET_CHART_DIR}"

# Optional: fix kubeconfig permission warning from helm/kubectl
chmod 600 ~/.kube/config

helm upgrade --install gardenlet \
  "${GARDENLET_CHART_DIR}" \
  -n garden --create-namespace \
  -f ~/gardenlet_airtelcloud_values.yaml
```

### 5.3 Watch gardenlet bootstrap

```bash
# Gardenlet pod comes up and self-registers the Seed
kubectl --context kind-gardener-local2 get pods -n garden -w

# On garden cluster — watch Seed become Ready
kubectl --context kind-gardener-local get seed ojasvi-seed -w
# Takes 3-10 min. Status moves:  Unknown → Bootstrapping → Ready
```

### What gardenlet does during seed reconciliation

1. Deploys CRDs (machine, extension, etcd, Istio, VPA ...)
2. Deploys `gardener-resource-manager`
3. Copies `tcpwave-credentials` from garden → seed namespace
4. Waits for required ControllerInstallations (Gardener creates them automatically):
   - `gardener-extension-f5-xxxxx` → F5 pod in `extension-gardener-extension-f5-xxxxx` ns
   - `provider-dns-tcpwave-xxxxx`  → TCPWave pod
   - `provider-airtelcloud-xxxxx`  → Airtelcloud pod
5. Deploys Istio
6. Deploys nginx-ingress
7. Waits for `istio-ingressgateway` Service `status.loadBalancer.ingress[].ip` to be populated
   — either by `seed-service-lb-controller` (CMP) or by manual BIG-IP patch (Part 4.4)
8. Creates `DNSRecord/seed-ingress` → TCPWave extension creates `*.ingress.hema-seed` A record
9. Seed status flips to **Ready**

---

## Part 6 — Shoot prerequisites

All objects go on the **garden cluster** (`kind-gardener-local`).

### 6.1 Project

```bash
cat > ~/shoot-prereqs/05-project.yaml << 'EOF'
apiVersion: core.gardener.cloud/v1beta1
kind: Project
metadata:
  name: dev
spec:
  namespace: garden-dev
  owner:
    apiGroup: rbac.authorization.k8s.io
    kind: User
    name: admin@example.com
  members:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: admin@example.com
    roles:
    - admin
EOF

kubectl --context kind-gardener-local apply -f ~/shoot-prereqs/05-project.yaml
kubectl --context kind-gardener-local get namespace garden-dev   # wait until exists
```

### 6.2 CloudProfile

Look up these values from OpenStack first:
```bash
source app-cred-gardener-openrc.sh
export OS_INSECURE=true
openstack image list | grep -i ubuntu          # → Glance image ID
openstack network list --external              # → floating pool name
openstack flavor list                          # → flavor names
```

```bash
cat > ~/shoot-prereqs/30-cloudprofile.yaml << 'EOF'
apiVersion: core.gardener.cloud/v1beta1
kind: CloudProfile
metadata:
  name: airtelcloud
spec:
  type: airtelcloud
  kubernetes:
    versions:
    - version: 1.34.2
      classification: supported
    - version: 1.33.5
      classification: supported
  machineImages:
  - name: ubuntu
    versions:
    - version: 22.04.0
      architectures: [amd64]
      cri:
      - name: containerd
  - name: gardenlinux
    versions:
    - version: 1592.1.0
      architectures: [amd64]
      cri:
      - name: containerd
  machineTypes:
  - name: m1.medium
    cpu: "2"
    gpu: "0"
    memory: 4Gi
    usable: true
    architecture: amd64
    storage:
      class: standard
      type: default
      size: 40Gi
  volumeTypes:
  - name: standard
    class: standard
    usable: true
  regions:
  - name: regionOne
    zones:
    - name: nova
  providerConfig:
    apiVersion: airtelcloud.provider.extensions.gardener.cloud/v1alpha1
    kind: CloudProfileConfig
    keystoneURL: "https://100.65.247.153:13000/v3/"
    dhcpDomain: "airtelcloud.local"
    requestTimeout: 60s
    useOctavia: true
    constraints:
      floatingPools:
      - name: external        # openstack network list --external
        region: regionOne
      loadBalancerProviders:
      - name: f5
        region: regionOne
    machineImages:
    - name: ubuntu
      versions:
      - version: 22.04.0
        image: ubuntu-22.04-server-cloudimg-amd64
        regions:
        - name: regionOne
          id: "<GLANCE_IMAGE_ID>"    # openstack image list | grep ubuntu-22
          architecture: amd64
    - name: gardenlinux
      versions:
      - version: 1592.1.0
        image: gardenlinux-1592.1
        regions:
        - name: regionOne
          id: "<GLANCE_GARDENLINUX_ID>"
          architecture: amd64
EOF

kubectl --context kind-gardener-local apply -f ~/shoot-prereqs/30-cloudprofile.yaml
```

### 6.3 Cloud Provider Secret

```bash
cat > ~/shoot-prereqs/70-openstack-secret.yaml << 'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: airtelcloud-creds
  namespace: garden-dev
type: Opaque
stringData:
  auth-url:                      "https://100.65.247.153:13000/v3/"
  application-credential-id:     "<APP_CRED_ID>"
  application-credential-secret: "<APP_CRED_SECRET>"
EOF

# Get app credentials:
# openstack application credential create gardener-shoot \
#   --description "Gardener shoot provisioning"

kubectl --context kind-gardener-local apply -f ~/shoot-prereqs/70-openstack-secret.yaml
```

### 6.4 CredentialsBinding

```bash
cat > ~/shoot-prereqs/80-credentialsbinding.yaml << 'EOF'
apiVersion: security.gardener.cloud/v1alpha1
kind: CredentialsBinding
metadata:
  name: airtelcloud-creds
  namespace: garden-dev
provider:
  type: airtelcloud
credentialsRef:
  apiVersion: v1
  kind: Secret
  name: airtelcloud-creds
  namespace: garden-dev
quotas: []
EOF

kubectl --context kind-gardener-local apply -f ~/shoot-prereqs/80-credentialsbinding.yaml
```

### 6.5 Shoot

```bash
cat > ~/shoot-prereqs/90-shoot.yaml << 'EOF'
apiVersion: core.gardener.cloud/v1beta1
kind: Shoot
metadata:
  name: hema-shoot
  namespace: garden-dev
  annotations:
    shoot.gardener.cloud/cloud-config-execution-max-delay-seconds: "300"
spec:
  seedName: ojasvi-seed
  cloudProfile:
    name: airtelcloud
  credentialsBindingName: airtelcloud-creds
  region: regionOne
  purpose: evaluation

  kubernetes:
    version: "1.34.2"

  networking:
    type: calico
    nodes: "10.250.0.0/16"
    pods: "10.243.128.0/17"
    services: "10.243.0.0/17"

  provider:
    type: airtelcloud
    infrastructureConfig:
      apiVersion: airtelcloud.provider.extensions.gardener.cloud/v1alpha1
      kind: InfrastructureConfig
      networks:
        vpc:
          id: "<OPENSTACK_VPC_ID>"    # openstack network list | grep <your network>
        workers: "10.250.0.0/16"
    controlPlaneConfig:
      apiVersion: airtelcloud.provider.extensions.gardener.cloud/v1alpha1
      kind: ControlPlaneConfig
      loadBalancer:
        provider: f5
    workers:
    - name: worker-pool-1
      machine:
        type: m1.medium
        image:
          name: gardenlinux
          version: "1592.1.0"
      minimum: 1
      maximum: 3
      maxSurge: 1
      maxUnavailable: 0
      zones:
      - nova
      volume:
        type: standard
        size: 50Gi
      cri:
        name: containerd

  extensions:
  - type: f5-loadbalancer
  - type: tcpwave-dns
    providerConfig:
      apiVersion: tcpwave.extensions.gardener.cloud/v1alpha1
      kind: TCPWaveConfig
      apiServer:
        hostname: api.hema-shoot.garden-dev.internal
        targetIP: 172.18.255.1     # Seed Istio VIP (set after seed is Ready)
        ttl: 300
      zone: test-gkaas.com
      tcpwave:
        endpoint: https://n1devcmp-user.airteldev.com
        credentialsSecretRef:
          name: tcpwave-credentials
          namespace: garden

  maintenance:
    autoUpdate:
      kubernetesVersion: true
      machineImageVersion: true
    timeWindow:
      begin: "020000+0000"
      end: "030000+0000"

  hibernation:
    schedules: []
EOF

kubectl --context kind-gardener-local apply -f ~/shoot-prereqs/90-shoot.yaml
kubectl --context kind-gardener-local -n garden-dev get shoot hema-shoot -w
```

---

## Part 7 — Shoot reconciliation flow (what happens automatically)

```
Shoot CR applied on garden cluster
        │
gardenlet picks it up → creates shoot namespace on SEED
        │
        ├── Extension/f5-loadbalancer  → F5 extension pod reconciles
        │     Sets ControlPlaneLoadBalancerReady=True (shared Seed Ingress VIP)
        │
        ├── Extension/tcpwave-dns      → TCPWave extension pod reconciles
        │     Creates A record: api.hema-shoot.garden-dev.internal → 172.18.255.1
        │
        ├── Infrastructure CR → provider-airtelcloud
        │     Verifies OpenStack VPC/subnet exists
        │     Creates router, security groups if needed
        │     Writes InfrastructureStatus
        │
        ├── ControlPlane CR → provider-airtelcloud
        │     Sets up cloud-controller-manager config
        │
        ├── etcd, kube-apiserver, kube-scheduler, kube-controller-manager
        │     (deployed by gardenlet directly)
        │
        ├── DNSRecord: <shoot>-internal → TCPWave creates A record
        │
        └── Worker CR → provider-airtelcloud
              MachineDeployments created
              MCM + provider-openstack provisions OpenStack VMs
              VMs bootstrap via cloud-init (gardener-node-agent)
              Nodes join Shoot cluster
              Shoot becomes Healthy
```

### Dependency chain

```
Project  (garden-dev namespace)
  └── CloudProfile airtelcloud        (cluster-scoped)
  └── Secret airtelcloud-creds        (in garden-dev)
        └── CredentialsBinding        (in garden-dev, type: airtelcloud)
               └── Shoot              (references credentialsBindingName + cloudProfile)
                     └── gardenlet schedules onto seed hema-seed
                           ├── provider-airtelcloud: Infrastructure / ControlPlane / Worker
                           ├── gardener-extension-tcpwave: DNSRecord for shoot
                           └── gardener-extension-f5: F5LoadBalancerConfig
```

---

## Part 8 — Verification commands

```bash
# Seed status
kubectl --context kind-gardener-local get seed ojasvi-seed

# Extension pods on seed
kubectl --context kind-gardener-local2 get pods -A | grep -E "extension|gardenlet"

# ControllerInstallations (garden cluster)
kubectl --context kind-gardener-local get controllerinstallation

# Shoot status
kubectl --context kind-gardener-local -n garden-dev get shoot hema-shoot

# Extension status inside shoot namespace (seed cluster)
kubectl --context kind-gardener-local2 get extension -n shoot--garden-dev--hema-shoot
kubectl --context kind-gardener-local2 get dnsrecord -n shoot--garden-dev--hema-shoot

# F5LoadBalancerConfig
kubectl --context kind-gardener-local2 get f5loadbalancerconfig -A

# Infrastructure status
kubectl --context kind-gardener-local2 get infrastructure -A

# Seed ingress DNS record
kubectl --context kind-gardener-local2 get dnsrecord -n garden
```

---

## Part 9 — Seed cleanup (start over)

```bash
GARDEN="kind-gardener-local"
SEED="kind-gardener-local2"

# 1. Scale down gardenlet on seed to stop reconciliation
kubectl --context $SEED -n garden scale deployment gardenlet --replicas=0

# 2. Remove ControllerInstallations and Seed from garden
kubectl --context $GARDEN get controllerinstallation -o json \
  | jq -r '.items[] | select(.spec.seedRef.name=="ojasvi-seed") | .metadata.name' \
  | xargs -I {} kubectl --context $GARDEN patch controllerinstallation {} \
      --type=merge -p '{"metadata":{"finalizers":[]}}'

kubectl --context $GARDEN get controllerinstallation -o json \
  | jq -r '.items[] | select(.spec.seedRef.name=="ojasvi-seed") | .metadata.name' \
  | xargs kubectl --context $GARDEN delete controllerinstallation

kubectl --context $GARDEN patch seed ojasvi-seed \
  -p '{"metadata":{"finalizers":null}}' --type=merge
kubectl --context $GARDEN delete seed ojasvi-seed --wait=false

# 3. Clean up seed cluster
kubectl --context $SEED get managedresources -n garden -o name \
  | xargs -I {} kubectl --context $SEED patch {} -n garden \
      --type merge -p '{"metadata":{"finalizers":[]}}'
kubectl --context $SEED delete managedresources --all -n garden

kubectl --context $SEED get ns -o json \
  | jq -r '.items[] | select(.metadata.name | startswith("extension-")) | .metadata.name' \
  | xargs -I {} kubectl --context $SEED patch ns {} \
      -p '{"metadata":{"finalizers":[]}}' --type=merge
kubectl --context $SEED get ns -o json \
  | jq -r '.items[] | select(.metadata.name | startswith("extension-")) | .metadata.name' \
  | xargs kubectl --context $SEED delete ns

# Force-finalize garden namespace
kubectl --context $SEED get ns garden -o json \
  | jq '.spec.finalizers=[]' \
  | kubectl --context $SEED replace \
      --raw "/api/v1/namespaces/garden/finalize" -f -

# 4. Delete the kind cluster
kind delete cluster --name gardener-local2

# 5. Clean up bootstrap token on garden cluster
kubectl --context $GARDEN -n kube-system get secrets | grep bootstrap-token
# Delete the one you created manually
```

---

## Troubleshooting quick reference

| Symptom | Check |
|---|---|
| gardenlet stays in `Pending` | `kubectl --context $SEED describe pod -n garden gardenlet-xxx` — likely image pull |
| Seed stays `Unknown` | `kubectl --context $GARDEN describe seed ojasvi-seed` — check bootstrap secret name matches token |
| Extension pod `ErrImageNeverPull` | Re-run `docker save ... \| ctr import -` with exact image name |
| `quay.io` ImagePullBackOff | `sudo chmod -R a+rX ~/gardener/dev-setup/infra/bind && docker restart bind9` |
| TCPWave `401 Authentication failed` | Check `seed-ingress` secret keys match what extension reads (`apiId`, `secret`, `organisationName`, `projectId`) |
| Shoot stuck at Infrastructure | MCM provider-openstack missing — patch `machine-controller-manager` deployment to add provider sidecar |
| No space left on device during `make gardener-up` | Extend `/home` (+20G) and `/tmp` (+10G) via `lvextend` + `xfs_growfs` |
| Seed stuck in `Bootstrapping` — Istio Service has no external IP | `seed-service-lb-controller` not deployed or CMP unreachable. Manual fix: create VIP on BIG-IP then `kubectl patch svc istio-ingressgateway -n istio-system --subresource=status --type=merge -p '{"status":{"loadBalancer":{"ingress":[{"ip":"<BIGIP_VIP>"}]}}}'` |
| Status patch on Istio Service disappears / gets cleared | Controller (e.g. cloud-controller-manager) is reconciling the Service and wiping status. Ensure `seed-service-lb-controller` is running so it can re-assert the IP, or check what controller is managing that Service class. |
