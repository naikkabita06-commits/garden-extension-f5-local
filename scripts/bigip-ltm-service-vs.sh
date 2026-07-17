#!/usr/bin/env bash
set -euo pipefail

# Creates/updates BIG-IP LTM Pool + members + Virtual Server for a Kubernetes Service.
# This is intended for demos when AS3 is not available (so CIS cannot program BIG-IP).
#
# It discovers:
# - Service port + allocated NodePort
# - Node InternalIP addresses
# And then programs BIG-IP via iControl REST from a temporary in-cluster curl pod.

usage() {
  cat <<'EOF'
Usage:
  SERVICE_NS=<ns> SERVICE_NAME=<name> VIP=<vip> \
  BIGIP_HOST=<mgmt-ip-or-dns> BIGIP_USER=<user> BIGIP_PASS=<pass> \
  [KUBECONFIG=/path/to/kubeconfig] \
  ./scripts/bigip-ltm-service-vs.sh

Required env vars:
  SERVICE_NS     Namespace of the Service
  SERVICE_NAME   Name of the Service
  VIP            Virtual server destination IP (must exist/routable to BIG-IP)
  BIGIP_HOST     BIG-IP management IP/DNS (no scheme)
  BIGIP_USER     BIG-IP username
  BIGIP_PASS     BIG-IP password

Optional env vars:
  SERVICE_PORT   Service port to expose (defaults to .spec.ports[0].port)
  BACKEND_MODE   Backend mode: nodeport (default) or vm-portforward
  PARTITION      BIG-IP partition (default: k8s-apps)
  BIGIP_CALL_MODE How to call BIG-IP iControl REST: local (default) or pod
  DEBUG_NS       Namespace for temporary curl pod (default: f5-cis-system if exists, else default)
  POD_NAME       Name of temporary curl pod (default: bigip-ltm)
  CLEANUP        Whether to delete the temp pod on exit (default: true)
  POOL_NAME      Override pool name
  VS_NAME        Override virtual server name
  DRY_RUN        If true, prints actions but does not call BIG-IP (default: false)

vm-portforward mode env vars:
  VM_IP          VM IP address that BIG-IP can reach (default: auto-detect via `ip route get $BIGIP_HOST`)
  PF_COUNT       How many pod port-forwards to create (default: 2)
  PF_BASE_PORT   First local TCP port on the VM to bind (default: 7445)
  PF_ADDRESS     Address to bind for port-forward (default: 0.0.0.0)
  KEEP_ALIVE     Keep port-forward processes running after programming BIG-IP (default: true)

Examples:
  SERVICE_NS=demo-lb SERVICE_NAME=echo-lb VIP=100.72.200.30 \
  BIGIP_HOST=100.72.44.146 BIGIP_USER=admin BIGIP_PASS='***' \
  KUBECONFIG=/tmp/shoot-pf.kubeconfig \
  ./scripts/bigip-ltm-service-vs.sh

  # If BIG-IP cannot reach node/pod networks, use VM port-forwards as pool members.
  SERVICE_NS=demo-lb SERVICE_NAME=echo-lb VIP=100.72.200.30 \
  BIGIP_HOST=100.72.44.146 BIGIP_USER=admin BIGIP_PASS='***' \
  KUBECONFIG=/tmp/shoot-pf.kubeconfig BACKEND_MODE=vm-portforward VM_IP=100.72.44.199 \
  PF_BASE_PORT=7445 PF_COUNT=2 \
  ./scripts/bigip-ltm-service-vs.sh
EOF
}

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "ERROR: $name is required" >&2
    usage >&2
    exit 2
  fi
}

sanitize_name() {
  # BIG-IP object names are more permissive, but keep it simple.
  # Replace anything except alnum, dot, dash, underscore with underscore.
  echo "$1" | tr -c 'a-zA-Z0-9._-' '_'
}

k() {
  if [[ -n "${KUBECONFIG:-}" ]]; then
    kubectl --request-timeout "${KUBECTL_REQUEST_TIMEOUT:-10s}" --kubeconfig "$KUBECONFIG" "$@"
  else
    kubectl --request-timeout "${KUBECTL_REQUEST_TIMEOUT:-10s}" "$@"
  fi
}

