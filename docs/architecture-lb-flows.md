# F5 Load Balancing — Architecture Diagrams & Flows

Two planes of load balancing are managed by the `gardener-extension-f5`:

| Plane | Scope | Default | Feature Flag |
|---|---|---|---|
| **Control Plane LB** | Seed Ingress VIP (shared, one per Seed) | ✅ On | — |
| **Application Plane LB** | Per-Shoot app services via F5 CIS | ❌ Off | `enableApplicationLB: true` |
| *(Per-Shoot CP VIP)* | Dedicated VIP per kube-apiserver | ❌ Off (toggle) | TBD |

---

## 1. Control Plane Load Balancing — Seed Ingress VIP via F5

### What it is

A single F5 Virtual Server fronts the Seed cluster's Istio ingress gateway.  
All Shoot kube-apiservers on that Seed are reachable through this **one shared VIP**.  
Traffic is distinguished per-Shoot using **TLS SNI** (hostname-based routing inside Istio).

### Architecture

```mermaid
graph TD
    subgraph External
        Client["fa:fa-laptop kubectl / curl\n(tenant)"]
    end

    subgraph "F5 BIG-IP"
        SeedVIP["Seed Ingress VIP\n100.72.200.10 : 443\none VIP per Seed cluster"]
    end

    subgraph "Seed Cluster (kind / VM)"
        IstioNP["Istio NodePort\n: 30443"]
        IstioGW["Istio Ingress Gateway\n(TLS SNI routing)"]

        subgraph "Shoot namespace: shoot--tenant--a"
            API_A["kube-apiserver A\nClusterIP svc"]
        end
        subgraph "Shoot namespace: shoot--tenant--b"
            API_B["kube-apiserver B\nClusterIP svc"]
        end
        subgraph "Shoot namespace: shoot--tenant--n"
            API_N["kube-apiserver N ..."]
        end
    end

    Client -->|"HTTPS :443"| SeedVIP
    SeedVIP -->|"TCP :30443"| IstioNP
    IstioNP --> IstioGW
    IstioGW -->|"SNI: api.a.local.seed.gardener.cloud"| API_A
    IstioGW -->|"SNI: api.b.local.seed.gardener.cloud"| API_B
    IstioGW -->|"SNI: api.n.local.seed.gardener.cloud"| API_N
```

### Request Flow

```mermaid
sequenceDiagram
    participant T as Tenant (kubectl)
    participant F5 as F5 BIG-IP<br/>Seed VIP
    participant I as Istio Ingress Gateway<br/>(Seed)
    participant K as kube-apiserver<br/>(Shoot)

    T->>F5: HTTPS GET /api/v1/nodes<br/>SNI: api.tenant-a.seed.gardener.cloud
    Note over F5: L4 TCP forward to<br/>Istio NodePort :30443
    F5->>I: TLS pass-through
    Note over I: Inspect TLS SNI hostname<br/>→ route to shoot--tenant--a
    I->>K: forward to kube-apiserver ClusterIP
    K-->>T: 200 OK (or 401 if no kubeconfig)
```

### How the extension wires this up

```mermaid
flowchart LR
    GE["Gardener\nf5 Extension\n(reconcile loop)"]
    CMP["CMP LBaaS API\n(production)"]
    F5["F5 BIG-IP\nSeed VIP + VS"]
    CFG["F5LoadBalancerConfig\n(Shoot namespace)"]

    GE -->|"POST /lb_service\nGET /vip\nPOST /virtual-servers"| CMP
    CMP -->|"provisions"| F5
    CMP -->|"returns VIP + VS name"| GE
    GE -->|"writes status.vip\nControlPlaneLoadBalancerReady=True"| CFG
```

> **Kind demo shortcut:** `controlPlaneReady: true` + `controlPlaneVIP: 100.72.200.20`  
> skips the CMP call — extension marks `ControlPlaneLoadBalancerReady=True` immediately.

---

## 2. Application Plane Load Balancing — F5 CIS in Shoot

### What it is

After the Shoot's control-plane VIP is ready (`ControlPlaneLoadBalancerReady=True`),  
the extension deploys **F5 CIS (Container Ingress Services)** into the Shoot cluster.  
CIS watches `Service type=LoadBalancer` objects and automatically provisions a  
**dedicated F5 Virtual Server + VIP** per application service — without any manual F5 work.

Gated by `spec.enableApplicationLB: true` in `F5LoadBalancerConfig`.

### Architecture

```mermaid
graph TD
    subgraph External
        AppUser["fa:fa-user App User\n(HTTP / HTTPS)"]
    end

    subgraph "F5 BIG-IP"
        AppVIP_A["App VIP A\n(per Service — auto-allocated)"]
        AppVIP_B["App VIP B"]
    end

    subgraph "Seed Cluster"
        Ext["f5 Extension Pod\n(lifecycle controller)"]
    end

    subgraph "Shoot Cluster"
        direction TB
        CIS["F5 CIS Pod\n(f5-cis-system/f5-cis)\nwatches Services"]
        Bridge["svc-lb-bridge\n(optional)\nmirrors VIP → Service status"]
        SvcA["Service A\ntype=LoadBalancer\n← EXTERNAL-IP set by bridge"]
        SvcB["Service B\ntype=LoadBalancer"]
        Pods_A["App Pods A\n(nginx, echo, ...)"]
        Pods_B["App Pods B"]
    end

    subgraph "CMP"
        CMPAPI["CMP LBaaS API"]
    end

    AppUser -->|"HTTPS"| AppVIP_A
    AppUser -->|"HTTPS"| AppVIP_B
    AppVIP_A -->|"NodePort / direct"| Pods_A
    AppVIP_B -->|"NodePort / direct"| Pods_B

    Ext -->|"deploys CIS + bridge\nwhen ControlPlaneReady=True"| CIS
    CIS -->|"watches"| SvcA
    CIS -->|"watches"| SvcB
    CIS -->|"POST /lb_service\nPOST /virtual-servers"| CMPAPI
    CMPAPI -->|"provisions"| AppVIP_A
    CMPAPI -->|"provisions"| AppVIP_B
    Bridge -->|"patches status.loadBalancer.ingress"| SvcA
    Bridge -->|"patches status.loadBalancer.ingress"| SvcB
```

