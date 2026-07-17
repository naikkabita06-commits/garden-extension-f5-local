# Gardener Platform: Complete Setup, Implementation & Blocker Guide

**Date:** 15 May 2026
**Scope:** Everything in one place — architecture context, step-by-step implementation from empty VM to running Shoots, offline workarounds, per-extension deep dives, and dependency/blocker analysis.

> **⚠️ IMPORTANT — CMP API is not reachable via HTTP:**
> All cloud resources must be pre-created through the **CMP web UI** in a browser — not via curl/API calls. The extension code's runtime HTTP calls to CMP (LBaaS, DNS, Compute) will also fail until API connectivity is restored. This guide accounts for this limitation: **every resource that the code would normally create via API is instead pre-created in the CMP UI, and the corresponding Kubernetes objects are manually patched.** Phase 2 covers static resources, Phase 6 covers per-extension runtime resources, and Appendix C provides the complete step-by-step UI workflow.

---

## Document Structure

| Part | What it covers | When to read |
|---|---|---|
| **Part I: Architecture & Context** | 3-tier topology, extension roles, CMP explanation, repository map, CMP LBaaS resource chain | First — understand the system before building it |
| **Part II: Implementation (Phases 1–7)** | Chronological steps from empty VM → running Shoots, including offline workarounds | During implementation — follow step by step |
| **Part III: Per-Extension Deep Dive** | Detailed reconciler flows, code paths for all 5 extensions | When debugging or exploring code |
| **Part IV: Debugging & Troubleshooting** | Log commands, common issues table, port-forwarding | When things break |
| **Part V: Dependency & Blocker Matrix** | All 39 blockers by phase, failure cascades, critical paths, offline analysis, pre-implementation checklist | Before starting — identify what's missing; during implementation — diagnose stuck steps |

---

# Part I: Architecture & Context

## 1.1 Three-Tier Topology

Gardener is a Kubernetes-native system that manages Kubernetes clusters (called **Shoots**) at scale:

```
┌────────────────────────────────────────────────────────────────────┐
│                        GARDEN CLUSTER                              │
│  Central management plane. Runs:                                   │
│  - Gardener API Server (extends K8s API with Shoot, Seed, etc.)   │
│  - Gardener Controller Manager (quota, project lifecycle)          │
│  - Gardener Scheduler (assigns Shoots to Seeds)                   │
│  - Gardener Admission Controller                                   │
└────────────────────────────┬───────────────────────────────────────┘
                             │ gardenlet registers Seed
┌────────────────────────────▼───────────────────────────────────────┐
│                         SEED CLUSTER                               │
│  Hosts Shoot control planes. Runs:                                 │
│  - gardenlet (manages Shoot lifecycle on this Seed)               │
│  - Extension controllers (F5, TCPWave, airtelcloud provider, etc.)│
│  - Istio ingress gateway (SNI routing to Shoot apiservers)        │
│  - Per-Shoot namespace: etcd, kube-apiserver, controller-manager  │
└────────────────────────────┬───────────────────────────────────────┘
                             │ kubelet joins via bootstrap
┌────────────────────────────▼───────────────────────────────────────┐
│                        SHOOT CLUSTER                               │
│  Tenant workload cluster. Runs:                                    │
│  - Worker nodes (VMs provisioned by provider extension)           │
│  - kubelet, kube-proxy, CNI (Calico)                              │
│  - svc-lb-bridge (F5 app-plane LB)                                │
│  - Application workloads                                           │
└────────────────────────────────────────────────────────────────────┘
```

## 1.2 Extension Roles

| Extension | Layer | What it provisions | External API |
|---|---|---|---|
| **airtelcloud provider** | Infrastructure | VMs, networks, security groups, volumes | CMP Compute/Networking APIs |
| **F5 extension** | Load Balancing | VIPs, Virtual Servers for apiserver + app Services | CMP LBaaS v2.1 API |
| **TCPWave extension** | DNS | DNS records for Shoot domains, Seed ingress | CMP DNS / TCPWave IPAM API |
| **NetApp backup** | Backup | etcd backup/restore to NetApp storage | NetApp ONTAP API |
| **Trident storage** | Storage | PersistentVolumes for Shoot workloads | NetApp Trident CSI |

> **CMP (Cloud Management Platform)** is the unified Airtel Cloud portal and API layer. All cloud resources — compute, networking, load balancers, DNS, storage, API keys — are created and managed through CMP. Each extension calls different CMP API subsystems (Compute, Networking, LBaaS, DNS) but they all operate within the same CMP organization and project.

## 1.3 Repository Map

### Airtel Cloud Provider Extension

**Role:** Infrastructure lifecycle — VMs, networks, security groups, volumes.

**Gardener contract:** `Infrastructure`, `ControlPlane`, `Worker` extension kinds.

```
pkg/controller/infrastructure/    ← Reconciles Infrastructure CR (VPC, subnet, SG, router)
pkg/controller/controlplane/      ← Reconciles ControlPlane CR (CCM, CSI)
pkg/controller/worker/            ← Reconciles Worker CR (MachineDeployments)
pkg/apis/airtelcloud/             ← InfrastructureConfig, ControlPlaneConfig types
```

**External API:** CMP Compute & Networking (OpenStack-compatible: Nova, Neutron, Cinder)

---

### F5 Load Balancer Extension (this repo)

**Role:** Load balancing for control-plane (Shoot kube-apiserver VIP) and application-plane (`type: LoadBalancer` Services).

**Gardener contract:** `Extension` kind (`f5-loadbalancer` / `f5`)

```
pkg/controller/lifecycle/controller.go           ← Extension actuator (Reconcile/Delete/Migrate/Restore)
pkg/controller/lifecycle/extension_controller.go  ← Controller registration + Secret watch
cmd/svc-lb-bridge/main.go                        ← Shoot-side Service LB controller
cmd/svc-lb-bridge/ingress_controller.go           ← Shoot-side Ingress controller
cmd/seed-service-lb-controller/main.go            ← Seed-side Service LB controller
pkg/f5/client.go                                  ← CMP HTTP client
```

**CMP LBaaS resource chain (extension attempts to create via HTTP — pre-create these in CMP UI instead):**

```
LBService                          ← Container object; scoped to VPC
  ├── VIP (ip_address)             ← Virtual IP allocated from the VPC subnet
  └── VirtualServer                ← Listener on the VIP; routes traffic to backend nodes
        ├── protocol (TCP/HTTP)
        ├── port (frontend port)
        ├── routing_algorithm (round-robin, least-connections, etc.)
        ├── health monitor (interval, type, path)
        ├── persistence (source_ip, cookie, etc.)
        ├── connection_draining_timeout
        ├── allowed_cidrs (source IP restrictions)
        └── nodes[] (backend pool members)
              ├── compute_ip (node IP)
              ├── port (NodePort on the worker)
              └── weight (traffic share)
```

| CMP Resource | What it is | Who creates it | Code path (`pkg/f5/client.go`) | UI alternative |
|---|---|---|---|---|
| **LBService** | Top-level LB container, tied to VPC | Extension (find-or-create) | `POST /lb_service/` | CMP UI → Load Balancers → Create |
| **VIP** | Virtual IP from VPC subnet | Extension (one per LBService) | `POST /lb_service/{id}/vip` | Auto-created with LB in UI |
| **VirtualServer** | Listener: protocol+port → backend pool | Extension (one per Service port) | `POST /load-balancers/{id}/virtual-servers` | CMP UI → LB → Add Listener |
| **Nodes** | Backend targets: node IPs + NodePorts | Embedded in VS create | `nodes[]` param | CMP UI → Listener → Add Members |
| **LB Flavor** | Size/tier of LB | Pre-exists; referenced by `flavor_id` | `GET /lb_flavors/` | CMP UI → LB Flavors |

**Per-Shoot resource count:**
- Control-plane LB (Mechanism B): 1 LBService + 1 VIP + 1 VS (port 443 → apiserver NodePort)
- App-plane LB: 1 LBService per Service + 1 VIP + 1 VS per port

---

### TCPWave DNS Extension

**Role:** DNS record management for Shoot API endpoints and Seed ingress.

**Gardener contract:** `DNSRecord` extension kind.

```
pkg/controller/dnsrecord/    ← Reconciles DNSRecord CR
pkg/tcpwave/client.go        ← TCPWave IPAM/DNS API client
```

---

### NetApp Backup Extension

**Role:** etcd backup/restore using NetApp storage.

**Gardener contract:** `BackupBucket` and `BackupEntry` extension kinds.

```
pkg/controller/backupbucket/    ← Creates storage bucket/volume
pkg/controller/backupentry/     ← Manages backup objects
```

---

### Trident Storage Extension

**Role:** PersistentVolumes in Shoot clusters via NetApp Trident CSI.

```
charts/                      ← Helm charts for Trident CSI + StorageClasses
pkg/controller/              ← Deploys Trident into Shoots
```

## 1.4 Sequence Diagrams

### Shoot Creation → Ready

```
User                    Garden          Scheduler       gardenlet (Seed)      Extensions
  │                       │                │                │                    │
  │── kubectl apply ──────▶                │                │                    │
  │   Shoot manifest      │                │                │                    │
  │                       │── Shoot ───────▶                │                    │
  │                       │   created      │── Assign ──────▶                    │
  │                       │                │   to Seed      │                    │
  │                       │                │                │── Create ns ───────│
  │                       │                │                │   shoot--p--s      │
  │                       │                │                │                    │
  │                       │                │                │── Infrastructure ──▶ airtelcloud
  │                       │                │                │   CR created       │── VPC, subnet,
  │                       │                │                │                    │   SG via OpenStack
  │                       │                │                │◀── status: ready ──│
  │                       │                │                │                    │
  │                       │                │                │── DNSRecord ───────▶ TCPWave
  │                       │                │                │   CR created       │── A record
  │                       │                │                │◀── status: ready ──│
  │                       │                │                │                    │
  │                       │                │                │── BackupBucket ────▶ NetApp backup
  │                       │                │                │   CR created       │── storage bucket
  │                       │                │                │◀── status: ready ──│
  │                       │                │                │                    │
  │                       │                │                │── Deploy etcd ─────│
  │                       │                │                │── Deploy apiserver─│
  │                       │                │                │                    │
  │                       │                │                │── Extension CR ────▶ F5 extension
  │                       │                │                │   (f5-loadbalancer)│── CMP: LBService
  │                       │                │                │                    │── CMP: VIP
  │                       │                │                │                    │── CMP: VirtualServer
  │                       │                │                │                    │── Deploy svc-lb-bridge
  │                       │                │                │◀── status: ready ──│
  │                       │                │                │                    │
  │                       │                │                │── Worker CR ───────▶ airtelcloud
  │                       │                │                │   created          │── VMs via Nova
  │                       │                │                │                    │── kubelet bootstrap
  │                       │                │                │◀── status: ready ──│
  │                       │                │                │                    │
  │                       │                │                │── Shoot Ready ─────│
  │◀── Shoot status: ────│                │                │                    │
  │    Ready              │                │                │                    │
```

### Application LB Flow (after Shoot is Ready)

> **⚠️ Requires CMP API access.** Without it, pre-create LBs in CMP UI and manually patch Service status (see Phase 6).

```
Developer                  Shoot API          svc-lb-bridge         CMP API          F5 BIG-IP
    │                         │                    │                    │                │
    │── kubectl apply ────────▶                    │                    │                │
    │   Service type=LB       │                    │                    │                │
    │                         │── watch event ─────▶                    │                │
    │                         │                    │── POST lb_service ─▶                │
    │                         │                    │◀── lb_service_id ──│                │
    │                         │                    │── POST vip ────────▶                │
    │                         │                    │◀── vip: 10.0.0.5 ─│                │
    │                         │                    │── POST virtual ────▶                │
    │                         │                    │   server           │── Provision ───▶
    │                         │                    │◀── vs_id ──────────│   VS + pool    │
    │                         │                    │                    │                │
    │                         │◀── patch status ───│                    │                │
    │                         │    ip: 10.0.0.5    │                    │                │
    │                         │                    │                    │                │
    │── curl 10.0.0.5 ────────────────────────────────────────────────────────────────▶│
    │◀── app response ──────────────────────────────────────────────────────────────────│
```

## 1.5 Secret Handling (all extensions)

| Secret | Namespace | Contains | Created by | Consumed by |
|---|---|---|---|---|
| `airtelcloud-credentials` | `garden-<project>` | OpenStack app credentials | Operator (manual) | airtelcloud provider |
| `tcpwave-credentials` | `garden` | TCPWave API ID + secret | Operator (manual) | TCPWave extension |
| `cmp-f5-credentials` | `shoot--project--shoot` | CMP api-key-id + api-secret + project-id | Operator (manual) | F5 extension |
| `netapp-backup-credentials` | `garden` | NetApp ONTAP credentials | Operator (manual) | NetApp backup |
| `trident-backend-config` | Shoot cluster | NetApp SVM credentials | Trident deployer | Trident CSI |
| `gardenlet-kubeconfig` | `garden` (Seed) | Garden API kubeconfig | gardenlet (auto) | gardenlet |
| Shoot kubeconfig | `shoot--project--shoot` | Shoot API kubeconfig | gardenlet (auto) | F5 extension (deploys svc-lb-bridge) |