http_code() {
  # $1 = URL
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    echo "404"
    return 0
  fi

  if [[ "${BIGIP_CALL_MODE}" == "local" ]]; then
    curl -sk -u "${BIGIP_USER}:${BIGIP_PASS}" -o /dev/null -w '%{http_code}' "$1" 2>/dev/null || true
    return 0
  fi

  k -n "$DEBUG_NS" exec "$POD_NAME" -- env BIGIP_USER="$BIGIP_USER" BIGIP_PASS="$BIGIP_PASS" sh -lc \
    "curl -sk -u \"\$BIGIP_USER:\$BIGIP_PASS\" -o /dev/null -w '%{http_code}' '$1'" 2>/dev/null || true
}

bigip_call() {
  # $1=METHOD $2=URL $3=JSON(optional)
  local method="$1" url="$2" json="${3:-}"

  if [[ "${DRY_RUN}" == "true" ]]; then
    echo "DRY_RUN: $method $url"
    if [[ -n "$json" ]]; then
      echo "DRY_RUN body: $json"
    fi
    return 0
  fi

  if [[ -n "$json" ]]; then
    if [[ "${BIGIP_CALL_MODE}" == "local" ]]; then
      curl -sk -u "${BIGIP_USER}:${BIGIP_PASS}" -H 'Content-Type: application/json' -X "$method" "$url" -d "$json"
    else
      printf '%s' "$json" | k -n "$DEBUG_NS" exec -i "$POD_NAME" -- env BIGIP_USER="$BIGIP_USER" BIGIP_PASS="$BIGIP_PASS" sh -lc \
        "curl -sk -u \"\$BIGIP_USER:\$BIGIP_PASS\" -H 'Content-Type: application/json' -X '$method' '$url' -d @-"
    fi
  else
    if [[ "${BIGIP_CALL_MODE}" == "local" ]]; then
      curl -sk -u "${BIGIP_USER}:${BIGIP_PASS}" -X "$method" "$url"
    else
      k -n "$DEBUG_NS" exec "$POD_NAME" -- env BIGIP_USER="$BIGIP_USER" BIGIP_PASS="$BIGIP_PASS" sh -lc \
        "curl -sk -u \"\$BIGIP_USER:\$BIGIP_PASS\" -X '$method' '$url'"
    fi
  fi
}

create_debug_pod() {
  # Ensure namespace exists
  k get ns "$DEBUG_NS" >/dev/null 2>&1 || {
    echo "ERROR: Namespace '$DEBUG_NS' not found" >&2
    exit 2
  }

  k -n "$DEBUG_NS" delete pod "$POD_NAME" --ignore-not-found >/dev/null 2>&1 || true

  k -n "$DEBUG_NS" run "$POD_NAME" \
    --image="curlimages/curl:8.6.0" \
    --restart=Never \
    --command -- sleep 3600 >/dev/null

  k -n "$DEBUG_NS" wait --for=condition=Ready pod/"$POD_NAME" --timeout=90s >/dev/null
}

detect_vm_ip() {
  local ip
  ip="$(ip route get "${BIGIP_HOST}" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if ($i=="src") {print $(i+1); exit}}')"
  if [[ -n "$ip" ]]; then
    echo "$ip"
    return 0
  fi
  hostname -I 2>/dev/null | awk '{print $1}'
}

portforward_pids=()

start_portforward() {
  # $1=podName $2=localPort $3=targetPort
  local pod="$1" local_port="$2" target_port="$3" log
  log="/tmp/portforward_${SERVICE_NS}_${SERVICE_NAME}_${pod}_${local_port}.log"
  echo "Starting port-forward: pod/${pod} ${local_port}:${target_port} (log: ${log})"
  if [[ "${DRY_RUN}" == "true" ]]; then
    return 0
  fi
  k -n "$SERVICE_NS" port-forward --address "${PF_ADDRESS}" "pod/${pod}" "${local_port}:${target_port}" >"${log}" 2>&1 &
  portforward_pids+=("$!")

  # Wait until it is listening locally.
  local ok=false
  for _ in {1..30}; do
    if timeout 1 bash -lc "</dev/tcp/127.0.0.1/${local_port}" >/dev/null 2>&1; then
      ok=true
      break
    fi
    sleep 0.2
  done
  if [[ "$ok" != "true" ]]; then
    echo "ERROR: port-forward did not become ready on local port ${local_port}." >&2
    echo "Log: ${log}" >&2
    return 1
  fi
}

