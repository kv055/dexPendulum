# nginx edge: secrets

## Development TLS cert (`certs/server.{crt,key}`) — automatic

Host-local (gitignored, never committed). Created/renewed by the CI deploy
guard on every rollout via `gen-self-signed-cert.sh` (ENSURE mode). Locally, run
the script once before `docker compose up nginx`. See the script header for
Let's Encrypt notes.

This self-signed path remains supported for development servers and provides a
certificate before nginx starts.

Provision a development server with:

```bash
deploy/provision/run.sh --edge-mode development
```

This skips GoDaddy DNS and the Let's Encrypt renewal timer. Access the server
directly through its VPS IP and accept or explicitly trust its self-signed
certificate.

## Production TLS — GoDaddy DNS + Let's Encrypt

Production serves `PUBLIC_HOSTNAME` on port 443. Port 80 is intentionally kept
public only to serve `/.well-known/acme-challenge/` and redirect all other
requests to HTTPS. The hostname's apex A record is reconciled by
`deploy/provision/run.sh --dns-only`; see the production DNS section in
[`deploy/README.md`](../README.md#production-dns-and-https-godaddy).

After nginx is running, the CI deploy flow runs `letsencrypt.sh issue` when
`LETSENCRYPT_EMAIL` is set. It uses Certbot's HTTP-01 webroot challenge, copies
the resulting certificate and key to `certs/server.{crt,key}`, and reloads
nginx. Ansible installs `dexarb-certbot.timer`, which runs the renewal command
daily on the VPS. The certificate, private key, ACME webroot, and Certbot state
are all host-local and gitignored.

Required production `.env` values:

```dotenv
PUBLIC_HOSTNAME=dex.example.com
LETSENCRYPT_EMAIL=admin@example.com
```

## Authentication — application login (no edge basic auth)

The edge no longer does HTTP basic auth. Access is gated by the application: the
dexMonitor service serves a login page and a Postgres-backed session cookie
(`dex_session`), and both dexMonitor and unified-api require a valid session on
their browser-facing API/WS routes (see `commons/session`). nginx just terminates
TLS and proxies; only Grafana keeps its own separate login.

No nginx auth secret is needed here anymore. The legacy `EDGE_AUTH_HTPASSWD` env
var was removed from `docker-compose.yml`; any stale `EDGE_AUTH_HTPASSWD` left in
`.env`/`.env.enc` is simply unused.