---

# Part II: Implementation

---

# Phase 1: Starting from an Empty VM

## 1.1 OS Prerequisites

Start with a **RHEL 8/9** or **Ubuntu 22.04** server VM. Minimum specs:

| Resource | Minimum | Recommended | Why |
|---|---|---|---|
| vCPUs | 4 | 8 | KIND clusters + Go compilation are CPU-intensive |
| RAM | 16 GB | 32 GB | Two KIND clusters + extension pods + etcd instances |
| Disk | 100 GB | 200 GB | Docker images, Go module cache, container layers eat ~50-80 GB |
| Network | Internal LAN access | + CMP endpoint reachable | Seed must reach CMP APIs; see Phase 6 for offline mode |

**Disk partitions to check** (RHEL with LVM):
```bash
df -h /var /home /tmp
# /var needs ~6 GB free (Docker data)
# /home needs ~20 GB free (Go module cache, repos)
# /tmp needs ~10 GB free (Go build temp files)

# If low:
sudo lvextend -L +6G /dev/mapper/rhel-var && sudo xfs_growfs /var
sudo lvextend -L +20G /dev/mapper/rhel-home && sudo xfs_growfs /home
sudo lvextend -L +10G /dev/mapper/rhel-tmp && sudo xfs_growfs /tmp
```

## 1.2 Package Installation (in order)

### Step 1: System packages

```bash
# RHEL/CentOS
sudo dnf install -y git curl wget tar gzip jq openssl make gcc

# Ubuntu
sudo apt update && sudo apt install -y git curl wget tar gzip jq openssl make gcc build-essential
```

### Step 2: Docker (container runtime)

```bash
# RHEL
sudo dnf config-manager --add-repo https://download.docker.com/linux/rhel/docker-ce.repo
sudo dnf install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
sudo systemctl enable --now docker
sudo usermod -aG docker $USER
newgrp docker

# Ubuntu
curl -fsSL https://get.docker.com | sh
sudo usermod -aG docker $USER
newgrp docker
```

**Configure insecure registry** (needed for KIND local registry):
```bash
sudo mkdir -p /etc/docker
cat <<EOF | sudo tee /etc/docker/daemon.json
{
  "insecure-registries": ["registry.local.gardener.cloud:5001"]
}
EOF
sudo systemctl restart docker
```

### Step 3: Go (1.24.6+)

```bash
GO_VERSION=1.24.6
wget "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz"
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf "go${GO_VERSION}.linux-amd64.tar.gz"
rm "go${GO_VERSION}.linux-amd64.tar.gz"

# Add to ~/.bashrc or ~/.zshrc:
echo 'export PATH=$PATH:/usr/local/go/bin:$HOME/go/bin' >> ~/.bashrc
source ~/.bashrc

go version
# go1.24.6 linux/amd64
```

### Step 4: kubectl

```bash
KUBECTL_VERSION=v1.34.2
curl -LO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl"
chmod +x kubectl
sudo mv kubectl /usr/local/bin/

kubectl version --client
```

### Step 5: Helm

```bash
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
helm version
```

### Step 6: KIND (Kubernetes IN Docker)

```bash
KIND_VERSION=v0.25.0
curl -Lo kind "https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64"
chmod +x kind
sudo mv kind /usr/local/bin/

kind version
```

### Step 7: yq (YAML processor, used by Gardener scripts)

```bash
YQ_VERSION=v4.44.3
wget "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_amd64" -O yq
chmod +x yq
sudo mv yq /usr/local/bin/

yq --version
```

## 1.3 Repository Cloning

```bash
mkdir -p ~/extension_repos && cd ~/extension_repos

# 1. Gardener core (contains KIND setup, gardenlet chart, all core controllers)
git clone https://github.com/gardener/gardener.git
cd gardener && git checkout v1.135.1 && cd ..

# 2. Airtel Cloud provider extension
git clone <your-org>/gardener-extension-provider-airtelcloud.git

# 3. F5 LB extension (this repo)
git clone <your-org>/gardener-extension-f5.git

# 4. TCPWave DNS extension
git clone <your-org>/gardener-extension-tcpwave.git

# 5. NetApp backup extension
git clone <your-org>/gardener-extension-netapp-backup.git

# 6. Trident storage extension (if separate repo)
git clone <your-org>/gardener-extension-trident.git
```

## 1.4 Environment Variables

```bash
cat > ~/gardener-env.sh << 'ENVEOF'
#!/bin/bash

# ---- Cluster contexts ----
export GARDEN_CTX="kind-gardener-local"
export SEED_CTX="kind-gardener-local2"

# ---- Container registry ----
export LOCAL_REGISTRY="registry.local.gardener.cloud:5001"

# ---- CMP / Airtel Cloud credentials ----
# These come from CMP UI → Phase 2
export CMP_ENDPOINT="https://n1devcmp-user.airteldev.com"
export CMP_ORG_NAME="qa-tenant"
export CMP_ORG_ID="c62e0e9d-011f-4dae-9f98-35e816de6cde"
export CMP_PROJECT_ID="943258ef-ea73-40e1-8fbd-8d9dadc60d02"
export CMP_PROJECT_NAME="qa-first-cell"
export CMP_API_KEY_ID=""       # Fill after generating in CMP UI
export CMP_API_SECRET=""       # Fill after generating in CMP UI

# ---- OpenStack / IaaS credentials ----
export OS_AUTH_URL="https://100.65.247.153:13000/v3/"
export OS_APP_CRED_ID=""       # Fill after creating in CMP UI
export OS_APP_CRED_SECRET=""   # Fill after creating in CMP UI

# ---- TCPWave credentials ----
export TCPWAVE_ENDPOINT="https://n1devcmp-user.airteldev.com"
export TCPWAVE_API_ID="f54a9387-0432-40b2-8508-8ecfd30be52a"
export TCPWAVE_SECRET="0iU2PlFfE+7iMcZL/0zswAjKfeAwC+okYtkg"

# ---- Network CIDRs (from CMP UI VPC/subnet creation) ----
export SEED_NODE_CIDR="172.18.0.0/16"
export SEED_POD_CIDR="10.1.134.0/16"
export SEED_SVC_CIDR="10.2.0.0/16"
export SHOOT_WORKER_CIDR="10.250.0.0/16"

# ---- Image versions ----
export F5_IMAGE="${LOCAL_REGISTRY}/gardener-extension-f5:v0.1.1-dev"
export PROVIDER_IMAGE="${LOCAL_REGISTRY}/gardener-extension-provider-airtelcloud:v0.1.0"
export TCPWAVE_IMAGE="${LOCAL_REGISTRY}/gardener-extension-tcpwave:v0.1.0"

# ---- Paths ----
export REPOS_DIR="${HOME}/extension_repos"
export GARDENER_DIR="${REPOS_DIR}/gardener"

ENVEOF

chmod +x ~/gardener-env.sh
source ~/gardener-env.sh
```

## 1.5 Kubeconfig and Certificate Preparation

At this stage, no kubeconfigs exist yet. They will be created when KIND clusters are brought up in Phase 4. Prepare the directory structure:

```bash
mkdir -p ~/.kube
mkdir -p ~/shoot-prereqs
mkdir -p ~/secrets
```

## 1.6 Verification: Is the VM Ready?

```bash
echo "=== VM Readiness Check ==="
docker version          && echo "✓ Docker" || echo "✗ Docker"
go version              && echo "✓ Go" || echo "✗ Go"
kubectl version --client && echo "✓ kubectl" || echo "✗ kubectl"
helm version            && echo "✓ Helm" || echo "✗ Helm"
kind version            && echo "✓ KIND" || echo "✗ KIND"
yq --version            && echo "✓ yq" || echo "✗ yq"
git --version           && echo "✓ git" || echo "✗ git"
jq --version            && echo "✓ jq" || echo "✗ jq"
openssl version         && echo "✓ openssl" || echo "✗ openssl"

echo ""
echo "Disk free:"
df -h /var /home /tmp 2>/dev/null || df -h /
echo ""
echo "Docker info:"
docker info 2>/dev/null | grep -E "Server Version|Storage Driver|Docker Root Dir"
echo ""
echo "Repos cloned:"
ls -1 ~/extension_repos/
```

---

# Phase 2: Infrastructure Preparation from CMP UI

CMP (Cloud Management Platform) is the unified Airtel Cloud portal. **All** cloud resources — compute, networking, LB, DNS, storage, API keys — are created here via the web UI (not curl).

## 2.1 Resource Creation Timeline

```
CMP UI Actions (manual, done ONCE before code runs)
│
├── Stage A: Account setup (before anything)
│   ├── A1. Organization — pre-exists (your tenant)
│   └── A2. Project — create or use existing
│
├── Stage B: Networking (before Seed cluster)
│   ├── B1. VPC
│   ├── B2. Subnets (seed-nodes, shoot-workers, storage-data)
│   └── B3. Security Groups
│
├── Stage C: Compute prerequisites (before Shoot creation)
│   ├── C1. SSH Key Pair
│   ├── C2. Machine Image (upload to Glance / image catalog)
│   └── C3. Machine Flavor (verify exists)
│
├── Stage D: Credentials (before gardenlet deploy)
│   ├── D1. API Keys (for CMP LBaaS + DNS + Compute APIs)
│   └── D2. Application Credentials (OpenStack)
│
└── Stage E: DNS zone (before Shoot creation)
    └── E1. DNS Zone for Seed ingress domain
```

## 2.2 Stage A: Account Setup

### A1. Organization

| Field | Value | Notes |
|---|---|---|
| **Where** | CMP UI → already exists | Your tenant name |
| **Output** | `organisation-name`: e.g. `qa-tenant` | Used in HTTP header on every CMP API call (runtime — must exist in credentials Secret even if API unreachable) |
| **Output** | `organisation-id`: e.g. `c62e0e9d-...` | Used in TCPWave credentials Secret |
| **Used in code** | `pkg/f5/client.go` line ~322: `req.Header.Set("organisation-name", c.organisationName)` | Every HTTP request to CMP includes this |
| **Used in code** | `cmd/svc-lb-bridge/main.go` line ~276: `orgName := os.Getenv("CMP_ORG_NAME")` | svc-lb-bridge env var |

### A2. Project

| Field | Value | Notes |
|---|---|---|
| **Where** | CMP UI → Projects → Create (or use existing) | Isolation boundary within org |
| **Output** | `project-id`: e.g. `943258ef-...` (UUID) | Scopes ALL CMP API calls to this project |
| **Output** | `project-name`: e.g. `qa-first-cell` | Human-readable name |
| **Used in code** | `pkg/f5/client.go` line ~321-322: `req.Header.Set("project-id", c.projectID)` | HTTP header on every request |
| **Used in code** | `pkg/f5/client.go` line ~1108-1111: URL path `/api/v2.1/load-balancers/domain/{org}/project/{projectID}/...` | LBaaS URL scoping |

## 2.3 Stage B: Networking

### B1. VPC (Virtual Private Cloud)

| Field | Value | Notes |
|---|---|---|
| **Where** | CMP UI → Networking → VPCs → Create |  |
| **Name** | `gardener-seed-vpc` | Identifies the Seed/Shoot network |
| **CIDR** | `10.250.0.0/16` | Must NOT overlap with pod CIDR (10.1.134.0/16) or service CIDR (10.2.0.0/16) |
| **Region** | `regionOne` | Must match `spec.provider.region` in gardenlet values |
| **Output** | `vpc-id`: e.g. `net-12345-...` (UUID) | Used everywhere networking is referenced |
| **Used in code** | Shoot manifest `spec.provider.infrastructureConfig.networks.vpc.id` | airtelcloud provider reads this |
| **Used in code** | `cmd/svc-lb-bridge/main.go` line ~713: `query.Set("vpc_id", r.vpcID)` | LBService create param |

### B2. Subnets

| Subnet Name | CIDR | Purpose | CMP UI Path | Consumed By |
|---|---|---|---|---|
| `seed-nodes` | `10.250.0.0/24` | Seed K8s node IPs | Networking → Subnets → Create | Seed cluster infra |
| `shoot-workers` | `10.250.1.0/24` | Shoot worker VM IPs | Networking → Subnets → Create | airtelcloud provider — `InfrastructureConfig.networks.workers` |
| `storage-data` | `10.250.2.0/24` | NetApp data LIF traffic | Networking → Subnets → Create | Trident CSI backend config |

### B3. Security Groups

| SG Name | Key Rules | Auto-created? |
|---|---|---|
| `seed-nodes-sg` | Ingress: 443, 10250, 30000-32767; Egress: all | No — create manually for Seed |
| `shoot-workers-sg` | Ingress: 10250, 30000-32767; Egress: all | **Yes** — airtelcloud provider creates during Infrastructure reconcile |
| `shoot-etcd-sg` | Ingress: 2379-2380 from Seed CIDR; Egress: all | **Yes** — airtelcloud provider creates |

## 2.4 Stage C: Compute Prerequisites

### C1. SSH Key Pair

