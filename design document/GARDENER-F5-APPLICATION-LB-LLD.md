# Low-Level Design: Gardener F5 Application Load Balancer Extension

## 1. Purpose

This document defines the target architecture and low-level design for a Gardener extension that provides application load balancing through CMP/F5.

The design is written as a greenfield system specification. It describes how the solution shall be built, how its components shall interact, and how Kubernetes resources shall be reconciled into CMP/F5 load-balancer resources.

The AWS Load Balancer Controller is used only as an architectural reference for proven controller patterns such as:

- event-driven reconciliation
- internal desired-state models
- resource-specific deployers
- explicit ownership
- safe finalization
- typed cloud API clients
- idempotent convergence

The solution remains specific to Gardener, Kubernetes, CMP, and F5.

---

## 2. Goals

The system shall:

1. Provision application load balancers for Kubernetes `Service` resources of type `LoadBalancer`.
2. Support `Ingress` resources through an F5-specific Ingress class.
3. Translate Kubernetes state into CMP/F5 load-balancer resources.
4. Reconcile continuously until actual CMP state matches desired Kubernetes state.
5. Handle application scaling, node replacement, configuration changes, and deletion safely.
6. Avoid duplicate or orphaned CMP resources.
7. Reconcile backend membership without recreating stable frontend resources.
8. Expose clear Kubernetes status, events, metrics, and logs.
9. Support high availability of the controller.
10. Use typed and testable internal contracts.

---

## 3. Non-Goals

The initial design does not aim to:

- manage worker VM lifecycle
- delete or modify compute instances
- replace Gardener networking
- replace kube-proxy or the cluster CNI
- implement every Kubernetes Ingress feature without validating CMP capabilities
- provide direct pod-IP target mode unless network reachability and CMP support are confirmed
- expose raw CMP API concepts directly to application users
- use Kubernetes CRDs as a storage mechanism for every internal object

---

## 4. System Context

The controller runs inside each Shoot cluster as a managed system component.

```text
Garden / Seed control plane
        │
        │ deploys and configures
        ▼
Shoot cluster
        │
        ├── F5 application LB controller
        ├── Services
        ├── Ingresses
        ├── EndpointSlices
        ├── Nodes
        └── application Pods
                │
                │ controller calls
                ▼
             CMP APIs
                │
                ▼
              F5 LB
```

The controller uses the Shoot Kubernetes API for desired application state and CMP APIs for external load-balancer state.

---

## 5. High-Level Architecture

```text
Kubernetes API
    │
    ├── Service
    ├── Ingress
    ├── EndpointSlice
    └── Node
    │
    ▼
Controller Layer
    ├── Service Controller
    └── Ingress Controller
    │
    ▼
Model Builder Layer
    ├── Service Model Builder
    └── Ingress Model Builder
    │
    ▼
Internal Desired-State Model
    ├── Load Balancer
    ├── VIP
    ├── Virtual Server
    ├── Pool
    └── Backend Members
    │
    ▼
Deployment Layer
    ├── LB Service Manager
    ├── VIP Manager
    ├── Virtual Server Manager
    ├── Pool Manager
    └── Backend Member Manager
    │
    ▼
Typed CMP Clients
    ├── LBaaS Client
    ├── Compute Client
    └── Network Client
    │
    ▼
CMP / F5
```

Supporting components:

```text
Configuration
Annotations
Ownership
Finalizers
Backend Resolution
Status Writers
NetworkPolicy Reconciler
Metrics
Events
Logging
Retry Policy
```

---

## 6. Component Responsibilities

### 6.1 Controller Manager

The controller manager shall:

- initialize the Kubernetes client
- initialize CMP clients
- register controllers
- enable leader election
- expose health, readiness, and metrics endpoints
- handle graceful shutdown
- inject immutable runtime configuration

The controller manager shall not contain load-balancer business logic.

### 6.2 Service Controller

The Service controller shall:

- watch `Service` resources
- watch related `EndpointSlice` and `Node` changes
- determine whether a Service belongs to this controller
- manage the Service finalizer
- invoke the Service model builder
- invoke the deployment layer
- invoke the Service status writer
- emit events and metrics
- coordinate generated NetworkPolicy reconciliation when enabled

The Service controller shall not construct HTTP requests or parse CMP responses.

### 6.3 Ingress Controller

The Ingress controller shall:

- watch `Ingress` resources owned by the F5 Ingress class
- watch referenced backend Services
- watch related EndpointSlices and Nodes
- manage the Ingress finalizer
- invoke the Ingress model builder
- invoke the shared deployment layer
- update Ingress status
- emit events and metrics

