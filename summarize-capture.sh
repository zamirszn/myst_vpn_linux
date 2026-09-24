#!/usr/bin/env bash
# Summarize capture-api.js output: which hosts were contacted, and which look like APIs.
#
# Usage:  ./summarize-capture.sh [path-to-requests.ndjson]
#
# Prints metadata only — the log it reads contains no header values and no query strings.

set -uo pipefail
LOG="${1:-$HOME/.myst-capture/requests.ndjson}"

if [[ ! -s "$LOG" ]]; then
  echo "no capture at $LOG — run capture-api.js first" >&2
  exit 1
fi

# Noise we never care about.
NOISE='google|gstatic|googleapis|doubleclick|facebook|fbcdn|segment|sentry|intercom|stripe\.com|cloudflare|cloudfront|fonts\.|jquery|hotjar|clarity|analytics|cdn\.jsdelivr|unpkg'

# Tolerate a non-JSON comment line, so this works against captures from either
# the current script or an earlier revision of it.
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT
grep -v '^[[:space:]]*#' "$LOG" | grep -v '^[[:space:]]*$' >"$TMP"

hr() { printf '\n\033[1m── %s\033[0m\n' "$1"; }

hr "hosts contacted (by request count)"
jq -rs --arg noise "$NOISE" '
  [ .[] | select(.host) | select(.host | test($noise) | not) ]
  | group_by(.host)
  | map({ host: .[0].host, n: length })
  | sort_by(-.n)
  | .[]
  | "\(.n|tostring|.[0:5]|(" " * (5 - length)) + .)  \(.host)"
' "$TMP" 2>/dev/null | sed 's/^/  /'

hr "hosts that carried auth headers (Bearer / Cookie / api-key)"
jq -rs --arg noise "$NOISE" '
  [ .[] | select(.host) | select(.host | test($noise) | not)
         | select((.authHeaders // []) | length > 0) ]
  | group_by(.host)
  | map({ host: .[0].host,
          headers: ([.[].authHeaders[]] | unique),
          n: length })
  | sort_by(-.n)
  | .[]
  | "  \(.host)  [\(.n) reqs]  headers: \(.headers | join(", "))"
' "$TMP" 2>/dev/null

hr "XHR / fetch calls (candidate API surface)"
jq -rs --arg noise "$NOISE" '
  [ .[] | select(.kind == "response") | select(.host | test($noise) | not) ]
  | map("\(.method) \(.path)  -> \(.status)   [\(.host)]")
  | unique
  | .[]
' "$TMP" 2>/dev/null | sed 's/^/  /'

hr "XHR / fetch paths with NO response logged yet"
jq -rs --arg noise "$NOISE" '
  [ .[] | select(.kind == "request") | select(.type == "xhr" or .type == "fetch")
         | select(.host | test($noise) | not) ]
  | map("\(.method) \(.path)   [\(.host)]")
  | unique
  | .[]
' "$TMP" 2>/dev/null | sed 's/^/  /'
