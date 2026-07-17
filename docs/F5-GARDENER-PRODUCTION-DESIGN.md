F5 LOAD BALANCING WITH GARDENER
PRODUCTION DESIGN & READINESS DOCUMENT


Document Information

    Version:        1.0
    Date:           14 April 2026
    Status:         Draft - For Product Team Sign-Off
    Authors:        Managed Kubernetes Platform Team
    Classification: Internal - Confidential


TABLE OF CONTENTS

    1.  Executive Summary
    2.  Solution Overview
    3.  Architecture
        3.1  High-Level Architecture
        3.2  Component Breakdown
        3.3  Network Topology
    4.  Load Balancing Design
        4.1  Control-Plane Load Balancing
        4.2  Application-Plane Load Balancing
        4.3  Seed Ingress Load Balancing
    5.  Integration Points
        5.1  CMP LBaaS API Integration
        5.2  F5 BIG-IP Integration (via CMP)
        5.3  Gardener Extension Framework
    6.  Data Flow & Lifecycle
    7.  Configuration Reference
    8.  Security Considerations
    9.  What Is Working Today
    10. External Dependencies & Blockers
    11. Risk Register
    12. Recommendations & Next Steps
    Appendix


------------------------------------------------------------------------

1. EXECUTIVE SUMMARY

This document describes the production design for integrating F5 BIG-IP load balancing with the Gardener-based Managed Kubernetes platform on Airtel Cloud (OpenStack).

The Gardener platform provides fully managed Kubernetes clusters ("Shoots") to tenants. Each Shoot requires load balancing at two layers:

    - Control-Plane LB: A stable Virtual IP (VIP) to front the Shoot's kube-apiserver so that worker nodes and external clients reach the API server at a consistent address.

    - Application-Plane LB: Tenant workloads inside Shoots can request Service type=LoadBalancer and receive a real F5-backed VIP, giving production-grade L4/L7 load balancing for their applications.

Additionally, the Seed cluster itself requires a VIP for its ingress gateway. All three tiers are handled by the F5 extension.

Summary of Load Balancing Tiers:

    Tier                    Purpose                                          Mechanism                                          Default
    ----------------------  -----------------------------------------------  -------------------------------------------------  -------
    Control-Plane LB        Expose Shoot kube-apiserver with a stable VIP    Shared Seed Ingress VIP via Istio SNI (default)    ON
                                                                             OR dedicated per-Shoot VIP via CMP (toggle)        OFF
    Application-Plane LB    Expose tenant workloads via Service type=LB      svc-lb-bridge (Shoot) --> CMP LBaaS (L4 VIP)
                                                                             + L7 via in-shoot Ingress controller (Option A)     OFF
    Seed Ingress LB         Expose Seed ingress gateway (istio) with VIP     seed-service-lb-controller --> CMP LBaaS           ON

The central component is the gardener-extension-f5 (extension type: f5-loadbalancer). It runs on the Seed cluster and orchestrates both control-plane and application-plane load balancing for every Shoot cluster.


------------------------------------------------------------------------

2. SOLUTION OVERVIEW

What We Are Building

A Managed Kubernetes offering on Airtel Cloud where:

    - Gardener orchestrates the lifecycle of Kubernetes clusters (Shoots)
    - F5 BIG-IP provides enterprise-grade L4/L7 load balancing
    - CMP (Cloud Management Platform) acts as the LBaaS API layer on top of F5
    - TCPWave provides DNS management
    - OpenStack provides compute (VMs), networking (VPCs, subnets), and storage

How the Pieces Fit Together

    +----------------------------------------------------------------+
    |                       GARDEN CLUSTER                            |
    |  (Gardener Control Plane - manages all Seed/Shoot lifecycle)   |
    |                                                                |
    |  ControllerRegistration: provider-airtelcloud                  |
    |  ControllerRegistration: gardener-extension-f5                 |
    |  ControllerRegistration: gardener-extension-tcpwave            |
    |  CloudProfile: airtelcloud                                     |
    |  Seed: <seed-name>                                             |
    |  Shoot: <shoot-name>                                           |
    +----------------------------+-----------------------------------+
                                 | gardenlet
                                 v
    +----------------------------------------------------------------+
    |                       SEED CLUSTER                              |
    |  (Hosts Shoot control planes + extension controllers)          |
    |                                                                |
    |  Extension Controllers:                                        |
    |    - gardener-extension-provider-airtelcloud                   |
    |    - gardener-extension-f5                                     |
    |    - gardener-extension-tcpwave                                |
    |                                                                |
    |  Per-Shoot namespace: shoot--<project>--<shoot-name>           |
    |    - Extension/f5-loadbalancer (trigger)                       |
    |    - F5LoadBalancerConfig/<name> (per-shoot config + status)   |
    |    - Shoot control plane pods (apiserver, etcd, etc.)          |
    |    - Credentials secrets                                       |
    +----------------------------+-----------------------------------+
                                 | provisions via APIs
                                 v
    +----------------------------------------------------------------+
    |              AIRTEL CLOUD INFRASTRUCTURE                        |
    |                                                                |
    |  +-------------+   +--------------+   +----------------+      |
    |  |  OpenStack   |   |  F5 BIG-IP   |   |   TCPWave      |      |
    |  |  (Compute,   |   |  (LTM)       |   |   (DNS)        |      |
    |  |   Network)   |   |              |   |                |      |
    |  +------+-------+   +------+-------+   +-------+--------+      |
    |         |                  |                    |               |
    |    OpenStack API     CMP LBaaS API        TCPWave API          |
    +----------------------------------------------------------------+
                                 |
                                 v
    +----------------------------------------------------------------+
    |                     SHOOT CLUSTER                               |
    |  (Tenant Kubernetes - worker nodes on OpenStack VMs)           |
    |                                                                |
    |  +----------------------------------------------------------+  |
    |  | svc-lb-bridge (Shoot-side controller)                      |  |
    |  |   - Watches Service type=LoadBalancer                      |  |
    |  |   - Provisions VIP via CMP LBaaS (LBService->VIP->VS)      |  |
    |  |   - Mirrors VIP into Service.status.loadBalancer           |  |
    |  +----------------------------------------------------------+  |
    |                                                                |
    |  +----------------------------------------------------------+  |
    |  | Ingress controller (optional, for L7)                      |  |
    |  |   - e.g. nginx-ingress, istio-ingressgateway               |  |
    |  |   - Provides HTTP routing from Kubernetes Ingress objects  |  |
    |  +----------------------------------------------------------+  |
    |                                                                |
    |  Tenant workloads (Deployments, Services, Ingress, etc.)       |
    +----------------------------------------------------------------+