| Field | Value | CMP UI Path | Output |
|---|---|---|---|
| **Name** | `gardener-shoot-keypair` | Compute → Key Pairs → Create | Public key (stored in CMP), Private key (download) |

### C2. Machine Image

| Field | Value | CMP UI Path | Output |
|---|---|---|---|
| **Image** | Ubuntu 22.04 or Garden Linux 1592.1 cloud image | Compute → Images → Upload | `image-id` (Glance UUID) |

### C3. Machine Flavor

| Field | Value | CMP UI Path | Notes |
|---|---|---|---|
| **Flavor** | `m1.medium` (2 vCPU, 4 GB RAM) | Compute → Flavors → verify exists | Usually pre-provisioned by cloud admin |

## 2.5 Stage D: Credentials

### D1. CMP API Keys

> **Note:** API keys are generated in the CMP web UI. They are stored in K8s Secrets and read by extension code at runtime to authenticate HTTP calls to CMP. Since CMP APIs are currently unreachable, the keys will not produce working connections — but they must still exist in the Secrets for the extension pods to start without errors. LB resources are pre-created in CMP UI instead (see Phase 6).

| Field | Value | CMP UI Path | Output |
|---|---|---|---|
| **API Key** | Generate new | API Keys → Generate | `api-key-id` (string) + `api-secret` (string) |

**How the secret flows through code:**

```
CMP UI → Generate API Key
  │
  ├── api-key-id: "abc123"
  ├── api-secret: "xyz789"
  │
  ▼ (operator creates K8s Secret manually)
  │
K8s Secret on Seed cluster
  name: cmp-f5-credentials
  namespace: shoot--project--shoot
  data:
    api-key-id: "abc123"
    api-secret: "xyz789"
    project-id: "<CMP_PROJECT_ID>"
    organisation-name: "<CMP_ORG_NAME>"
  │
  ▼ (extension controller reads on every reconcile)
  │
pkg/controller/lifecycle/controller.go line 532:
  apiKeyID, _ := readSecretKey(seedSecret, "api-key-id")
  apiSecret, _ := readSecretKey(seedSecret, "api-secret")
  │
  ▼ (generates short-lived Ce-Auth HMAC token)
  │
pkg/controller/lifecycle/controller.go → refreshCeAuthIfNeeded():
  token := f5client.GenerateCeAuthToken(apiKeyID, apiSecret)
  // Token format: {apiKeyID}.{expiryTimestamp}.{hmac_sha256_hex}
  // Validity: ~299 seconds
  │
  ▼ (writes back to Secret + passes to CMP client)
  │
pkg/f5/client.go:
  req.Header.Set("Ce-Auth", token)        // line ~319
  req.Header.Set("organisation-name", ..) // line ~322
  req.Header.Set("project-id", ...)       // line ~321
  │
  ▼ (also injected into svc-lb-bridge Deployment as env vars)
  │
pkg/controller/lifecycle/controller.go line ~1100-1110:
  container.Env = []corev1.EnvVar{
    {Name: "CMP_ENDPOINT",  Value: cfg.Spec.CcpApiEndpoint},
    {Name: "CMP_CE_AUTH",   Value: ceAuth},
    {Name: "CMP_ORG_NAME",  Value: orgName},
    {Name: "CMP_PROJECT_ID", Value: projectID},
  }
```

### D2. OpenStack Application Credentials

| Field | Value | How to create | Output |
|---|---|---|---|
| **App Credential** | For Gardener to provision VMs | `openstack application credential create gardener-shoot` | `application-credential-id` + `application-credential-secret` |

## 2.6 Stage E: DNS Zone

| Field | Value | CMP UI Path | Output |
|---|---|---|---|
| **Zone** | `test-gkaas.com` (or your domain) | DNS → Zones → Create | Zone ID |

## 2.7 Complete Output Summary

After completing all CMP UI actions, you should have these values ready:

```bash
# Save these into ~/gardener-env.sh
CMP_ORG_NAME="qa-tenant"
CMP_ORG_ID="c62e0e9d-..."
CMP_PROJECT_ID="943258ef-..."
CMP_PROJECT_NAME="qa-first-cell"
CMP_API_KEY_ID="<generated>"
CMP_API_SECRET="<generated>"
VPC_ID="<from CMP>"
VPC_NAME="gardener-seed-vpc"
SUBNET_SEED_NODES_ID="<from CMP>"
SUBNET_SHOOT_WORKERS_ID="<from CMP>"
SUBNET_STORAGE_ID="<from CMP>"
SSH_KEYPAIR_NAME="gardener-shoot-keypair"
GLANCE_UBUNTU_IMAGE_ID="<from CMP>"
GLANCE_GARDENLINUX_IMAGE_ID="<from CMP>"
OS_APP_CRED_ID="<from OpenStack>"
OS_APP_CRED_SECRET="<from OpenStack>"
DNS_ZONE="test-gkaas.com"
```

---

# Phase 3: Mapping UI Resources into Code

## 3.1 Files That Need Updating

| File | What to update | Source of values |
|---|---|---|
| `gardenlet-airtelcloud-values.yaml` | Bootstrap token, Garden CA, Garden IP, network CIDRs | Phase 4 KIND setup + Phase 2 CIDRs |
| `~/tcpwave-creds-secret.yaml` | TCPWave API credentials | Phase 2 Stage D |
| `deploy/kind/cmp-f5-credentials.secret.yaml` | CMP API keys, project-id, org-name | Phase 2 Stage D1 |
| `~/shoot-prereqs/30-cloudprofile.yaml` | Image IDs, flavor names, region, OpenStack keystone URL | Phase 2 Stage C |
| `~/shoot-prereqs/70-openstack-secret.yaml` | OpenStack app credentials | Phase 2 Stage D2 |
| `~/shoot-prereqs/90-shoot.yaml` | VPC ID, worker subnet CIDR, extension configs | Phase 2 Stages B+E |

## 3.2 gardenlet-airtelcloud-values.yaml

```yaml
# CIDRs from Phase 2 Stage B (VPC/Subnet creation)
networks:
  nodes: 172.18.0.0/16       # ← Seed node CIDR (KIND assigns this)
  pods: 10.1.134.0/16        # ← Seed pod CIDR (KIND assigns this)
  services: 10.2.0.0/16      # ← Seed service CIDR (KIND assigns this)

# Provider type from Phase 2 Stage A (your cloud type)
provider:
  type: airtelcloud           # ← tells Gardener which provider extension to use
  region: regionOne           # ← must match CMP region

# DNS credentials from Phase 2 Stage D
dns:
  provider:
    type: tcpwave
    credentialsRef:
      name: tcpwave-credentials    # ← K8s Secret created from Phase 2 TCPWave creds
      namespace: garden

# Extension declaration
extensions:
- type: f5-loadbalancer       # ← tells gardenlet to install F5 extension on this Seed
```

## 3.3 CMP F5 Credentials Secret

File: `deploy/kind/cmp-f5-credentials.secret.yaml`

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cmp-f5-credentials
  namespace: shoot--local--local    # ← Change to your Shoot namespace
type: Opaque
stringData:
  api-key-id: "<CMP_API_KEY_ID>"          # ← From Phase 2 Stage D1
  api-secret: "<CMP_API_SECRET>"          # ← From Phase 2 Stage D1
  project-id: "<CMP_PROJECT_ID>"          # ← From Phase 2 Stage A2
  organisation-name: "<CMP_ORG_NAME>"     # ← From Phase 2 Stage A1
```

**Which controllers read it:**

1. **Extension controller** (`pkg/controller/lifecycle/controller.go` line ~530-570):
   ```go
   apiKeyID, _ := readSecretKey(seedSecret, "api-key-id", "apiKeyId")
   apiSecret, _ := readSecretKey(seedSecret, "api-secret", "apiSecret")
   projectID, _ := readSecretKey(seedSecret, "project-id", "projectId", "project_id")
   orgName, _ := readSecretKey(seedSecret, "organisation-name", "organisationName")
   ceAuth = f5client.GenerateCeAuthToken(apiKeyID, apiSecret)
   cmp, err = f5client.NewClientWithCeAuth(log, cfg.Spec.CcpApiEndpoint, orgName, projectID, ceAuth)
   ```

2. **svc-lb-bridge** (`cmd/svc-lb-bridge/main.go` line ~270-283):
   ```go
   endpoint := os.Getenv("CMP_ENDPOINT")
   ceAuth := os.Getenv("CMP_CE_AUTH")
   orgName := os.Getenv("CMP_ORG_NAME")
   projectID := os.Getenv("CMP_PROJECT_ID")
   ```

3. **Secret watch** (`pkg/controller/lifecycle/extension_controller.go` line ~80+):
   ```go
   ctrl.NewControllerManagedBy(mgr).
     Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToExtensions)).
     Complete(r)
   ```

## 3.4 CloudProfile Manifest

```yaml
# ~/shoot-prereqs/30-cloudprofile.yaml
spec:
  providerConfig:
    machineImages:
    - name: ubuntu
      versions:
      - version: 22.04.0
        image: ubuntu-22.04-server-cloudimg-amd64
        regions:
        - name: regionOne
          id: "<GLANCE_UBUNTU_IMAGE_ID>"    # ← From Phase 2 Stage C2
    keystoneURL: "https://100.65.247.153:13000/v3/"  # ← OpenStack endpoint from CMP
```

## 3.5 Shoot Manifest

```yaml
# ~/shoot-prereqs/90-shoot.yaml
spec:
  provider:
    infrastructureConfig:
      networks:
        vpc:
          id: "<VPC_ID>"               # ← From Phase 2 Stage B1
        workers: "10.250.0.0/16"       # ← Shoot worker subnet CIDR
    workers:
    - machine:
        type: m1.medium                # ← Flavor from Phase 2 Stage C3
        image:
          name: gardenlinux
          version: "1592.1.0"          # ← Image from Phase 2 Stage C2
  extensions:
  - type: f5-loadbalancer              # ← Triggers F5 extension
  - type: tcpwave-dns
    providerConfig:
      zone: test-gkaas.com             # ← DNS zone from Phase 2 Stage E1
```

## 3.6 K8s Secrets to Create

| Secret Name | Namespace | When to create | Keys | Created on which cluster |
|---|---|---|---|---|
| `bootstrap-token-<id>` | `kube-system` | Before gardenlet deploy (Phase 4) | `token-id`, `token-secret` | Garden + Seed |
| `tcpwave-credentials` | `garden` | Before gardenlet deploy (Phase 4) | `endpoint`, `apiId`, `secret`, `organisationName`, `organisationId`, `projectId` | Garden + Seed |
| `airtelcloud-creds` | `garden-dev` | Before Shoot creation (Phase 5) | `auth-url`, `application-credential-id`, `application-credential-secret` | Garden |
| `cmp-f5-credentials` | `shoot--<project>--<shoot>` | Before/during Shoot reconcile | `api-key-id`, `api-secret`, `project-id`, `organisation-name` | Seed |

---

# Phase 4: Seed Setup

## 4.1 Create Garden Cluster (KIND)

```bash
source ~/gardener-env.sh
cd $GARDENER_DIR

# Create first KIND cluster (Garden)
make kind-up
# Output: kind cluster "gardener-local" created

# Deploy Gardener components
make gardener-up
# Deploys: gardener-apiserver, gardener-controller-manager, gardener-scheduler,
#          gardener-admission-controller, plus CRDs, RBAC, etcd for Gardener
```

**Verify:**
```bash
kubectl --context $GARDEN_CTX get pods -n garden-system
# Expected: all Running

kubectl --context $GARDEN_CTX api-resources | grep gardener
# Expected: shoots, seeds, cloudprofiles, controllerregistrations, etc.
```

## 4.2 Create Seed Cluster (second KIND)

```bash
cd $GARDENER_DIR
make kind2-up
```

**Merge kubeconfigs:**
```bash
kind get kubeconfig --name gardener-local  > ~/.kube/config
kind get kubeconfig --name gardener-local2 > ~/.kube/config2
KUBECONFIG=~/.kube/config:~/.kube/config2 kubectl config view --merge --flatten > ~/.kube/config.merged
mv ~/.kube/config.merged ~/.kube/config
unset KUBECONFIG
chmod 600 ~/.kube/config

kubectl config get-contexts
# Should show: kind-gardener-local (Garden), kind-gardener-local2 (Seed)
```

**Note key IPs:**
```bash
GARDEN_IP=$(docker inspect gardener-local-control-plane -f '{{.NetworkSettings.Networks.kind.IPAddress}}')
echo "Garden IP: $GARDEN_IP"    # e.g. 172.18.0.8

GARDEN_CA=$(kubectl --context $GARDEN_CTX config view --raw \
  -o jsonpath='{.clusters[?(@.name=="kind-gardener-local")].cluster.certificate-authority-data}')
echo "Garden CA: ${GARDEN_CA:0:40}..."
```

## 4.3 Create Bootstrap Token

```bash
TOKEN_ID=$(openssl rand -hex 3)
TOKEN_SECRET=$(openssl rand -hex 8)
echo "Bootstrap token: ${TOKEN_ID}.${TOKEN_SECRET}"

