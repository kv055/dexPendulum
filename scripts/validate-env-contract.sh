#!/usr/bin/env bash
# Validate the production dotenv contract without ever printing secret values.
#
# Usage:
#   validate-env-contract.sh keys [repository-root]
#   validate-env-contract.sh values <decrypted-bundle-root>
#   validate-env-contract.sh file <contract> <keys|values> <path>
#
# Bundle layout mirrors the repository:
#   .env
#   dexUnifiedApi/.env
#   dexMonitor/.env
#   dexTakerController/.env
#   dexFiatController/.env
set -euo pipefail

ROOT_REQUIRED=(
  APP_ENCRYPTION_KEY
  INTERNAL_API_SECRET
  PUBLIC_HOSTNAME
  TELEGRAM_WEBHOOK_SECRET
  BISQ_API_PASSWORD
  POSTGRES_DB
  POSTGRES_ADMIN_USER
  POSTGRES_ADMIN_PASSWORD
  POSTGRES_UNIFIED_API_PASSWORD
  POSTGRES_MONITOR_PASSWORD
  POSTGRES_MM_CONTROLLER_PASSWORD
  POSTGRES_FIAT_CONTROLLER_PASSWORD
  GRAFANA_ADMIN_PASSWORD
)

# Provisioned hosts read this from root-owned /etc/dexarb/edge.env. Keep the
# dotenv fallback key present for older hosts, but allow it to be empty.
ROOT_PRESENT=(
  LETSENCRYPT_EMAIL
  # VPS deployments derive this from /etc/dexarb/edge.env. Keep the dotenv key
  # as a local-development fallback, but allow it to be empty.
  DEXMM_TELEGRAM_WEBHOOK_URL
  # Co-hosted public monitor (profile "public", see deploy/public/README.md).
  # All three stay empty when the public monitor is off; enabling it means
  # COMPOSE_PROFILES=public, PUBLIC_LISTEN_ADDR=:5011 on unified-api, and a
  # generated role password for dex_monitor_public.
  POSTGRES_MONITOR_PUBLIC_PASSWORD
  PUBLIC_LISTEN_ADDR
  COMPOSE_PROFILES
)

UNIFIED_API_REQUIRED=(
  DATABASE_URL
  POSTGRES_DB
  POSTGRES_HOST
  POSTGRES_PORT
  POSTGRES_USER
  POSTGRES_PASSWORD
  POSTGRES_ADMIN_USER
  POSTGRES_ADMIN_PASSWORD
  POSTGRES_UNIFIED_API_PASSWORD
  POSTGRES_MONITOR_PASSWORD
  POSTGRES_MM_CONTROLLER_PASSWORD
  POSTGRES_FIAT_CONTROLLER_PASSWORD
)

UNIFIED_API_PRESENT=(
  BISQ_API_PASSWORD
)

MONITOR_REQUIRED=(
  DATABASE_URL
  POSTGRES_DB
  POSTGRES_HOST
  POSTGRES_PORT
  POSTGRES_USER
  POSTGRES_PASSWORD
)

MONITOR_PRESENT=(
  BISQ_API_PASSWORD
)

MM_CONTROLLER_REQUIRED=(
  DATABASE_URL
  POSTGRES_DB
  POSTGRES_HOST
  POSTGRES_PORT
  POSTGRES_USER
  POSTGRES_PASSWORD
)

FIAT_CONTROLLER_REQUIRED=(
  DATABASE_URL
  POSTGRES_DB
  POSTGRES_HOST
  POSTGRES_PORT
  POSTGRES_USER
  POSTGRES_PASSWORD
)

usage() {
  echo "usage: $0 keys [repository-root] | values <bundle-root> | file <contract> <keys|values> <path>" >&2
  exit 2
}

contract_keys() {
  case "$1" in
    root) printf '%s\n' "${ROOT_REQUIRED[@]}" ;;
    unified-api) printf '%s\n' "${UNIFIED_API_REQUIRED[@]}" ;;
    monitor) printf '%s\n' "${MONITOR_REQUIRED[@]}" ;;
    mm-controller) printf '%s\n' "${MM_CONTROLLER_REQUIRED[@]}" ;;
    fiat-controller) printf '%s\n' "${FIAT_CONTROLLER_REQUIRED[@]}" ;;
    *) echo "unknown environment contract: $1" >&2; return 2 ;;
  esac
}