------------------------------------------------------------------------

3. ARCHITECTURE

3.1 High-Level Architecture

The system is organized in the standard Gardener three-tier model:

    Layer       Cluster                 What Runs There
    ----------  ----------------------  -----------------------------------------------
    Garden      Management cluster      Gardener API server, scheduler, controller-
                                        manager; ControllerRegistrations; CloudProfiles;
                                        Seed/Shoot objects

    Seed        Seed cluster            gardenlet, extension controllers (F5, provider-
                (on Airtel Cloud)       airtelcloud, TCPWave), Shoot control planes
                                        (each in a dedicated namespace)

    Shoot       Tenant clusters         Tenant workloads, svc-lb-bridge (if application LB
                (OpenStack VMs)         enabled), optional in-shoot ingress controller (L7)


3.2 Component Breakdown

3.2.1 gardener-extension-f5 (Seed-Side Controller)

    Property            Value
    ------------------  ----------------------------------------------------------
    Extension type      f5-loadbalancer
    Deployed to         Seed cluster (auto-installed by Gardener via
                        ControllerInstallation)
    CRD managed         F5LoadBalancerConfig (f5.extensions.gardener.cloud/v1alpha1)
    Image               gardener-extension-f5:<version>
    Resources           100m CPU / 128Mi mem (request), 256Mi mem (limit)

    Responsibilities:
    1. Watches Extension resources (type f5-loadbalancer) in Shoot technical namespaces
    2. Creates/syncs F5LoadBalancerConfig CR from the Extension's providerConfig
    3. Provisions control-plane VIP via CMP LBaaS API (LBService -> VIP -> VirtualServer)
    4. Deploys svc-lb-bridge into the Shoot cluster (when control-plane LB is ready and
       enableApplicationLB=true)
    5. Propagates CMP credentials/config into the Shoot (Ce-Auth, org, project, endpoint)
    6. Reports status conditions: ControlPlaneLoadBalancerReady, ApplicationLoadBalancerReady
    7. Discovers kube-apiserver backends (Node IPs + NodePorts) for control-plane VS


3.2.2 In-shoot Ingress controller (Shoot-Side, for L7 - Option A)

    Property            Value
    ------------------  ----------------------------------------------------------
     Examples            nginx-ingress, istio-ingressgateway
     Deployed by         Gardener (default) or tenant/team (optional)
     Namespace           Typically `ingress-nginx` or `istio-system` (implementation-defined)

    Responsibilities:
     1. Watches Kubernetes Ingress objects and performs HTTP(S) routing (host/path)
     2. Terminates TLS if configured (cert-manager, Secrets, SNI)
     3. For external reachability, is typically exposed via a Service type=LoadBalancer
         with loadBalancerClass f5.extensions.gardener.cloud/bigip (handled by svc-lb-bridge)


3.2.3 svc-lb-bridge (Shoot-Side - Application LB controller)

    Property            Value
    ------------------  ----------------------------------------------------------
    Image               Same image as the extension (multi-binary)
        Deployed by         gardener-extension-f5 controller (into Shoot)
        Purpose             Implements CMP-only application-plane load balancing:
                                                - Watches Service type=LoadBalancer with loadBalancerClass
                                                    f5.extensions.gardener.cloud/bigip
                                                - Provisions CMP LBaaS resources (LBService -> VIP -> VirtualServer)
                                                    with backends = worker NodeInternalIP + Service NodePort
                                                - Mirrors VIP into Service.status.loadBalancer.ingress[].ip
                                                - Reconciles backend changes on Node churn (best-effort)


3.2.4 seed-service-lb-controller (Seed-Side)

    Property            Value
    ------------------  ----------------------------------------------------------
    Purpose             Handles Service type=LoadBalancer on the Seed (e.g., istio
                        ingress gateway)
    Trigger             Services with loadBalancerClass:
                        f5.extensions.gardener.cloud/bigip
    Action              Provisions VIP via CMP LBaaS API, sets
                        Service.status.loadBalancer.ingress[].ip


3.2.5 gardener-extension-provider-airtelcloud

    Property            Value
    ------------------  ----------------------------------------------------------
    Extension types     Infrastructure, ControlPlane, Worker
    Purpose             Core Airtel Cloud provider - provisions networks, VMs,
                        control plane wiring
    Integration         Uses OpenStack (gophercloud), delegates LB to F5 extension,
                        DNS to TCPWave