cat > ~/bootstrap-token.yaml << EOF
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-${TOKEN_ID}
  namespace: kube-system
type: bootstrap.kubernetes.io/token
stringData:
  description: "gardenlet bootstrap for ojasvi-seed"
  token-id: "${TOKEN_ID}"
  token-secret: "${TOKEN_SECRET}"
  usage-bootstrap-authentication: "true"
  usage-bootstrap-signing: "true"
EOF

kubectl --context $GARDEN_CTX apply -f ~/bootstrap-token.yaml
kubectl --context $SEED_CTX  apply -f ~/bootstrap-token.yaml

kubectl --context $GARDEN_CTX get clusterrolebinding | grep node-bootstrapper || \
  kubectl --context $GARDEN_CTX create clusterrolebinding bootstrap-token-csr \
    --clusterrole=system:node-bootstrapper --group=system:bootstrappers
```

## 4.4 Create Credential Secrets

```bash
# TCPWave credentials — on BOTH clusters
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

kubectl --context $GARDEN_CTX apply -f ~/tcpwave-creds-secret.yaml
kubectl --context $SEED_CTX  create ns garden --dry-run=client -o yaml | kubectl --context $SEED_CTX apply -f -
kubectl --context $SEED_CTX  apply -f ~/tcpwave-creds-secret.yaml
```

## 4.5 Build and Load Extension Images into Seed

```bash
source ~/gardener-env.sh

# ---- F5 Extension ----
cd $REPOS_DIR/gardener-extension-f5
docker build -t $F5_IMAGE .
docker push $F5_IMAGE
docker save $F5_IMAGE | docker exec -i gardener-local2-control-plane ctr -n k8s.io images import -

# Install CRD on Seed (must exist BEFORE extension pod starts)
kubectl --context $SEED_CTX apply -f config/crd/f5loadbalancerconfigs.f5.extensions.gardener.cloud.yaml

# Register on Garden
kubectl --context $GARDEN_CTX apply -f deploy/garden/controllerdeployment-f5.yaml
kubectl --context $GARDEN_CTX apply -f deploy/garden/controllerregistration-f5.yaml

# ---- Airtelcloud Provider ----
cd $REPOS_DIR/gardener-extension-provider-airtelcloud
make docker-images
docker tag europe-docker.pkg.dev/gardener-project/public/gardener/extensions/provider-airtelcloud:v0.1.0-dev \
  $PROVIDER_IMAGE
docker push $PROVIDER_IMAGE
docker save $PROVIDER_IMAGE | docker exec -i gardener-local2-control-plane ctr -n k8s.io images import -
sh example/generate_controllerregistration.sh
kubectl --context $GARDEN_CTX apply -f example/controller-registration.yaml

# ---- TCPWave Extension ----
cd $REPOS_DIR/gardener-extension-tcpwave
./deploy.sh

# ---- Verify ----
kubectl --context $GARDEN_CTX get controllerregistration
```

## 4.6 Update gardenlet Values and Deploy

```bash
cd $REPOS_DIR/gardener-extension-f5
cp gardenlet-airtelcloud-values.yaml ~/gardenlet_airtelcloud_values.yaml
# Edit ~/gardenlet_airtelcloud_values.yaml with: Garden CA, Garden IP, bootstrap token, CIDRs

GARDENLET_CHART=$(find $GARDENER_DIR/charts -maxdepth 6 -name Chart.yaml \
  | grep -i '/gardenlet/Chart.yaml$' | head -1 | xargs dirname)

kubectl config use-context $SEED_CTX
helm upgrade --install gardenlet "$GARDENLET_CHART" \
  -n garden --create-namespace \
  -f ~/gardenlet_airtelcloud_values.yaml
```

## 4.7 What Happens During Seed Reconciliation

```
gardenlet starts on Seed cluster
│
├── 1. TLS bootstrap: sends CSR to Garden API using bootstrap token
│      → gets signed certificate → stores in gardenlet-kubeconfig Secret
│
├── 2. Registers Seed object in Garden cluster
│
├── 3. Reconciles Seed:
│   ├── a. Deploys CRDs (machine.*, extensions.*, etcd.*, istio.*, druid.*, vpa.*)
│   ├── b. Deploys gardener-resource-manager
│   ├── c. Copies Secrets to extension namespaces
│   │
│   ├── d. Creates ControllerInstallations:
│   │   ├── F5 extension pod deployed on Seed
│   │   ├── TCPWave extension pod deployed on Seed
│   │   └── airtelcloud provider pod deployed on Seed
│   │
│   ├── e. Deploys Istio (istio-ingressgateway Service type: LoadBalancer)
│   │       → Needs external IP (CMP: seed-service-lb-controller; KIND: manual patch)
│   │
│   ├── f. Deploys nginx-ingress
│   │
│   └── g. Creates DNSRecord "seed-ingress" → TCPWave extension creates A record
│
└── 4. Seed status becomes Ready
```

## 4.8 Validate Seed Setup

```bash
kubectl --context $GARDEN_CTX get seed ojasvi-seed            # STATUS: Ready
kubectl --context $GARDEN_CTX get controllerinstallation      # All Installed + Healthy
kubectl --context $SEED_CTX get pods -A | grep -E "extension|gardenlet"  # All Running
kubectl --context $SEED_CTX get crd | grep -E "f5|extension|machine"     # CRDs present
kubectl --context $SEED_CTX get svc -n istio-system istio-ingressgateway # Has EXTERNAL-IP
```

## 4.9 Debug Points

```bash
kubectl --context $SEED_CTX -n garden logs deploy/gardenlet --tail=200

kubectl --context $SEED_CTX logs -n $(kubectl --context $SEED_CTX get ns | grep extension-gardener-extension-f5 | awk '{print $1}') \
  deploy/gardener-extension-f5 --tail=200

# Force re-reconcile
kubectl --context $GARDEN_CTX annotate seed ojasvi-seed \
  "gardener.cloud/operation=reconcile" --overwrite
```

---

# Phase 5: Shoot Setup

## 5.1 Create Shoot Prerequisites (Garden cluster)

```bash
source ~/gardener-env.sh

# 1. Project
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
    roles: [admin]
EOF
kubectl --context $GARDEN_CTX apply -f ~/shoot-prereqs/05-project.yaml
kubectl --context $GARDEN_CTX get ns garden-dev  # Wait until created

# 2. CloudProfile
kubectl --context $GARDEN_CTX apply -f ~/shoot-prereqs/30-cloudprofile.yaml

# 3. Cloud Credentials Secret
cat > ~/shoot-prereqs/70-openstack-secret.yaml << EOF
apiVersion: v1
kind: Secret
metadata:
  name: airtelcloud-creds
  namespace: garden-dev
type: Opaque
stringData:
  auth-url:                      "${OS_AUTH_URL}"
  application-credential-id:     "${OS_APP_CRED_ID}"
  application-credential-secret: "${OS_APP_CRED_SECRET}"
EOF
kubectl --context $GARDEN_CTX apply -f ~/shoot-prereqs/70-openstack-secret.yaml

# 4. CredentialsBinding
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
kubectl --context $GARDEN_CTX apply -f ~/shoot-prereqs/80-credentialsbinding.yaml
```

## 5.2 Create CMP Credentials Secret (Seed cluster)

```bash
SHOOT_NS="shoot--dev--my-shoot"
kubectl --context $SEED_CTX create ns $SHOOT_NS --dry-run=client -o yaml | kubectl --context $SEED_CTX apply -f -

cat > ~/secrets/cmp-f5-credentials.yaml << EOF
apiVersion: v1
kind: Secret
metadata:
  name: cmp-f5-credentials
  namespace: $SHOOT_NS
type: Opaque
stringData:
  api-key-id: "${CMP_API_KEY_ID}"
  api-secret: "${CMP_API_SECRET}"
  project-id: "${CMP_PROJECT_ID}"
  organisation-name: "${CMP_ORG_NAME}"
EOF
kubectl --context $SEED_CTX apply -f ~/secrets/cmp-f5-credentials.yaml
```

## 5.3 Create Shoot

```bash
cat > ~/shoot-prereqs/90-shoot.yaml << 'EOF'
apiVersion: core.gardener.cloud/v1beta1
kind: Shoot
metadata:
  name: my-shoot
  namespace: garden-dev
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
          id: "<VPC_ID>"
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
      zones: [nova]
      volume:
        type: standard
        size: 50Gi
      cri:
        name: containerd
  extensions:
  - type: f5-loadbalancer
  maintenance:
    autoUpdate:
      kubernetesVersion: true
      machineImageVersion: true
    timeWindow:
      begin: "020000+0000"
      end: "030000+0000"
EOF

kubectl --context $GARDEN_CTX apply -f ~/shoot-prereqs/90-shoot.yaml
kubectl --context $GARDEN_CTX -n garden-dev get shoot my-shoot -w
```

## 5.4 What Happens During Shoot Reconciliation

```
Shoot CR applied on Garden cluster
│
├── 1. Gardener Scheduler assigns Shoot → Seed (ojasvi-seed)
│
├── 2. gardenlet creates Shoot namespace on Seed: shoot--dev--my-shoot
│
├── 3. gardenlet creates Extension CRs in Shoot namespace:
│
│   ├── 3a. Infrastructure CR → airtelcloud provider
│   │   ├── Reads InfrastructureConfig (VPC ID, worker subnet CIDR)
│   │   ├── Calls OpenStack Neutron API:
│   │   │   ├── Verify VPC exists
│   │   │   ├── Create subnet in VPC (if not exists)
│   │   │   ├── Create router + attach subnet
│   │   │   ├── Create security groups + rules
│   │   │   └── Optionally create floating IP
│   │   └── Writes Infrastructure.status.providerStatus
│   │
│   ├── 3b. DNSRecord CRs → TCPWave extension
│   │   ├── Creates A record: api.my-shoot.dev.seed.gardener.cloud → Seed Istio VIP
│   │   └── Creates internal DNS records
│   │
│   ├── 3c. ControlPlane CR → airtelcloud provider
│   │   ├── Configures cloud-controller-manager settings
│   │   └── Configures CSI driver settings
│   │
│   ├── 3d. Extension CR (f5-loadbalancer) → F5 extension
│   │   ├── ensureF5LoadBalancerConfig() → creates/syncs F5LoadBalancerConfig CRD
│   │   │
│   │   ├── Control-plane LB decision:
│   │   │   ├── Mechanism A (default): Sets Ready immediately — **no CMP calls**
│   │   │   │
│   │   │   └── Mechanism B: (**⚠️ requires CMP API — use dev stub with UI-created LB instead**)
│   │   │       POST /lb_service/ → LBService ID
│   │   │       POST /lb_service/{id}/vip → VIP address
│   │   │       POST /load-balancers/{id}/virtual-servers → VS
│   │   │
│   │   └── Application-plane LB (gated on CP Ready):
│   │       → reconcileCISInShoot():
│   │           Creates in Shoot: Namespace, SA, ClusterRole, ClusterRoleBinding,
│   │           Deployment (svc-lb-bridge with CMP env vars)
│   │
│   ├── 3e. gardenlet deploys: etcd, kube-apiserver, kube-controller-manager, kube-scheduler
│   │
│   └── 3f. Worker CR → airtelcloud provider
│       ├── Creates MachineClass → MachineDeployment → Machine → Nova VM
│       └── VMs bootstrap kubelet → join Shoot cluster
│
└── 4. All components Ready → Shoot status Ready
```

## 5.5 Application LB Flow (after Shoot is Ready)

```bash
kubectl --kubeconfig <shoot-kubeconfig> apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: echo-lb
  namespace: default
spec:
  type: LoadBalancer
  ports:
  - port: 80
    targetPort: 8080
  selector:
    app: echo
EOF
```

> **⚠️ Every CMP HTTP call below will fail if CMP API is unreachable.** Services stay `Pending`. Pre-create LBs in CMP UI and manually patch (see Phase 6).

```
Service type=LoadBalancer created
│
├── 1. Watch event triggers serviceReconciler.Reconcile()
├── 2. parseLBServiceConfig() — reads 7 annotations from Service
├── 3. choosePorts() — extracts all ports
├── 4. listBackendNodes() — EndpointSlice-based, weighted by pod count
│
├── 5. For each port → ensureCMPResources():
│   ├── Step 1: findLBServiceByName() or CreateLBService()
│   │   POST /lb_service/ { name: "app-default-echo-lb", vpc_id: "..." }
│   │
│   ├── Step 2: findOrCreateVIP()
│   │   POST /lb_service/{id}/vip → VIP address (e.g. "10.0.0.5")
│   │
│   └── Step 3: createVirtualServer()
│       POST /load-balancers/{id}/virtual-servers { protocol, port, nodes[], ... }
│
├── 6. ensureNetworkPolicy() — auto-creates NetworkPolicy
├── 7. Patches Service status.loadBalancer.ingress[].ip = VIP
└── 8. Records Kubernetes Event: "EnsuredLoadBalancer"
```

## 5.6 Verify Shoot Setup

```bash
kubectl --context $GARDEN_CTX -n garden-dev get shoot my-shoot
SHOOT_NS="shoot--dev--my-shoot"
kubectl --context $SEED_CTX -n $SHOOT_NS get extension
kubectl --context $SEED_CTX -n $SHOOT_NS get f5loadbalancerconfig
kubectl --context $SEED_CTX -n $SHOOT_NS get infrastructure
kubectl --context $SEED_CTX -n $SHOOT_NS get worker
kubectl --context $SEED_CTX -n $SHOOT_NS get dnsrecord

