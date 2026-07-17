#!/usr/bin/env bash
set -euo pipefail

# Demo dry-run for gardener-extension-f5 (Feb 24, 2026)
# Focus: Seed-side reconciliation + CIS deployment + CIS→BIG-IP mgmt/AS3 connectivity.
# Non-goals: control-plane VIP load balancing verification.

log() { printf '\n==> %s\n' "$*"; }

SEED_CTX="${SEED_CTX:-$(kubectl config current-context 2>/dev/null || true)}"
if [[ -z "$SEED_CTX" ]]; then
  echo "ERROR: cannot determine SEED_CTX (set SEED_CTX or configure kubectl context)" >&2
  exit 1
fi

log "Using seed context: $SEED_CTX"

# Discover shoot technical namespace.
SHOOT_TECH_NS="${SHOOT_TECH_NS:-}"
if [[ -z "$SHOOT_TECH_NS" ]]; then
  mapfile -t shoot_namespaces < <(kubectl --context "$SEED_CTX" get ns -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | grep '^shoot--' || true)
  if [[ ${#shoot_namespaces[@]} -eq 0 ]]; then
    echo "ERROR: no shoot technical namespace found (no namespace starting with 'shoot--')" >&2
    exit 1
  fi
  if [[ ${#shoot_namespaces[@]} -gt 1 ]]; then
    echo "ERROR: multiple shoot namespaces found; set SHOOT_TECH_NS explicitly:" >&2
    printf '  - %s\n' "${shoot_namespaces[@]}" >&2
    exit 1
  fi
  SHOOT_TECH_NS="${shoot_namespaces[0]}"
fi

log "Shoot technical namespace: $SHOOT_TECH_NS"

log "Seed-side objects (Extension + F5LoadBalancerConfig)"
kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" get extension || true
kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" get f5loadbalancerconfig f5 2>/dev/null || kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" get f5loadbalancerconfig || true

BIGIP_URL="$(kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" get f5loadbalancerconfig f5 -o jsonpath='{.spec.cis.bigipUrl}' 2>/dev/null || true)"
PARTITION="$(kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" get f5loadbalancerconfig f5 -o jsonpath='{.spec.cis.partition}' 2>/dev/null || true)"
EXTRA_ARGS="$(kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" get f5loadbalancerconfig f5 -o jsonpath='{.spec.cis.extraArgs}' 2>/dev/null || true)"

log "F5LoadBalancerConfig summary"
echo "BIG-IP URL: ${BIGIP_URL:-<missing>}"
echo "Partition: ${PARTITION:-<missing>}"
echo "CIS extraArgs: ${EXTRA_ARGS:-<missing>}"

log "Provider-local reachability prerequisite (Seed NetworkPolicy allow to BIG-IP mgmt/443)"
if [[ -f deploy/kind/allow-shoot-to-bigip-mgmt.networkpolicy.yaml ]]; then
  kubectl --context "$SEED_CTX" apply -f deploy/kind/allow-shoot-to-bigip-mgmt.networkpolicy.yaml
else
  echo "WARN: deploy/kind/allow-shoot-to-bigip-mgmt.networkpolicy.yaml not found; skipping apply" >&2
fi

# Port-forward the shoot kube-apiserver (service in shoot technical namespace) to localhost.
PF_PORT="${PF_PORT:-7443}"
PF_LOG="/tmp/shoot-apiserver-portforward.log"

log "Setting up port-forward: svc/kube-apiserver (ns=$SHOOT_TECH_NS) -> 127.0.0.1:${PF_PORT}"

cleanup() {
  if [[ -n "${PF_PID:-}" ]]; then
    kill "$PF_PID" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# Kill any existing port-forward on same service/port (best-effort).
pkill -f "kubectl .*--context ${SEED_CTX} .* -n ${SHOOT_TECH_NS} port-forward svc/kube-apiserver" >/dev/null 2>&1 || true

nohup kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" port-forward svc/kube-apiserver "${PF_PORT}:443" >"$PF_LOG" 2>&1 &
PF_PID=$!

for _ in {1..30}; do
  if grep -q 'Forwarding from 127.0.0.1' "$PF_LOG"; then
    break
  fi
  sleep 0.2
done

if ! grep -q 'Forwarding from 127.0.0.1' "$PF_LOG"; then
  echo "ERROR: port-forward did not become ready" >&2
  sed -n '1,60p' "$PF_LOG" >&2 || true
  exit 1
fi

# Pick a kubeconfig secret that works from the VM (token embedded or certs) and has enough RBAC.
log "Selecting a Shoot kubeconfig secret for demo access"

candidates=("gardener-internal" "gardener" "shoot-access-dependency-watchdog-probe")
KCFG=""
for secret in "${candidates[@]}"; do
  if ! kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" get secret "$secret" >/dev/null 2>&1; then
    continue
  fi
  if ! kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" get secret "$secret" -o jsonpath='{.data.kubeconfig}' 2>/dev/null | grep -q .; then
    continue
  fi

  raw="/tmp/${secret}.kubeconfig"
  pf="/tmp/${secret}.pf.kubeconfig"
  kubectl --context "$SEED_CTX" -n "$SHOOT_TECH_NS" get secret "$secret" -o jsonpath='{.data.kubeconfig}' | base64 -d > "$raw"

  # Reject kubeconfigs that reference tokenFile (in-cluster mount).
  if grep -q 'tokenFile:' "$raw"; then
    continue
  fi

  # Rewrite *any* server: https://... to localhost port-forward.
  sed -E "s#server: https://[^ ]+#server: https://127.0.0.1:${PF_PORT}#" "$raw" > "$pf"

  # Check minimal RBAC for demo (namespaced reads + logs).
  can_pods=$(kubectl --kubeconfig "$pf" auth can-i list pods -n f5-cis-system 2>/dev/null || echo no)
  can_logs=$(kubectl --kubeconfig "$pf" auth can-i get pods/log -n f5-cis-system 2>/dev/null || echo no)
  if [[ "$can_pods" == "yes" && "$can_logs" == "yes" ]]; then
    KCFG="$pf"
    echo "Selected kubeconfig secret: $secret"
    break
  fi

done

if [[ -z "$KCFG" ]]; then
  echo "ERROR: could not find a usable shoot kubeconfig with enough RBAC to read CIS logs." >&2
  echo "Tried secrets: ${candidates[*]}" >&2
  echo "Next: create a shoot-access kubeconfig secret with broader RBAC, or run demo from inside the Seed cluster network." >&2
  exit 2
fi

log "Shoot-side CIS resources (application-plane wiring)"
kubectl --kubeconfig "$KCFG" get ns f5-cis-system
kubectl --kubeconfig "$KCFG" -n f5-cis-system get deploy,pod -o wide

log "CIS deployment args (verify bigipUrl/partition + insecure workaround)"
kubectl --kubeconfig "$KCFG" -n f5-cis-system get deploy f5-cis -o jsonpath='{.spec.template.spec.containers[0].args}'; echo

log "CIS logs (tail)"
kubectl --kubeconfig "$KCFG" -n f5-cis-system logs deploy/f5-cis --tail=120

# Prove BIG-IP mgmt/AS3 auth from inside CIS.
if [[ -n "$BIGIP_URL" ]]; then
  host="$(python3 - <<PY
import sys, urllib.parse
u=sys.argv[1]
print(urllib.parse.urlparse(u).hostname or u)
PY
"$BIGIP_URL")"

  log "CIS → BIG-IP mgmt/AS3 connectivity check (expected http_code=200)"
  kubectl --kubeconfig "$KCFG" -n f5-cis-system exec deploy/f5-cis -- sh -lc "\
    curl -ksS -u \"\${BIGIP_USERNAME}:\${BIGIP_PASSWORD}\" \
      https://${host}/mgmt/shared/appsvcs/info \
      -o /tmp/as3.json -w 'http_code=%{http_code}\n' && head -c 200 /tmp/as3.json && echo"
else
  echo "WARN: BIG-IP URL not found in F5LoadBalancerConfig.spec.cis.bigipUrl; skipping AS3 connectivity check" >&2
fi

log "Done. For the live demo, narrate: Seed reconciles Extension → deploys CIS → CIS can reach BIG-IP mgmt/AS3."