3.3 Network Topology

    +-------------------------------------------------------------------+
    |                         Seed Cluster                               |
    |  Nodes: 172.18.0.0/16   Pods: 10.1.134.0/16   SVC: 10.2.0.0/16  |
    |                                                                    |
    |  +-------------------------------------------------------------+  |
    |  | gardener-extension-f5 pod                                    |  |
    |  |   --> CMP LBaaS API (HTTPS, Ce-Auth)                        |  |
    |  |   --> Shoot kube-apiserver (TCP 443, via NetworkPolicy)      |  |
    |  +-------------------------------------------------------------+  |
    +----------------------------+--------------------------------------+
                                 |
                +----------------+------------------+
                v                v                  v
       +---------------+  +----------+  +------------------+
       | CMP LBaaS     |  | BIG-IP   |  | Shoot Worker     |
       | API Endpoint  |  | Mgmt     |  | Nodes            |
       | (HTTPS)       |  | (443)    |  | (NodePorts)      |
       +---------------+  +----------+  +------------------+

    Required Network Policies (pre-provisioned in Helm chart):
    - Extension pod --> DNS (allowed)
    - Extension pod --> Garden/Seed API server (allowed)
    - Extension pod --> Private networks / CMP endpoints (allowed)
    - Extension pod --> Public networks (allowed)
    - Extension pod --> Shoot kube-apiserver on port 443 (allowed)
    - Shoot svc-lb-bridge pod --> CMP LBaaS API endpoint (HTTPS, must be allowed)


------------------------------------------------------------------------

4. LOAD BALANCING DESIGN

4.1 Control-Plane Load Balancing

Purpose: Every Shoot cluster needs a stable endpoint for its kube-apiserver.
The extension supports TWO modes, selected by a feature flag.


4.1.1 Mode A — Shared Seed Ingress VIP (DEFAULT, enablePerShootControlPlaneVIP: false)

This is the default behaviour and matches vanilla Gardener. The Seed ingress
VIP (managed by the seed-service-lb-controller, Section 4.3) fronts ALL Shoots
on the Seed. Istio SNI-routes each TLS connection to the correct Shoot's
kube-apiserver based on the hostname.

    - No per-Shoot VIP is allocated.
    - No CMP API call is made by the extension for control-plane LB.
    - The extension sets ControlPlaneLoadBalancerReady=True immediately
      (reason: SharedSeedIngressVIP) so the app-plane gate still functions.
    - All Shoots share the single Seed Ingress VIP.
    - Istio in the Seed cluster performs TLS SNI routing:
        api.<shoot>.<project>.<seed>.gardener.cloud --> correct kube-apiserver

    Traffic path:
        kubectl/client --> F5 Seed VIP:443
            --> Seed Node:NodePort (Istio)
                --> Istio ingress gateway (SNI inspect)
                    --> kube-apiserver pod (correct Shoot namespace)

    When to use:
        - Default for all new Shoots.
        - Equivalent to cloud-provider LB but delivered via F5 on Airtel Cloud.
        - Suitable for standard tenants who do not require IP isolation.


4.1.2 Mode B — Per-Shoot Dedicated VIP (OPTIONAL, enablePerShootControlPlaneVIP: true)

When this flag is enabled, the extension allocates a DEDICATED F5 VIP per
Shoot kube-apiserver. Traffic bypasses Istio entirely; F5 routes directly to
the Shoot's kube-apiserver NodePort.

    - Each Shoot gets its own F5 Virtual Server and dedicated IP.
    - Istio is NOT in the data path; F5 does L4 TCP forwarding.
    - Provides full network isolation between Shoots at the VIP level.
    - F5 health-checks each Shoot's kube-apiserver independently.
    - Premium tenants can get dedicated F5 profiles (rate limiting, WAF).

    Traffic path:
        kubectl/client --> F5 per-Shoot VIP:443
            --> Seed Node:kube-apiserver NodePort
                --> kube-apiserver pod (directly, no Istio)

    Automated Flow (via CMP LBaaS API) — production path:

        Step 1:  Shoot created --> Gardener creates Extension/f5-loadbalancer
        Step 2:  Extension reads providerConfig (enablePerShootControlPlaneVIP=true,
                 ccpApiEndpoint, credentials)
        Step 3:  Controller calls CMP LBaaS API:
                 a. POST /lb_service/          --> Create LBService (or reuse)
                 b. GET  /lb_service/{id}/vip  --> Allocate VIP
                 c. POST /virtual-servers      --> Create Virtual Server
                    - VIP: allocated from step (b)
                    - Protocol: TCP, Port: 443
                    - Backends: kube-apiserver Node IPs + NodePorts
        Step 4:  Controller writes VIP to F5LoadBalancerConfig.status.vip
        Step 5:  Controller sets ControlPlaneLoadBalancerReady=True
        Step 6:  Provider extension wires VIP into Shoot kubeconfig

    Alternative (out-of-band / kind demo): Set spec.controlPlaneReady=true and
    spec.controlPlaneVIP. Controller skips CMP calls and marks ready immediately.

    When to use:
        - Premium tenants requiring IP isolation.
        - Tenants needing dedicated F5 WAF/rate-limit profiles per Shoot.
        - When Istio must be removed from the kube-apiserver data path.
        - Multi-region: each Shoot maps to a geographically distinct VIP.


