#!/usr/bin/env bash
# Issue or refresh the active public edge certificate and install it for nginx.
# Run from the repository root on the VPS. Ansible writes /etc/dexarb/edge.env;
# otherwise PUBLIC_HOSTNAME / LETSENCRYPT_EMAIL from .env are used. There is
# no built-in default: an unset hostname fails instead of requesting a
# certificate for someone else's domain.
set -Eeuo pipefail

action="${1:-renew}"
edge_config="${DEXARB_EDGE_CONFIG:-/etc/dexarb/edge.env}"
env_file=".env"

if [[ -r "$edge_config" ]]; then
  # This file is root-owned Ansible output containing only quoted scalar values.
  # shellcheck disable=SC1090
  source "$edge_config"
fi

configured_email=""
configured_domain=""
if [[ -f "$env_file" ]]; then
  configured_email="$(sed -n 's/^LETSENCRYPT_EMAIL=//p' "$env_file" | tail -n 1 | tr -d '\r')"
  configured_domain="$(sed -n 's/^PUBLIC_HOSTNAME=//p' "$env_file" | tail -n 1 | tr -d '\r')"
fi

cert_type="${EDGE_CERT_TYPE:-domain}"
cert_identifier="${EDGE_CERT_IDENTIFIER:-$configured_domain}"
letsencrypt_email="${LETSENCRYPT_EMAIL:-$configured_email}"
certbot_image="${CERTBOT_IMAGE:-certbot/certbot:v5.6.0}"
http01_webroot="${EDGE_HTTP01_WEBROOT:-$PWD/deploy/nginx/acme-webroot}"

[[ -n "$letsencrypt_email" ]] || { echo "LETSENCRYPT_EMAIL must be configured" >&2; exit 2; }
[[ -n "$cert_identifier" ]] || { echo "PUBLIC_HOSTNAME (or EDGE_CERT_IDENTIFIER) must be configured" >&2; exit 2; }

case "$cert_type" in
  ip)
    cert_name="dexarb-edge-ip"
    ;;
  domain)
    cert_name="dexarb-edge-domain"
    ;;
  *)
    echo "EDGE_CERT_TYPE must be ip or domain" >&2
    exit 2
    ;;
esac

mkdir -p deploy/nginx/{acme-webroot,certs,letsencrypt}
cert_dir="deploy/nginx/letsencrypt/live/$cert_name"
cert_file="$cert_dir/fullchain.pem"
key_file="$cert_dir/privkey.pem"

nginx_running() {
  docker compose ps --status running --services nginx 2>/dev/null | grep -qx nginx
}

prepare_webroot() {
  local challenge_dir="$http01_webroot/.well-known/acme-challenge"

  mkdir -p "$challenge_dir"
  # nginx runs as an unprivileged user in its container. Every bind-mounted
  # directory in the challenge path must therefore be world-searchable.
  chmod 0755 "$http01_webroot" "$http01_webroot/.well-known" "$challenge_dir"
}

webroot_reachable() {
  local probe_name="dexarb-acme-probe-$$"
  local probe_dir="$http01_webroot/.well-known/acme-challenge"
  local probe_file="$probe_dir/$probe_name"
  local response=""

  prepare_webroot
  printf '%s' "$probe_name" > "$probe_file"
  chmod 0644 "$probe_file"
  response="$(curl -fsS --max-time 5 "http://$cert_identifier/.well-known/acme-challenge/$probe_name" 2>/dev/null || true)"
  rm -f "$probe_file"
  [[ "$response" == "$probe_name" ]]
}

