# Co-hosted public monitor

The public monitor is a second, read-only instance of the dex Monitor served
from the **same prod server** as the operator stack. It shows only the
**dex Monitor** and **Statistics** tabs, with no login. There is no Taker
Controller, no Fiat Controller, no LND, no exchange API keys, and no separate
database to keep in sync: it reads the same Postgres, through a restricted
role, that the trading stack already writes.

Nothing is duplicated: the public instance shares Postgres, Tor (fee fetches
through `tor:9050`), the unified-api container (which gains a second
read-only listener), and — once a public domain exists — the same nginx edge
container. Bisq containers are not needed at all: the public instance never
talks to Bisq; offers arrive via the database the existing snapshot worker
already feeds.

| Piece | File |
|---|---|
| Service (profile `public`) | `docker-compose.yml`, service `dexmonitor-public` |
| unified-api read-only listener | `dexUnifiedApi/cmd/server/public_listener.go` (`PUBLIC_LISTEN_ADDR`) |
| Restricted DB role `dex_monitor_public` | `dexUnifiedApi/db/bootstrap.go` (`POSTGRES_MONITOR_PUBLIC_PASSWORD`) |
| PUBLIC_EDITION app switch | `dexMonitor/cmd/server/public_edition.go` |
| Edge (security boundary, future domain) | `nginx/public.conf` (template) |
| Outside check | `scripts/public-edge-check.sh`, workflow `public-monitor-check.yml` |

## What PUBLIC_EDITION=true does

- **dexMonitor** serves `/api/past_offers`, `/api/past_offers/{id}/lifecycle`,
  `/api/ohlc`, `/api/payment_method_groups`, `/api/dashboard/analytics` and
  `/api/statistics` without a session, skips its startup writes (it cannot
  write anything anyway), disables the session-store cleanup goroutine, and
  reads runtime settings from env instead of `app_settings`.
- **unified-api** (separate listener, `PUBLIC_LISTEN_ADDR`, e.g. `:5011`)
  serves `GET /api/live_offers{,/counts,/{id}/lifecycle}`, `/ws/live_offers`,
  `GET /api/trade_cost_config` and a stripped `GET /api/cex_permissions`.
  Nothing publishes the port; only the public nginx vhost would proxy to it.
- The frontend gets `<meta name="dexpendulum-edition" content="public">` and
  shows only the two tabs, no login, no Metrics link, no logout, no shared
  exchange switches. The execution panel explains "Trading isn't available on
  the public monitor." instead of operator reasons.

Every other route still needs a session, including login and registration.
The `dex_monitor_public` role is rebuilt from nothing on every unified-api
reconciliation (REVOKE ALL, then explicit grants): full SELECT on the
reporting tables, column-only SELECT on `accounts`/`api_keys` (never the key,
secret, login or contact details), and nothing else — no sessions, no
settings, no writes.

Pricing needs no seed co-hosted: the real taker controller keeps the
capability checks and fee tables fresh, so `MM_CEX_CAPABILITY_MAX_AGE` keeps
the operator default (30m).

## Activation (what is committed)

The stack default is **off**: `dexmonitor-public` sits behind compose profile
`public` and the unified-api listener behind `PUBLIC_LISTEN_ADDR`, both
inert until the env bundle sets:

```dotenv
POSTGRES_MONITOR_PUBLIC_PASSWORD=<openssl rand -hex 24>
PUBLIC_LISTEN_ADDR=:5011
COMPOSE_PROFILES=public
```

With these set (they are, in the current bundle):

1. `docker compose up -d --wait` — on the VPS this happens through the
   normal transactional deploy — recreates unified-api, whose startup
   reconciles `dex_monitor_public`, and starts `dexmonitor-public`.
2. The deploy digest tooling pins `dexmonitor-public` to whatever
   `dexmonitor` resolved to (same image reference = alias), so it always
   moves with the operator image instead of freezing at its first pull.
3. `scripts/compose-validate.sh` asserts the public service's restricted
   shape (restricted role URL, no secrets, loopback-only publish), and
   `scripts/validate-env-contract.sh` refuses a bundle that enables the
   profile without the role password.

The instance stays **unpublished**: it publishes only on loopback
(`127.0.0.1:5003` on the VPS) for operator preview (SSH tunnel or a
`tailscale serve`/port-forward target). No public URL serves it until a
domain exists.

## Previewing on the VPS

```bash
ssh -L 5003:127.0.0.1:5003 prod
# then open http://127.0.0.1:5003 in a browser
```

Live offers (`/api/live_offers`, `/ws/live_offers`) come from unified-api's
public listener, which only the nginx vhost proxies; the loopback preview
therefore shows everything except the live-offers section until the edge
exists. Statistics, past offers and analytics work fully.

## Going public (when a domain exists)

The edge is ready as a template — see the activation steps at the top of
[`nginx/public.conf`](../../nginx/public.conf): point the domain's DNS at
the server, put TLS material in `deploy/nginx/certs/public/`, mount the
template plus `PUBLIC_MONITOR_HOSTNAME` into the `nginx` service, and
recreate just nginx. The operator vhost (`nginx/nginx.conf`, `server_name _`)
is untouched.

Then check from outside:

```bash
scripts/public-edge-check.sh https://<host>          # boundary
CHECK_ROUTES=1 scripts/public-edge-check.sh https://<host>   # routes priced
```

`public-monitor-check.yml` repeats that check hourly once
`PUBLIC_DEPLOY_ENABLED`/`PUBLIC_HOSTNAME` repository variables are set.