The Ingress controller shall support multiple hosts, paths, and backend Services when CMP supports the required routing model.

Unsupported semantics shall fail validation explicitly.

### 6.4 Model Builders

Model builders shall convert Kubernetes resources into a typed internal resource graph.

They shall:

- parse Kubernetes-native fields
- parse supported annotations
- validate combinations
- discover backends
- resolve CMP backend identity
- generate deterministic resource names
- attach ownership metadata
- produce desired resources and dependencies

Model builders shall not perform mutating CMP operations.

### 6.5 Deployment Layer

The deployment layer shall:

- discover actual CMP resources
- verify ownership
- compare desired and actual state
- create missing resources
- update changed resources
- delete obsolete owned resources
- wait for asynchronous readiness
- return typed observed state

The deployment layer shall be shared by Service and Ingress controllers.

### 6.6 CMP Client Layer

The CMP client layer shall:

- own HTTP transport
- own authentication
- own URL construction
- encode typed requests
- decode typed responses
- classify HTTP errors
- implement client-side rate limiting
- expose request IDs for diagnostics
- support context cancellation and timeouts

Raw JSON shall not escape the CMP client package.

### 6.7 Backend Resolver

The backend resolver shall map Kubernetes nodes to CMP network identities.

For node-based backends, it shall resolve:

```text
Kubernetes Node
    ├── provider ID
    └── Internal IP
          ↓
CMP compute/network lookup
          ↓
resource ID
resource type
resource IP
backend port ID
```

The controller shall never invent backend resource IDs or network port IDs.

### 6.8 Status Writers

Status writers shall update only Kubernetes status subresources.

They shall:

- write the allocated VIP
- avoid unnecessary writes
- expose frontend readiness
- preserve unrelated status fields
- use optimistic concurrency-safe patches

### 6.9 Ownership Provider

The ownership provider shall generate and validate ownership metadata for every managed CMP resource.

Ownership shall include:

- controller identity
- Shoot or cluster identity
- source Kubernetes kind
- source namespace
- source name
- source UID
- resource role
- optional shared frontend group

Resource names alone shall never be treated as sufficient proof of ownership.

---

## 7. Resource Model

### 7.1 Resource Relationship

```text
LB Service
    ↓
VIP
    ↓
Virtual Server
    ↓
Pool
    ↓
Backend Members
```

### 7.2 LB Service

Represents the CMP load-balancer service container.

Desired attributes include:

- name
- description
- flavor ID
- network ID
- VPC ID
- VPC name
- ownership labels
- visibility or scheme when supported

Observed attributes include:

- CMP resource ID
- provisioning state
- failure reason
- timestamps when available

### 7.3 VIP

Represents the allocated frontend address.

Desired attributes include:

- parent LB Service
- optional requested address where supported
- ownership metadata

Observed attributes include:

- VIP resource ID
- allocated IP address
- readiness state

### 7.4 Virtual Server

Represents a frontend listener.

Desired attributes include:

- parent LB Service
- VIP reference
- protocol
- frontend port
- routing policy
- allowed source CIDRs
- persistence
- connection draining
- health-check association
- ownership metadata

Observed attributes include:

- Virtual Server ID
- readiness state
- active configuration

### 7.5 Pool

Represents a backend target group.

Desired attributes include:

- algorithm
- health monitor
- ownership metadata
- association with a Virtual Server

Observed attributes include:

- pool ID
- health state

### 7.6 Backend Member

Represents one load-balancer target.

For node-based mode:

```text
resource_id     = CMP compute/VM identifier
resource_type   = compute
resource_ip     = Kubernetes Node InternalIP
backend_port_id = CMP network-port identifier
port            = Kubernetes NodePort
weight          = computed or configured weight
```

Deleting a backend member removes only the pool membership. It does not delete the VM.

---

## 8. Kubernetes-to-CMP Mapping

### 8.1 Service Mapping

```text
Kubernetes Service
    ↓
LB Service + VIP

Each Service port
    ↓
Virtual Server + Pool

Eligible worker node
    ↓
Backend Member using NodeIP:NodePort
```

Example:

```text
Service:
  port: 443
  nodePort: 32443

Node:
  InternalIP: 10.10.1.21

CMP backend member:
  resource_ip: 10.10.1.21
  port: 32443
```

### 8.2 Ingress Mapping

