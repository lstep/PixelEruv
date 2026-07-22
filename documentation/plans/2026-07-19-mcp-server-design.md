# MCP Server Design — 2026-07-19

## Goal

Expose PixelEruv's internal data and admin actions to MCP clients (Claude
Desktop, Devin, Cursor, etc.) so an LLM can:

- Inspect live world state (players, entities, zones, extensions).
- Query historical audit events and per-player timelines.
- Read PocketBase records (players, maps, sprite_bases, bans).
- Take administrative actions (kick, ban, teleport, send chat as a player,
  set name / status / sprite / player options, dispatch extension actions).

## Why a separate binary

worldsim is the critical path: every player input and replication tick flows
through it. Coupling MCP request handling (which can be slow, can hang on
PocketBase, can be hammered by an LLM retry loop) into worldsim's process
would risk tail-latency spikes on the game loop. A separate `backend/cmd/mcp`
binary:

- Isolates MCP load from worldsim.
- Lets ops restart / redeploy the MCP surface without dropping players.
- Keeps the MCP server's dependency surface (the MCP Go SDK, HTTP/SSE) out of
  the worldsim binary.
- Makes it trivial to scale the MCP surface horizontally (multiple replicas
  behind nginx) without touching worldsim.

The MCP server talks to worldsim over NATS request-reply, to the audit
service over HTTP (its new JSON API), and to PocketBase over its REST API.
No new shared state is introduced.

## Transport

HTTP/SSE only (no stdio). The binary is designed to run as a Docker service
behind nginx. The MCP Go SDK's `SSEHandler` serves the 2024-11-05 SSE
transport on `/mcp`. `/healthz` is an unauthenticated health check for
Docker / nginx.

Auth: bearer token (`MCP_AUTH_TOKEN` env, required). The server refuses to
start if unset, and refuses all requests if the token is empty. Compared in
constant time. nginx does NOT apply the admin cookie auth_request to `/mcp`
— MCP clients present a bearer token in the `Authorization` header, not a
browser session cookie.

## Surface

### Tools (callable, take arguments)

**Read:**
- `get_world_stats` — worldsim snapshot (tick rate, uptime, players, entities,
  extensions, per-map counts).
- `get_zones` — zone metadata for all maps.
- `query_entities` — filter by map_id / entity_type / owner_extension /
  zone_id, limit (default 500, hard cap 500).
- `get_entity` — single entity by ID.
- `query_audit_events` — historical audit events with filters (type, severity,
  actor_sub, entity_id, limit, offset).
- `get_audit_event` — single audit event by ID.
- `player_timeline` — audit timeline for a player by OIDC subject.
- `list_pb_records` — PocketBase collection list with filter/sort/pagination.
- `get_pb_record` — single PocketBase record by collection + ID.
- `get_world_options` — current server-wide runtime config (world_options KV
  bucket): SMTP, AppURL, YouTube RTMP defaults, ffmpeg limits, world king,
  error-email recipients, recording gate, readOnly env mirrors.

**Control:**
- `teleport_entity` — teleport a player to a map / beacon.
- `kick_player` — force-disconnect a client by client_id.
- `ban_player` — insert a ban record (user_id / ip / device_id) and kick any
  matching connected client.

**Admin overrides (bypass connected-client validation):**
- `send_chat_as` — send chat as a specific entity (global or proximity).
- `set_player_name` — rename an entity (sanitized, truncated to 20 runes).
- `set_player_status` — set presence (0=Available, 1=Busy, 2=DND).
- `set_player_sprite` — set sprite_base (validated against sprite_bases).
- `set_player_options` — replace player options JSON.
- `set_world_options` — replace server-wide runtime config (world_options KV
  bucket). Full replace; clients should call `get_world_options` first, modify
  the desired fields, then pass the full object back. worldsim validates,
  writes KV, broadcasts `world_options.update` so consumers (SMTP client,
  ext-rec ffmpeg/YouTube, frontend recording gate) hot-reload. readOnly
  fields (`public_host`, `livekit_public_url`) are preserved by worldsim.
- `dispatch_extension_action` — publish to `extension.<id>.action`.

### Resources (URI-addressable, read-only)

Static:
- `pixeleruv://world/stats`
- `pixeleruv://world/zones`
- `pixeleruv://world/players`
- `pixeleruv://world/extensions`
- `pixeleruv://audit/stats`

Templated:
- `pixeleruv://world/maps/{name}`
- `pixeleruv://world/entities/{id}`
- `pixeleruv://audit/events/{id}`
- `pixeleruv://audit/players/{sub}`
- `pixeleruv://pb/{collection}`
- `pixeleruv://pb/{collection}/{id}`

### Prompts (pre-baked, fetch live data)

- `summarize_recent_audit` — last N events grouped by severity/type.
- `investigate_player` — timeline + online state for a player by sub.
- `world_health_report` — stats + extension status + recent warn/error events.

## New worldsim NATS subjects

The MCP server needs richer read access and admin actions than worldsim
previously exposed. New subjects (all request-reply, all reply with JSON):

**Read:**
- `worldsim.entities.query` — filter entities by map / type / owner / zone,
  return up to 500 snapshots. (entities_query.go)
