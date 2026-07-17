#!/usr/bin/env bash
set -euo pipefail

# Keeps a local port-forward to the shoot kube-apiserver alive.
# Useful when /tmp/shoot-pf.kubeconfig points to https://127.0.0.1:7443
#
# Usage:
#   scripts/shoot-apiserver-portforward.sh
#
# Logs:
#   /tmp/shoot-apiserver-portforward.log

NAMESPACE="${PF_NAMESPACE:-shoot--local--local}"
LOCAL_PORT="${PF_LOCAL_PORT:-7443}"
REMOTE_PORT="443"
LOG_FILE="/tmp/shoot-apiserver-portforward.log"
ADDRESS="${PF_ADDRESS:-127.0.0.1}"
POD_INDEX="${PF_POD_INDEX:-0}"
KUBECTL_CONTEXT="${PF_KUBECTL_CONTEXT:-}"
KUBECONFIG_FILE="${PF_KUBECONFIG:-}"

k() {
	local args=()
	if [[ -n "${KUBECONFIG_FILE}" ]]; then
		args+=("--kubeconfig" "${KUBECONFIG_FILE}")
	fi
	if [[ -n "${KUBECTL_CONTEXT}" ]]; then
		args+=("--context" "${KUBECTL_CONTEXT}")
	fi
	kubectl "${args[@]}" "$@"
}

pick_apiserver_pod() {
	k -n "${NAMESPACE}" get pods -l app=kubernetes,role=apiserver \
		-o jsonpath="{.items[${POD_INDEX}].metadata.name}" 2>/dev/null || true
}

cleanup() {
	if [[ -n "${PF_PID:-}" ]] && kill -0 "${PF_PID}" 2>/dev/null; then
		kill "${PF_PID}" 2>/dev/null || true
		wait "${PF_PID}" 2>/dev/null || true
	fi
}
trap cleanup EXIT INT TERM

{
	echo "[$(date -Is)] Starting shoot kube-apiserver port-forward supervisor"
		echo "[$(date -Is)] Context=${KUBECTL_CONTEXT:-<default>} Kubeconfig=${KUBECONFIG_FILE:-<default>}"
	echo "[$(date -Is)] Namespace=${NAMESPACE} Local=${ADDRESS}:${LOCAL_PORT} Remote=${REMOTE_PORT}"
	echo "[$(date -Is)] PodIndex=${POD_INDEX}"
	echo "[$(date -Is)] Log=${LOG_FILE}"
} | tee -a "${LOG_FILE}"

while true; do
	pod="$(pick_apiserver_pod)"
	if [[ -z "${pod}" ]]; then
		echo "[$(date -Is)] ERROR: could not find kube-apiserver pod in ${NAMESPACE}; retrying in 3s" | tee -a "${LOG_FILE}"
		sleep 3
		continue
	fi

	echo "[$(date -Is)] Port-forwarding pod/${pod} ${LOCAL_PORT}:${REMOTE_PORT}" | tee -a "${LOG_FILE}"
	# Bind explicitly to the chosen address. If it dies, loop restarts it.
	k -n "${NAMESPACE}" port-forward --address "${ADDRESS}" "pod/${pod}" "${LOCAL_PORT}:${REMOTE_PORT}" >>"${LOG_FILE}" 2>&1 &
	PF_PID=$!

	# Wait until the process exits, then restart.
	wait "${PF_PID}" || true
	echo "[$(date -Is)] Port-forward exited; restarting in 1s" | tee -a "${LOG_FILE}"
	unset PF_PID
	sleep 1

done