4.1.3 Decision Flow

    Reconcile() called
        |
        v
    enablePerShootControlPlaneVIP == false ?  (default)
        |  YES
        |  --> reconcileControlPlaneStatusSharedSeedIngress()
        |      Sets ControlPlaneLoadBalancerReady=True (reason: SharedSeedIngressVIP)
        |      No CMP call. No VIP allocated.
        |
        |  NO (flag=true)
        |  controlPlaneReady != nil ?
        |      YES --> out-of-band path: reads spec.controlPlaneVIP, marks ready
        |      NO + ccpApiEndpoint set --> CMP automation path (Steps 1-6 above)
        |      NO + no ccpApiEndpoint  --> dev-stub: marks ready from spec.controlPlaneVIP
        v
    App-plane gate: ControlPlaneLoadBalancerReady=True ? then deploy svc-lb-bridge.


4.1.4 Comparing the two modes

    Property                    Mode A (Shared Seed VIP)      Mode B (Per-Shoot VIP)
    --------------------------  ----------------------------  -----------------------------
    Default                     YES                           NO (opt-in per Shoot)
    VIPs provisioned            1 per Seed                    1 per Shoot
    Istio in data path          YES (SNI routing)             NO (L4 TCP bypass)
    CMP call on Shoot create    NO                            YES
    CMP call on Shoot delete    NO                            YES (cleanup VS/VIP/LBService)
    Shoot isolation             Shared VIP, SNI-separated     Full IP isolation
    F5 profile per Shoot        Not possible                  Possible (WAF, rate-limit)
    Config flag                 enablePerShootControlPlaneVIP: false  true


CMP LBaaS API Authentication (Mode B only):
    - Ce-Auth header: HMAC-SHA256 token generated from API key ID + secret
    - Required headers: Ce-Auth, organisation-name, project-id, ce-region
    - Token is short-lived; the extension client generates a fresh token per API call


4.2 Application-Plane Load Balancing

Purpose: Tenant workloads in Shoots can request external reachability and get a
real F5-backed VIP for production traffic.

In the CMP-only design, application-plane VIPs are provisioned by the Shoot-side
svc-lb-bridge controller via CMP LBaaS. BIG-IP programming is done behind CMP;
there is no direct AS3/CIS dependency.

4.2.1 L4 VIP per Service (Service type=LoadBalancer)

        When a tenant creates a Service type=LoadBalancer with:
            spec.loadBalancerClass: f5.extensions.gardener.cloud/bigip

        svc-lb-bridge:
        - detects the Service
        - reads Service.spec.ports[].nodePort
        - lists Shoot worker nodes (NodeInternalIP)
        - calls CMP LBaaS (LBService -> VIP -> VirtualServer)
        - writes the allocated VIP into Service.status.loadBalancer.ingress[].ip

        Backends are NodeIP:NodePort (kube-proxy then routes to pods). This avoids any
        dependency on pod CIDR routability from BIG-IP.

    Complete traffic path:

    Client --> VIP --> BIG-IP Virtual Server --> Pool Member (VM IP:NodePort)
                              |                        |
                              |                        v
                              |                  OpenStack VM (Worker Node)
                              |                        |
                              |                        v
                              |                  kube-proxy --> Pod
                              |
                       Health monitors each VM on NodePort
                       Distributes traffic using configured LB algorithm

    This means:
    - Every Shoot worker node VM is automatically a potential pool member
        - As Shoot scales (more VMs added/removed), svc-lb-bridge reconciles backend membership
    - BIG-IP does NOT need to know about pod IPs or pod networking
    - BIG-IP ONLY needs network reachability to the VM IPs on NodePort range
      (default: 30000-32767)

4.2.2 L7 / Ingress features (Option A)

    If you want Kubernetes-native L7 routing (host/path rules, TLS, etc.) without
    BIG-IP L7 programming, run an Ingress controller inside the Shoot (e.g.
    nginx-ingress or istio-ingressgateway) and expose it through a Service
    type=LoadBalancer that uses the F5 loadBalancerClass.

    In this model:
    - F5/CMP provides the external L4 VIP to reach the Ingress controller.
    - The in-shoot Ingress controller provides L7 features from Kubernetes Ingress objects.

    Traffic path:
        Client --> F5 VIP (CMP) --> NodeIP:NodePort --> Ingress controller Pod
              --> backend Service/Pod (based on Ingress rules)

Flow:

    Step 1:  Control-plane LB is Ready (Section 4.1)
    Step 2:  Extension controller deploys into Shoot:
             a. Namespace + RBAC for the bridge
             b. Deployment: f5-svc-lb-bridge
    Step 3:  Tenant creates a Service type=LoadBalancer with:
             - spec.loadBalancerClass: f5.extensions.gardener.cloud/bigip
    Step 4:  svc-lb-bridge provisions CMP LBaaS resources and patches Service status
    Step 5:  Traffic: Client --> VIP --> VM (NodePort) --> Pod

    Example: If a Shoot has 3 worker VMs (10.0.1.10, 10.0.1.11, 10.0.1.12) and a
    Service exposes NodePort 31234, the BIG-IP pool will contain:
        Pool Member 1: 10.0.1.10:31234
        Pool Member 2: 10.0.1.11:31234
        Pool Member 3: 10.0.1.12:31234

Gating Logic (safety):
    - Application LB is BLOCKED if control-plane LB is not ready
        - If enableApplicationLB is true but the svc-lb-bridge config/credentials are missing, the
      controller returns a permanent error and does not retry (avoids hot-loops)
    - Status condition ApplicationLoadBalancerReady tracks the state


4.3 Seed Ingress Load Balancing