request_certificate() {
  local -a certbot_args
  local certbot_state="$PWD/deploy/nginx/letsencrypt"
  local deploy_uid deploy_gid
  deploy_uid="$(id -u)"
  deploy_gid="$(id -g)"
  certbot_args=(
    certonly
    --non-interactive
    --agree-tos
    --email "$letsencrypt_email"
    --keep-until-expiring
    --cert-name "$cert_name"
  )

  case "$cert_type" in
    ip)
      certbot_args+=(--preferred-profile shortlived --ip-address "$cert_identifier")
      ;;
    domain)
      certbot_args+=(--domain "$cert_identifier")
      ;;
  esac

  if nginx_running; then
    if ! webroot_reachable; then
      echo "ACME webroot probe failed while nginx is running; refusing standalone mode because port 80 is already in use" >&2
      return 1
    fi
    certbot_args+=(--webroot --webroot-path /var/www/certbot)
    docker run --rm \
      -v "$http01_webroot:/var/www/certbot" \
      -v "$certbot_state:/etc/letsencrypt" \
      "$certbot_image" "${certbot_args[@]}"
  elif webroot_reachable; then
    certbot_args+=(--webroot --webroot-path /var/www/certbot)
    docker run --rm \
      -v "$http01_webroot:/var/www/certbot" \
      -v "$certbot_state:/etc/letsencrypt" \
      "$certbot_image" "${certbot_args[@]}"
  else
    # Initial provisioning has no webroot server yet. Standalone temporarily
    # publishes Certbot's HTTP-01 listener on the mandatory public port 80.
    certbot_args+=(--standalone)
    docker run --rm -p 80:80 \
      -v "$certbot_state:/etc/letsencrypt" \
      "$certbot_image" "${certbot_args[@]}"
  fi

  # Certbot runs as root in its container and creates 0700 lineage directories.
  # Return the bind-mounted state to the host deploy user so validation and
  # atomic installation can read the renewed fullchain and private key.
  docker run --rm \
    -v "$certbot_state:/state" \
    busybox:1.37 chown -R "$deploy_uid:$deploy_gid" /state
}

verify_certificate() {
  [[ -f "$cert_file" && -f "$key_file" ]] || {
    echo "certificate material missing for $cert_name" >&2
    return 1
  }
  # fullchain.pem contains the leaf followed by its intermediates. Supplying it
  # as the untrusted chain lets OpenSSL anchor the chain in the host CA bundle.
  openssl verify -CAfile /etc/ssl/certs/ca-certificates.crt -untrusted "$cert_file" "$cert_file" >/dev/null
  openssl x509 -checkend 86400 -noout -in "$cert_file" >/dev/null || {
    echo "certificate expires within 24 hours" >&2
    return 1
  }

  local sans
  sans="$(openssl x509 -noout -ext subjectAltName -in "$cert_file")"
  case "$cert_type" in
    ip) grep -Fq "IP Address:$cert_identifier" <<<"$sans" ;;
    domain) grep -Fq "DNS:$cert_identifier" <<<"$sans" ;;
  esac || {
    echo "certificate SAN does not include $cert_identifier" >&2
    return 1
  }
}

install_certificate() {
  verify_certificate

  local changed=false
  if ! cmp -s "$cert_file" deploy/nginx/certs/server.crt || ! cmp -s "$key_file" deploy/nginx/certs/server.key; then
    install -m 0644 "$cert_file" "deploy/nginx/certs/.server.crt.$$"
    install -m 0600 "$key_file" "deploy/nginx/certs/.server.key.$$"
    mv -f "deploy/nginx/certs/.server.crt.$$" deploy/nginx/certs/server.crt
    mv -f "deploy/nginx/certs/.server.key.$$" deploy/nginx/certs/server.key
    changed=true
  fi

  if "$changed"; then
    if nginx_running; then
      docker compose exec -T nginx nginx -s reload
    fi
    echo "certificate installed"
  else
    echo "certificate unchanged"
  fi
}

case "$action" in
  issue|renew)
    request_certificate
    install_certificate
    ;;
  install)
    install_certificate
    ;;
  verify)
    verify_certificate
    ;;
  *)
    echo "usage: $0 [issue|renew|install|verify]" >&2
    exit 2
    ;;
esac
