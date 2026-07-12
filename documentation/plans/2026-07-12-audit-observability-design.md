# Audit & Observability System Design

**Date:** 2026-07-12
**Status:** Implemented

## Overview

A two-pillar system for auditing system health and browsing event history.

- **Pillar 1 — OpenObserve (production OTel backend):** Replaces motel (dev-only) for production. Single Rust binary, OTel-native, SQL search UI. Traces and logs from all services.
- **Pillar 2 — Audit log service:** A standalone Go service that subscribes to `audit.event` on NATS, persists events to its own SQLite database, and serves a Go templates + HTMX web UI for searching and browsing.

The two pillars connect via `trace_id`: each audit event carries an optional trace ID that links to the corresponding OpenTelemetry trace in OpenObserve. Audit = "what happened" (persistent, searchable). OTel = "why/how" (traces, spans, logs).

## Pillar 1: OpenObserve

### What it is

[OpenObserve](https://openobserve.ai) is an open-source observability platform: single Rust binary, OTel-native, SQL search, Parquet columnar storage. Chosen over SigNoz (heavier, ClickHouse + multiple services, more APM-focused) for its simplicity and lighter footprint.

### Deployment

Added as a Docker service in `docker/docker-compose.yml`:

```yaml
openobserve:
  image: openobserve/openobserve:latest
  environment:
    ZO_ROOT_USER_EMAIL: "admin@pixeleruv.local"
    ZO_ROOT_USER_PASSWORD: "password123"
    ZO_DATA_DIR: "/data"
    ZO_TRACING_ENABLED: "true"
  volumes:
    - o2_data:/data
  ports:
    - "5080:5080"
```

### OTel exporter wiring

All backend services (pusher, worldsim, 4 extensions) have `OTEL_EXPORTER_OTLP_ENDPOINT` pointed at `http://openobserve:5080/api/default` in docker-compose. `OTEL_ENABLED` defaults to `false` — set to `true` to ship traces/logs.

motel stays dev-only: `make debug` still points at `http://127.0.0.1:27686`.

### Extension instrumentation

All 4 extensions (ext-demo, ext-walls, ext-props, ext-av) now call `otel.Init()` on startup. Previously they used plain slog to stdout with no OTel bridging. Now when `OTEL_ENABLED=true`, their logs are correlated to spans and exported to OpenObserve.

### Nginx proxy

`/otel/` proxies to `openobserve:5080/` — accessible at `http://localhost:4080/otel/` or `https://localhost:4043/otel/`.

## Pillar 2: Audit Log Service

### Event emission (`backend/internal/audit`)

A shared Go package with `Emit(nc, eventType, severity, actor, details, traceID)` that publishes a JSON envelope to NATS subject `audit.event`.

```go
audit.Emit(nc, "client.connected", audit.SeverityInfo,
    audit.Actor{Sub: sub, EntityID: eid, ClientID: cid, IP: ip},
    audit.Details{"map": mapID, "is_admin": isAdmin},
    traceID)
```

The envelope:
```json
{
  "event_type": "client.connected",
  "severity": "info",
  "timestamp": "2026-07-12T10:30:00Z",
  "actor": {"sub": "...", "entity_id": "...", "client_id": "...", "ip": "..."},
  "details": {"map": "town", "is_admin": false},
  "trace_id": "abcdef..."
}
```

### Event types (~25-30, lifecycle + interactions)

| Source | Events |
|--------|--------|
| **pusher** | `client.connected`, `client.disconnected`, `auth.failed`, `auth.banned`, `ws.keepalive_timeout` |
| **worldsim** | `player.provisioned`, `player.despawned`, `player.banned`, `player.set_name`, `player.set_sprite_base`, `player.set_player_options`, `player.teleport`, `player.map_transition`, `chat.message`, `map.reloaded`, `map.integrity_check`, `extension.registered`, `extension.stale`, `zone.enter`, `zone.exit` |
| **ext-props** | `props.action_triggered` |
| **ext-av** | `av.token_minted`, `av.token_revoked` |

### Storage: SQLite (own database, independent of worldsim)

The audit service uses its own SQLite database (`modernc.org/sqlite`, pure Go, WAL mode). This is deliberate:

- **Independent of worldsim** — survives worldsim crashes, can audit the crash
- **No write contention** with worldsim's SQLite (PocketBase)
- **Fast** — direct INSERT, no REST round-trip
- **Simple** — one Go binary, one file

**Schema:**
```sql
CREATE TABLE audit_events (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    event_type   TEXT    NOT NULL,
    severity     TEXT    NOT NULL,
    actor_sub    TEXT    NOT NULL DEFAULT '',
    actor_entity TEXT    NOT NULL DEFAULT '',
    actor_client TEXT    NOT NULL DEFAULT '',
    actor_ip     TEXT    NOT NULL DEFAULT '',
    actor_device TEXT    NOT NULL DEFAULT '',
    actor_ext    TEXT    NOT NULL DEFAULT '',
    details      TEXT    NOT NULL DEFAULT '',
    trace_id     TEXT    NOT NULL DEFAULT '',
    occurred_at  TEXT    NOT NULL
);
-- Indexes on event_type, severity, actor_sub, actor_entity, occurred_at
```

### Storage upgrade path

The storage layer is behind an `EventStore` interface. To upgrade:

- **ClickHouse (preferred):** Use `github.com/ClickHouse/clickhouse-go/v2`. Replace schema with a MergeTree table ordered by `(occurred_at, event_type)`. Queries stay SQL; JSON details map to ClickHouse's String/JSON type. Native TTL replaces manual retention. ClickHouse is already used by OpenObserve, so consolidation is possible.
- **TimescaleDB:** Use `lib/pq` or `jackc/pgx`. Create a hypertable on `occurred_at`. Retention via `drop_chunks` policy. JSONB for details.

The SQL queries in `store.go` are standard and port easily to either.

### Audit service (`backend/cmd/audit`)

Standalone Go binary, independent Docker container. Subscribes to `audit.event` on NATS, persists to SQLite, serves HTTP UI on `:8082`.

Templates and static files are embedded via `go:embed` — the binary is self-contained, no external file dependencies.

### Web UI (Go templates + HTMX)

| Route | Purpose |
|-------|---------|
| `/audit/` | Dashboard: service health cards, event severity counts (24h), event type counts, recent events |
| `/audit/events` | Searchable event table: filter by type, severity, actor. HTMX partial reload on filter. |
| `/audit/events/:id` | Event detail: full payload, actor info, link to OpenObserve trace |
| `/audit/players/:sub` | Player timeline: all events for one player, chronological |
| `/audit/health` | Service health detail (from pusher `/healthz`) |

Dark theme, no JS build step. HTMX for interactivity (filter form submits via HTMX, returns table fragment).

### Nginx proxy

`/audit/` proxies to `audit:8082/` — accessible at `http://localhost:4080/audit/` or `https://localhost:4043/audit/`.

### Retention

Default 30 days. Configurable via `AUDIT_RETENTION_HOURS` env var. The service runs a hourly cleanup loop that deletes events older than the retention period.

## Architecture diagram

```
                                  ┌─────────────────────────────────┐
                                  │         BROWSER (admin)         │
                                  │  /audit/  → Audit UI (HTMX)     │
                                  │  /otel/   → OpenObserve UI      │
                                  └────────┬───────────┬────────────┘
                                           │           │
                                        ┌──▼──┐    ┌───▼────────────┐
                                        │nginx│    │  OpenObserve    │
                                        │ 4080│    │  :5080 (Rust)   │
                                        └─┬───┘    │  traces+logs    │
                                          │        │  SQL search UI  │
                  ┌───────────────────────┼────────┴────────────────┘
                  │                       │                 ▲
            ┌─────▼─────┐          ┌──────▼─────┐           │ OTLP/HTTP
            │  pusher   │          │  worldsim  │──┐        │
            │  :8081    │          │  + PB :8090│  │        │
            └─────┬─────┘          └──────┬─────┘  │   ┌────┴────┐
                  │                       │        ├──>│  NATS   │
                  │ audit.event           │ audit  │   └────┬────┘
                  │ spans→OTLP            │ .event │        │ audit.event
                  │                       │ spans  │        │
            ┌─────▼─────┐          ┌──────▼─────┐  │   ┌────▼─────┐
            │  ext-*    │          │  audit svc │  │   │  ext-*   │
            │  (4 svcs) │          │  :8082     │◄─┘   │  emit    │
            │  emit     │          │  → SQLite  │      │  audit   │
            │  audit    │          │  HTMX UI   │      │  events  │
            └───────────┘          └────────────┘      └──────────┘
```

## Files

### New

| File | Purpose |
|------|---------|
| `backend/internal/audit/audit.go` | Event/Actor/Details types + Emit helper |
| `backend/internal/audit/audit_test.go` | Unit tests |
| `backend/cmd/audit/main.go` | Service entrypoint |
| `backend/cmd/audit/server.go` | NATS subscriber + HTTP server |
| `backend/cmd/audit/store.go` | EventStore interface + SQLite implementation |
| `backend/cmd/audit/embed.go` | go:embed for templates and static files |
| `backend/cmd/audit/templates/*.html` | Go templates (base, dashboard, events, detail, player timeline, health) |
| `backend/cmd/audit/static/*` | HTMX + CSS |

### Modified

| File | Changes |
|------|---------|
| `backend/internal/pusher/pusher.go` | 5 audit.Emit calls (connected, disconnected, auth.failed, auth.banned, keepalive_timeout) |
| `backend/internal/worldsim/worldsim.go` | ~15 audit.Emit calls (provisioned, despawned, banned, set_name, set_sprite_base, set_player_options, teleport, map_transition, chat.message, map.reloaded, map.integrity_check, zone.enter, zone.exit) |
| `backend/internal/worldsim/extensions.go` | 2 audit.Emit calls (extension.registered, extension.stale) |
| `backend/cmd/ext-demo/main.go` | otel.Init() |
| `backend/cmd/ext-walls/main.go` | otel.Init() |
| `backend/cmd/ext-props/main.go` | otel.Init() + audit.Emit (props.action_triggered) |
| `backend/cmd/ext-av/main.go` | otel.Init() + audit.Emit (av.token_minted, av.token_revoked) |
| `docker/docker-compose.yml` | OpenObserve + audit services, OTel endpoints for all services |
| `docker/backend.Dockerfile` | audit build target + image |
| `docker/nginx.conf` | /audit/ and /otel/ proxy routes |
| `Makefile` | audit in build target |

## Verification

- `make build` compiles all binaries including `audit`
- `go test ./internal/audit/` — unit tests pass
- `go test ./internal/worldsim/` — existing tests pass
- `make up` — OpenObserve at `:5080`, audit UI at `:8082` or `/audit/`
- Connect/disconnect a client → events appear in audit UI
- Failed auth → `auth.failed` event with severity `warn`
- Click `trace_id` in audit UI → opens trace in OpenObserve
- Filter events by `event_type=chat.message` → see only chat events
- View player timeline at `/audit/players/:sub`
- OpenObserve shows traces from pusher, worldsim, and all 4 extensions
- Stop worldsim container → audit container keeps running, records events

## Future work

- **Audit UI auth:** Currently relies on nginx-level restriction. Full auth (Dex OIDC or PB admin token) is a follow-up.
- **ClickHouse/TimescaleDB upgrade:** If volume grows or analytical dashboards are needed, implement the `EventStore` interface with ClickHouse (preferred — columnar, fast filtered scans, SQL, JSON support, already used by OpenObserve) or TimescaleDB (hypertable, JSONB, retention policies).
- **Real-time updates:** The dashboard uses polling. Could switch to SSE for live event stream.
- **Alerting:** OpenObserve has built-in alerts. Could configure alerts for `auth.failed` spikes, `extension.stale`, error rate thresholds.
