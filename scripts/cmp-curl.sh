#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
cmp-curl.sh: run CMP/CCP LBaaS curl calls with auto-refreshed auth token

Setup:
  cp scripts/cmp.env.example scripts/cmp.env
  # edit scripts/cmp.env to set CMP_ENDPOINT/CMP_PREFIX/CMP_ORG/CMP_PROJECT + token generator

Usage:
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] lb-create --name NAME [--description TEXT]
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] lb-get --id ID
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] lb-list [--search TEXT] [--limit N] [--offset N]
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] lb-update --id ID [--name NAME] [--description TEXT]
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] lb-delete --id ID

  scripts/cmp-curl.sh [--creds /path/to/cmp.env] lbsvc-create --name NAME --flavor-id ID --vpc-id UUID --network-id UUID --vpc-name NAME [--description TEXT]
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] lbsvc-get --id ID
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] lbsvc-list [--search TEXT] [--limit N] [--offset N]
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] vip-create --lbsvc-id ID
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] vs-create --lb-id ID --param key=value [--param key=value ...]

  # Advanced: call any endpoint under CMP_ENDPOINT+CMP_PREFIX
  # Examples:
  #   scripts/cmp-curl.sh raw GET /load-balancers/lb_service/?search=gardener
  #   scripts/cmp-curl.sh raw GET /load-balancers/lb_service/123/vip
  #   scripts/cmp-curl.sh raw POST /load-balancers/123/virtual-servers \
  #     -H 'Content-Type: application/x-www-form-urlencoded' \
  #     --data-urlencode 'name=vs1'
  scripts/cmp-curl.sh [--creds /path/to/cmp.env] raw METHOD PATH_OR_URL [curl args...]

Notes:
  - Token is generated fresh for every invocation.
  - Your token generator must print the Ce-Auth token to stdout.
EOF
}

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
CREDS_FILE="$SCRIPT_DIR/cmp.env"
if [[ ${1:-} == "--creds" ]]; then
  CREDS_FILE=${2:-}
  shift 2
fi

if [[ ${1:-} == "-h" || ${1:-} == "--help" || ${1:-} == "help" || ${1:-} == "" ]]; then
  usage
  exit 0
fi

if [[ ! -f "$CREDS_FILE" ]]; then
  echo "creds file not found: $CREDS_FILE" >&2
  echo "copy scripts/cmp.env.example to scripts/cmp.env and fill values" >&2
  exit 2
fi

# shellcheck disable=SC1090
source "$CREDS_FILE"

: "${CMP_ENDPOINT:?CMP_ENDPOINT is required}"
: "${CMP_PREFIX:?CMP_PREFIX is required}"
: "${CMP_ORG:?CMP_ORG is required}"
: "${CMP_PROJECT:?CMP_PROJECT is required}"

trim() {
  local s="$1"
  # trim leading/trailing whitespace
  s="${s#${s%%[![:space:]]*}}"
  s="${s%${s##*[![:space:]]}}"
  printf '%s' "$s"
}

get_token() {
  local token=""
  if [[ -n "${CMP_TOKEN_CMD:-}" ]]; then
    # Forward token-related vars into the subprocess even when they come from a
    # sourced creds file (shell vars are not automatically exported).
    token="$(CMP_TOKEN_FILE="${CMP_TOKEN_FILE:-}" CMP_TOKEN_VALUE="${CMP_TOKEN_VALUE:-}" $CMP_TOKEN_CMD)"
  elif [[ -n "${CMP_TOKEN_PY:-}" ]]; then
    token="$(python3 "$CMP_TOKEN_PY")"
  else
    echo "set CMP_TOKEN_PY or CMP_TOKEN_CMD in $CREDS_FILE" >&2
    exit 2
  fi
  token="$(trim "$token")"
  if [[ -z "$token" ]]; then
    echo "token generator produced empty output" >&2
    exit 1
  fi
  printf '%s' "$token"
}

base_url() {
  local ep="$CMP_ENDPOINT"
  local pref="$CMP_PREFIX"
  ep="${ep%/}"
  pref="/${pref#/}"
  pref="${pref%/}"
  printf '%s' "$ep$pref"
}

curl_common=(
  -sS
  -H 'Accept: application/json'
)

if [[ "${CMP_INSECURE:-0}" == "1" || "${CMP_INSECURE:-0}" == "true" ]]; then
  curl_common+=( -k )
fi