stop_portforwards() {
  if [[ "${#portforward_pids[@]}" -eq 0 ]]; then
    return 0
  fi
  for pid in "${portforward_pids[@]}"; do
    kill "$pid" >/dev/null 2>&1 || true
  done
}

cleanup() {
  if [[ "${CLEANUP}" == "true" ]]; then
    if [[ "${BIGIP_CALL_MODE}" == "pod" ]]; then
      k -n "$DEBUG_NS" delete pod "$POD_NAME" --ignore-not-found >/dev/null 2>&1 || true
    fi
  fi

  stop_portforwards
}

main() {
  if [[ "${1:-}" == "-h" || "${1:-}" == "--help" ]]; then
    usage
    exit 0
  fi

  require_env SERVICE_NS
  require_env SERVICE_NAME
  require_env VIP
  require_env BIGIP_HOST
  require_env BIGIP_USER
  require_env BIGIP_PASS

  PARTITION="${PARTITION:-k8s-apps}"
  BACKEND_MODE="${BACKEND_MODE:-nodeport}"
  BIGIP_CALL_MODE="${BIGIP_CALL_MODE:-local}"
  POD_NAME="${POD_NAME:-bigip-ltm}"
  CLEANUP="${CLEANUP:-true}"
  DRY_RUN="${DRY_RUN:-false}"

  echo "Backend mode:        ${BACKEND_MODE}"
  echo "BIG-IP call mode:    ${BIGIP_CALL_MODE}"

  # Choose a namespace where we can create a pod (only needed for BIGIP_CALL_MODE=pod).
  if [[ "${BIGIP_CALL_MODE}" == "pod" ]]; then
    if [[ -z "${DEBUG_NS:-}" ]]; then
      if k get ns f5-cis-system >/dev/null 2>&1; then
        DEBUG_NS="f5-cis-system"
      else
        DEBUG_NS="default"
      fi
    fi
  else
    DEBUG_NS="${DEBUG_NS:-default}"
  fi

  if ! command -v kubectl >/dev/null 2>&1; then
    echo "ERROR: kubectl not found in PATH" >&2
    exit 2
  fi

  if [[ "${BIGIP_CALL_MODE}" != "local" && "${BIGIP_CALL_MODE}" != "pod" ]]; then
    echo "ERROR: BIGIP_CALL_MODE must be 'local' or 'pod'" >&2
    exit 2
  fi
  if [[ "${BIGIP_CALL_MODE}" == "local" && "${DRY_RUN}" != "true" ]]; then
    if ! command -v curl >/dev/null 2>&1; then
      echo "ERROR: curl not found in PATH (required for BIGIP_CALL_MODE=local)" >&2
      exit 2
    fi
  fi

  echo "Validating Kubernetes Service exists ..."
  k -n "$SERVICE_NS" get svc "$SERVICE_NAME" >/dev/null

  local service_port node_port
  service_port="${SERVICE_PORT:-}"
  if [[ -z "$service_port" ]]; then
    service_port="$(k -n "$SERVICE_NS" get svc "$SERVICE_NAME" -o jsonpath='{.spec.ports[0].port}')"
  fi

  local target_port
  target_port="$(k -n "$SERVICE_NS" get svc "$SERVICE_NAME" -o jsonpath="{.spec.ports[?(@.port==${service_port})].targetPort}")"
  if [[ -z "$target_port" || "$target_port" == "null" ]]; then
    target_port="$service_port"
  fi
  if ! [[ "$target_port" =~ ^[0-9]+$ ]]; then
    echo "ERROR: targetPort for ${SERVICE_NS}/${SERVICE_NAME} port ${service_port} is not numeric (${target_port})." >&2
    echo "Hint: set SERVICE_PORT to a port whose targetPort is numeric, or update the Service to use a numeric targetPort for this demo." >&2
    exit 2
  fi

  local member_endpoints=()
  local backend_desc=""
  if [[ "$BACKEND_MODE" == "nodeport" ]]; then
    node_port="$(k -n "$SERVICE_NS" get svc "$SERVICE_NAME" -o jsonpath="{.spec.ports[?(@.port==${service_port})].nodePort}")"
    if [[ -z "$node_port" || "$node_port" == "null" ]]; then
      echo "ERROR: Could not find nodePort for Service ${SERVICE_NS}/${SERVICE_NAME} port ${service_port}." >&2
      echo "Hint: the Service must be type LoadBalancer or NodePort (or have allocateLoadBalancerNodePorts enabled)." >&2
      exit 2
    fi

    mapfile -t node_ips < <(k get nodes -o jsonpath='{range .items[*]}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}{end}')
    if [[ "${#node_ips[@]}" -eq 0 ]]; then
      echo "ERROR: No node InternalIP addresses discovered." >&2
      exit 2
    fi
    for ip in "${node_ips[@]}"; do
      member_endpoints+=("${ip}:${node_port}")
    done
    backend_desc="Nodes (InternalIP) + NodePort"
  elif [[ "$BACKEND_MODE" == "vm-portforward" ]]; then
    VM_IP="${VM_IP:-}"
    if [[ -z "$VM_IP" ]]; then
      VM_IP="$(detect_vm_ip)"
    fi
    if [[ -z "$VM_IP" ]]; then
      echo "ERROR: Could not detect VM_IP (set VM_IP explicitly)." >&2
      exit 2
    fi

    PF_COUNT="${PF_COUNT:-2}"
    PF_BASE_PORT="${PF_BASE_PORT:-7445}"
    PF_ADDRESS="${PF_ADDRESS:-0.0.0.0}"
    KEEP_ALIVE="${KEEP_ALIVE:-true}"

    if ! [[ "$PF_COUNT" =~ ^[0-9]+$ ]] || [[ "$PF_COUNT" -lt 1 ]]; then
      echo "ERROR: PF_COUNT must be a positive integer." >&2
      exit 2
    fi
    if ! [[ "$PF_BASE_PORT" =~ ^[0-9]+$ ]] || [[ "$PF_BASE_PORT" -lt 1 ]]; then
      echo "ERROR: PF_BASE_PORT must be a positive integer." >&2
      exit 2
    fi

    # Prefer EndpointSlice (newer clusters); fall back to Endpoints.
    mapfile -t ep_pods < <(
      k -n "$SERVICE_NS" get endpointslice -l "kubernetes.io/service-name=${SERVICE_NAME}" \
        -o jsonpath='{range .items[*]}{range .endpoints[*]}{.targetRef.name}{"\n"}{end}{end}' 2>/dev/null \
      | awk 'NF' | sort -u
    )
    if [[ "${#ep_pods[@]}" -eq 0 ]]; then
      mapfile -t ep_pods < <(
        k -n "$SERVICE_NS" get endpoints "$SERVICE_NAME" \
          -o jsonpath='{range .subsets[*]}{range .addresses[*]}{.targetRef.name}{"\n"}{end}{end}' 2>/dev/null \
        | awk 'NF' | sort -u
      )
    fi
    if [[ "${#ep_pods[@]}" -eq 0 ]]; then
      echo "ERROR: No endpoint pods discovered for ${SERVICE_NS}/${SERVICE_NAME}." >&2
      echo "Hint: ensure the Service has ready endpoints." >&2
      exit 2
    fi

    echo "Selected backend mode: vm-portforward"
    echo "VM_IP:               ${VM_IP}"
    echo "Port-forward bind:   ${PF_ADDRESS}"
    echo "Target port:         ${target_port}"
    echo

    local selected=()
    for pod in "${ep_pods[@]}"; do
      selected+=("$pod")
      if [[ "${#selected[@]}" -ge "$PF_COUNT" ]]; then
        break
      fi
    done

    local idx=0
    for pod in "${selected[@]}"; do
      local_port=$((PF_BASE_PORT + idx))
      start_portforward "$pod" "$local_port" "$target_port"
      member_endpoints+=("${VM_IP}:${local_port}")
      idx=$((idx + 1))
    done
    backend_desc="VM (${VM_IP}) + kubectl port-forward to pods"
  else
    echo "ERROR: Unsupported BACKEND_MODE=${BACKEND_MODE}. Use nodeport or vm-portforward." >&2
    exit 2
  fi

  local pool_name vs_name
  pool_name="${POOL_NAME:-}"
  vs_name="${VS_NAME:-}"
  if [[ -z "$pool_name" ]]; then
    pool_name="$(sanitize_name "k8s_${SERVICE_NS}_${SERVICE_NAME}_${service_port}_pool")"
  fi
  if [[ -z "$vs_name" ]]; then
    vs_name="$(sanitize_name "k8s_${SERVICE_NS}_${SERVICE_NAME}_${service_port}_vs")"
  fi

  echo "Kubernetes Service: ${SERVICE_NS}/${SERVICE_NAME}"
  echo "Service port:       ${service_port}"
  echo "Target port:        ${target_port}"
  if [[ "$BACKEND_MODE" == "nodeport" ]]; then
    echo "NodePort:           ${node_port}"
  fi
  echo "Backends:           ${backend_desc}"
  echo "Pool members:       ${member_endpoints[*]}"
  echo "BIG-IP host:        ${BIGIP_HOST}"
  echo "Partition:          ${PARTITION}"
  echo "Pool:               ${pool_name}"
  echo "Virtual:            ${vs_name}"
  echo "Destination:        ${VIP}:${service_port}"
  echo

  trap cleanup EXIT
  if [[ "${BIGIP_CALL_MODE}" == "pod" && "${DRY_RUN}" != "true" ]]; then
    echo "Creating temporary curl pod in namespace ${DEBUG_NS} ..."
    create_debug_pod
  fi

  local base pool_item_url members_url vs_item_url
  base="https://${BIGIP_HOST}/mgmt/tm/ltm"
  pool_item_url="${base}/pool/~${PARTITION}~${pool_name}"
  members_url="${base}/pool/~${PARTITION}~${pool_name}/members"
  vs_item_url="${base}/virtual/~${PARTITION}~${vs_name}"

  # Ensure pool exists
  if [[ "$(http_code "$pool_item_url")" != "200" ]]; then
    echo "Creating pool ${PARTITION}/${pool_name} ..."
    bigip_call POST "${base}/pool" "{\"name\":\"${pool_name}\",\"partition\":\"${PARTITION}\",\"loadBalancingMode\":\"round-robin\"}" >/dev/null
  else
    echo "Pool exists: ${PARTITION}/${pool_name}"
  fi

  # Ensure members exist
  for member in "${member_endpoints[@]}"; do
    member_name="$member"
    member_ip="${member_name%:*}"
    member_port="${member_name##*:}"
    member_id="${member_ip}%3A${member_port}"
    member_url="${members_url}/~${PARTITION}~${member_id}"

    if [[ "$(http_code "$member_url")" == "200" ]]; then
      echo "Member exists: ${member_name}"
      continue
    fi

    echo "Adding member: ${member_name} ..."
    bigip_call POST "$members_url" "{\"name\":\"${member_name}\",\"partition\":\"${PARTITION}\"}" >/dev/null
  done

  # Ensure virtual server exists
  if [[ "$(http_code "$vs_item_url")" != "200" ]]; then
    echo "Creating virtual server ${PARTITION}/${vs_name} ..."
    bigip_call POST "${base}/virtual" "{\"name\":\"${vs_name}\",\"partition\":\"${PARTITION}\",\"destination\":\"${VIP}:${service_port}\",\"mask\":\"255.255.255.255\",\"ipProtocol\":\"tcp\",\"pool\":\"/${PARTITION}/${pool_name}\",\"sourceAddressTranslation\":{\"type\":\"automap\"}}" >/dev/null
  else
    echo "Virtual exists; updating destination/pool/SNAT ..."
    bigip_call PATCH "$vs_item_url" "{\"destination\":\"${VIP}:${service_port}\",\"pool\":\"/${PARTITION}/${pool_name}\",\"sourceAddressTranslation\":{\"type\":\"automap\"}}" >/dev/null
  fi

  echo
  echo "Done. Test from a network that can reach the VIP:"
  echo "  curl -sS http://${VIP}:${service_port}/"

  if [[ "$BACKEND_MODE" == "vm-portforward" && "${KEEP_ALIVE}" == "true" ]]; then
    echo
    echo "Port-forwards are running on this VM (pool members)."
    echo "Press Ctrl-C to stop them."
    wait
  fi
}

main "$@"