# Get Shoot kubeconfig
kubectl --context $GARDEN_CTX -n garden-dev get secret my-shoot.kubeconfig -o jsonpath='{.data.kubeconfig}' | base64 -d > ~/shoot.kubeconfig

kubectl --kubeconfig ~/shoot.kubeconfig get nodes
kubectl --kubeconfig ~/shoot.kubeconfig -n f5-cis-system get pods
kubectl --kubeconfig ~/shoot.kubeconfig get svc -A | grep LoadBalancer
```

---

# Phase 6: Workaround — Pre-Create All Resources from CMP UI

Since CMP REST APIs are not reachable from the Seed cluster, **every resource that extension code would normally create at runtime must be pre-created in the CMP web UI** and the corresponding Kubernetes objects manually patched.

## 6.1 What Each Extension Needs from CMP (and What to Do Instead)

| Extension | What code creates via API at runtime | When | UI Workaround |
|---|---|---|---|
| **F5 (control-plane LB)** | LBService + VIP + VirtualServer for Shoot apiserver | During Shoot reconcile | **Pre-create in CMP UI** OR use Mechanism A (skip entirely) |
| **F5 (app-plane LB)** | LBService + VIP + VirtualServer per `type: LoadBalancer` Service | Every time a Service is created | **Disable svc-lb-bridge** + pre-create in CMP UI + `kubectl patch` |
| **F5 (Seed Istio ingress)** | LBService + VIP + VirtualServer for Istio gateway | During Seed reconcile | **Pre-create in CMP UI** + `kubectl patch svc` |
| **airtelcloud provider** | VPC, subnet, SG, router, VMs via OpenStack | During Shoot reconcile | **Use `provider-local`** (no OpenStack needed) |
| **TCPWave DNS** | DNS A records for Shoot API + Seed ingress | During Shoot + Seed reconcile | **Create DNS records in CMP UI** OR use CoreDNS static override |
| **NetApp backup** | S3 bucket / ONTAP volume | During Seed reconcile | **Skip** (remove `spec.backup` from Seed) |
| **Trident storage** | PVs via NetApp CSI | After Shoot is Ready | **Skip** (KIND uses `local-path-provisioner` by default) |

## 6.2 Per-Extension UI Resource Creation Guide

### 🔵 Extension: F5 Load Balancer

#### Resource 1: Seed Istio Ingress LB (when: Phase 4, Seed setup)

**Why:** gardenlet creates an Istio `type: LoadBalancer` Service. Without an external IP, Seed stays `Bootstrapping`. Normally `seed-service-lb-controller` calls CMP to create this.

**CMP UI steps:**

| Step | CMP UI Path | Input | Output |
|---|---|---|---|
| 1. Find Seed node IPs | `kubectl --context $SEED_CTX get nodes -o wide` | — | e.g. `172.18.0.3` |
| 2. Find Istio NodePort | `kubectl --context $SEED_CTX -n istio-system get svc istio-ingressgateway -o jsonpath='{.spec.ports[?(@.port==443)].nodePort}'` | — | e.g. `30443` |
| 3. Create LB | CMP UI → Load Balancers → Create | Name: `seed-istio-ingress`, VPC: your VPC, Flavor: small | **LBService ID**, **VIP** (e.g. `10.100.5.10`) |
| 4. Add Listener | CMP UI → LB → Add Listener | Protocol: TCP, Port: 443, Backends: `172.18.0.3:30443` | VirtualServer created |

**Kubernetes patch:**
```bash
kubectl --context $SEED_CTX -n istio-system patch svc istio-ingressgateway \
  --subresource=status --type=merge \
  -p '{"status":{"loadBalancer":{"ingress":[{"ip":"10.100.5.10"}]}}}'
```

---

#### Resource 2: Control-Plane LB per Shoot (when: Phase 5, Shoot creation — optional)

**Why:** If using Mechanism B (`enablePerShootControlPlaneVIP: true`), the extension calls CMP to create a dedicated VIP for the Shoot apiserver.

**Recommended: Use Mechanism A instead** (default — no CMP call, uses shared Seed Istio VIP):
```yaml
spec:
  extensions:
  - type: f5-loadbalancer
    providerConfig:
      enablePerShootControlPlaneVIP: false   # ← Mechanism A, default, no CMP needed
```

**If you want per-Shoot VIP for the demo,** pre-create in CMP UI:

| Step | CMP UI Path | Input | Output |
|---|---|---|---|
| 1. Find apiserver NodePort | `kubectl --context $SEED_CTX -n $SHOOT_NS get svc kube-apiserver -o jsonpath='{.spec.ports[0].nodePort}'` | — | e.g. `31443` |
| 2. Find Seed node IPs | (same as above) | — | e.g. `172.18.0.3` |
| 3. Create LB | CMP UI → Load Balancers → Create | Name: `cp-<shoot-name>`, VPC: your VPC | **VIP** (e.g. `10.100.5.20`) |
| 4. Add Listener | CMP UI → LB → Add Listener | Protocol: TCP, Port: 443, Backends: `172.18.0.3:31443` | VirtualServer created |

**Extension config:**
```yaml
spec:
  extensions:
  - type: f5-loadbalancer
    providerConfig:
      enablePerShootControlPlaneVIP: true
      controlPlaneReady: true              # ← Trust the UI-created LB
      controlPlaneVIP: "10.100.5.20"       # ← VIP from CMP UI
      enableApplicationLB: false           # ← Disable svc-lb-bridge (see next resource)
```

---

#### Resource 3: Application LB per Service (when: after Shoot is Ready — for each demo Service)

**Why:** When users create `type: LoadBalancer` Services in the Shoot, `svc-lb-bridge` normally calls CMP automatically. Since CMP API is unreachable, disable the bridge and pre-create each LB in CMP UI.

**Extension config (disable svc-lb-bridge):**
```yaml
enableApplicationLB: false   # ← svc-lb-bridge will NOT be deployed
```

**For each demo Service:**

| Step | Where | Input | Output |
|---|---|---|---|
| 1. Deploy app + Service | Shoot cluster | `kubectl apply -f echo-app.yaml` | Service shows `EXTERNAL-IP: <pending>` |
| 2. Find NodePort | `kubectl --kubeconfig ~/shoot.kubeconfig get svc echo-lb -o jsonpath='{.spec.ports[0].nodePort}'` | — | e.g. `31080` |
| 3. Find worker IPs | `kubectl --kubeconfig ~/shoot.kubeconfig get nodes -o jsonpath='{.items[*].status.addresses[?(@.type=="InternalIP")].address}'` | — | e.g. `10.250.0.5 10.250.0.6` |
| 4. Create LB | CMP UI → Load Balancers → Create | Name: `app-default-echo-lb`, VPC | **VIP** (e.g. `10.100.5.30`) |
| 5. Add Listener | CMP UI → LB → Add Listener | Protocol: TCP, Port: 80, Backends: `10.250.0.5:31080`, `10.250.0.6:31080` | VirtualServer |
| 6. Patch Service | `kubectl patch svc echo-lb --subresource=status --type=merge -p '{"status":{"loadBalancer":{"ingress":[{"ip":"10.100.5.30"}]}}}'` | — | `EXTERNAL-IP: 10.100.5.30` |
| 7. Test | `curl http://10.100.5.30` | — | App response (real traffic through real F5) |

**Helper script for step 6 (reusable for each Service):**
```bash
#!/bin/bash
# demo-patch-svc.sh <service-name> <namespace> <vip-from-cmp-ui>
SVC="${1:?}" NS="${2:?}" VIP="${3:?}"
KC="${SHOOT_KUBECONFIG:-$HOME/shoot.kubeconfig}"
kubectl --kubeconfig "$KC" annotate svc "$SVC" -n "$NS" --overwrite \
  "f5.extensions.gardener.cloud/vip-address=$VIP"
kubectl --kubeconfig "$KC" patch svc "$SVC" -n "$NS" \
  --subresource=status --type=merge \
  -p "{\"status\":{\"loadBalancer\":{\"ingress\":[{\"ip\":\"$VIP\"}]}}}"
echo "✓ $NS/$SVC → EXTERNAL-IP=$VIP"
```

---

### 🟢 Extension: TCPWave DNS

#### Resource 4: Seed Ingress DNS Record (when: Phase 4, Seed setup)

**Why:** gardenlet creates a `DNSRecord` CR for the Seed ingress domain. TCPWave extension calls TCPWave API to create an A record.

**Option A — CMP UI (if DNS management is available in CMP):**

| Step | CMP UI Path | Input | Output |
|---|---|---|---|
| 1. Create A record | CMP UI → DNS → Zones → your zone → Add Record | Name: `*.ingress.ojasvi-seed.airtel.local`, Type: A, Value: `10.100.5.10` (Istio VIP from Resource 1) | DNS record created |

**Option B — CoreDNS override (if CMP DNS UI is unavailable or using KIND):**
```bash
kubectl --context $SEED_CTX -n kube-system edit configmap coredns
```
Add the following block inside the Corefile:
```
hosts {
    10.100.5.10 *.ingress.ojasvi-seed.airtel.local
    fallthrough
}
```
```bash
kubectl --context $SEED_CTX -n kube-system rollout restart deploy/coredns
```

---

#### Resource 5: Shoot API DNS Record (when: Phase 5, Shoot creation)

**Why:** gardenlet creates a `DNSRecord` CR for `api.<shoot>.<project>.<seed-domain>`. TCPWave extension creates the A record.

**Option A — CMP UI:**

| Step | CMP UI Path | Input | Output |
|---|---|---|---|
| 1. Create A record | CMP UI → DNS → Zones → your zone → Add Record | Name: `api.my-shoot.dev.ojasvi-seed.airtel.local`, Type: A, Value: `10.100.5.10` (Istio VIP — SNI routes to Shoot apiserver) | DNS record |

**Option B — CoreDNS override:**
```bash
# Add to the hosts block in CoreDNS ConfigMap:
10.100.5.10 api.my-shoot.dev.ojasvi-seed.airtel.local
```

**Option C — Use `provider-local` DNS (easiest for KIND):**

Gardener's `provider-local` handles DNS automatically using `*.local.gardener.cloud`. No manual DNS needed.

---

### 🟠 Extension: Airtelcloud Provider (Compute/Networking)

#### Resource 6: Infrastructure (VPC, subnet, SG, VMs) (when: Phase 5, Shoot creation)

**Why:** The provider extension calls OpenStack APIs to create VPC, subnet, security groups, router, and VMs.

**Recommended: Use `provider-local` instead** — it creates "workers" as Pods inside KIND. No CMP/OpenStack needed:

```yaml
# In Shoot manifest:
spec:
  provider:
    type: local    # Instead of "airtelcloud"
```

**If using real airtelcloud provider,** the Phase 2 UI resources (VPC, subnet, SG) are sufficient for Infrastructure reconcile. But **Worker reconcile (VM creation) will fail** because it calls Nova API at runtime. There is no UI workaround for VM creation — use `provider-local` for the demo.

---

### ⚪ Extension: NetApp Backup

#### Resource 7: Backup Bucket (when: Seed setup)

**Skip entirely for the demo.** Remove `spec.backup` from gardenlet values:

```yaml
# In gardenlet-airtelcloud-values.yaml:
# Comment out or remove:
# backup:
#   provider: netapp
#   ...
```

etcd will use local PVs (KIND default) — no backup/restore capability, but the platform works fine.

---

### ⚪ Extension: Trident Storage

#### Resource 8: PersistentVolumes (when: after Shoot is Ready)

**Skip entirely for the demo.** KIND includes `local-path-provisioner` by default:

```bash
kubectl --context $SEED_CTX get sc
# standard (default)     rancher.io/local-path   Delete
```

No NetApp, no Trident, no CMP interaction needed.

---

## 6.3 Complete Timeline: When to Create What in CMP UI

```
PHASE 2: Static Infrastructure (CMP UI — done once)
│
├── Organization + Project                    → note org-name, project-id
├── VPC + Subnets                             → note vpc-id, subnet-ids
├── Security Groups                           → note sg-ids
├── SSH Key Pair + Machine Image              → note image-id
├── API Keys                                  → note api-key-id, api-secret
└── DNS Zone                                  → note zone name

PHASE 4: Seed Setup
│
├── [CMP UI] Create Istio ingress LB          → Resource 1
│   └── note VIP (e.g. 10.100.5.10)
│
├── [kubectl] Patch istio-ingressgateway      → with VIP from above
│
├── [CMP UI or CoreDNS] Seed DNS record       → Resource 4
│   └── *.ingress.seed-name → Istio VIP
│
└── Seed becomes Ready ✓

PHASE 5: Shoot Creation
│
├── [CMP UI] Control-plane LB (optional)      → Resource 2
│   └── note VIP (e.g. 10.100.5.20)
│   └── OR: use Mechanism A (default, skip this)
│
├── [CMP UI or CoreDNS] Shoot API DNS         → Resource 5
│   └── api.shoot-name → Istio VIP
│
├── [kubectl] Create Shoot
│   └── provider: local, enableApplicationLB: false
│
└── Shoot becomes Ready ✓

POST-SHOOT: Application LB Demo
│
├── Deploy echo app + Service type=LoadBalancer
│   └── Service shows EXTERNAL-IP: <pending>
│
├── [CMP UI] Create app LB                    → Resource 3
│   └── note VIP (e.g. 10.100.5.30)
│
├── [kubectl] Patch Service status with VIP
│
├── curl VIP → real response through real F5 ✓
│
└── Repeat for each additional demo Service
```