run_curl() {
  local method="$1"; shift
  local url="$1"; shift

  local token
  token="$(get_token)"

  # Pass sensitive headers/cookies via curl config on stdin so the token
  # does not show up in argv / process list.
  curl -i -X "$method" "${curl_common[@]}" -K - "$url" "$@" <<EOF
header = "Ce-Auth: $token"
header = "organisation-name: $CMP_ORG"
header = "organization-name: $CMP_ORG"
header = "project-id: $CMP_PROJECT"
EOF
}

run_curl_ui_extras() {
  # Adds optional headers/cookies commonly seen in the CMP UI.
  # These are only sent when present in the creds file.
  local method="$1"; shift
  local url="$1"; shift

  local token
  token="$(get_token)"

  curl -i -X "$method" "${curl_common[@]}" -K - "$url" "$@" <<EOF
header = "Ce-Auth: $token"
header = "organisation-name: $CMP_ORG"
header = "organization-name: $CMP_ORG"
header = "project-id: $CMP_PROJECT"
${CMP_REGION:+header = "ce-region: $CMP_REGION"}
${CMP_EXTERNAL_PROJECT:+header = "external-project: $CMP_EXTERNAL_PROJECT"}
${CMP_PROJECT_NAME:+header = "project-name: $CMP_PROJECT_NAME"}
EOF
}

cmd="$1"; shift
api="$(base_url)"