```text
Ingress frontend
    ↓
VIP + HTTP/HTTPS Virtual Server

Host/path rule
    ↓
routing rule

Backend Service/port
    ↓
Pool

Eligible nodes for that backend Service
    ↓
Backend Members
```

If CMP cannot model host/path rules directly, the design shall define a supported subset and reject unsupported Ingresses.

---

## 9. Desired-State Model

The controller shall build an internal resource graph representing what must exist.

The model shall contain:

```text
Stack
  ├── LoadBalancer
  ├── VIP
  ├── VirtualServer(s)
  ├── Pool(s)
  └── BackendMember(s)
```

Each resource shall have:

```text
Metadata
Desired Spec
Observed Status
Dependencies
Ownership
```

The internal model is not a Kubernetes API and is not stored in etcd.

---

## 10. Reconciliation Model

### 10.1 General Algorithm

```text
Receive event
    ↓
Fetch latest Kubernetes object
    ↓
Validate ownership and configuration
    ↓
Build desired resource graph
    ↓
Discover actual CMP state
    ↓
Verify ownership
    ↓
Compute required resource-specific changes
    ↓
Apply changes in dependency order
    ↓
Wait for required readiness
    ↓
Update Kubernetes status
```

### 10.2 Resource Ordering

Creation order:

```text
LB Service
    ↓
VIP
    ↓
Virtual Server
    ↓
Pool
    ↓
Backend Members
```

Deletion order:

```text
Backend Members
    ↓
Pool
    ↓
Virtual Server
    ↓
VIP
    ↓
LB Service
```

### 10.3 Desired Versus Actual Reconciliation

Resource managers shall perform the minimum required changes.

Example:

```text
Desired pool members:
  node-a:32443
  node-b:32443
  node-c:32443

Actual pool members:
  node-a:32443
  node-b:32443
  node-old:32443
```

Required actions:

```text
keep node-a
keep node-b
add node-c
remove node-old from the pool
```

The Virtual Server shall remain unchanged because its frontend configuration did not change.

---

## 11. End-to-End Flows

### 11.1 Application Load Balancer Creation

```text
User creates Service type=LoadBalancer
    ↓
Service controller receives event
    ↓
Service ownership and configuration validated
    ↓
EndpointSlices and Nodes discovered
    ↓
CMP backend identity resolved
    ↓
Desired resource graph built
    ↓
Finalizer added
    ↓
LB Service created
    ↓
LB Service readiness confirmed
    ↓
VIP created and allocated
    ↓
Virtual Server and Pool created
    ↓
Backend Members added
    ↓
Frontend readiness confirmed
    ↓
Service status updated with VIP
```

### 11.2 Pod Scale-Up

```text
Application scales up
    ↓
EndpointSlice changes
    ↓
Controller identifies affected Service
    ↓
Desired backend set recalculated
    ↓
Existing frontend resources remain unchanged
    ↓
Member weights or membership updated if needed
```

### 11.3 Pod Scale-Down

```text
Pod becomes unready or is removed
    ↓
EndpointSlice changes
    ↓
Desired backend eligibility recalculated
    ↓
Member removed only when the node no longer has an eligible endpoint
    or weight adjusted according to policy
```

### 11.4 Worker Node Replacement

```text
Old node leaves
New node joins
    ↓
Node and EndpointSlice events received
    ↓
New node CMP identity resolved
    ↓
New pool member added
    ↓
Old pool member removed
```

No VM deletion is performed.

The VIP and Virtual Server remain active.

### 11.5 Service Port Change

```text
Service port configuration changes
    ↓
Desired Virtual Servers and Pools recalculated
    ↓
Changed frontend reconciled
    ↓
Obsolete owned frontend resources deleted
    ↓
Unchanged ports remain untouched
```

### 11.6 Service Deletion

```text
Service receives DeletionTimestamp
    ↓
Finalizer prevents immediate removal
    ↓
All owned backend members deleted
    ↓
Owned pools deleted
    ↓
Owned Virtual Servers deleted
    ↓
VIP deleted when no longer shared
    ↓
LB Service deleted when no longer shared
    ↓
Generated Kubernetes resources deleted
    ↓
Finalizer removed
```

### 11.7 Controller Restart

```text
Controller restarts
    ↓
Kubernetes objects are re-listed
    ↓
CMP resources rediscovered through IDs and ownership
    ↓
Reconciliation resumes from actual state
```

No in-memory state shall be required for correctness.

---

## 12. Backend Selection

A node is eligible when:

