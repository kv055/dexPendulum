# Monitoring Stack

Prometheus + Grafana setup for the dexPendulum stack. All services run as Docker Compose services alongside the application.

## Architecture

```
┌─────────────────────────────────────────────────────────┐
│                      Grafana :3000                      │
│              (served via nginx at /grafana/)             │
└───────────────────────┬─────────────────────────────────┘
                        │ datasource
┌───────────────────────▼─────────────────────────────────┐
│                   Prometheus :9090                       │
│              retention: 30d  scrape: 15s                 │
└──┬──────────┬──────────┬──────────┬──────────┬──────────┘
   │          │          │          │          │
:9100      :8080      :9187      :5010      :5000
node-     cadvisor  postgres-  unified-    app
exporter            exporter    api     (dexMonitor)
```

## Services

| Service | Image | Role |
|---|---|---|
| `prometheus` | prom/prometheus:v2.55.1 | Metrics storage and scraping |
| `grafana` | grafana/grafana:11.3.0 | Dashboards and visualization |
| `node-exporter` | prom/node-exporter:v1.8.2 | Host CPU, memory, disk, network metrics |
| `cadvisor` | gcr.io/cadvisor/cadvisor:v0.49.1 | Per-container resource metrics |
| `postgres-exporter` | prometheuscommunity/postgres-exporter:v0.16.0 | PostgreSQL metrics |

The application services (`unified-api`, `dexmonitor`, `mm-controller`) expose their own `/metrics` endpoints and are scraped directly. `fiat-controller` also serves `/metrics`; its scrape job in `prometheus.yml` is commented out until the compose service is re-enabled.

## Dashboards

Pre-provisioned dashboards live in [grafana/dashboards/](grafana/dashboards/) and are loaded automatically on startup:

**Start here:** `system-overview.json` is the single-pane golden-signals view; drill
from it into the logs, traces and domain dashboards below.

| File | What it shows |
|---|---|
| `system-overview.json` | Golden signals across metrics + logs in one pane (exchanges up, snapshot age, error rate, latency, recent errors) |
| `trading-overview.json` | Arbitrage opportunity: best net premium, profitable offers, liquidity, premium distribution |
| `data-freshness.json` | Snapshot/OHLC age, exchange up/down, price-feed staleness & reconnects |
| `accounts-and-ws.json` | Trading-account health + live-feed WebSocket clients/broadcasts |
| `logs-and-errors.json` | **(Loki)** Log volume by level, error/warn rate per service, live error stream — filter by `$service` |
| `traces-and-latency.json` | **(Tempo + Prom)** Snapshot-run durations, errored spans, HTTP/ingest latency |
| `go-services.json` | Go runtime + HTTP RED metrics from `unified-api` and `app` |
| `postgres.json` | PostgreSQL connections, transactions, cache hit rate |
| `host.json` | Node-exporter host metrics (CPU, RAM, disk, network) |
| `containers.json` | cAdvisor per-container resource usage |
| `trading-execution.json` | mm-controller trade engine: order submissions/latency, hedge failures, tradeflow runs & recoveries, realized PnL, LND wallet/balances, Telegram API calls |
| `business-pnl.json` | **(Postgres)** Realized PnL over time, per-trade PnL, transaction status funnel, fiat push pipeline (status timeline, match latency, recent notifications) |
| `market-history.json` | **(Postgres)** Net-premium history from `order_lifecycle`, offer appearance/lifetime by exchange, reference price, snapshot cycle duration |

The logs/traces dashboards rely on the `loki` and `tempo` datasources (provisioned in
[grafana/provisioning/datasources/](grafana/provisioning/datasources/)). `service` is the
same label across metrics, logs and traces (`unified-api`, `unified-snapshot`,
`dexmonitor`), so you can pivot between dashboards on one identity.

The two **(Postgres)** dashboards query the app schema directly through the read-only
`postgres-dexarb` datasource (role `dex_grafana_ro`, SELECT-only). unified-api's role
bootstrap creates the role once `POSTGRES_GRAFANA_RO_PASSWORD` is present in
`dexUnifiedApi/.env`; the same variable must be in the root `.env` so compose can hand
it to the grafana container. Until then the datasource shows a connection error and
everything else keeps working.

Dashboard updates from the Grafana UI are allowed (`allowUiUpdates: true`). To persist changes back to the repo, export the dashboard JSON from Grafana and overwrite the file.

## Configuration Files

```
deploy/monitoring/
├── prometheus.yml                          # Scrape targets
└── grafana/
    ├── provisioning/
    │   ├── datasources/
    │   │   ├── prometheus.yml              # Default datasource
    │   │   ├── loki.yml                     # Logs (+ trace_id -> Tempo link)
    │   │   └── tempo.yml                    # Traces (+ span -> Loki link)
    │   └── dashboards/dashboards.yml       # Points Grafana at the dashboards directory
    └── dashboards/                          # Auto-loaded; see the Dashboards table above
```