Purpose: The Seed's ingress gateway (typically istio) needs a VIP for external access to Shoot API servers and Gardener dashboard.

Flow:
    Step 1:  gardenlet creates istio-ingress Service with
             loadBalancerClass: f5.extensions.gardener.cloud/bigip
             (this is the trigger; configured via gardenlet Helm values)
    Step 2:  seed-service-lb-controller watches all Services with that loadBalancerClass
    Step 3:  Controller discovers Seed node VM IPs by reading Node objects:
             node.status.addresses[InternalIP] (e.g., 172.18.0.2, 172.18.0.3, 172.18.0.4)
    Step 4:  Controller reads the istio-ingress Service's NodePort
             (e.g., service.spec.ports[].nodePort = 31443)
    Step 5:  Controller calls CMP LBaaS API:
             a. POST /lb_service/       --> Create LBService
             b. GET  /lb_service/{id}/vip  --> Allocate VIP (e.g., 10.50.1.100)
             c. POST /virtual-servers   --> Create Virtual Server
                - Pool members: Seed node IPs + NodePort
                  (172.18.0.2:31443, 172.18.0.3:31443, 172.18.0.4:31443)
    Step 6:  Controller writes VIP into Service status:
             Service.status.loadBalancer.ingress[0].ip = "10.50.1.100"
    Step 7:  gardenlet sees the IP appear on the Service status (standard Kubernetes
             watch) and updates the Seed resource with this ingress IP
    Step 8:  When new Shoots are created, Gardener uses this Seed ingress IP as the
             base for Shoot apiserver DNS addresses
    Step 9:  CMP resource IDs (LBServiceID, VIPID, VirtualServerID) are stored in
             Service annotations for cleanup on Seed deletion

    This is a ONE-TIME setup per Seed (not per Shoot). All Shoots on the Seed
    share this single ingress VIP. Istio routes each request to the correct
    Shoot's kube-apiserver based on SNI (Server Name Indication).

    Pool Members for Seed Ingress:

    If the Seed has 3 nodes:
        Pool Member 1: 172.18.0.2:31443
        Pool Member 2: 172.18.0.3:31443
        Pool Member 3: 172.18.0.4:31443

    Traffic path:
        External client --> F5 VIP:443 --> Seed Node:31443 --> istio pod
            --> routes by SNI to correct Shoot's kube-apiserver pod

    Code location: cmd/seed-service-lb-controller/main.go
        - listNodeInternalIPs() discovers Seed node IPs
        - ensureServiceStatusVIP() writes VIP to Service status

Seed Configuration (gardenlet values):

    settings:
      loadBalancerServices:
        class: f5.extensions.gardener.cloud/bigip


------------------------------------------------------------------------

5. INTEGRATION POINTS

5.1 CMP LBaaS API Integration

    Endpoint                                         Method   Purpose
    -----------------------------------------------  ------   -----------------------------------
    /api/v2.1/load-balancers/domain/{org}/project/{project}/load-balancers/lb_service/             POST     Create Load Balancer Service
    /api/v2.1/load-balancers/domain/{org}/project/{project}/load-balancers/lb_service/{id}/        GET      Get LBService details
    /api/v2.1/load-balancers/domain/{org}/project/{project}/load-balancers/lb_service/{id}/vip     GET      Get/allocate VIP
    /api/v2.1/load-balancers/domain/{org}/project/{project}/load-balancers/{lb_id}/virtual-servers POST     Create Virtual Server
    /api/v2.1/load-balancers/domain/{org}/project/{project}/load-balancers/{lb_id}/virtual-servers GET      List Virtual Servers
    /api/v2.1/load-balancers/flavors                                                         GET      List available flavors

    Environment Variables on the extension pod:

    Variable                                 Purpose
    ---------------------------------------  -------------------------------------------
    F5_INSECURE_SKIP_TLS_VERIFY              Skip TLS verification (dev only, must be
                                             false in production)
    F5_HTTP_API_PREFIX                       CMP API base path
    F5_HTTP_POOL_PATH_TEMPLATE              Pool endpoint template
    F5_HTTP_MONITOR_PATH_TEMPLATE           Monitor endpoint template
    F5_HTTP_VIRTUALSERVER_PATH_TEMPLATE     VS endpoint template
    F5_HTTP_CP_VS_ENSURE_PATH_TEMPLATE      Control-plane VS create/update path
    F5_HTTP_CP_VS_DELETE_PATH_TEMPLATE      Control-plane VS delete path
    F5_HTTP_PROBE_PATH                      Health probe path


5.2 F5 BIG-IP Integration (via CMP)

        In the CMP-only design, the extension and Shoot-side controllers do not talk
        directly to BIG-IP management. All BIG-IP programming happens behind CMP LBaaS.

        Prerequisites on BIG-IP are therefore operational (CMP connectivity + tenant
        setup) rather than Kubernetes controller prerequisites.


5.3 Gardener Extension Framework

    Object                   Location                    Purpose
    -----------------------  -------------------------   -----------------------------------
    ControllerRegistration   Garden cluster              Declares extension type f5-loadbalancer
    ControllerDeployment     Garden cluster              Contains Helm chart (base64-encoded)
    ControllerInstallation   Garden cluster              Links Registration+Deployment to Seed
    Extension                Seed (per-Shoot namespace)  Trigger object - gardenlet creates this
    F5LoadBalancerConfig     Seed (per-Shoot namespace)  Per-Shoot config and status CRD


------------------------------------------------------------------------

6. DATA FLOW & LIFECYCLE

