#!/usr/bin/env bash
set -euo pipefail

# Demo: Lead use case for app-plane LB with >=3 replicas.
#
# What this does
# - Deploys an HTTP server that returns/logs: pod name + pod IP per request.
# - Creates Service type=LoadBalancer; the svc-lb-bridge assigns EXTERNAL-IP.
# - Creates BIG-IP VS+pool via AS3 (posted from inside the CIS pod).
# - Uses VM port-forwards as BIG-IP pool members (staging workaround when BIG-IP cannot
#   reach shoot node/pod networks).
#
# Success criteria
# - Opening http://$VIP:$VS_PORT/ routes to random-ish replicas (pod name/IP changes).

KUBECONFIG_SHOOT=${KUBECONFIG_SHOOT:-/tmp/shoot-pf.kubeconfig}
MANIFEST=${MANIFEST:-config/samples/lead-usecase-appplane-lb.yaml}
VIP=${VIP:-100.72.200.20}
VS_PORT=${VS_PORT:-8085}
N_REPLICAS=${N_REPLICAS:-3}
LB_CLASS=${LB_CLASS:-f5.extensions.gardener.cloud/bigip}
VM_IP=${VM_IP:-100.72.44.199}
VM_PORT_BASE=${VM_PORT_BASE:-7450}
BIGIP_MGMT=${BIGIP_MGMT:-100.72.44.146}

NS=${NS:-demo-lead-lb}
DEPLOY=${DEPLOY:-pod-ident}
SVC=${SVC:-pod-ident-lb}
ING=${ING:-f5-svc-lb-pod-ident-lb}

PID_FILE=${PID_FILE:-/tmp/pf-${NS}.pids}

usage() {
  cat <<'EOF'
Usage:
  scripts/demo-lead-appplane-lb-vm-as3.sh [--apply] [--cleanup] [--help]

Required context:
  - KUBECONFIG_SHOOT points to the Shoot cluster kubeconfig
  - BIG-IP is reachable at https://$BIGIP_MGMT
  - BIG-IP can reach $VM_IP (pool members are $VM_IP:$VM_PORT_BASE..)
  - AS3 is installed/enabled on BIG-IP

Key environment variables:
  KUBECONFIG_SHOOT=/tmp/shoot-pf.kubeconfig
  MANIFEST=config/samples/lead-usecase-appplane-lb.yaml
  VIP=100.72.200.20
  VS_PORT=8085
  N_REPLICAS=3
  VM_IP=100.72.44.199
  VM_PORT_BASE=7450
  BIGIP_MGMT=100.72.44.146
  NS=demo-lead-lb

Cleanup:
  --cleanup deletes the demo namespace, removes the AS3 tenant, and kills any port-forwards
  started by this script (tracked via $PID_FILE).
EOF
}

need() { command -v "$1" >/dev/null 2>&1 || { echo "missing required command: $1" >&2; exit 1; }; }
need kubectl
need curl

step_n=0
step() {
  step_n=$((step_n + 1))
  echo
  echo "== Step ${step_n}: $*"
}

preflight() {
  [[ -f "$KUBECONFIG_SHOOT" ]] || { echo "KUBECONFIG_SHOOT not found: $KUBECONFIG_SHOOT" >&2; exit 1; }
  [[ -f "$MANIFEST" ]] || { echo "MANIFEST not found: $MANIFEST" >&2; exit 1; }
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" version --short >/dev/null
}

apply_manifest() {
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" apply -f "$MANIFEST"
  # ensure replicas match N_REPLICAS
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" scale deploy "$DEPLOY" --replicas="$N_REPLICAS"
}

wait_pods_ready() {
  echo "Waiting for $N_REPLICAS pods to be Ready in $NS..."
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" rollout status deploy/"$DEPLOY" --timeout=180s
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" get pods -l app=pod-ident -o wide
}