contract_present_keys() {
  case "$1" in
    root) printf '%s\n' "${ROOT_PRESENT[@]}" ;;
    unified-api) printf '%s\n' "${UNIFIED_API_PRESENT[@]}" ;;
    monitor) printf '%s\n' "${MONITOR_PRESENT[@]}" ;;
    mm-controller|fiat-controller) ;;
    *) echo "unknown environment contract: $1" >&2; return 2 ;;
  esac
}

is_required_key() {
  local contract="$1" wanted="$2" candidate
  while IFS= read -r candidate; do
    [[ "$candidate" == "$wanted" ]] && return 0
  done < <(contract_keys "$contract")
  return 1
}

value_for() {
  local file="$1" key="$2"
  awk -v wanted="$key" '
    index($0, wanted "=") == 1 {
      print substr($0, length(wanted) + 2)
      exit
    }
  ' "$file"
}

validate_file() {
  local contract="$1" mode="$2" file="$3"
  local key value normalized fail=0

  if [[ ! -f "$file" ]]; then
    echo "ERROR: missing environment file for $contract: $file" >&2
    return 1
  fi

  while IFS= read -r key; do
    if ! grep -qE "^${key}=" "$file"; then
      echo "ERROR: $file is missing required key $key" >&2
      fail=1
      continue
    fi
    value="$(value_for "$file" "$key")"

    case "$mode" in
      keys)
        if [[ ! "$value" =~ ^\"?encrypted: ]]; then
          echo "ERROR: $file has an unencrypted value for required key $key" >&2
          fail=1
        fi
        ;;
      values)
        normalized="${value%$'\r'}"
        normalized="${normalized#\"}"
        normalized="${normalized%\"}"
        normalized="${normalized#\'}"
        normalized="${normalized%\'}"
        if [[ -z "$normalized" ]] && is_required_key "$contract" "$key"; then
          echo "ERROR: $file has an empty required value for $key" >&2
          fail=1
        fi
        ;;
      *) usage ;;
    esac
  done < <({ contract_keys "$contract"; contract_present_keys "$contract"; })

  return "$fail"
}

validate_tree() {
  local mode="$1" root="$2"
  local suffix
  case "$mode" in
    keys) suffix=".env.enc" ;;
    values) suffix=".env" ;;
    *) usage ;;
  esac

  validate_file root "$mode" "$root/$suffix"
  validate_file unified-api "$mode" "$root/dexUnifiedApi/$suffix"
  validate_file monitor "$mode" "$root/dexMonitor/$suffix"
  validate_file mm-controller "$mode" "$root/dexTakerController/$suffix"
  validate_file fiat-controller "$mode" "$root/dexFiatController/$suffix"

  # The co-hosted public monitor must not be enabled half-way: with its
  # profile active but no role password, the container would come up unable
  # to authenticate and fail the deploy at the healthcheck — catch it in the
  # bundle instead, before anything restarts.
  if [[ "$mode" == values ]]; then
    local profiles public_password
    profiles="$(value_for "$root/$suffix" COMPOSE_PROFILES)"
    public_password="$(value_for "$root/$suffix" POSTGRES_MONITOR_PUBLIC_PASSWORD)"
    profiles="${profiles%%$'\r'}"
    public_password="${public_password%%$'\r'}"
    if [[ ",$profiles," == *",public,"* && -z "$public_password" ]]; then
      echo "ERROR: $root/$suffix enables the public profile without POSTGRES_MONITOR_PUBLIC_PASSWORD" >&2
      return 1
    fi
  fi
}

command="${1:-}"
case "$command" in
  keys)
    validate_tree keys "${2:-$(git rev-parse --show-toplevel)}"
    ;;
  values)
    [[ $# -eq 2 ]] || usage
    validate_tree values "$2"
    ;;
  file)
    [[ $# -eq 4 ]] || usage
    validate_file "$2" "$3" "$4"
    ;;
  *)
    usage
    ;;
esac