- the Node is Ready
- the Service has at least one ready endpoint associated with that Node
- the Node has a usable InternalIP
- CMP identity resolution succeeds
- the Service traffic policy permits the node

The weighting strategy shall be configurable.

Initial supported policy:

```text
equal weight per eligible node
```

Optional future policy:

```text
weight proportional to ready endpoints per node
```

The weighting policy shall not be hard-coded into the CMP client.

---

## 13. Ownership and Naming

### 13.1 Naming

Names shall be deterministic, readable, and collision-resistant.

A name may contain:

- cluster identifier
- namespace
- source object name
- frontend port
- protocol
- shared group identifier

Names shall be sanitized to CMP constraints.

### 13.2 Ownership Metadata

Recommended labels:

```text
managed-by=gardener-extension-f5
cluster-uid=<shoot-uid>
source-kind=Service
source-namespace=<namespace>
source-name=<name>
source-uid=<uid>
resource-role=<lb|vip|virtual-server|pool|member>
shared-group=<optional>
```

### 13.3 Adoption Rules

A resource may be reused only when:

- its ownership metadata matches
- its tenant/project context matches
- its VPC/network context matches
- its resource role matches
- its shared group matches when applicable

A matching name without ownership is not enough.

---

## 14. Shared VIP Design

A shared VIP shall be represented by an explicit group identity.

Services may share an LB Service and VIP only when they have compatible:

- tenant and project
- VPC and network
- visibility
- protocol model
- ownership domain
- shared group identifier

Each Service shall retain independent Virtual Servers and Pools.

Shared resources shall not be deleted while any valid binding still references them.

A dedicated binding resource may be introduced if shared ownership cannot be represented safely through labels and Kubernetes state alone.

---

## 15. Status and Conditions

### 15.1 Service Status

The controller shall update:

```text
Service.status.loadBalancer.ingress
```

only after the frontend is ready.

The allocated VIP shall be published as an IP address.

### 15.2 Ingress Status

The controller shall update:

```text
Ingress.status.loadBalancer.ingress
```

after required HTTP/HTTPS frontends are ready.

### 15.3 Conditions and Events

Relevant states shall be exposed through Kubernetes Events and, where supported, Conditions:

- Accepted
- Provisioning
- Ready
- Degraded
- InvalidConfiguration
- BackendResolutionFailed
- CMPUnavailable
- RateLimited
- CleanupFailed

---

## 16. Failure Handling

### 16.1 Error Categories

The system shall classify:

- permanent validation errors
- temporary transport errors
- authentication and authorization errors
- not-found responses
- conflict responses
- rate limiting
- asynchronous operation not ready
- dependency not ready
- malformed CMP responses

### 16.2 Retry Behavior

```text
Validation error:
  wait for Kubernetes object change

Rate limited:
  requeue using Retry-After

Temporary server or network failure:
  return error and use controller-runtime backoff

Asynchronous provisioning:
  requeue after a bounded polling interval

Unauthorized:
  expose event and metric; avoid rapid retry
```

### 16.3 Partial Failure

The system shall tolerate:

- LB Service created but response lost
- VIP created but status patch failed
- Virtual Server created before controller restart
- one backend operation failing while others succeed

The next reconciliation shall rediscover actual state and continue safely.

---

## 17. Idempotency and Concurrency

Every operation shall be idempotent.

Repeated reconciliation shall not create duplicates.

The controller shall:

- use leader election
- use optimistic Kubernetes patches
- verify CMP ownership before mutation
- tolerate duplicate events
- tolerate stale cached objects
- handle create conflicts by rediscovering actual state
- use stable idempotency keys where CMP supports them

Only the elected leader shall mutate CMP state.

---

## 18. Observability

### 18.1 Metrics

At minimum:

- reconciliation count
- reconciliation errors
- reconciliation duration
- managed Service count
- managed Ingress count
- CMP request count
- CMP request latency
- CMP response status
- rate-limit count
- backend resolution failures
- provisioning duration
- cleanup duration

### 18.2 Logs

Logs shall be structured and include:

- namespace/name
- UID
- reconciliation ID
- CMP request ID
- CMP resource ID
- resource role
- operation
- duration
- error category

Credentials and tokens shall never be logged.

### 18.3 Events

Events shall identify user-visible lifecycle transitions and failures.

Examples:

```text
ProvisioningLoadBalancer
AllocatedVIP
LoadBalancerReady
BackendResolutionFailed
CMPRequestFailed
DeletingLoadBalancer
LoadBalancerDeleted
```

