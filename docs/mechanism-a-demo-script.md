# Mechanism A Demo Script — Seed Ingress LB (§4.3)

## What you are showing

> "OpenStack has no built-in cloud LB controller. Vanilla Gardener expects one.
> Our `seed-service-lb-controller` fills this gap — it watches the Istio ingress
> Service and calls CMP LBaaS to allocate a real F5 VIP, exactly like AWS ELB
> does automatically on AWS."

---

## Step 1 — Show the Istio ingress Service (the trigger)

```bash
kubectl -n istio-ingress get svc istio-ingressgateway
```

- `TYPE = LoadBalancer` — gardenlet creates this, expects a VIP to be assigned
- `EXTERNAL-IP = 172.18.255.1` — in this kind demo, assigned by cloud-provider-kind
- On OpenStack this would be `<pending>` until `seed-service-lb-controller` acts


> "This Service is the trigger. On AWS, the cloud LB controller sees this and
> creates an ELB automatically. On OpenStack there is no such controller — so
> our seed-service-lb-controller watches this Service and calls CMP instead."

---

## Step 2 — Show the Seed ingress domain is configured

```bash
kubectl get seed local -o yaml | grep -A10 "ingress:"
```

**Expected output:**
```yaml
  ingress:
    controller:
      kind: nginx
    domain: ingress.local.seed.local.gardener.cloud
```


- `ingress.domain` = the base domain for this Seed's ingress
- All Shoots on this Seed derive their apiserver hostname from this domain
- In this kind demo it is set statically; on a real cloud Seed, gardenlet derives it from `Service.status.loadBalancer.ingress` — the IP our `seed-service-lb-controller` writes

> "This domain is the foundation. gardenlet takes the Istio Service's external IP,
> constructs this ingress domain, and stamps it onto every Shoot's apiserver hostname.
> On OpenStack, our seed-service-lb-controller is the one that writes the IP
> into the Service status — gardenlet takes over from there, identical to AWS."

---

## Step 3 — Show the Shoot's apiserver hostname

```bash
kubectl get shoot -n garden-local local \
  -o jsonpath='{.status.advertisedAddresses}' | python3 -m json.tool
```


- `api.local.local.external.local.gardener.cloud` — derived from Seed ingress IP
- This is what goes into the tenant's kubeconfig

---

## Step 4 — Prove live traffic works through Mechanism A

```bash

# Kill existing port-forward on 9443
kill $(lsof -ti tcp:9443) 2>/dev/null
sleep 1

# Restart
kubectl -n istio-ingress port-forward svc/istio-ingressgateway 9443:443 &
# If not already running, start the port-forward
kubectl -n istio-ingress port-forward svc/istio-ingressgateway 9443:443 &

# Hit the kube-apiserver via the Seed ingress VIP path
# --connect-to routes the TLS SNI hostname through the local port-forward
curl -sk --connect-to api.local.local.external.local.gardener.cloud:443:127.0.0.1:9443 \
  https://api.local.local.external.local.gardener.cloud/livez
```


```json
{
  "kind": "Status",
  "apiVersion": "v1",
  "status": "Failure",
  "message": "Unauthorized",
  "reason": "Unauthorized",
  "code": 401
}
```

- `401 Unauthorized` = kube-apiserver received the request and responded
- This is NOT an error — it means the traffic path works end-to-end
- The apiserver is asking for credentials, exactly like `kubectl` before you configure a kubeconfig
- Traffic path: port-forward → Istio ingress gateway → **SNI routing by TLS hostname** → kube-apiserver pod


> "The Istio gateway reads the TLS SNI hostname from the TLS handshake and routes
> to the correct kube-apiserver — no URL path, no HTTP headers, just the hostname
> in the TLS ClientHello. This is how all Shoots share one VIP: same IP,
> different hostname, Istio separates them. In production the VIP is a real
> F5 IP from CMP, not a kind IP."

---


> "In production, instead of cloud-provider-kind, this controller runs on the
> Seed. It calls CMP LBaaS: creates an LB service → allocates a VIP → creates
> a Virtual Server on F5 with pool members = Seed node IPs + Istio NodePort.
> Then patches the Service status with the real F5 IP. gardenlet picks it up
> automatically — same flow as AWS ELB, just done via CMP."


why we are doing port forwarding

Because 172.18.255.1 (the Istio VIP) is only reachable from inside the kind network bridge . From the VM host, packets to 172.18.255.1 get DNAT'd to 172.18.0.7:30443, but the FORWARD chain drops them before they reach the kind node.

Port-forward is a workaround — it creates a tunnel: 127.0.0.1:9443 on the VM → directly into the kind cluster via the Kubernetes API, bypassing all networking. curl then hits 127.0.0.1:9443 which arrives at the Istio pod.
