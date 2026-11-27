#!/usr/bin/env bash
# Check the public monitor edge (nginx/public.conf) from the outside.
#
#   scripts/public-edge-check.sh https://monitor.example.com
#   INSECURE=1 scripts/public-edge-check.sh https://localhost:8443   # self-signed
#   CHECK_ROUTES=1 scripts/public-edge-check.sh https://monitor.example.com
#
# Asserts that the public read routes answer, the execution panel's trading
# calls get the canned public_monitor answer, and every other API, auth,
# trading and internal path stays blocked. CHECK_ROUTES=1 also asserts that
# live offers with a positive premium are priced with a route; that catches a
# public seed that stopped matching the routing rules (net premium 0 everywhere).
set -euo pipefail

base="${1:?usage: $0 <base-url>}"
base="${base%/}"
curl_opts=(-sS --max-time 30)
[[ "${INSECURE:-}" == 1 ]] && curl_opts+=(-k)

fail=0

status_of() {
  local method="$1" path="$2"
  local extra=()
  if [[ "$method" == POST || "$method" == PUT ]]; then
    extra=(-H 'Content-Type: application/json' --data '{}')
  fi
  curl "${curl_opts[@]}" -o /dev/null -w '%{http_code}' -X "$method" "${extra[@]}" "$base$path" || echo 000
}

# expect METHOD PATH ALLOWED_STATUS...
expect() {
  local method="$1" path="$2"
  shift 2
  local got want
  got="$(status_of "$method" "$path")"
  for want in "$@"; do
    if [[ "$got" == "$want" ]]; then
      echo "✓ $method $path -> $got"
      return 0
    fi
  done
  echo "✖ $method $path -> $got (want one of: $*)" >&2
  fail=1
}

echo "Public read routes"
for path in / /health /api/live_offers /api/live_offers/counts /api/trade_cost_config \
  /api/cex_permissions /api/past_offers /api/ohlc /api/payment_method_groups; do
  expect GET "$path" 200
done
# The analytics POSTs reach dexMonitor; an empty body may be rejected by the
# app itself (400), but never by the edge or the login gate.
expect POST /api/dashboard/analytics 200 400
expect POST /api/statistics 200 400

echo "Execution panel gets the canned answer"
for path in /api/execution-quotes /api/mm-controller/cex/tradeflow /api/mm-controller/cex/pre-trade-check \
  /api/mm-controller/bisq/execution/quote /api/mm-controller/bisq/execution/accept; do
  expect POST "$path" 503
done
body="$(curl "${curl_opts[@]}" -X POST -H 'Content-Type: application/json' --data '{}' "$base/api/execution-quotes" || true)"
if [[ "$body" == *'"code":"public_monitor"'* ]]; then
  echo "✓ canned answer carries code public_monitor"
else
  echo "✖ canned answer body unexpected: ${body:0:200}" >&2
  fail=1
fi

echo "Blocked: writes on read routes"
expect PUT /api/cex_permissions 403 404 405
expect PUT /api/cex_permissions/kraken 403 404 405
expect POST /api/live_offers 403 404 405
expect GET /api/statistics 403 404 405

echo "Blocked: login, trading, settings and internal paths"
for path in /api/auth/me /api/auth/registration_available /api/trading/accounts /api/trading/payment_methods \
  /api/app-settings /api/mm-controller/lnd/wallet /api/mm-controller/transactional-liquidity \
  /api/phone-push/setup /api/payment_notifications /api/past_order_stats /api/general_stats/overview \
  /internal/live_offers /internal/cex_permissions /metrics /ws/cex/hedge-stream /grafana/; do
  expect GET "$path" 403 404
done
for path in /api/auth/register /api/auth/login /api/accounts/health_check /internal/live_offers \
  /telegram-webhook /pushpush/notify /pushpush/phone-heartbeat; do
  expect POST "$path" 403 404 405
done

if [[ "${CHECK_ROUTES:-}" == 1 ]]; then
  echo "Live offers are priced with a route"
  command -v jq >/dev/null || { echo "✖ CHECK_ROUTES needs jq" >&2; exit 2; }
  offers="$(curl "${curl_opts[@]}" "$base/api/live_offers")"
  positive="$(jq '[.[] | select((.pricing.raw_premium_percent // 0) > 0)] | length' <<<"$offers")"
  routed="$(jq '[.[] | select((.pricing.raw_premium_percent // 0) > 0 and ((.analysis.full_volume.route.cex // "") != ""))] | length' <<<"$offers")"
  if [[ "$positive" -eq 0 ]]; then
    echo "• no live offer has a positive premium right now; nothing to check"
  elif [[ "$routed" -eq 0 ]]; then
    echo "✖ none of $positive positive-premium live offers has a route: the public seed no longer opens routing" >&2
    fail=1
  else
    echo "✓ $routed of $positive positive-premium live offers have a route"
  fi
fi

if [[ "$fail" -ne 0 ]]; then
  echo >&2
  echo "✖ public edge check FAILED" >&2
  exit 1
fi
echo
echo "✓ public edge check passed"