6.1 Shoot Creation (Happy Path)

    Step   Action                                              Actor
    ----   --------------------------------------------------  --------------------
     1     User creates Shoot with F5 extension config         User/API
     2     Gardener creates Extension/f5-loadbalancer           gardenlet
     3     F5 controller creates F5LoadBalancerConfig           extension-f5
     4     F5 controller calls CMP --> LBService                extension-f5
     5     F5 controller calls CMP --> VIP allocation           extension-f5
     6     F5 controller calls CMP --> Virtual Server           extension-f5
           (backends = kube-apiserver NodeIPs:NodePorts)
     7     Status: ControlPlaneLoadBalancerReady=True           extension-f5
     8     Provider extension wires VIP into Shoot kubeconfig   provider-airtelcloud
     9     Workers join, Shoot becomes Ready                    gardenlet
        10     F5 controller deploys svc-lb-bridge into Shoot       extension-f5
            (if enableApplicationLB=true)
        11     Status: ApplicationLoadBalancerReady=True            extension-f5
        12     Tenants create Service type=LB --> F5-backed VIP     svc-lb-bridge/CMP
            (Option A for L7: expose in-shoot Ingress controller via such a Service)

6.2 Shoot Deletion

    Step   Action                                              Actor
    ----   --------------------------------------------------  --------------------
     1     User deletes Shoot                                  User/API
     2     Gardener triggers Extension Delete                   gardenlet
    3     F5 controller cleans up svc-lb-bridge from Shoot    extension-f5
     4     F5 controller deletes CMP VS/VIP/LBService          extension-f5
     5     F5 controller removes finalizer                     extension-f5
     6     Gardener completes Shoot deletion                   gardenlet


------------------------------------------------------------------------

7. CONFIGURATION REFERENCE

7.1 Shoot Extension ProviderConfig

    spec:
      extensions:
      - type: f5-loadbalancer
        providerConfig:
          spec:
            # -- Control-Plane LB (via CMP) --
            ccpApiEndpoint: "https://cmp.airtelcloud.internal/api/v1"
            tenantOrPartition: "qa-tenant"
            credentialsSecretRef:
              name: f5-credentials
              namespace: shoot--<project>--<shoot>

            # CMP LBaaS parameters
            flavorId: "flavor-uuid"
            networkId: "network-uuid"
            vpcId: "vpc-uuid"
            vpcName: "my-vpc"

            # -- Control-Plane LB mode selection --
            # false (default): use shared Seed Ingress VIP; no per-Shoot CMP call
            # true:            allocate dedicated VIP per Shoot via CMP (Mechanism B)
            enablePerShootControlPlaneVIP: false

            # -- Per-Shoot VIP only (ignored when enablePerShootControlPlaneVIP=false) --
            # Option 1: Automated via CMP (production)
            ccpApiEndpoint: "https://cmp.airtelcloud.internal/api/v1"
            tenantOrPartition: "qa-tenant"
            credentialsSecretRef:
              name: f5-credentials
              namespace: shoot--<project>--<shoot>
            flavorId: "flavor-uuid"
            networkId: "network-uuid"
            vpcId: "vpc-uuid"
            vpcName: "my-vpc"
            # Option 2: Out-of-band (manual / kind demo)
            controlPlaneReady: true
            controlPlaneVIP: "100.72.200.20"

            # -- Application-Plane LB --
            enableApplicationLB: true
            cis:
                            # CMP-only mode: deploy svc-lb-bridge into the Shoot.
                            # NOTE: The field name is legacy ("cis"), but CIS/AS3 are not used.
                            bridgeImage: "registry.local.gardener.cloud:5001/gardener-extension-f5:<version>"
                            # The following legacy fields are ignored in CMP-only mode:
                            # bigipUrl, image, partition, extraArgs


7.2 F5 Credentials Secret

    apiVersion: v1
    kind: Secret
    metadata:
      name: f5-credentials
      namespace: <shoot-technical-namespace>
    type: Opaque
    data:
            # Ce-Auth (CMP token auth)
      Ce-Auth: <base64>
            organisation-name: <base64>
      project-id: <base64>


7.3 Seed Registration (gardenlet values)

    seedConfig:
      spec:
        provider:
          type: airtelcloud
          region: regionOne
        extensions:
        - type: f5-loadbalancer
        settings:
          loadBalancerServices:
            class: f5.extensions.gardener.cloud/bigip
        networks:
          nodes: 172.18.0.0/16
          pods: 10.1.134.0/16
          services: 10.2.0.0/16


------------------------------------------------------------------------

8. SECURITY CONSIDERATIONS

    Area                 Measure
    -------------------  ----------------------------------------------------------
    CMP authentication   Short-lived HMAC tokens (Ce-Auth); API key secret stored
                         in Kubernetes Secrets

    TLS                  All API calls use HTTPS; F5_INSECURE_SKIP_TLS_VERIFY
                         must be false in production

    RBAC                 Extension uses scoped ServiceAccount; least-privilege
                         ClusterRole to be defined for production

    NetworkPolicy        Gardener networking labels restrict pod egress; explicit
                         policies required for Seed/Shoot controllers to reach CMP

    Image provenance     Extension image built from source (multi-binary: extension,
                         seed-service-lb-controller, svc-lb-bridge)

    Secret rotation      Credentials can be rotated by updating the Secret;
                         controller re-propagates on next reconciliation

    Production Hardening Required:
    1. Define scoped RBAC ClusterRole (replace dev-only clusterAdmin)
    2. Set F5_INSECURE_SKIP_TLS_VERIFY=false and configure proper CA certificates
    3. Implement Secret rotation policy for BIG-IP and CMP credentials
    4. Audit NetworkPolicy rules for Shoot-to-CMP and Seed-to-CMP access