### Request Flow — new app Service being exposed

```mermaid
sequenceDiagram
    participant Dev as Developer
    participant K8s as Shoot kube-apiserver
    participant CIS as F5 CIS Pod<br/>(in Shoot)
    participant CMP as CMP LBaaS API
    participant F5 as F5 BIG-IP
    participant Bridge as svc-lb-bridge<br/>(in Shoot)

    Dev->>K8s: kubectl apply Service<br/>type: LoadBalancer
    K8s-->>CIS: watch event: Service added
    CIS->>CMP: POST /lb_service (tenant, flavor, network)
    CMP-->>CIS: lb_service_id
    CIS->>CMP: GET /vip (lb_service_id)
    CMP-->>CIS: vip = 100.72.201.50
    CIS->>CMP: POST /virtual-servers (vip, port, pool-members)
    CMP->>F5: provisions VS + pool on BIG-IP
    CMP-->>CIS: vs_id
    CIS->>Bridge: (or direct) update Service status
    Bridge->>K8s: patch Service status.loadBalancer.ingress[0].ip = 100.72.201.50
    Note over Dev: kubectl get svc → EXTERNAL-IP: 100.72.201.50
    Dev->>F5: curl https://100.72.201.50 → app response
```

### Gate: Control Plane must be Ready first

```mermaid
flowchart TD
    R["Reconcile F5LoadBalancerConfig"]
    CP{"ControlPlaneLoadBalancerReady\n= True ?"}
    AE{"enableApplicationLB\n= true ?"}
    SKIP["Skip CIS deployment\nSet ApplicationLoadBalancerReady=False\nreason: CPNotReady"]
    NODEPLOY["CIS not deployed\n(feature disabled)"]
    DEPLOY["Deploy F5 CIS + svc-lb-bridge\ninto Shoot cluster\nSet ApplicationLoadBalancerReady=True"]

    R --> CP
    CP -- No --> SKIP
    CP -- Yes --> AE
    AE -- No --> NODEPLOY
    AE -- Yes --> DEPLOY
```

---

## 3. Combined Architecture — Both Planes Together

```mermaid
graph TD
    subgraph External
        Kubectl["fa:fa-terminal kubectl\n(Gardener operator / tenant)"]
        AppUser["fa:fa-user App User"]
    end

    subgraph "F5 BIG-IP"
        SeedVIP["Seed Ingress VIP\nControl Plane\n(shared, 1 per Seed)"]
        AppVIP["App VIP\nApplication Plane\n(1 per Service, auto)"]
    end

    subgraph "Seed Cluster"
        FExt["f5 Extension\n(lifecycle controller)"]
        Istio["Istio Ingress Gateway\n(SNI routing)"]

        subgraph "Shoot A — shoot--tenant--a"
            KAPI["kube-apiserver A"]
            CIS_A["F5 CIS\n(deployed by extension)"]
            SvcLB["Service type=LB"]
            AppPodsA["App Pods"]
        end
    end

    subgraph "CMP"
        CMPAPI["CMP LBaaS API"]
    end

    %% Control plane path
    Kubectl -->|"HTTPS :443\nSNI routing"| SeedVIP
    SeedVIP --> Istio --> KAPI

    %% App plane path
    AppUser -->|"HTTPS"| AppVIP --> AppPodsA

    %% Extension wiring
    FExt -->|"1. provisions Seed VIP"| CMPAPI
    CMPAPI --> SeedVIP
    FExt -->|"2. deploys CIS\n(after CP ready)"| CIS_A
    CIS_A -->|"3. provisions App VIPs"| CMPAPI
    CMPAPI --> AppVIP
    CIS_A --> SvcLB
```

---

## 4. Component Summary

| Component | Where it runs | Role |
|---|---|---|
| `gardener-extension-f5` | Seed cluster (`extension-gardener-extension-f5` ns) | Lifecycle controller — provisions CP VIP, deploys CIS |
| `F5LoadBalancerConfig` CRD | Seed cluster (Shoot namespace) | Config + status for one Shoot's LB wiring |
| `Istio Ingress Gateway` | Seed cluster (`istio-ingress` ns) | SNI-based routing from Seed VIP to per-Shoot kube-apiservers |
| `F5 CIS` | **Shoot cluster** (`f5-cis-system` ns) | Watches `Service type=LB`, calls CMP to provision App VIPs |
| `svc-lb-bridge` | **Shoot cluster** | Mirrors allocated VIP into `Service.status.loadBalancer.ingress` |
| `CMP LBaaS API` | External (production infra) | Allocates VIPs, creates VS + pools on F5 BIG-IP |
| `F5 BIG-IP` | External (hardware/VELOS) | Data-plane: routes real traffic to pool members |
