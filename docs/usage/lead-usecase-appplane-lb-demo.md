# Lead demo: application-plane load balancing (staging workaround)

This runbook demonstrates the lead’s requirement:
- Deploy an app with **3+ replicas**.
- Expose it via **Service type=LoadBalancer**.
- Assign a manual VIP (for the demo).
- Access the VIP repeatedly and observe **requests routed to random replicas**.

## What we achieved
- A repeatable end-to-end demo that shows real load balancing by returning `pod=<name> ip=<podIP>` on every request.
- VIP assignment is driven by a Kubernetes Service annotation and mirrored into `status.loadBalancer` by the shoot-side bridge.
- BIG-IP objects (VS+pool) are created via **AS3** (API automation), without using the BIG-IP UI.

## Important staging constraint
In this environment, BIG-IP **cannot reach** the Shoot’s node/pod networks. That means “normal” CIS/NodePort backends won’t work.

Workaround used in this demo:
- BIG-IP pool members are `VM_IP:port`.
- The VM runs `kubectl port-forward` to each pod, so each forwarded port maps to a single replica.

## Prerequisites
- A Shoot cluster where:
  - F5 CIS is running in `f5-cis-system` (label `app=f5-cis`).
  - The `svc-lb-bridge` is running (it creates/updates the Ingress and Service status).
- A VM that:
  - BIG-IP can reach (used as pool-member address).
  - Can reach the Shoot API (has a working kubeconfig).
- BIG-IP AS3 installed/enabled (AS3 declare endpoint works).
- A routable VIP for the demo (example used: `100.72.200.20`).

## Run the demo
1) Ensure you have a Shoot kubeconfig (example path used by the script):
- `export KUBECONFIG_SHOOT=/tmp/shoot-pf.kubeconfig`

2) Run the script (defaults match the known-good staging values):
- `scripts/demo-lead-appplane-lb-vm-as3.sh --apply`

Optional overrides:
- `VIP=100.72.200.20 VS_PORT=8085 N_REPLICAS=3 VM_IP=100.72.44.199 VM_PORT_BASE=7450 BIGIP_MGMT=100.72.44.146 scripts/demo-lead-appplane-lb-vm-as3.sh --apply`

## Verify success
From any host that can reach the VIP:
- `curl http://$VIP:$VS_PORT/`

Expected:
- The response line changes across calls, e.g. `pod=... ip=...` varies between replicas.

Also useful:
- `kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n demo-lead-lb logs deploy/pod-ident --tail=50`

## Cleanup
- `scripts/demo-lead-appplane-lb-vm-as3.sh --cleanup`

This will:
- Delete the demo namespace (`demo-lead-lb`).
- Delete the AS3 tenant used by the demo (`demo_lead_lb`).
- Kill port-forward processes started by the demo script (tracked via `/tmp/pf-demo-lead-lb.pids`).
