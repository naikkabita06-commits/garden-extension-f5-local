#!/usr/bin/env bash
set -euo pipefail

CHART_DIR=${CHART_DIR:-charts/gardener-extension-f5}
NAME=${NAME:-gardener-extension-f5}
IMAGE_REPOSITORY=${IMAGE_REPOSITORY:-europe-docker.pkg.dev/gardener-project/public/gardener/extensions/f5}
IMAGE_TAG=${IMAGE_TAG:-latest}
OUTPUT=${OUTPUT:-deploy/garden/controllerdeployment-f5.yaml}

if ! command -v helm >/dev/null 2>&1; then
  echo "helm not found in PATH; please install helm v3" >&2
  exit 1
fi

TMP_DIR=$(mktemp -d)
trap 'rm -rf "${TMP_DIR}"' EXIT

helm package "${CHART_DIR}" -d "${TMP_DIR}" >/dev/null
CHART_TGZ=$(ls -1 "${TMP_DIR}"/*.tgz | head -n 1)

if base64 --help 2>/dev/null | grep -q -- '-w'; then
  CHART_B64=$(base64 -w0 "${CHART_TGZ}")
else
  CHART_B64=$(base64 "${CHART_TGZ}" | tr -d '\n')
fi

{
  cat <<EOF
apiVersion: core.gardener.cloud/v1beta1
kind: ControllerDeployment
metadata:
  name: ${NAME}
type: helm
providerConfig:
  chart: |
EOF
  echo "${CHART_B64}" | fold -w 76 | sed 's/^/    /'
  cat <<EOF
  values:
    image:
      repository: ${IMAGE_REPOSITORY}
      tag: ${IMAGE_TAG}
    rbac:
      clusterAdmin: true
EOF
} >"${OUTPUT}"

echo "Wrote ${OUTPUT}" >&2