---

## 19. Security

The controller shall:

- use a dedicated ServiceAccount
- use least-privilege RBAC
- read credentials from a Secret
- use tenant/project-scoped CMP credentials
- avoid broad administrator credentials
- validate TLS using a configured CA bundle
- permit insecure TLS only as an explicit non-production option
- run as non-root
- use a read-only filesystem where possible
- restrict egress to DNS, Kubernetes API, CMP, and monitoring endpoints
- avoid exposing credentials through command-line arguments or logs

---

## 20. Runtime Deployment

The controller shall run as a Kubernetes Deployment inside the Shoot.

Recommended production settings:

```text
replicas: 2
leader election: enabled
PodDisruptionBudget: enabled
topology spread: enabled
liveness probe: enabled
readiness probe: enabled
resource requests and limits: defined
graceful shutdown: enabled
```

It shall not run as a DaemonSet and shall not be pinned permanently to one worker node.

---

## 21. Package-Level Design

The implementation shall use responsibility-oriented packages.

```text
cmd/
  executable startup and dependency wiring

controllers/
  Kubernetes reconciliation entry points

pkg/model/
  internal desired and observed resource models

pkg/service/
  Service-specific model building

pkg/ingress/
  Ingress-specific model building

pkg/deploy/
  desired-to-actual CMP reconciliation

pkg/cmp/
  typed CMP API clients and transport

pkg/backend/
  EndpointSlice, Node, compute, and network resolution

pkg/annotations/
  annotation parsing and validation

pkg/status/
  Kubernetes status writers

pkg/finalizers/
  finalizer lifecycle

pkg/networkpolicy/
  generated NetworkPolicy reconciliation

pkg/metrics/
  Prometheus metrics
```

The Kubernetes CRD API remains under:

```text
pkg/apis/f5/v1alpha1
```

Internal CMP models shall not be placed in the CRD package.

---

## 22. Testing Strategy

### 22.1 Unit Tests

Cover:

- annotation parsing
- Service model building
- Ingress model building
- naming and ownership
- backend selection
- backend identity resolution
- resource-specific diffing
- status updates
- error classification

### 22.2 CMP Client Tests

Use an HTTP test server to verify:

- paths
- headers
- encoding
- typed response parsing
- status handling
- rate-limit handling
- malformed response handling

### 22.3 Reconciliation Tests

Use fake Kubernetes clients and fake CMP managers to verify:

- create
- update
- delete
- restart recovery
- multi-port Service
- node replacement
- shared VIP
- partial failure
- idempotency

### 22.4 Integration Tests

Validate against a CMP test environment:

- real LB Service creation
- VIP allocation
- Virtual Server readiness
- backend registration
- backend removal
- cleanup order
- authentication expiry
- rate limiting

---

## 23. Design Invariants

The following must always hold:

1. A Kubernetes object can be reconciled repeatedly without creating duplicates.
2. A controller restart does not lose resource ownership.
3. A backend member deletion never deletes a compute VM.
4. A backend change does not recreate an unchanged Virtual Server.
5. A resource is deleted only when ownership is proven.
6. A VIP is not published before readiness.
7. Multi-port Services maintain independent frontend state.
8. Shared resources are not deleted while valid references remain.
9. Raw CMP JSON does not reach controller or model-builder layers.
10. CMP resource IDs are never invented.
11. Finalizers are removed only after required cleanup.
12. All external operations are cancellable and bounded by timeouts.

---

## 24. Extension Points

The architecture supports future additions without changing the controller fundamentals:

- direct pod-IP targets
- Gateway API
- certificate management
- advanced health monitors
- WAF policy attachment
- dual-stack VIPs
- multi-AZ load balancers
- global traffic management
- richer shared frontend policies
- custom backend weighting
- per-Service LB flavors
- workload identity for CMP authentication

---

## 25. Summary

The system is designed as a standard Kubernetes cloud-controller integration:

```text
Kubernetes resource
    ↓
Controller
    ↓
Model builder
    ↓
Internal desired-state graph
    ↓
Resource-specific deployment managers
    ↓
Typed CMP clients
    ↓
CMP/F5
    ↓
Observed readiness
    ↓
Kubernetes status
```

The architecture keeps Kubernetes orchestration, desired-state modeling, CMP reconciliation, transport, ownership, and status handling clearly separated.

This separation provides the basis for a production-grade, testable, idempotent, and maintainable Gardener F5 application load-balancer extension.
