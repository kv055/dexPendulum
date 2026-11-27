#!/usr/bin/env bash
# Assert the docker-compose network topology is what the bridge-network
# migration expects. Runs `docker compose config` (which fully resolves
# interpolation and env_file merges) and checks:
#   - no service uses network_mode: host
#   - mm-controller reaches every upstream by docker DNS, not localhost
#   - lnd's TLS cert carries the "lnd" SAN so https://lnd:8080 verifies
#   - dexmonitor reaches mm-controller by docker DNS, not host.docker.internal
#
# Designed to run both locally (with real .env files) and in CI (without):
# any env_file referenced by a service that doesn't exist on disk is
# materialized as an empty placeholder for the duration of the check, then
# removed. Only files this script created are removed — pre-existing .env
# files are left untouched.
#
# Usage: bash scripts/compose-validate.sh [path/to/docker-compose.yml]
set -euo pipefail

COMPOSE_FILE="${1:-docker-compose.yml}"
PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$PROJECT_DIR"

if ! command -v docker >/dev/null 2>&1; then
	echo "✖ docker not found on PATH" >&2
	exit 2
fi

# env_file references look like "  - ./some/.env" or "  - ./some/.env:ro".
mapfile -t ENV_FILES < <(
	awk '
		/^[[:space:]]*env_file:/ { in_env=1; next }
		in_env && /^[[:space:]]*-/ {
			line=$0; sub(/^[[:space:]]*-[[:space:]]*/,"",line); sub(/:ro$/,"",line);
			gsub(/^\.\/+/, "", line); print line; next
		}
		in_env && !/^[[:space:]]*-/ { in_env=0 }
	' "$COMPOSE_FILE"
)

created=()
cleanup() {
	for f in "${created[@]:-}"; do
		if [ -f "$f" ] && [ ! -s "$f" ]; then
			rm -f "$f"
		fi
	done
	# Remove dirs we created only to host a placeholder .env (if empty).
	for d in dexUnifiedApi dexMonitor dexTakerController; do
		if [ -d "$d" ] && [ -z "$(ls -A "$d" 2>/dev/null)" ]; then
			rmdir "$d" 2>/dev/null || true
		fi
	done
}
trap cleanup EXIT

for f in "${ENV_FILES[@]:-}"; do
	if [ -n "$f" ] && [ ! -f "$f" ]; then
		mkdir -p "$(dirname "$f")"
		: >"$f"
		created+=("$f")
	fi
done

CONFIG="$(docker compose -f "$COMPOSE_FILE" config 2>/dev/null)"
# Mail is intentionally opt-in because its live deployment requires external
# DNS/TLS material and an IMAP credential. Validate its topology separately
# while keeping the normal render free of unprovisioned mail services.
MAIL_CONFIG="$(COMPOSE_PROFILES=mail MADDY_HOSTNAME=mx.example.test MADDY_DOMAIN=example.test MADDY_RECIPIENT=payments@example.test MADDY_IMAP_PASSWORD=validation-password docker compose -f "$COMPOSE_FILE" config 2>/dev/null)"
# The co-hosted public monitor is likewise opt-in (profile "public"): validate
# its restricted shape separately while the default render stays unchanged.
PUBLIC_CONFIG="$(COMPOSE_PROFILES=public docker compose -f "$COMPOSE_FILE" config 2>/dev/null)"

fail=0

# The runtime file ships to prod and end users without the source tree, so it
# must not build anything; build contexts live in docker-compose.override.yml.
BUILD_SERVICES="$(docker compose -f "$COMPOSE_FILE" config --format json 2>/dev/null |
	python3 -c 'import json, sys; print(" ".join(sorted(n for n, s in json.load(sys.stdin)["services"].items() if "build" in s)))')"
if [ -n "$BUILD_SERVICES" ]; then
	echo "✖ $COMPOSE_FILE must be image-only (build: found in: $BUILD_SERVICES; move it to docker-compose.override.yml)" >&2
	fail=1
else
	echo "✓ $COMPOSE_FILE is image-only (no build: keys)"
fi
# Prefer here-strings over printf|grep -q: under pipefail, an early grep -q exit
# SIGPIPEs the writer and turns a real match into a false miss (seen on CI).
assert_contains() {
	local label="$1" needle="$2"
	if grep -qF -- "$needle" <<<"$CONFIG"; then
		echo "✓ $label"
	else
		echo "✖ $label (missing: $needle)" >&2
		fail=1
	fi
}
assert_absent() {
	local label="$1" needle="$2"
	if grep -qF -- "$needle" <<<"$CONFIG"; then
		echo "✖ $label (forbidden but present: $needle)" >&2
		fail=1
	else
		echo "✓ $label"
	fi
}