patch_service() {
  # enforce VIP + service port
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" annotate svc "$SVC" cis.f5.com/ip="$VIP" --overwrite
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" patch svc "$SVC" --type='merge' -p "{\"spec\":{\"loadBalancerClass\":\"${LB_CLASS}\"}}" >/dev/null
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" patch svc "$SVC" --type='json' -p="[{\"op\":\"replace\",\"path\":\"/spec/ports/0/port\",\"value\":${VS_PORT}}]" >/dev/null
}

wait_bridge_reconcile() {
  echo "Waiting for bridge to reconcile Ingress annotations..."
  for i in $(seq 1 60); do
    ip=$(kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" get ingress "$ING" -o jsonpath='{.metadata.annotations.virtual-server\.f5\.com/ip}' 2>/dev/null || true)
    p=$(kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" get ingress "$ING" -o jsonpath='{.metadata.annotations.virtual-server\.f5\.com/http-port}' 2>/dev/null || true)
    if [[ "$ip" == "$VIP" && "$p" == "$VS_PORT" ]]; then
      echo "Ingress ready: ip=$ip port=$p"
      return 0
    fi
    if [[ $((i % 10)) -eq 0 ]]; then
      echo "still waiting... (ip=$ip port=$p)"
    fi
    sleep 1
  done
  echo "Timed out waiting for Ingress reconcile" >&2
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" get svc "$SVC" -o wide || true
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" get ingress "$ING" -o yaml | sed -n '1,220p' || true
  return 1
}

start_portforwards() {
  local pods
  mapfile -t pods < <(kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" get pods -l app=pod-ident -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}')

  if [[ ${#pods[@]} -lt $N_REPLICAS ]]; then
    echo "Expected $N_REPLICAS pods, found ${#pods[@]}" >&2
    exit 1
  fi

  : >"$PID_FILE"
  echo "Starting $N_REPLICAS VM port-forwards on $VM_IP (ports $VM_PORT_BASE..)"

  for idx in $(seq 0 $((N_REPLICAS-1))); do
    local pod=${pods[$idx]}
    local vmport=$((VM_PORT_BASE + idx))

    # if already listening, skip
    if ss -lnt 2>/dev/null | awk '{print $4}' | grep -q ":${vmport}$"; then
      echo "Port $vmport already listening; skipping"
      continue
    fi

    echo "port-forward $vmport -> pod/$pod:8080"
    nohup kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n "$NS" port-forward --address 0.0.0.0 pod/"$pod" "$vmport":8080 \
      >/tmp/pf-${NS}-${vmport}.log 2>&1 &
    echo $! >>"$PID_FILE"
    sleep 0.5
  done

  echo "Verifying backends via localhost..."
  for idx in $(seq 0 $((N_REPLICAS-1))); do
    local vmport=$((VM_PORT_BASE + idx))
    echo "backend :$vmport => $(curl -fsS --max-time 3 http://127.0.0.1:${vmport}/ | tr -d '\n')"
  done
}

post_as3_declaration() {
  local cis_pod
  cis_pod=$(kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n f5-cis-system get pod -l app=f5-cis -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -z "$cis_pod" ]]; then
    echo "Could not find CIS pod in namespace f5-cis-system (label app=f5-cis)" >&2
    return 1
  fi

  # Generate members list in JSON with N_REPLICAS entries
  local members_json=""
  for idx in $(seq 0 $((N_REPLICAS-1))); do
    local vmport=$((VM_PORT_BASE + idx))
    local entry="{\"servicePort\": ${vmport}, \"serverAddresses\": [\"${VM_IP}\"]}"
    if [[ -z "$members_json" ]]; then
      members_json="$entry"
    else
      members_json="$members_json, $entry"
    fi
  done

  # Tenant/app names are stable so re-running is idempotent.
  local tenant="demo_lead_lb"
  local app="pod_ident_${VS_PORT}"

  echo "Posting AS3 declaration to BIG-IP $BIGIP_MGMT from CIS pod (no UI)..."

  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n f5-cis-system exec "$cis_pod" -c cis -- sh -lc "
    cat > /tmp/as3-lead.json <<'EOF'
{
  \"class\": \"AS3\",
  \"action\": \"deploy\",
  \"persist\": true,
  \"declaration\": {
    \"class\": \"ADC\",
    \"schemaVersion\": \"3.56.0\",
    \"id\": \"gardener-extension-f5-lead-demo-${VS_PORT}\",
    \"label\": \"lead_demo_vm_members\",
    \"${tenant}\": {
      \"class\": \"Tenant\",
      \"${app}\": {
        \"class\": \"Application\",
        \"template\": \"generic\",
        \"svc\": {
          \"class\": \"Service_HTTP\",
          \"virtualAddresses\": [\"${VIP}\"],
          \"virtualPort\": ${VS_PORT},
          \"snat\": \"auto\",
          \"pool\": \"pool\"
        },
        \"pool\": {
          \"class\": \"Pool\",
          \"monitors\": [\"tcp\"],
          \"members\": [ ${members_json} ]
        }
      }
    }
  }
}

delete_as3_tenant() {
  local cis_pod
  cis_pod=$(kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n f5-cis-system get pod -l app=f5-cis -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -z "$cis_pod" ]]; then
    echo "Skipping AS3 cleanup: CIS pod not found" >&2
    return 0
  fi

  local tenant="demo_lead_lb"

  echo "Deleting AS3 tenant '${tenant}' from BIG-IP $BIGIP_MGMT..."
  kubectl --kubeconfig "$KUBECONFIG_SHOOT" -n f5-cis-system exec "$cis_pod" -c cis -- sh -lc "
    cat > /tmp/as3-lead-delete.json <<'EOF'
{
  \"class\": \"AS3\",
  \"action\": \"deploy\",
  \"persist\": true,
  \"declaration\": {
    \"class\": \"ADC\",
    \"schemaVersion\": \"3.56.0\",
    \"id\": \"gardener-extension-f5-lead-demo-delete\",
    \"${tenant}\": {
      \"class\": \"Tenant\",
      \"state\": \"absent\"
    }
  }
}
EOF

    code=\$(curl -k -sS -u \"\$BIGIP_USERNAME:\$BIGIP_PASSWORD\" \
      -H \"Content-Type: application/json\" \
      -o /tmp/as3-lead-delete.resp.json \
      -w \"%{http_code}\" \
      -X POST https://${BIGIP_MGMT}/mgmt/shared/appsvcs/declare \
      --data-binary @/tmp/as3-lead-delete.json || true)

    echo \"AS3_DELETE_HTTP_CODE=\$code\"
    sed -n '1,80p' /tmp/as3-lead-delete.resp.json || true
  "
}

kill_portforwards() {
  if [[ ! -f "$PID_FILE" ]]; then
    echo "No PID file found at $PID_FILE; skipping port-forward cleanup"
    return 0
  fi

  echo "Killing port-forward processes from $PID_FILE..."
  while read -r pid; do
    [[ -n "$pid" ]] || continue
    if kill -0 "$pid" 2>/dev/null; then
      kill "$pid" 2>/dev/null || true
    fi
  done <"$PID_FILE"
}
EOF

    code=\$(curl -k -sS -u \"\$BIGIP_USERNAME:\$BIGIP_PASSWORD\" \
      -H \"Content-Type: application/json\" \
      -o /tmp/as3-lead.resp.json \
      -w \"%{http_code}\" \
      -X POST https://${BIGIP_MGMT}/mgmt/shared/appsvcs/declare \
      --data-binary @/tmp/as3-lead.json || true)

    echo \"AS3_DECLARE_HTTP_CODE=\$code\"
    sed -n '1,80p' /tmp/as3-lead.resp.json || true
  "
}

curl_vip_loop() {
  echo
  echo "SUCCESS CRITERION CHECK: open http://${VIP}:${VS_PORT}/ in a browser"
  echo "Or run curl loop (shows random-ish replica):"
  for i in $(seq 1 20); do
    echo -n "$i: "
    curl -fsS --max-time 5 "http://${VIP}:${VS_PORT}/" | tr -d '\n'
    echo
  done
}

main() {
  local mode="apply"
  case "${1:-}" in
    ""|"--apply") mode="apply" ;;
    "--cleanup") mode="cleanup" ;;
    "-h"|"--help") usage; exit 0 ;;
    *)
      echo "Unknown argument: ${1}" >&2
      usage
      exit 2
      ;;
  esac

  preflight

  if [[ "$mode" == "cleanup" ]]; then
    step "Delete Kubernetes demo resources"
    kubectl --kubeconfig "$KUBECONFIG_SHOOT" delete ns "$NS" --ignore-not-found

    step "Remove BIG-IP config (AS3 tenant)"
    delete_as3_tenant || true

    step "Stop VM port-forwards started by this script"
    kill_portforwards || true
    echo
    echo "Cleanup done."
    return 0
  fi

  step "Apply demo manifest and set replicas"
  apply_manifest

  step "Wait for pods to be Ready"
  wait_pods_ready

  step "Patch Service VIP + port"
  patch_service

  step "Wait for svc-lb-bridge reconcile (Ingress annotations)"
  wait_bridge_reconcile

  step "Start VM port-forwards (pool members)"
  start_portforwards

  step "Deploy BIG-IP Virtual Server + Pool via AS3"
  post_as3_declaration

  step "Verify: curl VIP and see replica changes"
  curl_vip_loop
}

main "$@"
