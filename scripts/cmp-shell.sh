#!/usr/bin/env bash
# Source this file to get helper functions for CMP curl calls.
#
# Usage:
#   source scripts/cmp.env
#   source scripts/cmp-shell.sh
#
#   # POST form example:
#   cmp_curl POST /load-balancers/lb_service/ \
#     -H 'Content-Type: application/x-www-form-urlencoded' \
#     --data-urlencode 'name=api_lb1' \
#     --data-urlencode 'vpc_name=vpc-27oct' \
#     --data-urlencode 'vip_subnet=vip-subnet'

set -euo pipefail

_cmp_trim() {
  local s="${1:-}"
  s="${s#${s%%[![:space:]]*}}"
  s="${s%${s##*[![:space:]]}}"
  printf '%s' "$s"
}

cmp_base_url() {
  : "${CMP_ENDPOINT:?CMP_ENDPOINT is required (e.g. https://cmp.example)}"
  : "${CMP_PREFIX:?CMP_PREFIX is required (e.g. /api/v1)}"

  local ep pref
  ep="${CMP_ENDPOINT%/}"
  pref="/${CMP_PREFIX#/}"
  pref="${pref%/}"
  printf '%s' "$ep$pref"
}

cmp_token() {
  local token=""
  if [[ -n "${CMP_TOKEN_CMD:-}" ]]; then
    token="$(CMP_TOKEN_FILE="${CMP_TOKEN_FILE:-}" CMP_TOKEN_VALUE="${CMP_TOKEN_VALUE:-}" ${CMP_TOKEN_CMD})"
  elif [[ -n "${CMP_TOKEN_PY:-}" ]]; then
    token="$(python3 "${CMP_TOKEN_PY}")"
  else
    echo "set CMP_TOKEN_PY or CMP_TOKEN_CMD in scripts/cmp.env" >&2
    return 2
  fi

  token="$(_cmp_trim "$token")"
  if [[ -z "$token" ]]; then
    echo "token generator produced empty output" >&2
    return 1
  fi
  printf '%s' "$token"
}

cmp_curl() {
  : "${CMP_ORG:?CMP_ORG is required}"
  : "${CMP_PROJECT:?CMP_PROJECT is required}"

  local method="${1:-}"; shift || true
  local path_or_url="${1:-}"; shift || true
  if [[ -z "$method" || -z "$path_or_url" ]]; then
    echo "usage: cmp_curl METHOD PATH_OR_URL [curl args...]" >&2
    return 2
  fi

  local url="$path_or_url"
  if [[ "$url" != http://* && "$url" != https://* ]]; then
    url="$(cmp_base_url)/${url#/}"
  fi

  local token
  token="$(cmp_token)"

  local curl_common=(
    -sS
    -i
    -X "$method"
    -H 'Accept: application/json'
    --max-redirs 0
    --fail-with-body
  )

  if [[ "${CMP_INSECURE:-0}" == "1" || "${CMP_INSECURE:-0}" == "true" ]]; then
    curl_common+=( -k )
  fi

  # Pass auth headers via -K stdin so the token doesn't show up in argv / process list.
  local cfg
  cfg=$(
    cat <<EOF
header = "Ce-Auth: $token"
header = "organisation-name: $CMP_ORG"
header = "organization-name: $CMP_ORG"
header = "project-id: $CMP_PROJECT"
EOF
  )

  # Optional UI scoping headers (only when set in scripts/cmp.env)
  if [[ -n "${CMP_REGION:-}" ]]; then
    cfg+=$'\n'
    cfg+="header = \"ce-region: ${CMP_REGION}\""
  fi
  if [[ -n "${CMP_EXTERNAL_PROJECT:-}" ]]; then
    cfg+=$'\n'
    cfg+="header = \"external-project: ${CMP_EXTERNAL_PROJECT}\""
  fi
  if [[ -n "${CMP_PROJECT_NAME:-}" ]]; then
    cfg+=$'\n'
    cfg+="header = \"project-name: ${CMP_PROJECT_NAME}\""
  fi
  if [[ -n "${CMP_USERNAME:-}" ]]; then
    cfg+=$'\n'
    cfg+="header = \"username: ${CMP_USERNAME}\""
  fi

  printf '%s\n' "$cfg" | curl "${curl_common[@]}" -K - "$url" "$@"
}