## 6.4 provider-local (Instead of airtelcloud provider)

```bash
# provider-local is already deployed by `make gardener-up`
# To use it, change Shoot manifest:
spec:
  provider:
    type: local    # Instead of "airtelcloud"
```

No OpenStack credentials needed. Workers created as Pods.

## 6.5 local-path-provisioner (Instead of Trident)

KIND includes this by default. No changes needed.

```bash
kubectl --context $SEED_CTX get sc
# standard (default)     rancher.io/local-path   Delete
```

## 6.6 Local Container Registry (No Internet)

**On internet-connected machine:**
```bash
IMAGES=("gardener-extension-f5:dev" "provider-airtelcloud:v0.1.0-dev" ...)
for img in "${IMAGES[@]}"; do docker pull "$img"; done
docker save "${IMAGES[@]}" > gardener-images.tar
# Transfer to staging VM
```

**On staging VM:**
```bash
docker load < gardener-images.tar
docker tag gardener-extension-f5:dev ${LOCAL_REGISTRY}/gardener-extension-f5:v0.1.1-dev
docker push ${LOCAL_REGISTRY}/gardener-extension-f5:v0.1.1-dev
docker save ${LOCAL_REGISTRY}/gardener-extension-f5:v0.1.1-dev \
  | docker exec -i gardener-local2-control-plane ctr -n k8s.io images import -
```

**Go module vendor:**
```bash
# On internet machine:
cd gardener-extension-f5 && go mod vendor && tar czf vendor.tar.gz vendor/
# On staging VM:
tar xzf vendor.tar.gz && go build -mod=vendor ./cmd/gardener-extension-f5
```

## 6.7 Complete Workaround Summary

| Original dependency | Workaround | What you do |
|---|---|---|
| CMP LBaaS API (F5) | Pre-create LBs in CMP UI + `kubectl patch` | CMP UI → Create LB → note VIP → patch Service status |
| CMP LBaaS API (Istio) | Pre-create Istio LB in CMP UI + `kubectl patch` | CMP UI → Create LB → patch istio-ingressgateway |
| CMP Compute/Network | `provider-local` | Change `provider.type: local` in Shoot manifest |
| TCPWave DNS API | CMP UI DNS records OR CoreDNS override | CMP UI → DNS → Add A record, or edit CoreDNS ConfigMap |
| NetApp ONTAP | Skip backup | Remove `spec.backup` from gardenlet values |
| Trident CSI | `local-path-provisioner` | No changes (KIND default) |
| Container registry | Pre-loaded images | `docker save/load` + `ctr import` |
| Go modules | `go mod vendor` + `-mod=vendor` | Build with vendored dependencies |

---

# Phase 7: End-to-End Execution Flow

## 7.1 Complete Chronological Sequence

```
Day 0: Empty VM
│
├── 1. Install OS packages (git, curl, gcc, openssl, jq)
├── 2. Install Docker, configure insecure registry
├── 3. Install Go 1.24.6
├── 4. Install kubectl, Helm, KIND, yq
├── 5. Clone all 6 repositories
├── 6. Create ~/gardener-env.sh
│
├── [IF CMP UI AVAILABLE]: Phase 2 in CMP UI
│   ├── Create/verify Organization + Project
│   ├── Create VPC, subnets, security groups
│   ├── Upload machine images, verify flavors
│   ├── Generate API keys
│   ├── Create DNS zone
│   └── Record all IDs/credentials in ~/gardener-env.sh
│
├── [IF OFFLINE]: Skip Phase 2, use provider-local
│
├── 7. make kind-up (Garden cluster)
├── 8. make gardener-up (Gardener components)
├── 9. make kind2-up (Seed cluster)
├── 10. Merge kubeconfigs
├── 11. Note Garden IP + CA
│
├── 12. Create bootstrap token (Garden + Seed)
├── 13. Create TCPWave credentials Secret (Garden + Seed)
│
├── 14. Build extension images:
│   ├── F5 extension → docker build → docker save → ctr import
│   ├── airtelcloud provider → make docker-images → tag → save → import
│   └── TCPWave extension → ./deploy.sh
│
├── 15. Install CRDs on Seed (F5LoadBalancerConfig CRD)
├── 16. Register extensions on Garden (ControllerDeployment + ControllerRegistration)
├── 17. Update gardenlet-airtelcloud-values.yaml (Garden CA, IP, token, CIDRs)
├── 18. helm install gardenlet (Seed cluster)
│
├── 19. WAIT: gardenlet bootstraps → Seed reconciles → Extensions deploy → Seed Ready
│
├── [CMP UI — Phase 6 workaround]:
│   ├── 20. CMP UI: Create Istio ingress LB → kubectl patch svc istio-ingressgateway
│   ├── 21. CMP UI: Create Seed DNS record (or CoreDNS override)
│   └── Wait for Seed Ready
│
├── 22. Create Shoot prerequisites (Project, CloudProfile, Creds, Binding)
├── 23. Create CMP F5 credentials Secret in Shoot namespace
├── 24. [Optional] CMP UI: Create control-plane LB → note VIP → set controlPlaneReady: true
├── 25. kubectl apply -f shoot.yaml (enableApplicationLB: false, provider: local)
│
├── 26. WAIT: Shoot reconciles → Ready
│
├── 27. Get Shoot kubeconfig
├── 28. Deploy demo app + Service type=LoadBalancer (shows Pending)
│
├── 29. CMP UI: Create app LB → note VIP
├── 30. kubectl patch svc status with VIP
├── 31. curl VIP → app response (real traffic through real F5)
│
└── 32. DONE: Platform operational with real LBs
```

## 7.2 Per-Repository Role

| Repo | Steps | What it does |
|---|---|---|
| **gardener (core)** | 7-9, 17-18, 19, 24 | KIND clusters, gardenlet, Seed/Shoot reconciliation |
| **provider-airtelcloud** | 14, 16, 24 (Infra+Worker) | VMs via OpenStack (or skip with provider-local) |
| **extension-f5** | 14-16, 22, 24 (Extension CR), 27 | LB for control-plane + application-plane |
| **extension-tcpwave** | 14, 16, 19 (DNS), 24 (DNS) | DNS records for Seed ingress + Shoot API |
| **extension-netapp-backup** | N/A (optional) | etcd backup to NetApp |
| **extension-trident** | N/A (optional) | PVs via NetApp CSI |

## 7.3 Validation Matrix

| Test | Command | Expected | Works without CMP API? |
|---|---|---|---|
| Garden healthy | `kubectl --context $GARDEN_CTX get pods -n garden-system` | All Running | Yes |
| Seed Ready | `kubectl --context $GARDEN_CTX get seed` | Ready | Yes (after Istio VIP patch) |
| Extensions installed | `kubectl --context $GARDEN_CTX get controllerinstallation` | Installed + Healthy | Yes |
| F5 CRD exists | `kubectl --context $SEED_CTX get crd \| grep f5` | Present | Yes |
| Shoot Ready | `kubectl --context $GARDEN_CTX -n garden-dev get shoot` | Ready | Yes (provider-local) |
| Service gets VIP | `kubectl --kubeconfig shoot.kubeconfig get svc echo-lb` | EXTERNAL-IP set | Yes (after manual patch) |
| Real traffic works | `curl http://<VIP>` | App response | Yes (real F5 routes traffic) |
| NetworkPolicy created | `kubectl --kubeconfig shoot.kubeconfig get networkpolicy` | Present (if svc-lb-bridge ran) | N/A (bridge disabled) |

## 7.4 Quick-Start Cheat Sheet (Happy Path — UI Workaround)

```bash
# === VM SETUP ===
# Install: Docker, Go, kubectl, Helm, KIND, yq, git, jq
# Clone: gardener, gardener-extension-f5

# === GARDEN + SEED ===
cd ~/extension_repos/gardener && make kind-up && make gardener-up && make kind2-up
# Merge kubeconfigs (see 4.2)

# === BOOTSTRAP ===
# Create bootstrap token (4.3)
# Create tcpwave credentials Secret (4.4)
# Build F5 extension: docker build → docker save → ctr import
# Install CRD: kubectl apply -f config/crd/...
# Register: kubectl apply -f deploy/garden/controller{deployment,registration}-f5.yaml
# Deploy gardenlet: helm install gardenlet ... -f gardenlet-values.yaml

# === CMP UI: Istio ingress LB (Phase 6, Resource 1) ===
# CMP UI → Load Balancers → Create (name: seed-istio-ingress) → note VIP
kubectl patch svc istio-ingressgateway -n istio-system --context $SEED_CTX \
  --subresource=status --type=merge -p '{"status":{"loadBalancer":{"ingress":[{"ip":"<VIP>"}]}}}'
# Wait for Seed Ready

# === CMP UI: DNS (Phase 6, Resource 4+5) ===
# CMP UI → DNS → Add A record for *.ingress.seed → Istio VIP
# CMP UI → DNS → Add A record for api.shoot → Istio VIP
# (or use CoreDNS override)

# === SHOOT ===
# Create: Project, CloudProfile (local), CredentialsBinding
# Create: cmp-f5-credentials Secret
# kubectl apply -f shoot.yaml (provider: local, enableApplicationLB: false)
# → Wait for Ready

# === DEMO: APP LB (Phase 6, Resource 3) ===
# Deploy echo app + Service type=LoadBalancer
# CMP UI → Load Balancers → Create (backends: shoot-node-IPs:NodePort) → note VIP
# kubectl patch svc echo-lb --subresource=status -p '{"status":{"loadBalancer":{"ingress":[{"ip":"<VIP>"}]}}}'
# curl <VIP> → "Hello from Gardener Shoot!"
```

---

# Part III: Per-Extension Deep Dive

## Airtel Cloud Provider — Reconciler Flows

**Infrastructure reconciler:**
```
Infrastructure CR → Reconcile()
  ├── Read InfrastructureConfig from CR.spec.providerConfig
  ├── Read cloud credentials from Secret (SecretBinding)
  ├── Call OpenStack Neutron API:
  │   ├── Create/ensure VPC (network)
  │   ├── Create/ensure subnet
  │   ├── Create/ensure security groups + rules
  │   ├── Create/ensure router + attach subnet
  │   └── Optionally create floating IP
  ├── Write InfrastructureStatus to CR.status.providerStatus
  │   (subnet IDs, SG IDs, router ID — consumed by Worker reconciler)
  └── Done
```

**Worker reconciler:**
```
Worker CR → Reconcile()
  ├── Read InfrastructureStatus (subnet ID, SG ID from Infrastructure)
  ├── For each worker pool:
  │   ├── Create MachineClass (OpenStack spec: flavor, image, network, SG, key)
  │   ├── Create/update MachineDeployment (desired replicas)
  │   └── machine-controller-manager → creates Machine → Nova API → VM
  ├── Wait for VMs to bootstrap kubelet
  └── Write Worker.status (machine addresses, provider IDs)
```

**Code exploration:**
```
pkg/controller/infrastructure/actuator.go    ← Infrastructure reconcile
pkg/controller/worker/actuator.go            ← Worker (VM) reconcile
pkg/controller/controlplane/actuator.go      ← ControlPlane (CCM, CSI)
pkg/apis/airtelcloud/v1alpha1/types.go       ← CRD types
pkg/airtelcloud/client/                      ← OpenStack client wrappers
```

---

## F5 Extension — Reconciler Flows

**Extension reconciler (Seed side):**
```
Extension CR → Reconcile() [pkg/controller/lifecycle/controller.go]
  │
  ├── Step 1: Ensure F5LoadBalancerConfig CR
  │   └── ensureF5LoadBalancerConfig() — creates/syncs from providerConfig
  │
  ├── Step 2: Control-plane LB
  │   ├── If Mechanism A (default):
  │   │   └── Mark ControlPlaneLoadBalancerReady=True (shared Seed VIP)
  │   └── If Mechanism B:
  │       └── provisionControlPlaneViaCMP()
  │           ├── refreshCeAuthIfNeeded()
  │           ├── CMP: POST /lb_service → LBService ID
  │           ├── CMP: POST /lb_service/{id}/vip → VIP
  │           ├── CMP: POST /load-balancers/{id}/virtual-servers → VS
  │           └── Write VIP to status
  │
  ├── Step 3: Application-plane LB (gated on CP Ready + enableApplicationLB)
  │   └── reconcileCISInShoot()
  │       ├── refreshCeAuthIfNeeded()
  │       ├── Get Shoot client (kubeconfig from Seed Secret)
  │       ├── In Shoot: create Namespace, SA, ClusterRole, ClusterRoleBinding
  │       └── Create Deployment: svc-lb-bridge (with CMP env vars)
  │
  └── Step 4: Update Extension.status.lastOperation = Succeeded
```

