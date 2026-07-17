#!/usr/bin/env bash
set -euo pipefail

# Prints a Ce-Auth token to stdout.
#
# If a cached token exists and is still "fresh", it is reused.
# Otherwise, prompts you to paste a new token (copied from CMP UI DevTools).
#
# Env vars:
#   CMP_CE_AUTH_CACHE   path to cache file (default: /tmp/cmp.ce_auth.cache)
#   CMP_CE_AUTH_MAX_AGE max age in seconds before re-prompt (default: 240)

CACHE_FILE="${CMP_CE_AUTH_CACHE:-/tmp/cmp.ce_auth.cache}"
MAX_AGE="${CMP_CE_AUTH_MAX_AGE:-240}"

trim() {
  local s="$1"
  s="${s#${s%%[![:space:]]*}}"
  s="${s%${s##*[![:space:]]}}"
  printf '%s' "$s"
}

now_epoch() {
  date +%s
}

read_cache() {
  [[ -f "$CACHE_FILE" ]] || return 1
  local ts token
  ts="$(head -n 1 "$CACHE_FILE" 2>/dev/null || true)"
  token="$(sed -n '2p' "$CACHE_FILE" 2>/dev/null || true)"
  ts="$(trim "$ts")"
  token="$(trim "$token")"
  [[ -n "$ts" && -n "$token" ]] || return 1
  printf '%s\n%s\n' "$ts" "$token"
}

write_cache() {
  local ts="$1" token="$2"
  umask 077
  {
    printf '%s\n' "$ts"
    printf '%s\n' "$token"
  } > "$CACHE_FILE"
}

cached=""
if cached="$(read_cache)"; then
  cached_ts="$(printf '%s' "$cached" | head -n 1)"
  cached_token="$(printf '%s' "$cached" | tail -n 1)"
  now="$(now_epoch)"

  if [[ "$cached_ts" =~ ^[0-9]+$ ]]; then
    age=$(( now - cached_ts ))
    if (( age >= 0 && age < MAX_AGE )); then
      printf '%s' "$cached_token"
      exit 0
    fi
  fi
fi

echo "Ce-Auth token missing/expired (>${MAX_AGE}s)." >&2
echo "Paste a fresh Ce-Auth token from CMP UI (DevTools → Network → any API call → Request Headers → Ce-Auth):" >&2
read -r token

token="$(trim "$token")"
if [[ -z "$token" ]]; then
  echo "empty token" >&2
  exit 2
fi

write_cache "$(now_epoch)" "$token"
printf '%s' "$token"