case "$cmd" in
  lb-create)
    name=""; desc=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --name) name="$2"; shift 2;;
        --description) desc="$2"; shift 2;;
        -h|--help)
          usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done
    if [[ -z "$name" ]]; then
      echo "--name is required" >&2
      exit 2
    fi
    run_curl POST "$api/load-balancers/" \
      -H 'Content-Type: application/x-www-form-urlencoded' \
      --data-urlencode "name=$name" \
      --data-urlencode "description=$desc"
    ;;

  lb-get)
    id=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --id) id="$2"; shift 2;;
        -h|--help) usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done
    if [[ -z "$id" ]]; then
      echo "--id is required" >&2
      exit 2
    fi
    run_curl GET "$api/load-balancers/$id/"
    ;;

  lb-list)
    search=""; limit=""; offset=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --search) search="$2"; shift 2;;
        --limit) limit="$2"; shift 2;;
        --offset) offset="$2"; shift 2;;
        -h|--help) usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done

    qs=()
    [[ -n "$search" ]] && qs+=("search=$search")
    [[ -n "$limit" ]] && qs+=("limit=$limit")
    [[ -n "$offset" ]] && qs+=("offset=$offset")

    url="$api/load-balancers/"
    if [[ ${#qs[@]} -gt 0 ]]; then
      url+="?$(IFS='&'; echo "${qs[*]}")"
    fi
    run_curl GET "$url"
    ;;

  lb-update)
    id=""; name=""; desc=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --id) id="$2"; shift 2;;
        --name) name="$2"; shift 2;;
        --description) desc="$2"; shift 2;;
        -h|--help) usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done
    if [[ -z "$id" ]]; then
      echo "--id is required" >&2
      exit 2
    fi
    if [[ -z "$name" && -z "$desc" ]]; then
      echo "provide at least one of --name or --description" >&2
      exit 2
    fi

    args=( -H 'Content-Type: application/x-www-form-urlencoded' )
    [[ -n "$name" ]] && args+=( --data-urlencode "name=$name" )
    [[ -n "$desc" ]] && args+=( --data-urlencode "description=$desc" )

    run_curl PATCH "$api/load-balancers/$id/" "${args[@]}"
    ;;

  lb-delete)
    id=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --id) id="$2"; shift 2;;
        -h|--help) usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done
    if [[ -z "$id" ]]; then
      echo "--id is required" >&2
      exit 2
    fi
    run_curl DELETE "$api/load-balancers/$id/"
    ;;

  lbsvc-create)
    name=""; desc=""; flavor_id=""; vpc_id=""; network_id=""; vpc_name=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --name) name="$2"; shift 2;;
        --description) desc="$2"; shift 2;;
        --flavor-id) flavor_id="$2"; shift 2;;
        --vpc-id) vpc_id="$2"; shift 2;;
        --network-id) network_id="$2"; shift 2;;
        --vpc-name) vpc_name="$2"; shift 2;;
        -h|--help)
          usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done
    if [[ -z "$name" || -z "$flavor_id" || -z "$vpc_id" || -z "$network_id" || -z "$vpc_name" ]]; then
      echo "--name, --flavor-id, --vpc-id, --network-id, --vpc-name are required" >&2
      exit 2
    fi
    run_curl_ui_extras POST "$api/load-balancers/lb_service/" \
      -H 'Content-Type: application/x-www-form-urlencoded' \
      --data-urlencode "name=$name" \
      --data-urlencode "flavor_id=$flavor_id" \
      --data-urlencode "description=$desc" \
      --data-urlencode "vpc_id=$vpc_id" \
      --data-urlencode "network_id=$network_id" \
      --data-urlencode "vpc_name=$vpc_name"
    ;;

  lbsvc-get)
    id=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --id) id="$2"; shift 2;;
        -h|--help) usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done
    if [[ -z "$id" ]]; then
      echo "--id is required" >&2
      exit 2
    fi
    run_curl_ui_extras GET "$api/load-balancers/lb_service/$id/"
    ;;

  lbsvc-list)
    search=""; limit=""; offset=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --search) search="$2"; shift 2;;
        --limit) limit="$2"; shift 2;;
        --offset) offset="$2"; shift 2;;
        -h|--help) usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done

    qs=()
    [[ -n "$search" ]] && qs+=("search=$search")
    [[ -n "$limit" ]] && qs+=("limit=$limit")
    [[ -n "$offset" ]] && qs+=("offset=$offset")

    url="$api/load-balancers/lb_service/"
    if [[ ${#qs[@]} -gt 0 ]]; then
      url+="?$(IFS='&'; echo "${qs[*]}")"
    fi
    run_curl_ui_extras GET "$url"
    ;;

  vip-create)
    lbsvc_id=""; vip=""; port=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --lbsvc-id) lbsvc_id="$2"; shift 2;;
        # Backwards-compat flags (Swagger doesn't define these params).
        --vip) vip="$2"; shift 2;;
        --port) port="$2"; shift 2;;
        -h|--help) usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done
    if [[ -z "$lbsvc_id" ]]; then
      echo "--lbsvc-id is required" >&2
      exit 2
    fi
    # Swagger: no request parameters, empty body.
    run_curl_ui_extras POST "$api/load-balancers/lb_service/${lbsvc_id}/vip"
    ;;

  vs-create)
    lb_id=""
    params=()
    while [[ $# -gt 0 ]]; do
      case "$1" in
        --lb-id) lb_id="$2"; shift 2;;
        --param) params+=("$2"); shift 2;;
        -h|--help) usage; exit 0;;
        *) echo "unknown arg: $1" >&2; exit 2;;
      esac
    done
    if [[ -z "$lb_id" ]]; then
      echo "--lb-id is required" >&2
      exit 2
    fi
    if [[ ${#params[@]} -eq 0 ]]; then
      echo "provide at least one --param key=value" >&2
      exit 2
    fi
    # Build query string with proper URL encoding.
    params_raw="$(printf '%s\n' "${params[@]}")"
    qs="$(PARAMS="$params_raw" python3 - <<'PY'
import os
import urllib.parse

raw = os.environ.get('PARAMS', '')
pairs = [p for p in raw.split('\n') if p.strip()]
q = []
for kv in pairs:
  if '=' not in kv:
    raise SystemExit(f'invalid --param {kv} (want key=value)')
  k, v = kv.split('=', 1)
  k = k.strip()
  v = v.strip()
  if not k:
    raise SystemExit(f'invalid --param {kv} (empty key)')
  q.append((k, v))
print(urllib.parse.urlencode(q, doseq=True))
PY
)"
    run_curl_ui_extras POST "$api/load-balancers/${lb_id}/virtual-servers?$qs"
    ;;

  raw)
    if [[ $# -lt 2 ]]; then
      echo "raw: requires METHOD and PATH_OR_URL" >&2
      echo "example: scripts/cmp-curl.sh raw GET /load-balancers/lb_service/" >&2
      exit 2
    fi

    method="$1"; shift
    method="$(echo "$method" | tr '[:lower:]' '[:upper:]')"
    target="$1"; shift

    if [[ "$target" == http://* || "$target" == https://* ]]; then
      url="$target"
    else
      url="$api/${target#/}"
    fi

    run_curl_ui_extras "$method" "$url" "$@"
    ;;

  *)
    echo "unknown command: $cmd" >&2
    usage
    exit 2
    ;;
esac