**svc-lb-bridge (Shoot side):**
```
Service type=LoadBalancer → Reconcile() [cmd/svc-lb-bridge/main.go]
  ├── Check loadBalancerClass matches
  ├── parseLBServiceConfig() — read annotations
  ├── choosePorts() + listBackendNodes()
  ├── For each port → ensureCMPResources()
  │   ├── findLBServiceByName() or CreateLBService()
  │   ├── findOrCreateVIP()
  │   └── createVirtualServer() with backends
  ├── ensureNetworkPolicy()
  ├── Patch Service status.loadBalancer.ingress[].ip = VIP
  └── Record Event: "EnsuredLoadBalancer"
```

---

## TCPWave DNS Extension — Reconciler Flow

```
DNSRecord CR → Reconcile()
  ├── Read desired DNS name + target IP from CR.spec
  ├── Read TCPWave credentials from Secret
  ├── Call TCPWave API:
  │   ├── Create/update A record (name → IP)
  │   └── Or TXT record for validation
  ├── Write DNSRecord.status
  └── Done
```

**Triggered by:** gardenlet creates DNSRecord CRs during Shoot provisioning.

```
pkg/controller/dnsrecord/actuator.go    ← Reconcile/Delete logic
pkg/tcpwave/client.go                   ← TCPWave HTTP client
```

---

## NetApp Backup Extension — Reconciler Flow

```
BackupBucket CR → Reconcile()
  ├── Read NetApp credentials from Secret
  ├── Create S3 bucket or ONTAP volume for backups
  ├── Write BackupBucket.status with bucket name/endpoint
  └── Done

BackupEntry CR → Reconcile()
  ├── Maps to a specific etcd backup within the bucket
  └── Used by etcd-backup-restore sidecar for snapshots
```

---

## Trident Storage Extension — Flow

```
Shoot ControlPlane → (Trident deployer)
  ├── Deploy Trident CSI driver DaemonSet + Deployment into Shoot
  ├── Create TridentBackendConfig CR pointing to NetApp SVM
  ├── Create StorageClass (e.g. "netapp-standard")
  └── Shoot users create PVCs → Trident provisions FlexVols
```

---

# Part IV: Debugging & Troubleshooting

## Log Commands

```bash
# F5 extension (Seed)
kubectl --context $SEED_CTX -n garden-f5-system logs deploy/gardener-extension-f5 --tail=200

# svc-lb-bridge (Shoot)
kubectl --kubeconfig ~/shoot.kubeconfig -n f5-cis-system logs deploy/svc-lb-bridge --tail=200

# gardenlet
kubectl --context $SEED_CTX -n garden logs deploy/gardenlet --tail=200

# Force re-reconcile extension
kubectl --context $SEED_CTX -n shoot--p--s annotate extension f5-loadbalancer \
  reconcile-at="$(date -Is)" --overwrite

# Port-forward to Shoot apiserver
kubectl --context $SEED_CTX -n shoot--project--shoot port-forward svc/kube-apiserver 7443:443
```

## Common Issues

| Symptom | Cause | Fix |
|---|---|---|
| Extension stuck "Progressing" | CMP API unreachable | Use Mechanism A (`enablePerShootControlPlaneVIP: false`); disable app LB |
| svc-lb-bridge CrashLooping | Missing env vars (CMP_ENDPOINT, etc.) | Check credentials Secret has all keys |
| Service stuck "Pending" | svc-lb-bridge not running or CMP error | Check svc-lb-bridge logs; verify credentials |
| VIP allocated but traffic fails | NetworkPolicy blocking or backend unreachable | Check auto-generated NetworkPolicy; verify node IPs |
| "Ce-Auth expired" errors | Token not being refreshed | Ensure Secret has `api-key-id` + `api-secret` (not just `Ce-Auth`) |
| Worker VMs not created | OpenStack quota or credential issue | Check airtelcloud provider logs; or use provider-local |
| DNS not resolving | TCPWave API error or zone misconfigured | Use CoreDNS override; check extension logs |
| Extension pod crash "no kind F5LoadBalancerConfig" | CRD not installed on Seed | `kubectl apply -f config/crd/...` |
| gardenlet "unauthorized" | Bootstrap token missing or wrong | Verify token Secret exists on Garden cluster |
| Seed stuck "Bootstrapping" | Istio ingress has no external IP | Manual patch: `kubectl patch svc istio-ingressgateway ...` |
| ImagePullBackOff | Images not loaded into KIND node | `docker save \| ctr import` |

---

# Part V: Dependency & Blocker Matrix

## Severity Legend

| Severity | Meaning |
|---|---|
| **🔴 CRITICAL** | Cannot proceed. All downstream steps blocked. |
| **🟠 HIGH** | Core functionality broken but partial progress possible. |
| **🟡 MEDIUM** | Degraded functionality. Platform works but feature missing. |
| **🟢 LOW** | Non-essential. Platform fully operational without it. |

## Phase 1: VM Bootstrap Blockers

| # | Dependency | If Unavailable | Severity | Workaround |
|---|---|---|---|---|
| 1.1 | VM access | Cannot start | 🔴 | None |
| 1.2 | OS packages (git, gcc, make, curl, jq) | Cannot compile/script | 🔴 | Pre-install on VM snapshot |
| 1.3 | Docker | Cannot build/run anything | 🔴 | Pre-install |
| 1.4 | Go 1.24.6+ | Cannot compile extensions | 🔴 | Pre-install + `go mod vendor` |
| 1.5 | kubectl + Helm + KIND | Cannot manage clusters | 🔴 | Pre-install binaries |
| 1.6 | Disk ≥100 GB | "no space left" | 🔴 | `lvextend`; prune Docker |
| 1.7 | RAM ≥16 GB | OOMKill | 🔴 | Reduce replicas |
| 1.8 | Source repos | Cannot compile | 🔴 | Offline transfer |
| 1.9 | Internet | Cannot download deps/images | 🟠 | `go mod vendor` + `docker save` |
| 1.10 | yq | `make gardener-up` may fail | 🟡 | Pre-install |

## Phase 2: CMP UI Resource Blockers

| # | Dependency | If Unavailable | Severity (real) | Severity (offline) | Workaround |
|---|---|---|---|---|---|
| 2.1 | CMP account | All extensions fail | 🔴 | 🟢 | `provider-local` + CMP UI pre-create LBs |
| 2.2 | VPC | No networking | 🔴 | 🟢 | `provider-local` auto-creates |
| 2.3 | Subnet | No worker IPs | 🔴 | 🟢 | `provider-local` |
| 2.4 | Machine image (Glance) | VMs can't boot | 🔴 | 🟢 | `provider-local` |
| 2.5 | CMP API keys | 401 from CMP | 🔴 | 🟢 | Keys still needed in Secret (for pod startup) but no API calls made |
| 2.6 | OpenStack app creds | No VMs | 🔴 | 🟢 | `provider-local` |
| 2.7 | DNS zone | DNS fails | 🟠 | 🟢 | CoreDNS static hosts |
| 2.8 | NetApp cluster | Backup fails | 🟡 | 🟢 | Skip backup |

> **For offline/staging:** ALL Phase 2 items bypassed with `provider-local` + CMP UI pre-created LBs + CoreDNS.

## Phase 3: Seed Setup Blockers

| # | Dependency | If Unavailable | Severity | Workaround |
|---|---|---|---|---|
| 3.1 | Garden cluster running | Nothing works | 🔴 | `make kind-up && make gardener-up` |
| 3.2 | Seed cluster running | No Shoots | 🔴 | `make kind2-up` |
| 3.3 | Garden↔Seed network | Seed never registers | 🔴 | KIND bridge auto-connects |
| 3.4 | Bootstrap token | gardenlet unauthorized | 🔴 | Generate + apply Secret |
| 3.5 | Garden CA cert | gardenlet rejects API | 🔴 | Extract from kubeconfig |
| 3.6 | Extension images in Seed | ImagePullBackOff | 🔴 | `docker save \| ctr import` |
| 3.7 | F5 CRD on Seed | Extension crash loop | 🔴 | `kubectl apply -f config/crd/...` |
| 3.8 | ControllerRegistration | Extension not deployed | 🔴 | Apply YAML on Garden |
| 3.9 | TCPWave credentials Secret | gardenlet panic | 🔴 | Create (even dummy values) |
| 3.10 | Istio ingress external IP | Seed stuck Bootstrapping | 🔴 | Manual `kubectl patch` |
| 3.11 | Leader election RBAC | Extension crash | 🔴 | Create Role + RoleBinding |
| 3.12 | Correct CIDRs | Networking issues | 🟠 | Discover from cluster |
| 3.13 | Seed ingress DNS | Shoot API unreachable by name | 🟠 | CoreDNS; provider-local DNS |

### Phase 3 Critical Path
```
Garden running → Seed running → Network OK
  → Bootstrap token + CA → gardenlet deployed
    → TCPWave Secret exists → gardenlet starts → Seed registered
      → Images loaded + CRD installed + Registrations applied
        → Extension pods start → RBAC OK → Extensions healthy
          → Istio ingress gets IP → DNS record
            → SEED READY ✓
```

## Phase 4: Shoot Setup Blockers

| # | Dependency | If Unavailable | Severity | Workaround |
|---|---|---|---|---|
| 4.1 | Seed is Ready | Shoot stays Pending | 🔴 | Complete Phase 3 |
| 4.2 | Project + CloudProfile | Shoot rejected | 🔴 | Apply manifests |
| 4.3 | CredentialsBinding | Shoot rejected | 🔴 | Apply manifest |
| 4.4 | CMP F5 credentials Secret | Extension CR fails | 🔴 | Create in Shoot ns |
| 4.5 | OpenStack API | Infrastructure fails | 🔴 (real) / 🟢 (offline) | `provider-local` |
| 4.6 | CMP LBaaS API | Mechanism B fails; Services Pending | 🟠 | Mechanism A + pre-create in CMP UI + `kubectl patch` |
| 4.7 | TCPWave API | DNS fails | 🟠 | CoreDNS |
| 4.8 | Worker images (Glance) | VMs can't boot | 🔴 (real) / 🟢 (offline) | `provider-local` |
| 4.9 | Networking (Calico) | Pods can't communicate | 🔴 | Deployed by `make gardener-up` |
| 4.10 | Shoot kubeconfig | svc-lb-bridge not deployed | 🟠 | Wait — auto-created by gardenlet |
| 4.11 | Bridge image in Shoot | svc-lb-bridge ImagePull | 🟠 | Pre-load into worker nodes |

### Phase 4 Critical Path
```
Seed Ready → Project + CloudProfile + Creds
  → Shoot applied → Namespace created → F5 Secret
    → Infrastructure → DNS → etcd + apiserver
      → Extension reconciled → Workers join
        → svc-lb-bridge deployed → SHOOT READY ✓
```

## Extension-Specific Blockers

### F5 Extension Decision Tree

```
CMP API available?
├── YES → Full functionality (Mechanism A or B, app LB)
└── NO
    ├── Control-plane LB:
    │   ├── Mechanism A (default): ✅ Works — no CMP calls
    │   └── Mechanism B: ❌ Blocked — use dev stub (`controlPlaneReady: true`) with UI-created LB
    │
    └── Application-plane LB:
        ├── Pre-create in CMP UI + kubectl patch? → ✅ Works (real VIPs)
        └── Nothing done? → ❌ Services stay Pending forever
```

### Airtelcloud Provider

Without OpenStack APIs → **completely non-functional**. Replace with `provider-local`.

### TCPWave DNS

Without TCPWave API → DNS fails but **Seed/Shoot still become Ready** (DNS is non-blocking with timeout). Access Shoot by IP or port-forward.

### NetApp/Trident

Non-critical. etcd uses local PVs. Skip entirely for offline testing.

## Offline Testing Coverage

| Feature | Testable Offline? | How |
|---|---|---|
| Seed bootstrap + registration | ✅ | KIND + gardenlet |
| Extension deployment | ✅ | Pre-loaded images |
| F5 extension (Mechanism A) | ✅ | `controlPlaneReady: true` |
| F5 extension (Mechanism B) | ✅ | Dev stub (`controlPlaneReady: true`) + UI-created VIP |
| svc-lb-bridge LB flow | ✅ | Disable bridge; pre-create LBs in CMP UI + `kubectl patch` |
| NetworkPolicy generation | ✅ | Purely in-cluster |
| Credential rotation | ✅ | Update Secret → watch triggers reconcile |
| Shoot with provider-local | ✅ | Workers as Pods |
| Real airtelcloud provider | ❌ | Needs OpenStack |
| Real DNS records | ❌ | Needs TCPWave |
| Real VIP traffic | ❌ | Needs F5 BIG-IP |
| etcd backup to NetApp | ❌ | Needs ONTAP |

