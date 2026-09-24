#!/usr/bin/env bash
# Probe whether Mysterium's connect-proxy endpoint works with YOUR OWN bearer token.
#
# Uses your own account credentials and nothing else. It does not extract,
# guess, or replay any client secret, and does not spoof an Origin or
# User-Agent to impersonate the official clients.
#
# Usage (provide TOKEN or COOKIE, whichever your session uses):
#   export BASE_URL='https://<host>/api/v1'
#   export TOKEN='<bearer token>'          # from an "Authorization: Bearer ..." header
#   # or
#   export COOKIE='<cookie header>'        # from a "Cookie: ..." header
#   ./test-connect-proxy.sh
#
# Optional:
#   COUNTRY=Germany IP_TYPE=residential OS_TYPE=linux ./test-connect-proxy.sh

set -uo pipefail

: "${BASE_URL:?set BASE_URL, e.g. export BASE_URL='https://host/api/v1'}"
if [[ -z "${TOKEN:-}" && -z "${COOKIE:-}" ]]; then
  echo "set TOKEN='eyJ...' or COOKIE='name=value; ...' from your own browser session" >&2
  exit 1
fi

BASE_URL="${BASE_URL%/}"
COUNTRY="${COUNTRY:-}"
IP_TYPE="${IP_TYPE:-residential}"
OS_TYPE="${OS_TYPE:-}"

if [[ -n "${TOKEN:-}" ]]; then
  AUTH_HEADER=(-H "Authorization: Bearer $TOKEN")
  masked="Bearer ${TOKEN:0:6}…${TOKEN: -4}"
else
  AUTH_HEADER=(-H "Cookie: $COOKIE")
  masked="Cookie ${COOKIE:0:12}…"
fi

hr() { printf '\n\033[1m── %s\033[0m\n' "$1"; }

# call METHOD PATH [JSON_BODY] -> prints status + body
call() {
  local method="$1" path="$2" body="${3:-}"
  local args=(-sS -o /tmp/myst_body -w '%{http_code}' -X "$method"
              "${AUTH_HEADER[@]}"
              -H 'Accept: application/json')
  [[ -n "$body" ]] && args+=(-H 'Content-Type: application/json' -d "$body")
  local code
  code=$(curl "${args[@]}" "$BASE_URL$path" 2>/tmp/myst_err) || {
    printf '  transport error: %s\n' "$(cat /tmp/myst_err)"; return 1; }
  printf '  HTTP %s\n' "$code"
  if [[ -s /tmp/myst_body ]]; then
    jq -C . </tmp/myst_body 2>/dev/null || head -c 800 /tmp/myst_body
    printf '\n'
  else
    printf '  (empty body)\n'
  fi
  LAST_CODE="$code"
  return 0
}

printf 'base:   %s\ntoken:  %s\n' "$BASE_URL" "$masked"

hr "1. GET /auth/config  (no auth — confirms host reachable + shows providers)"
cfg_status=$(curl -sS -o /tmp/myst_body -w '%{http_code}' \
  -H 'Accept: application/json' "$BASE_URL/auth/config" 2>/tmp/myst_err) || {
    printf '  transport error: %s\n' "$(cat /tmp/myst_err)"; cfg_status=000; }
printf '  HTTP %s\n' "$cfg_status"
if [[ -s /tmp/myst_body ]]; then jq -C . </tmp/myst_body 2>/dev/null || head -c 800 /tmp/myst_body; echo; fi
if [[ "$cfg_status" == "000" ]]; then
  printf '\n\033[31mHost unreachable — check BASE_URL.\033[0m\n'; exit 1
fi

hr "2. GET /auth/check  (validates your token)"
call GET /auth/check
auth_code="$LAST_CODE"

hr "3. GET /connection/config  (read-only; does your token authorise connection APIs?)"
call GET "/connection/config?ip_type=$IP_TYPE"
cfg_code="$LAST_CODE"

if [[ "$auth_code" == "401" || "$auth_code" == "403" ]]; then
  hr "Result"
  printf '\033[31mYour token was rejected for /auth/check (%s).\033[0m\n' "$auth_code"
  printf 'Get a fresh token from your own logged-in browser session, then retry.\n'
  exit 1
fi

if [[ "$cfg_code" == "401" || "$cfg_code" == "403" ]]; then
  hr "Result"
  printf '\033[33mToken is valid, but connection APIs returned %s.\033[0m\n' "$cfg_code"
  printf 'This is the client-attestation question. Inspect the body above:\n'
  printf '  - if it names a client secret / app attestation -> that is an access\n'
  printf '    control. Stop here; do not replay a secret from a shipped binary.\n'
  printf '  - if it is a scope/permission or subscription issue -> check your plan.\n'
  exit 2
fi

hr "4. POST /connection/connect-proxy  (the actual test)"
body=$(jq -n --arg c "$COUNTRY" --arg ip "$IP_TYPE" --arg os "$OS_TYPE" \
  '{country:$c, ip_type:$ip, reset_connection:false}
   + (if $os == "" then {} else {os_type:$os} end)')
printf '  request: %s\n' "$(jq -c . <<<"$body")"
call POST /connection/connect-proxy "$body"
proxy_code="$LAST_CODE"

hr "Result"
case "$proxy_code" in
  200)
    printf '\033[32mSUCCESS — connect-proxy works with a plain bearer token.\033[0m\n'
    printf 'proxy_config above is what a Linux client would consume.\n'
    printf 'Note expires_at: credentials rotate, so plan a refresh timer.\n'
    ;;
  401|403)
    printf '\033[31mRejected (%s).\033[0m Body above tells you why.\n' "$proxy_code"
    printf 'If it demands client attestation, that is the line — stop.\n'
    ;;
  404)
    printf '\033[33m404 — path or base URL prefix is wrong.\033[0m\n'
    printf 'Check whether BASE_URL should include /api/v1.\n'
    ;;
  429)
    printf '\033[33mRate limited. Wait and retry.\033[0m\n'
    ;;
  *)
    printf '\033[33mUnexpected %s — see body above.\033[0m\n' "$proxy_code"
    ;;
esac