## Access

Grafana is served through nginx at the `/grafana/` sub-path. The edge terminates
TLS on `5443`; plain `:5000` (HTTP) 301-redirects there.

```
https://<host>:5443/grafana/
```

Login with `admin` / `$GRAFANA_ADMIN_PASSWORD` (set in your `.env`).

Prometheus, Loki and Tempo are not exposed externally — they are only reachable
inside the Docker network and through Grafana's datasource proxy (below).

> **TLS cert.** nginx needs a cert/key at `deploy/nginx/certs/server.{crt,key}`
> (gitignored). In prod the **CI deploy guard auto-creates and renews** a self-signed
> pair on the VPS every rollout (`gen-self-signed-cert.sh` in `ENSURE` mode, CN = the
> VPS host), so there is no manual step and nginx can't crash-loop on a missing cert.
> Locally, run `deploy/nginx/gen-self-signed-cert.sh <CN>`. For a browser-trusted edge,
> drop a Let's Encrypt pair in at the same path.

## Agent datasource access

Scraping agents query the datasources through **Grafana's datasource-proxy API**
using a read-only **Viewer service-account token** — no datasource backend is
exposed directly, and the token reuses Grafana's RBAC (revocable, can't mutate).

**1. Mint the token once** (Grafana OSS can't provision SA tokens via YAML). The
script hits Grafana's Admin API through the nginx edge (`/grafana` prefix), so
`-k` is needed for the self-signed cert:

```bash
CURL_OPTS='-k' GRAFANA_ADMIN_PASSWORD=... \
  deploy/monitoring/grafana/bootstrap-agent-token.sh https://localhost:5443/grafana
# prints: GRAFANA_AGENT_TOKEN=glsa_...   -> store in .env.enc
```

**2. Query endpoints** (all gated by `Authorization: Bearer $GRAFANA_AGENT_TOKEN`,
rate-limited at the nginx edge). Pin the self-signed cert with `--cacert`. A remote
agent with no repo checkout fetches it once from the edge — the cert's CN/SAN is the
VPS host, so verification passes when you connect by that host:

```bash
openssl s_client -connect <host>:5443 </dev/null 2>/dev/null \
  | openssl x509 > edge.crt        # pin this; refetch after a cert rotation
```

```bash
H="Authorization: Bearer $GRAFANA_AGENT_TOKEN"
CA="--cacert edge.crt"            # locally: --cacert deploy/nginx/certs/server.crt
B="https://<host>:5443/grafana/api"

# Unified query — one endpoint for any datasource (recommended)
curl $CA -H "$H" -H 'Content-Type: application/json' "$B/ds/query" -d '{
  "queries":[{"datasource":{"uid":"prometheus"},"expr":"dex_exchange_up","refId":"A"}],
  "from":"now-1h","to":"now"}'

# Native Prometheus (PromQL)
curl $CA -H "$H" "$B/datasources/proxy/uid/prometheus/api/v1/query" \
  --data-urlencode 'query=dex_exchange_up'

# Native Loki (LogQL)
curl $CA -H "$H" "$B/datasources/proxy/uid/loki/loki/api/v1/query_range" \
  --data-urlencode 'query={service="unified-api"} | json | level="ERROR"'

# Native Tempo (search / trace by id)
curl $CA -H "$H" "$B/datasources/proxy/uid/tempo/api/search?tags=service.name%3Dunified-api"

# Discover datasource uids
curl $CA -H "$H" "$B/datasources"
```

Datasource uids are stable: `prometheus`, `loki`, `tempo`. Rotate or revoke the
token in Grafana → Administration → Service accounts → `agent-readonly`.

## Agent MCP access

[grafana/mcp-grafana](https://github.com/grafana/mcp-grafana) is a local stdio
MCP server that calls the same Grafana API as the curl recipes above. Grafana
OSS has no `/mcp` route; this wrapper does not add a compose service (a later
HTTP sidecar can sit beside it without changing the script).

**Prerequisites**

- Monitoring stack up (`make staging-up`, or grafana + nginx + Prometheus/Loki/Tempo).
  `dev-server.sh` does **not** start Grafana.
- `GRAFANA_AGENT_TOKEN` minted (step 1 above) and present in the repo-root `.env`.
- [`uv`](https://docs.astral.sh/uv/) (`uvx mcp-grafana`) or a `mcp-grafana` binary on `PATH`.

**Cursor** (user MCP — `.cursor/` is gitignored, so this is not project config):

```json
{
  "mcpServers": {
    "grafana": {
      "command": "/absolute/path/to/dexPendulum/deploy/monitoring/grafana/run-mcp.sh"
    }
  }
}
```

[`grafana/run-mcp.sh`](grafana/run-mcp.sh) reads `GRAFANA_AGENT_TOKEN` from `.env`,
maps it to `GRAFANA_SERVICE_ACCOUNT_TOKEN`, defaults `GRAFANA_URL` to
`https://localhost/grafana`, and runs `mcp-grafana --disable-write`. If
`deploy/nginx/certs/server.crt` exists it is passed as `--tls-ca-file`; otherwise
the script warns and uses `--tls-skip-verify`. Override `GRAFANA_URL` or
`GRAFANA_AGENT_TOKEN` in the environment; existing values win over `.env`.

The Viewer token cannot mutate Grafana. Write tools are disabled. First-class
queries are PromQL and Loki/LogQL plus dashboard search/summaries. Tempo
TraceQL and raw Postgres SQL are **not** first-class — use the curl recipes
above (uids `tempo`, `postgres-dexarb`).

## Adding a Scrape Target

Add a new job to [prometheus.yml](prometheus.yml):

```yaml
- job_name: my-service
  static_configs:
    - targets: ["my-service:PORT"]
```

Then reload Prometheus without restarting (lifecycle API is enabled):

```bash
docker compose exec prometheus wget -qO- --post-data='' http://localhost:9090/-/reload
```

## Data Persistence

| Volume | Content |
|---|---|
| `prometheus-data` | Time-series data, 30-day retention |
| `grafana-data` | Grafana state (users, alert rules, manually saved changes) |
| `loki-data` | Log chunks + index, 7-day retention |
| `tempo-data` | Trace blocks, 7-day retention |
| `alloy-data` | Alloy positions/WAL (so log scraping resumes after a restart) |

## Logs & Traces (OpenTelemetry)

Metrics answer "is something wrong"; logs and traces answer "why". The Go
services emit both via OpenTelemetry, stored in Loki (logs) and Tempo (traces)
and viewed in the same Grafana.

```
Go services ──stdout JSON──► Alloy ──► Loki ─┐
Go services ──OTLP :4317───► Alloy ──► Tempo ─┼──► Grafana (Explore + correlation)
Prometheus (metrics, unchanged) ─────────────┘
```

| Service | Image | Role |
|---|---|---|
| `loki` | grafana/loki:3.7.10 | Log storage/query (7-day retention) |
| `tempo` | grafana/tempo:2.7.1 | Trace storage/query, OTLP receiver (held on 2.x, see docker-compose.yml) |
| `alloy` | grafana/alloy:v1.22.0 | Scrapes container stdout → Loki; receives OTLP → Tempo |

- **Logs.** Services log structured JSON to stdout (`commons/logging`, built on
  `slog`). Alloy discovers containers via the docker socket, parses the JSON,
  promotes `level` to a label and `trace_id` to structured metadata, and ships
  to Loki. Non-JSON lines (bisq/nginx/postgres) still arrive, just without those
  fields. Config: [alloy/config.alloy](alloy/config.alloy),
  [loki/loki-config.yml](loki/loki-config.yml).
- **Traces.** Services build a TracerProvider in `commons/tracing` and push OTLP
  to Alloy, which forwards to Tempo. Instrumented boundaries: inbound HTTP
  (server spans, named by route), outbound HTTP (`otelhttp` transport — incl. the
  snapshot → unified-api `/internal/live_offers` push, so a snapshot run and the
  resulting ingest are **one trace**), Bisq gRPC (`otelgrpc`), and the snapshot
  cycle stages. Config: [tempo/tempo.yml](tempo/tempo.yml).
- **Correlation.** The Tempo datasource links a span to its logs in Loki
  (`tracesToLogsV2`); the Loki datasource turns a log line's `trace_id` into a
  link back to the trace. Both are provisioned in
  [grafana/provisioning/datasources/](grafana/provisioning/datasources/).

### Viewing

- **Logs:** Grafana → Explore → *Loki*, e.g. `{service="unified-snapshot"}` or
  `{service="unified-api"} | json | level="ERROR"`.
- **Traces:** Grafana → Explore → *Tempo* → Search by service/name, or paste a
  `trace_id` from a log line.

### Tuning

`OTEL_EXPORTER_OTLP_ENDPOINT` (default `alloy:4317`; empty disables tracing),
`OTEL_TRACES_SAMPLER_ARG` (head-sampling ratio, default `1.0`), `LOG_LEVEL`, and
`DEPLOY_ENV` are set per Go service in `docker-compose.yml`. See root
`.env.example`.

> **Security note:** Alloy mounts the docker socket read-only for log discovery.
> A read-only mount blocks writes to the socket *file* but not Docker API calls,
> so Alloy effectively has Docker API access — consistent with the existing
> privileged agents (cadvisor `privileged: true`, node-exporter host mounts).
> Loki/Tempo/Alloy publish no ports; they are reachable only on the docker
> network and through Grafana.