- `worldsim.entity.get` — single entity by ID. (entities_query.go)

**Control:**
- `worldsim.client.kick` — despawn a client (by `client_id` or `entity_id`),
  publish `client.<id>.force_close` so the pusher closes the player's
  WebSocket, emit `player.kicked` audit.
  Replies `{"ok":true}` or `{"ok":false,"error":"not_connected"}`.
  (admin_actions.go)
- `worldsim.client.ban` — insert a ban record via BanStore.Add, then kick +
  force_close any matching connected client. Replies `{"ok":true,"kicked":bool}`.
  (admin_actions.go)

**Admin overrides (bypass client-ID validation, use entity_id):**
- `worldsim.admin.chat` — send chat as an entity. (admin_actions.go)
- `worldsim.admin.set_name` — rename an entity. (admin_actions.go)
- `worldsim.admin.set_status` — set presence status. (admin_actions.go)
- `worldsim.admin.set_sprite` — set sprite_base. (admin_actions.go)
- `worldsim.admin.set_player_options` — replace player options. (admin_actions.go)

All admin handlers reply with `{"ok":bool,"error":"..."}` so the MCP client
can confirm the action landed. Audit events are emitted on the
`audit.event` subject with `actor.extension="mcp"` (configurable via
`MCP_ACTOR`) so audit consumers can filter admin-initiated events from
client-initiated ones.

## New audit JSON API endpoints

The audit service previously served only an HTML UI. New JSON endpoints
(behind the existing admin auth_request at nginx, basic auth at the app
layer for the MCP server's direct connection):

- `GET /audit/api/events?type=&severity=&actor=&entity=&limit=&offset=` —
  paginated event list.
- `GET /audit/api/events/{id}` — single event by ID.
- `GET /audit/api/players/{sub}` — timeline for a player (up to 200 events).
- `GET /audit/api/stats` — 24h severity/type counts + uptime + version.

## PII handling

The MCP server exposes full PII: IP addresses, device IDs, client IDs. This
is intentional — the MCP server is an admin tool, and these fields are
necessary for moderation (e.g. banning by IP, correlating a player across
sessions by device_id). The bearer token gate is the access control. Do NOT
expose the MCP server on the public internet without a strong token and
network-level restrictions (firewall / VPN / Tailscale).

## Deployment

Docker service `mcp` in both `docker/docker-compose.yml` (dev, builds from
source) and `dist/docker-compose.yml` (prod, uses pre-built binary). nginx
proxies `/mcp` to `http://mcp:8085` with `proxy_buffering off` and a 1h read
timeout (required for SSE). The `/mcp` location is NOT behind the admin
cookie auth_request — bearer-token auth at the app layer instead.

Env vars (see `backend/cmd/mcp/main.go` for the full list):
- `MCP_AUTH_TOKEN` (required) — bearer token.
- `NATS_URL` — worldsim + audit NATS.
- `AUDIT_BASE_URL` — audit HTTP API.
- `PB_BASE_URL` — PocketBase REST.
- `MCP_ACTOR` — audit actor.extension label (default "mcp").

## Testing

- `backend/internal/worldsim/entities_query_test.go` — filter + limit + get.
- `backend/internal/worldsim/admin_actions_test.go` — kick, ban, set_name,
  set_status (invalid), chat (global / unknown channel / missing entity),
  BanStore.Add validation.
- `backend/cmd/mcp/server_test.go` — WorldsimClient against mock NATS
  handlers, AuditClient against a mock HTTP server, PocketBaseClient against
  a mock HTTP server, bearerAuth middleware (valid / wrong / missing / empty
  token).

All tests use an in-process NATS server (no Docker). Run with:

```
cd backend && go test ./internal/worldsim/ ./cmd/mcp/
```

## Future work

- **Streamable HTTP transport** (2025-03-26 spec): the SDK supports it; we
  use SSE for now for broader client compatibility. Switch when the
  streamable transport is widely supported.
- **Per-tenant servers**: the `SSEHandler` getServer callback can return a
  different `*Server` per request. Currently we return one shared server;
  future multi-tenant deployments could branch on `Authorization` or
  `Host`.
- **Live audit notifications via ServerSession.SendNotification**: today we
  log audit events via slog and the SDK forwards log messages to clients
  that subscribed to `notifications/message`. A future version can push
  custom `notifications/audit-event` to subscribed clients.
- **OAuth**: the SDK has auth primitives; we use a static bearer token for
  simplicity. OAuth would let us issue per-client tokens with revocation.
- **MCP-attributed `world_options.updated` audit**: the
  `worldsim.world_options.set` handler accepts a `{options, actor}` request
  payload. The admin portal passes `actor.extension="admin"` +
  `actor.sub=<admin email>`; the MCP `set_world_options` tool passes
  `actor.extension="mcp"` (configurable via `MCP_ACTOR`). The handler
  defaults `actor.extension` to `"admin"` for back-compat with callers that
  omit it. The audit event is emitted with the passed actor, so MCP-initiated
  config changes are distinguishable from admin-portal-initiated ones.