# assert_in_service checks that needle appears within one service's resolved
# config block. Used for env vars that aren't unique across services (e.g.
# INTERNAL_API_SECRET is also set on unified-api/unified-snapshot, so a global
# grep would pass even if mm-controller's were removed).
assert_in_service() {
	local label="$1" service="$2" needle="$3"
	local block
	block=$(awk -v svc="  ${service}:" '
		$0 == svc { inblk=1; print; next }
		inblk && /^  [A-Za-z][A-Za-z0-9_-]*:$/ { inblk=0 }
		inblk { print }
	' <<<"$CONFIG")
	if grep -qF -- "$needle" <<<"$block"; then
		echo "✓ $label"
	else
		echo "✖ $label (missing in $service: $needle)" >&2
		fail=1
	fi
}

# assert_in_public_service is assert_in_service against the COMPOSE_PROFILES
# render (the co-hosted public monitor only exists there).
assert_in_public_service() {
	local label="$1" service="$2" needle="$3"
	local block
	block=$(awk -v svc="  ${service}:" '
		$0 == svc { inblk=1; print; next }
		inblk && /^  [A-Za-z][A-Za-z0-9_-]*:$/ { inblk=0 }
		inblk { print }
	' <<<"$PUBLIC_CONFIG")
	if grep -qF -- "$needle" <<<"$block"; then
		echo "✓ $label"
	else
		echo "✖ $label (missing in $service: $needle)" >&2
		fail=1
	fi
}

assert_absent_in_public_service() {
	local label="$1" service="$2" needle="$3"
	local block
	block=$(awk -v svc="  ${service}:" '
		$0 == svc { inblk=1; print; next }
		inblk && /^  [A-Za-z][A-Za-z0-9_-]*:$/ { inblk=0 }
		inblk { print }
	' <<<"$PUBLIC_CONFIG")
	if grep -qF -- "$needle" <<<"$block"; then
		echo "✖ $label (forbidden but present in $service: $needle)" >&2
		fail=1
	else
		echo "✓ $label"
	fi
}

echo "Checking compose topology from $COMPOSE_FILE"

# No service may use host networking — everything must be on the bridge.
assert_absent "no service uses network_mode: host" "network_mode: host"

# mm-controller reaches every upstream by docker DNS.
assert_contains "mm-controller BISQ_GRPC_HOST=bisq:9998"         "BISQ_GRPC_HOST: bisq:9998"
assert_contains "mm-controller POSTGRES_HOST=postgres"           "POSTGRES_HOST: postgres"
assert_contains "mm-controller LND_HOST=https://lnd:8080"        "LND_HOST: https://lnd:8080"
assert_contains "mm-controller TOR_PROXY_URL=socks5h://tor:9050" "TOR_PROXY_URL: socks5h://tor:9050"
assert_in_service "mm-controller INTERNAL_API_SECRET wired (protected API)" "mm-controller" "INTERNAL_API_SECRET:"

# lnd's cert must include the docker DNS name so https://lnd:8080 verifies.
assert_contains "lnd TLS cert includes lnd SAN" "--tlsextradomain=lnd"

# dexmonitor reaches mm-controller by docker DNS, not host.docker.internal.
assert_contains "dexmonitor reaches mm-controller by DNS" "MM_CONTROLLER_API_URL: http://mm-controller:5001"
assert_absent "no host.docker.internal in dexmonitor"     "host.docker.internal:5001"

# Maddy is receive-only when enabled: SMTP 25 is public, IMAP stays on the
# bridge, and no submission ports exist. The maddy services are currently
# commented out in compose — keep that allowed, and only enforce live-mail
# topology when the mail profile actually renders a maddy service.
if grep -qE '^  maddy(-bootstrap|-backup)?:$' <<<"$CONFIG"; then
	echo "✖ Maddy is unexpectedly enabled without the mail profile" >&2
	fail=1
else
	echo "✓ Maddy is disabled without the mail profile"
fi
if grep -qE '^  maddy:$' <<<"$MAIL_CONFIG"; then
	if grep -qF -- "image: foxcpp/maddy:0.9.5" <<<"$MAIL_CONFIG"; then
		echo "✓ Maddy image is pinned"
	else
		echo "✖ Maddy image is pinned (missing: image: foxcpp/maddy:0.9.5)" >&2
		fail=1
	fi
	if grep -qF -- 'published: "25"' <<<"$MAIL_CONFIG"; then
		echo "✓ Maddy publishes SMTP 25"
	else
		echo "✖ Maddy publishes SMTP 25 (missing published port)" >&2
		fail=1
	fi
	if grep -qF -- 'published: "993"' <<<"$MAIL_CONFIG"; then
		echo "✖ Maddy does not publish IMAP 993 (forbidden but present)" >&2
		fail=1
	else
		echo "✓ Maddy does not publish IMAP 993"
	fi
else
	echo "✓ Maddy mail stack inactive (commented out); skipping live-mail topology checks"
fi
if grep -qF -- "target: 465" <<<"$MAIL_CONFIG"; then
	echo "✖ no SMTP submission port 465 (forbidden but present)" >&2
	fail=1
else
	echo "✓ no SMTP submission port 465"
fi
if grep -qF -- "target: 587" <<<"$MAIL_CONFIG"; then
	echo "✖ no SMTP submission port 587 (forbidden but present)" >&2
	fail=1
else
	echo "✓ no SMTP submission port 587"
fi
assert_in_service "fiat-controller uses dedicated payment IMAP settings" "fiat-controller" "PAYMENT_EMAIL_IMAP_HOST: maddy"

# --- Co-hosted public monitor (profile "public") --------------------------
# Off by default: the default render must not contain the public instance at
# all, so a profile-less deployment is byte-for-byte the trading stack.
if grep -qE '^  dexmonitor-public:$' <<<"$CONFIG"; then
	echo "✖ dexmonitor-public renders without the public profile" >&2
	fail=1
else
	echo "✓ dexmonitor-public is off without the public profile"
fi
assert_absent "no PUBLIC_EDITION without the public profile" "PUBLIC_EDITION"

# On with the profile: restricted role, no secrets, shared images only.
if grep -qE '^  dexmonitor-public:$' <<<"$PUBLIC_CONFIG"; then
	echo "✓ dexmonitor-public renders with the public profile"
else
	echo "✖ dexmonitor-public is missing from the public profile render" >&2
	fail=1
fi
assert_in_public_service "public instance runs PUBLIC_EDITION=true"          "dexmonitor-public" 'PUBLIC_EDITION: "true"'
assert_in_public_service "public instance connects as dex_monitor_public"    "dexmonitor-public" "DATABASE_URL: postgres://dex_monitor_public:"
assert_in_public_service "public instance shares the operator tor"           "dexmonitor-public" "TOR_PROXY_ADDR: tor:9050"
# The release digest tooling pins dexmonitor-public as an alias of dexmonitor
# only when both resolve the same image reference.
if [ "$(awk '/^  dexmonitor:$/{f=1} f&&/^    image:/{print; exit}' <<<"$PUBLIC_CONFIG")" = \
	"$(awk '/^  dexmonitor-public:$/{f=1} f&&/^    image:/{print; exit}' <<<"$PUBLIC_CONFIG")" ]; then
	echo "✓ dexmonitor-public shares the dexmonitor image reference"
else
	echo "✖ dexmonitor-public must reuse the exact dexmonitor image reference" >&2
	fail=1
fi
# No operator secrets may reach the public instance.
assert_absent_in_public_service "public instance has no internal API secret"  "dexmonitor-public" "INTERNAL_API_SECRET"
assert_absent_in_public_service "public instance has no encryption key"       "dexmonitor-public" "APP_ENCRYPTION_KEY"
assert_absent_in_public_service "public instance has no Bisq credentials"     "dexmonitor-public" "BISQ_API_PASSWORD"
assert_absent_in_public_service "public instance has no mm-controller URL"    "dexmonitor-public" "MM_CONTROLLER_API_URL"
assert_absent_in_public_service "public instance has no fiat-controller URL"  "dexmonitor-public" "FIAT_CONTROLLER_URL"
assert_absent_in_public_service "public instance has no push secrets"         "dexmonitor-public" "PUSHPUSH_NOTIFY_SECRET"
# Loopback-only publish: nothing forwards to it publicly until a domain edge
# exists.
if awk '/^  dexmonitor-public:$/{f=1; next} f&&/^  [a-z]/{exit} f&&/host_ip:/{print; exit}' <<<"$PUBLIC_CONFIG" | grep -q 'host_ip: 127.0.0.1'; then
	echo "✓ dexmonitor-public publishes on loopback only"
else
	echo "✖ dexmonitor-public must publish on loopback only" >&2
	fail=1
fi

# unified-api carries the co-host hooks with safe empty defaults.
assert_in_service "unified-api PUBLIC_LISTEN_ADDR wired (empty default)"      "unified-api" 'PUBLIC_LISTEN_ADDR: ""'
assert_in_service "unified-api POSTGRES_MONITOR_PUBLIC_PASSWORD wired"        "unified-api" 'POSTGRES_MONITOR_PUBLIC_PASSWORD: ""'


if [ "$fail" -ne 0 ]; then
	echo >&2
	echo "✖ compose topology validation FAILED" >&2
	exit 1
fi
echo
echo "✓ compose topology validation passed"
