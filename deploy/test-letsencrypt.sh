#!/usr/bin/env bash
set -Eeuo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_root="$(mktemp -d)"
trap 'rm -rf "$tmp_root"' EXIT

make_fixture() {
  local name="$1"
  local fixture="$tmp_root/$name"
  mkdir -p "$fixture/bin" "$fixture/deploy/nginx"
  cp "$repo_root/deploy/nginx/letsencrypt.sh" "$fixture/deploy/nginx/letsencrypt.sh"
  printf 'LETSENCRYPT_EMAIL=test@example.invalid\nPUBLIC_HOSTNAME=203.0.113.10\n' > "$fixture/.env"

  cat > "$fixture/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$DOCKER_LOG"
if [[ "${1:-}" == compose && "${2:-}" == ps ]]; then
  [[ "${NGINX_RUNNING:-false}" == true ]] && printf 'nginx\n'
  exit 0
fi
if [[ "$*" == *'certonly'* ]]; then
  cert_dir="$PWD/deploy/nginx/letsencrypt/live/dexarb-edge-ip"
  mkdir -p "$cert_dir"
  : > "$cert_dir/fullchain.pem"
  : > "$cert_dir/privkey.pem"
fi
EOF
  cat > "$fixture/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -eu
[[ "${WEBROOT_REACHABLE:-false}" == true ]] || exit 22
url="${*: -1}"
printf '%s' "${url##*/}"
EOF
  cat > "$fixture/bin/openssl" <<'EOF'
#!/usr/bin/env bash
set -eu
if [[ "$*" == *'subjectAltName'* ]]; then
  printf 'X509v3 Subject Alternative Name:\n    IP Address:203.0.113.10\n'
fi
EOF
  chmod +x "$fixture/bin/"*
  printf '%s\n' "$fixture"
}

run_fixture() {
  local fixture="$1"
  shift
  (
    cd "$fixture"
    env PATH="$fixture/bin:$PATH" \
      DEXARB_EDGE_CONFIG="$fixture/missing-edge.env" \
      EDGE_CERT_TYPE=ip EDGE_CERT_IDENTIFIER=203.0.113.10 \
      EDGE_HTTP01_WEBROOT="$fixture/deploy/nginx/acme-webroot" \
      DOCKER_LOG="$fixture/docker.log" "$@" \
      bash deploy/nginx/letsencrypt.sh renew
  )
}

running_ok="$(make_fixture running-ok)"
run_fixture "$running_ok" NGINX_RUNNING=true WEBROOT_REACHABLE=true
grep -q -- '--webroot --webroot-path /var/www/certbot' "$running_ok/docker.log"
! grep -q -- '-p 80:80' "$running_ok/docker.log"

running_failed="$(make_fixture running-failed)"
if run_fixture "$running_failed" NGINX_RUNNING=true WEBROOT_REACHABLE=false >"$running_failed/output" 2>&1; then
  echo "expected a failed webroot probe to abort while nginx is running" >&2
  exit 1
fi
grep -q 'refusing standalone mode because port 80 is already in use' "$running_failed/output"
! grep -q -- '-p 80:80' "$running_failed/docker.log"

stopped="$(make_fixture stopped)"
run_fixture "$stopped" NGINX_RUNNING=false WEBROOT_REACHABLE=false
grep -q -- '-p 80:80' "$stopped/docker.log"
grep -q -- '--standalone' "$stopped/docker.log"

[[ "$(stat -c %a "$stopped/deploy/nginx/acme-webroot")" == 755 ]]
[[ "$(stat -c %a "$stopped/deploy/nginx/acme-webroot/.well-known")" == 755 ]]
[[ "$(stat -c %a "$stopped/deploy/nginx/acme-webroot/.well-known/acme-challenge")" == 755 ]]

echo "letsencrypt.sh regression tests passed"