------------------------------------------------------------------------

9. WHAT IS WORKING TODAY

The following capabilities have been implemented, tested, and verified:

    #    Capability                                           Verified On
    ---  ---------------------------------------------------  ----------------
     1   gardener-extension-f5 compiles, builds, and runs     kind + real Seed
     2   F5LoadBalancerConfig CRD with OpenAPI v3 schema      kind + real Seed
     3   Lifecycle controller reconciles Extension objects     kind + real Seed
     4   Control-plane LB via out-of-band VIP                 kind
         (controlPlaneReady=true path)
     5   Control-plane LB via CMP LBaaS API                   Code complete
         (LBService -> VIP -> Virtual Server)
     6   Application-plane LB gating on CP readiness           kind
         (svc-lb-bridge only deploys after ControlPlaneLoadBalancerReady=True)
     7   svc-lb-bridge deployment into Shoot                   kind
         (watches Services + Nodes; provisions CMP LBaaS resources)
     8   CMP credentials/config injected into Shoot            kind
         (Ce-Auth + org + project + CMP endpoint passed as env)
     9   Service type=LoadBalancer -> CMP VIP                  kind
         (vip provision + status.loadBalancer.ingress patched)
    10   Seed Service LB controller                            kind
         (VIP allocation via CMP for Seed ingress gateway)
    11   Status conditions and error reporting                 kind
         (Permanent vs transient errors; conditions on CRD)
    12   Finalizer-based cleanup on Shoot deletion             kind
            (svc-lb-bridge cleanup + CMP resource deletion)
    13   Helm chart with Gardener networking labels/policies   kind + real Seed
    14   End-to-end load distribution demo                     kind
         (Repeated curls to VIP alternate across pods,
          proving actual F5-backed load balancing)


------------------------------------------------------------------------

10. EXTERNAL DEPENDENCIES & BLOCKERS

These are dependencies on external teams or infrastructure that are currently preventing full end-to-end automation. These are NOT internal code issues; the extension code is complete and functional. Resolution of these items requires coordination with the F5/CMP/Network teams.


DEPENDENCY 1: BIG-IP to Shoot worker network reachability

    Severity:       HIGH - Blocks application-plane traffic in production
    Impact:         BIG-IP (data plane) must be able to reach Shoot worker nodes on
                    the NodePort range for health monitors and traffic forwarding.
                    The CMP LBaaS programming can succeed even if the data path is
                    blocked; traffic will still fail.

    Requirements:
    1. BIG-IP --> Shoot worker node network: NodePort reachability
    2. Return path from node/pod back to BIG-IP/client (routing/firewall)

    Action Owner:  Network Infrastructure Team


DEPENDENCY 2: CMP LBaaS API support for stable backend identity and updates

    Severity:       HIGH - Affects scale-up/down correctness
    Impact:         svc-lb-bridge uses NodeIP+NodePort backends. For robust
                    reconciliation, CMP should provide:
                    - a stable compute_id for each backend node, or an IP->compute_id lookup
                    - an API to update pool members / node list without recreating the VS

    Current State:  The current implementation recreates the virtual server when
                    backend membership changes (best-effort).

    Action Owner:  CMP Platform Team


DEPENDENCY 3: CMP LBaaS API Accessibility from Seed and Shoots


    Severity:       HIGH - Blocks automation
    Impact:         The Seed-side extension controller and Shoot-side svc-lb-bridge
                    need to call the CMP LBaaS API. If the endpoint is not reachable
                    due to network/firewall rules, VIP provisioning will not work.

    What Is Needed:
    1. Confirm CMP API endpoint URL(s) and verify network path from Seed + Shoots
    2. Provide API credentials (Ce-Auth + org + project)
       match the contract our extension implements
    4. Confirm VIP/VS creation endpoints and required form fields (some CMP
       deployments use different field names in the UI vs. API)

    Action Owner:  CMP Platform Team


DEPENDENCY 5: CMP tenancy strategy (org/project/VPC) for production

    Severity:       MEDIUM
    Impact:         Application-plane LBs are created under CMP tenant scoping
                    (organisation-name + project-id) and optionally VPC. A clear
                    strategy is required for multi-tenant isolation.

    What Is Needed:
    1. Production organisation/project mapping strategy (per project vs shared)
    2. VPC strategy (per project vs shared)
    3. Naming/labeling conventions for LBService/VS resources for auditability

    Action Owner:  F5 Administration Team


------------------------------------------------------------------------

11. RISK REGISTER

    #    Risk                                    Likelihood   Impact     Mitigation
    ---  --------------------------------------  ----------   --------   ---------------------------------
    R1   CMP LBaaS API changes or is             Medium       High       Out-of-band VIP workaround;
         unavailable                                                      API contract documentation

        R2   CMP LBaaS feature gaps (member         Medium       High       Define supported behaviors;
            updates, compute identity)                                     use recreate-on-change as fallback

    R3   Network segmentation blocks             High         Critical   Pre-validate routing; document
         BIG-IP <--> Shoot traffic                                       NetworkPolicy requirements

    R4   Ingress controller misconfiguration     Medium       Medium     Provide supported ingress controller
                                                                         profiles; document defaults

    R5   Credential rotation causes downtime     Medium       Medium     Implement automated rotation;
                                                                         test Secret update propagation

    R6   Many Shoots exhaust BIG-IP              Medium       High       Capacity planning; VIP pool
         partitions or VIP pool                                          sizing; partition strategy

    R7   Single BIG-IP = single point of         High         Critical   HA pair recommended;
         failure                                                         active-standby or active-active

    R8   Extension upgrade disrupts existing     Medium       High       Rolling update strategy;
         Shoots                                                          compatibility testing