## Workaround Cost

| Workaround | Effort | Config Changes |
|---|---|---|
| CMP UI LB pre-creation | ~30 min per LB | CMP UI + `kubectl patch svc --subresource=status` |
| provider-local | Zero (already in Gardener) | `provider.type: local` |
| CoreDNS static hosts | 15 min | CoreDNS ConfigMap |
| Manual Istio VIP patch | 5 min | One `kubectl patch` |
| Pre-load images | 30 min | `docker save/load` + `ctr import` |
| Go vendor | 10 min | `-mod=vendor` build flag |
| Skip backup | 5 min | Remove `spec.backup` from Seed |

## Pre-Implementation Checklist

### Day -7

- [ ] VM provisioned (≥8 vCPU, ≥32 GB RAM, ≥200 GB disk)
- [ ] All OS packages installed
- [ ] Docker, Go, kubectl, Helm, KIND, yq installed
- [ ] All 6 repos cloned/transferred

### Day -3

- [ ] CMP account verified (if using real cloud)
- [ ] API keys + app credentials generated
- [ ] VPC + subnets created (or decided on provider-local)
- [ ] All extension images built + saved as tarballs
- [ ] `go mod vendor` done for all repos

### Day -1

- [ ] `make kind-up && make gardener-up` succeeds
- [ ] `make kind2-up` succeeds
- [ ] Kubeconfigs merged
- [ ] Images loaded into KIND nodes
- [ ] CMP UI: Istio ingress LB pre-created (if CMP UI available)
- [ ] `~/gardener-env.sh` populated

### Day 0

- [ ] Bootstrap token created
- [ ] TCPWave Secret created
- [ ] gardenlet deployed → Seed Ready
- [ ] Extension pods running
- [ ] Istio ingress IP assigned
- [ ] Shoot prerequisites applied
- [ ] F5 credentials Secret created
- [ ] Shoot applied → Ready
- [ ] svc-lb-bridge running
- [ ] Test Service gets VIP ✓

---

## Appendix: Key File Paths

| Purpose | File |
|---|---|
| Extension main binary | `cmd/gardener-extension-f5/main.go` |
| Extension lifecycle controller | `pkg/controller/lifecycle/controller.go` |
| Extension controller registration | `pkg/controller/lifecycle/extension_controller.go` |
| CMP HTTP client | `pkg/f5/client.go` |
| F5LoadBalancerConfig CRD types | `pkg/apis/f5/v1alpha1/types.go` |
| CRD YAML | `config/crd/f5loadbalancerconfigs.f5.extensions.gardener.cloud.yaml` |
| svc-lb-bridge (Service LB) | `cmd/svc-lb-bridge/main.go` |
| Ingress controller | `cmd/svc-lb-bridge/ingress_controller.go` |
| Seed LB controller | `cmd/seed-service-lb-controller/main.go` |
| Prometheus metrics | `pkg/metrics/metrics.go` |
| gardenlet values | `gardenlet-airtelcloud-values.yaml` |
| Controller registration | `deploy/garden/controllerregistration-f5.yaml` |
| Controller deployment | `deploy/garden/controllerdeployment-f5.yaml` |
| Credentials Secret template | `deploy/kind/cmp-f5-credentials.secret.yaml` |
| Demo manifests | `config/samples/` |

---

## Appendix B: Software & Module Dependencies

### Build-Time Tool Dependencies

| Tool | Required Version | Purpose | Install Command |
|---|---|---|---|
| **Go** | 1.24.6+ | Compile all extension binaries | `wget go1.24.6.linux-amd64.tar.gz` |
| **Docker** | 24.x+ | Build container images, run KIND nodes | `dnf install docker-ce` / `apt install docker.io` |
| **kubectl** | v1.34.2+ | Cluster management | `curl -LO dl.k8s.io/release/v1.34.2/bin/linux/amd64/kubectl` |
| **Helm** | v3.x | Deploy gardenlet, extension charts | `get-helm-3` script |
| **KIND** | v0.25.0+ | Local Kubernetes clusters (Garden + Seed) | `kind-linux-amd64` binary |
| **yq** | v4.44.3+ | YAML processing in Gardener Makefile scripts | `yq_linux_amd64` binary |
| **jq** | 1.6+ | JSON processing in helper scripts | `dnf install jq` / `apt install jq` |
| **openssl** | 1.1+ | Bootstrap token generation, TLS cert inspection | System package |
| **make** | GNU Make 4.x | Gardener build system (`make kind-up`, `make gardener-up`) | System package |
| **gcc** | 9+ | CGO compilation (some Go packages require C) | System package |
| **git** | 2.x | Clone repositories | System package |

### Go Module Dependencies (Direct — from go.mod)

| Module | Version | Role in Project |
|---|---|---|
| `github.com/gardener/gardener` | **v1.135.1** | Core Gardener library: extension framework, APIs, reconciler helpers, Shoot/Seed types |
| `github.com/go-logr/logr` | v1.4.3 | Structured logging interface (used by all controllers) |
| `k8s.io/api` | v0.34.3 | Kubernetes core API types (Pod, Service, Secret, etc.) |
| `k8s.io/apimachinery` | v0.34.3 | Kubernetes API machinery (ObjectMeta, runtime.Object, labels, etc.) |
| `k8s.io/client-go` | v0.34.3 | Kubernetes client library (REST client, informers, caches) |
| `sigs.k8s.io/controller-runtime` | **v0.22.5** | Controller framework: manager, reconciler, client, scheme, leader election |

### Key Transitive Dependencies (from go.mod indirect)

| Module | Version | Why It's Pulled In |
|---|---|---|
| `github.com/gardener/machine-controller-manager` | v0.60.2 | Machine API types (MachineClass, MachineDeployment) used by Worker reconciler |
| `github.com/gardener/etcd-druid/api` | v0.34.0 | etcd CR types used by gardenlet for Shoot etcd lifecycle |
| `github.com/gardener/cert-management` | v0.19.0 | Certificate management CRDs referenced by Gardener core |
| `github.com/prometheus/client_golang` | v1.23.2 | Prometheus metrics (`f5_cmp_api_calls_total`, etc. in `pkg/metrics/`) |
| `github.com/fluent/fluent-operator/v3` | v3.5.0 | Fluent Bit logging CRDs (Gardener shoots use Fluent Bit) |
| `github.com/open-telemetry/opentelemetry-operator` | v0.142.0 | OpenTelemetry CRDs referenced by Gardener |
| `go.uber.org/zap` | v1.27.0 | Logging backend (controller-runtime uses zap via `go-logr/zapr`) |
| `github.com/zitadel/oidc/v3` | v3.38.1 | OIDC token handling (Gardener identity management) |
| `github.com/Masterminds/sprig/v3` | v3.3.0 | Helm template functions used in chart rendering |

### Runtime Infrastructure Dependencies

| Dependency | Required By | Protocol | Offline Alternative |
|---|---|---|---|
| **CMP LBaaS API** | F5 extension, svc-lb-bridge, seed-service-lb-controller | HTTPS (REST v2.1) | Pre-create LBs in CMP UI + `kubectl patch` (see Phase 6) |
| **CMP Compute/Network API** | airtelcloud provider extension | HTTPS (OpenStack Nova/Neutron) | `provider-local` |
| **CMP DNS / TCPWave API** | TCPWave DNS extension | HTTPS | CoreDNS static entries |
| **NetApp ONTAP API** | NetApp backup extension | HTTPS | Skip backup (`spec.backup: nil`) |
| **NetApp Trident CSI** | Trident storage extension | iSCSI/NFS | `local-path-provisioner` (KIND default) |
| **Container Registry** | All image pulls | HTTPS (Docker/OCI) | Pre-loaded images via `docker save/load` + `ctr import` |
| **Kubernetes API** | All controllers | HTTPS (kube-apiserver) | Always available (KIND clusters are local) |

### Version Compatibility Matrix

| Component | Version | Must Match |
|---|---|---|
| Gardener core | v1.135.1 | Extension API versions must be compatible |
| controller-runtime | v0.22.5 | Must match Gardener's pinned version exactly |
| Kubernetes (KIND) | v1.34.x | Matches `k8s.io/api` version; Shoot `spec.kubernetes.version` |
| Go toolchain | 1.24.6 | Declared in `go.mod`; older versions will not compile |
| Helm | v3.x | gardenlet chart uses Helm v3 API |
| Docker | 24.x+ | KIND requires modern Docker; cgroup v2 support |

---

## Appendix C: Code Decision Points — How the Extension Bypasses CMP

This appendix documents the **exact code paths** in the F5 extension that determine whether CMP is called or skipped. Understanding these is important for configuring the UI-based workaround (Phase 6).

### C.1 Control-Plane LB Decision Tree

From `pkg/controller/lifecycle/controller.go` (lines 92-134):

```go
if !cfg.Spec.EnablePerShootControlPlaneVIP {
    // Mechanism A — no CMP call at all. Uses shared Seed Istio VIP.
    a.reconcileControlPlaneStatusSharedSeedIngress(log, cfg)
} else if cfg.Spec.ControlPlaneReady == nil && cfg.Spec.CcpApiEndpoint != "" {
    // Mechanism B — full CMP provisioning. FAILS if CMP unreachable.
    a.provisionControlPlaneViaCMP(ctx, log, ex, cfg)
} else {
    // Dev stub — trusts controlPlaneReady flag. No CMP call.
    a.reconcileControlPlaneStatus(log, cfg)
}
```

| Condition | What happens | CMP needed? | Recommended for demo? |
|---|---|---|---|
| `enablePerShootControlPlaneVIP: false` (default) | Marks CP LB Ready immediately with reason `SharedSeedIngressVIP` | **No** | ✅ Yes — simplest |
| `enablePerShootControlPlaneVIP: true` + `controlPlaneReady: true` + `controlPlaneVIP: "x.x.x.x"` | Trusts the flag, records VIP, reason `ExternalProvisioned` | **No** | ✅ Yes — use VIP from CMP UI |
| `enablePerShootControlPlaneVIP: true` + `ccpApiEndpoint` set + no `controlPlaneReady` | Calls CMP to provision LBService→VIP→VS | **Yes** | ❌ Fails without API |

### C.2 Application-Plane LB Decision Tree

From `pkg/controller/lifecycle/controller.go` (lines 137-212):

```go
if cfg.Spec.EnableApplicationLB {
    // Deploys svc-lb-bridge to Shoot. Bridge calls CMP at runtime.
    if !isConditionTrue(cfg.Status.Conditions, "ControlPlaneLoadBalancerReady") {
        // Blocked until CP LB is ready
        return nil
    }
    a.reconcileCISInShoot(ctx, log, ex, cfg)  // deploys svc-lb-bridge
} else {
    // Cleanup: removes svc-lb-bridge from Shoot.
    // Sets ApplicationLoadBalancerReady=False, Reason=Disabled
    a.cleanupCISInShoot(ctx, log, ex)
}
```

| Condition | What happens | CMP needed? | Recommended for demo? |
|---|---|---|---|
| `enableApplicationLB: false` | svc-lb-bridge NOT deployed. No CMP calls. Services stay Pending unless manually patched. | **No** | ✅ Yes — pre-create LBs in CMP UI |
| `enableApplicationLB: true` | svc-lb-bridge deployed. Every `type: LoadBalancer` Service triggers CMP API calls. | **Yes (at runtime, automatically, for every Service)** | ❌ Fails without API |

### C.3 Credential Reading

From `pkg/controller/lifecycle/controller.go` — `refreshCeAuthIfNeeded()`:

```go
apiKeyID, keyIDErr := readSecretKey(secret, "api-key-id", ...)
apiSecret, secretErr := readSecretKey(secret, "api-secret", ...)
if keyIDErr != nil || secretErr != nil {
    // No API key credentials — fall back to existing Ce-Auth token as-is.
    return existingCeAuth, existingErr
}
// Otherwise: generate fresh HMAC token from api-key-id + api-secret
```

**For the UI workaround:** The credentials Secret must still exist for the extension pod to start, but since `enableApplicationLB: false` and `enablePerShootControlPlaneVIP: false` (or `controlPlaneReady: true`), no actual CMP HTTP calls are made. The credentials are read but never used.

### C.4 Summary: Recommended Demo Configuration

```yaml
spec:
  extensions:
  - type: f5-loadbalancer
    providerConfig:
      apiVersion: f5.extensions.gardener.cloud/v1alpha1
      kind: F5LoadBalancerConfig
      # Control-plane: Mechanism A (no CMP call)
      enablePerShootControlPlaneVIP: false
      # Application-plane: disabled (no svc-lb-bridge, no CMP calls)
      enableApplicationLB: false
      # Credentials still required for pod startup (not used for API calls)
      credentialsSecretRef:
        name: cmp-f5-credentials
        namespace: shoot--dev--my-shoot
```

With this configuration:
- **Zero CMP HTTP calls** are made by any controller
- Control-plane LB → Ready immediately (Mechanism A)
- Application LB → Disabled (Services stay Pending until manually patched via Phase 6, Resource 3)
- The F5 extension pod starts and reconciles successfully
- Shoot reaches Ready state without any CMP dependency
