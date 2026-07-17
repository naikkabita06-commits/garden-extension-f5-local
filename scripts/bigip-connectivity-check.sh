#!/usr/bin/env bash
set -euo pipefail

NS="${NS:-default}"
POD_NAME="${POD_NAME:-net-debug}"
IMAGE="${IMAGE:-nicolaka/netshoot:latest}"
BIGIP_HOST="${BIGIP_HOST:-}"
BIGIP_PORTS="${BIGIP_PORTS:-443 8443}"
CLEANUP="${CLEANUP:-true}"

if [[ -z "$BIGIP_HOST" ]]; then
  echo "ERROR: BIGIP_HOST is required (IP/hostname of Big-IP)." >&2
  echo "Example:" >&2
  echo "  BIGIP_HOST=10.0.0.10 NS=default ./scripts/bigip-connectivity-check.sh" >&2
  exit 2
fi

if ! command -v kubectl >/dev/null 2>&1; then
  echo "ERROR: kubectl not found in PATH" >&2
  exit 2
fi

current_context="$(kubectl config current-context 2>/dev/null || true)"
if [[ -z "$current_context" ]]; then
  echo "ERROR: No current kubecontext configured (kubectl config current-context is empty)." >&2
  exit 2
fi

echo "Kubecontext: ${current_context}"
echo "Namespace:  ${NS}"
echo "Big-IP:     ${BIGIP_HOST}"
echo "Ports:      ${BIGIP_PORTS}"
echo

cleanup() {
  if [[ "$CLEANUP" == "true" ]]; then
    kubectl -n "$NS" delete pod "$POD_NAME" --ignore-not-found >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# Ensure namespace exists
kubectl get ns "$NS" >/dev/null 2>&1 || {
  echo "ERROR: Namespace '$NS' not found" >&2
  exit 2
}

# Recreate pod for a clean run
kubectl -n "$NS" delete pod "$POD_NAME" --ignore-not-found >/dev/null 2>&1 || true

echo "Creating debug pod $NS/$POD_NAME (image: $IMAGE) ..."
if ! kubectl -n "$NS" run "$POD_NAME" \
  --image="$IMAGE" \
  --restart=Never \
  --command -- sleep 3600 >/dev/null 2>&1; then
  echo "WARN: Failed to create pod with $IMAGE; falling back to curl-only image." >&2
  IMAGE="curlimages/curl:8.6.0"
  kubectl -n "$NS" run "$POD_NAME" \
    --image="$IMAGE" \
    --restart=Never \
    --command -- sleep 3600 >/dev/null
fi

echo "Waiting for pod to be Ready ..."
kubectl -n "$NS" wait --for=condition=Ready pod/"$POD_NAME" --timeout=90s

echo
echo "=== TCP connectivity ==="
for port in $BIGIP_PORTS; do
  echo "-- ${BIGIP_HOST}:${port}"
  # netshoot has nc; curl-only image does not
  if kubectl -n "$NS" exec "$POD_NAME" -- sh -lc 'command -v nc >/dev/null 2>&1'; then
    kubectl -n "$NS" exec "$POD_NAME" -- sh -lc "nc -vz -w 5 '$BIGIP_HOST' '$port'"
  else
    echo "(nc not available in image $IMAGE; skipping raw TCP check)" >&2
  fi
  echo
done

echo "=== TLS handshake (if openssl available) ==="
if kubectl -n "$NS" exec "$POD_NAME" -- sh -lc 'command -v openssl >/dev/null 2>&1'; then
  for port in $BIGIP_PORTS; do
    echo "-- ${BIGIP_HOST}:${port}"
    kubectl -n "$NS" exec "$POD_NAME" -- sh -lc \
      "openssl s_client -connect '$BIGIP_HOST:$port' -servername '$BIGIP_HOST' -brief </dev/null" || true
    echo
  done
else
  echo "(openssl not available in image $IMAGE; skipping TLS handshake details)" >&2
fi

echo "=== HTTP(S) probe (expect any HTTP response) ==="
for port in $BIGIP_PORTS; do
  echo "-- https://${BIGIP_HOST}:${port}"
  kubectl -n "$NS" exec "$POD_NAME" -- sh -lc \
    "curl -k -sS -o /dev/null -D - 'https://$BIGIP_HOST:$port/' | head -n 5" || true

  # Common BIG-IP iControl REST endpoints (may 401 without creds; that's still connectivity)
  kubectl -n "$NS" exec "$POD_NAME" -- sh -lc \
    "curl -k -sS -o /dev/null -D - 'https://$BIGIP_HOST:$port/mgmt/shared/echo' | head -n 5" || true

  kubectl -n "$NS" exec "$POD_NAME" -- sh -lc \
    "curl -k -sS -o /dev/null -D - 'https://$BIGIP_HOST:$port/mgmt/tm/sys/version' | head -n 5" || true

  echo
done

echo "Done. If you see timeouts, it’s still a network/firewall issue; if you see HTTP headers (even 401/404), routing is working."