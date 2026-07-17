#!/usr/bin/env bash
set -euo pipefail

# Gardener kind-based demo (single context) for Shoot: garden-local/local
# Safe-by-default: does not print tokens, only writes them to /tmp.

CTX_DEFAULT="kind-gardener-local"
CTX="${CTX:-$CTX_DEFAULT}"

SHOOT_NS_DEFAULT="garden-local"
SHOOT_NAME_DEFAULT="local"
SHOOT_NS="${SHOOT_NS:-$SHOOT_NS_DEFAULT}"
SHOOT_NAME="${SHOOT_NAME:-$SHOOT_NAME_DEFAULT}"

TECH_NS_DEFAULT="shoot--local--local"
TECH_NS="${TECH_NS:-$TECH_NS_DEFAULT}"

KCFG_SECRET_DEFAULT="generic-token-kubeconfig-8a2b75c7"
KCFG_SECRET="${KCFG_SECRET:-$KCFG_SECRET_DEFAULT}"

TOKEN_SECRET_DEFAULT="shoot-access-gardener-resource-manager"
TOKEN_SECRET="${TOKEN_SECRET:-$TOKEN_SECRET_DEFAULT}"

LOCAL_PORT_DEFAULT="8443"
LOCAL_PORT="${LOCAL_PORT:-$LOCAL_PORT_DEFAULT}"

KUBECONFIG_OUT_DEFAULT="/tmp/shoot-local.kubeconfig"
TOKEN_OUT_DEFAULT="/tmp/shoot-local.token"
KUBECONFIG_OUT="${KUBECONFIG_OUT:-$KUBECONFIG_OUT_DEFAULT}"
TOKEN_OUT="${TOKEN_OUT:-$TOKEN_OUT_DEFAULT}"

say() {
  echo
  echo "### $*"
}

run() {
  echo "+ $*"
  "$@"
}

say "0) Context + cluster reachability"
run kubectl --context "$CTX" config current-context || true
run kubectl --context "$CTX" cluster-info
run kubectl --context "$CTX" get nodes -o wide

say "1) Gardener control plane pods (garden namespace)"
run kubectl --context "$CTX" -n garden get pods

say "2) Shoot list (pick the demo Shoot)"
run kubectl --context "$CTX" get shoot -A

echo
echo "Using Shoot: $SHOOT_NS/$SHOOT_NAME"

say "3) Shoot status + health conditions"
run kubectl --context "$CTX" -n "$SHOOT_NS" describe shoot "$SHOOT_NAME"

say "4) Shoot technical namespace on seed (Shoot control plane runs here)"
run kubectl --context "$CTX" get ns "$TECH_NS"
run kubectl --context "$CTX" -n "$TECH_NS" get pods -o wide
run kubectl --context "$CTX" -n "$TECH_NS" get svc | egrep -i 'kube-apiserver|apiserver' || true

cat <<EOF

---
NEXT STEP (interactive): Port-forward Shoot kube-apiserver

In TERMINAL 1 on the VM, run:
  kubectl --context $CTX -n $TECH_NS port-forward svc/kube-apiserver ${LOCAL_PORT}:443

Leave it running. Then re-run this script with DO_SHOOT_ACCESS=1
(or run the commands in the talk-track).
---
EOF

if [[ "${DO_SHOOT_ACCESS:-0}" != "1" ]]; then
  exit 0
fi

say "5) Prepare a working Shoot kubeconfig for localhost port-forward"

# Extract kubeconfig and token to local files.
# NOTE: The kubeconfig references an in-pod tokenFile path, so we redirect it to TOKEN_OUT.
run kubectl --context "$CTX" -n "$TECH_NS" get secret "$KCFG_SECRET" \
  -o jsonpath='{.data.kubeconfig}'

kubectl --context "$CTX" -n "$TECH_NS" get secret "$KCFG_SECRET" \
  -o jsonpath='{.data.kubeconfig}' | base64 -d > "$KUBECONFIG_OUT"

kubectl --context "$CTX" -n "$TECH_NS" get secret "$TOKEN_SECRET" \
  -o jsonpath='{.data.token}' | base64 -d > "$TOKEN_OUT"

# Patch tokenFile path and API server URL(s) to use localhost.
sed -i "s#/var/run/secrets/gardener.cloud/shoot/generic-kubeconfig/token#${TOKEN_OUT}#g" "$KUBECONFIG_OUT"

# Replace both internal/external Shoot URLs with localhost.
sed -i "s#https://api.local.local.internal.local.gardener.cloud#https://127.0.0.1:${LOCAL_PORT}#g" "$KUBECONFIG_OUT"
sed -i "s#https://api.local.local.external.local.gardener.cloud#https://127.0.0.1:${LOCAL_PORT}#g" "$KUBECONFIG_OUT"

say "6) Access Shoot cluster (nodes + sample system pods)"
run kubectl --kubeconfig "$KUBECONFIG_OUT" get nodes -o wide
run kubectl --kubeconfig "$KUBECONFIG_OUT" get pods -A | head

say "7) (Optional) Deploy a tiny workload"
run kubectl --kubeconfig "$KUBECONFIG_OUT" create ns demo || true
run kubectl --kubeconfig "$KUBECONFIG_OUT" -n demo create deployment hello --image=nginx || true
run kubectl --kubeconfig "$KUBECONFIG_OUT" -n demo get deploy,pods,svc

echo
echo "Done. If you want to cleanup demo namespace:"
echo "  kubectl --kubeconfig $KUBECONFIG_OUT delete ns demo"