------------------------------------------------------------------------

12. RECOMMENDATIONS & NEXT STEPS

    Priority   Action                                              Owner                    Target
    --------   ------------------------------------------------    ----------------------   ----------
    P0         Establish network path: BIG-IP <--> Shoot           Network Infrastructure   Immediate
               worker node networks (bidirectional)

    P0         Close CMP LBaaS feature gaps needed for             CMP Platform Team        Immediate
               stable backend identity + member updates

    P1         Validate CMP LBaaS API reachability from Seed       CMP Platform Team        Week 1
               and confirm API contract matches implementation

    P1         Define production CMP tenancy mapping               CMP Platform Team        Week 1
               (org/project/VPC strategy)

    P1         Define scoped RBAC ClusterRole for production       Platform Team            Week 1
               (replace dev clusterAdmin)

    P2         Configure TLS CA certificates for CMP and           Platform + Security      Week 2
               any required internal endpoints (remove insecure skip)

    P2         Capacity planning: VIP pool sizing, partition       Platform + F5 Team       Week 2
               strategy, BIG-IP HA configuration

    P3         Load/performance testing with multiple Shoots       Platform Team            Week 3

    P3         Monitoring and alerting setup for extension          Platform + SRE           Week 3
               health, svc-lb-bridge, CMP connectivity

    P3         Operations runbook and troubleshooting guide        Platform Team            Week 3


------------------------------------------------------------------------

APPENDIX

A. Extension Types Registered

    resources:
    - kind: Extension
      type: f5-loadbalancer    # Primary
      primary: true
    - kind: Extension
      type: f5                 # Legacy compatibility
      primary: false


B. F5LoadBalancerConfig Status Conditions

    Condition                            Reason                   Meaning
    -----------------------------------  -----------------------  -------------------------------------------
    ControlPlaneLoadBalancerReady=True    SharedSeedIngressVIP     enablePerShootControlPlaneVIP=false;
                                                                  shared Seed Ingress VIP in use (default)
    ControlPlaneLoadBalancerReady=True    ExternalProvisioned      out-of-band: controlPlaneReady=true set
    ControlPlaneLoadBalancerReady=True    Provisioned              CMP automated VIP/VS creation succeeded
    ControlPlaneLoadBalancerReady=True    Configured               Dev-stub: VIP present in spec
    ControlPlaneLoadBalancerReady=False   NotConfigured            spec.controlPlaneVIP is empty
    ControlPlaneLoadBalancerReady=False   NotReady                 controlPlaneReady=false (waiting)
    ControlPlaneLoadBalancerReady=False   ProvisionFailed          CMP API call failed
    ApplicationLoadBalancerReady=True     Reconciled               svc-lb-bridge deployed and healthy in Shoot
    ApplicationLoadBalancerReady=False    Disabled                 enableApplicationLB=false; svc-lb-bridge removed
    ApplicationLoadBalancerReady=False    Blocked                  CP LB not ready; svc-lb-bridge deployment deferred
    ApplicationLoadBalancerReady=False    ConfigError              bridgeImage/CMP credentials missing
    ApplicationLoadBalancerReady=False    ReconcileFailed          svc-lb-bridge deployment error (transient)


C. Versions

    Component                                Version
    -----------------------------------------  ---------------------
    gardener-extension-f5                      v0.1.0-dev
    gardener-extension-provider-airtelcloud    v0.1.0-dev
    Gardener                                   v1.137.7 / v1.139.0-dev
    Kubernetes                                 v1.34.2
    Go                                         1.24.6 / 1.25.0


D. Repository Locations

    Repository                                  Purpose
    ------------------------------------------  -----------------------------------
    gardener-extension-f5                        F5 LB extension
    gardener-extension-provider-airtelcloud      Airtel Cloud IaaS provider
    gardener-extension-tcpwave                   TCPWave DNS extension
    gardener-extension-os-ubuntu                 Ubuntu OS extension
    gardener-netapp-extension-shivanshi          NetApp storage extension


E. Key Commands Reference

    # Check Seed health
    kubectl get seed -o wide

    # Check extension installation
    kubectl get controllerinstallation | grep -i airtelcloud
    kubectl get controllerinstallation | grep -i f5

    # Check per-Shoot F5 config
    kubectl -n shoot--<project>--<shoot> get extension,f5loadbalancerconfig

    # Check extension pod health
    kubectl get pods -n extension-provider-airtelcloud-<id>
    kubectl get pods -n extension-gardener-extension-f5-<id>

    # Test CMP LBaaS API
    ./scripts/cmp-curl.sh lb-list
    ./scripts/cmp-curl.sh lbsvc-list

    # Option A (L7) verification (Shoot):
    # - Ensure the Ingress controller Service is type=LoadBalancer with the F5 loadBalancerClass
    # - Ensure it gets a VIP in status.loadBalancer.ingress
    kubectl -n <ingress-namespace> get svc -o wide


------------------------------------------------------------------------

This document should be reviewed and approved by:
    - Product Team
    - Platform Engineering
    - Network/Infrastructure Team
    - F5 Administration Team
    - Security Team
