# 25. MCP Server — Admin Tooling for LLM Clients

The MCP (Model Context Protocol) server exposes Pixel Eruv's internals to
LLM-powered clients — Claude Desktop, Devin, Cursor, any tool that speaks
MCP. Connect a client to `https://<host>/mcp` with a bearer token, and the
LLM can inspect the live world, query audit history, read PocketBase records,
edit server-wide runtime config, and take administrative actions: kick, ban,
teleport, send chat as a player, rename, set status, swap sprite, replace
player options, dispatch extension actions.

The design doc lives at
`documentation/plans/2026-07-19-mcp-server-design.md`. The storyboard is in
`features.md` §5.9. This document is the user-facing reference: what the
server gives you, how to access it, and how to configure each MCP client.

## Why a separate binary

The MCP server is a separate Go binary (`backend/cmd/mcp`), not loaded into
worldsim. MCP request handling can be slow, can hang on PocketBase, can be
hammered by an LLM retry loop, and none of that should touch the 20Hz game
loop. The server talks to worldsim over NATS request-reply, to the audit
service over its JSON API, and to PocketBase over REST. No new shared state
is introduced. Restart or redeploy the MCP surface without dropping a single
player.

## Access

### Endpoint

- URL: `https://<PUBLIC_HOST>/mcp` (HTTP/SSE, MCP 2024-11-05 transport)
- Health: `https://<PUBLIC_HOST>/healthz` (unauthenticated, for Docker /
  nginx probes)
- Dev: `http://localhost:8085/mcp` (the `mcp` container directly)

nginx config (`docker/nginx.conf`, `location /mcp`): `proxy_buffering off`,
`proxy_read_timeout 3600s`, `chunked_transfer_encoding off` — all required
for SSE. The `/mcp` location is **NOT** behind the admin cookie
`auth_request` — MCP clients present a bearer token in the `Authorization`
header, not a browser session cookie.

### Auth

- `MCP_AUTH_TOKEN` env var, **required**. The server refuses to start if
  unset, and refuses all requests if the token is empty. Compared in
  constant time.
- Generate a strong token: `openssl rand -hex 32`.
- Send as: `Authorization: Bearer <MCP_AUTH_TOKEN>`.

### Security

The MCP server exposes full PII (IP, device_id, client_id) and admin
actions (kick, ban, teleport, chat-as, set_*). This is intentional —
moderation needs those fields (ban by IP, correlate by device_id). Access
control is the bearer token. **Do NOT expose the MCP server on the public
internet without a strong token AND network-level restrictions** (firewall,
VPN, or Tailscale).

## Configuration

### Env vars (set in `.env` before `make up` / `make deploy-remote`)

| Var | Default | Purpose |
|---|---|---|
| `MCP_AUTH_TOKEN` | *(empty = server refuses to start)* | **Required.** Bearer token clients must present. Treat as a secret. |
| `MCP_HTTP_ADDR` | `:8085` | HTTP listen address. |
| `MCP_ACTOR` | `mcp` | `actor.extension` label stamped on audit events emitted by admin actions initiated via the MCP server. Lets audit consumers filter LLM-initiated actions from client-initiated ones. |
| `NATS_URL` | `nats://localhost:4222` | NATS connection URL for worldsim + audit. |
| `AUDIT_BASE_URL` | *(empty = audit tools return an error)* | Audit service base URL for historical queries (e.g. `http://audit:8082/audit`). |
| `AUDIT_AUTH_USER` / `AUDIT_AUTH_PASS` | *(empty = no auth)* | Optional basic auth for the audit HTTP API. Matches the audit service's `AUDIT_AUTH_USER` / `AUDIT_AUTH_PASS`. |
| `PB_BASE_URL` | *(empty = PB tools return an error)* | PocketBase base URL (e.g. `http://pocketbase:8090`). |
| `PB_ADMIN_TOKEN` | *(empty = unauthenticated)* | Optional PocketBase admin token (sent raw in the `Authorization` header, no `Bearer` prefix — matches PocketBase's convention). If empty, requests are unauthenticated (works only if PB has no API rules). |

### Docker

The `mcp` service is defined in both `docker/docker-compose.yml` (dev, builds
from source) and `docker/dist/docker-compose.yml` (prod, uses the pre-built
binary). The `backend.Dockerfile` has a `mcp` target. nginx proxies `/mcp`
to `http://mcp:8085`. `make build` produces `dist/bin/mcp`.

## Connecting an MCP client

The configuration shape is the same for every HTTP/SSE MCP client: a server
URL and an `Authorization: Bearer <token>` header. Examples below.

### Claude Desktop

Edit `claude_desktop_config.json` (macOS:
`~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "pixeleruv": {
      "url": "https://<PUBLIC_HOST>/mcp",
      "headers": {
        "Authorization": "Bearer <MCP_AUTH_TOKEN>"
      }
    }
  }
}
```

Restart Claude Desktop. The `pixeleruv` server appears under Settings →
Developer → MCP servers.

### Devin CLI

Edit `~/.config/devin/config.json` (or `.devin/config.json` in the repo):

```json
{
  "mcpServers": {
    "pixeleruv": {
      "url": "https://<PUBLIC_HOST>/mcp",
      "headers": {
        "Authorization": "Bearer <MCP_AUTH_TOKEN>"
      }
    }
  }
}
```

### Cursor / other HTTP-SSE clients

Same shape — server URL + `Authorization: Bearer <token>` header. Consult the
client's docs for the exact config file location.

### Quick test with curl

```bash
curl -H "Authorization: Bearer $MCP_AUTH_TOKEN" \
     -H "Accept: text/event-stream" \
     https://<PUBLIC_HOST>/mcp
```

A successful connection returns SSE frames; an unauthorized request returns
`401 Unauthorized`; an empty configured token returns `503 Service
Unavailable` (server misconfigured).

## Surface

The MCP server exposes three layers: tools (callable, take arguments),
resources (URI-addressable, read-only), and prompts (pre-baked, fetch live
data). All return JSON.

### Tools (18)

**Read (10):**

| Tool | What it does |
|---|---|
| `get_world_stats` | Live worldsim snapshot: tick rate, uptime, total players/entities, per-map counts, per-player state (entity_id, client_id, display_name, map, x/y, is_admin, is_guest, IP), per-extension health. |
| `get_zones` | Zone metadata for all maps: id, type, shape, AV flags, exclusive flag, portal targets. |
| `query_entities` | Filter entities by map_id / entity_type / owner_extension / zone_id, limit (default 500, hard cap 500). |
| `get_entity` | Single entity snapshot by ID. |
| `query_audit_events` | Historical audit events with filters (type, severity, actor_sub, entity_id, limit, offset). |
| `get_audit_event` | Single audit event by ID. |
| `player_timeline` | Audit timeline for a player by OIDC subject (up to 200 events). |
| `list_pb_records` | PocketBase collection list with filter/sort/pagination. |
| `get_pb_record` | Single PocketBase record by collection + ID. |
| `get_world_options` | Current server-wide runtime config (world_options KV bucket): SMTP, AppURL, YouTube RTMP defaults, ffmpeg limits, world king, error-email recipients, recording gate, readOnly env mirrors (public_host, livekit_public_url). |

**Control (3):**

| Tool | What it does |
|---|---|
| `teleport_entity` | Teleport a player to a target map, optionally to a named beacon. Fire-and-forget. |
| `kick_player` | Force-disconnect a client by client_id. Saves position, emits `player.kicked` audit. |
| `ban_player` | Insert a ban record (user_id / ip / device_id) into PocketBase and kick any matching connected client. `banned_until=0` = permanent. |

**Admin overrides (5 + 1 dispatch):**

| Tool | What it does |
|---|---|
| `send_chat_as` | Send chat as a specific entity (channel: `global` or `proximity`), bypassing the connected-client requirement. |
| `set_player_name` | Rename an entity. Sanitized to ASCII printable (32-126), truncated to 20 runes. Persists to PocketBase for logged-in players. |
| `set_player_status` | Set presence (0=Available, 1=Busy, 2=DND). Broadcasts on `worldsim.player_status` so ext-av enforces DND A/V exclusion. |
| `set_player_sprite` | Set sprite_base (validated against `sprite_bases`). Empty = revert to fallback. |
| `set_player_options` | Replace player options JSON (full replace, not partial merge). Common fields: `show_own_name_tag` (bool), `zoom` (1-4). |
| `set_world_options` | Replace server-wide runtime config (world_options KV bucket). Full replace — call `get_world_options` first, modify the fields you want, then pass the full object back. worldsim validates, writes KV, broadcasts `world_options.update` so consumers (SMTP client, ext-rec ffmpeg/YouTube, frontend recording gate) hot-reload without restart. readOnly fields (`public_host`, `livekit_public_url`) are preserved by worldsim and ignored in the input. Validation: `smtp_host` required, `smtp_port` 1-65535, `ffmpeg_concurrency` ≥ 1, `ffmpeg_timeout` ≥ 1s, `error_email_recipients_mode` ∈ {none, king, all_admins, custom} (king requires `king_email`, custom requires `error_email_custom_addresses`). |
| `dispatch_extension_action` | Publish to `extension.<id>.action`. Fire-and-forget — use the audit log to confirm whether the extension handled it. |

### Resources (11)

URI-addressable, read-only. Use the `pixeleruv://` scheme.

**Static (5):**

| URI | Returns |
|---|---|
| `pixeleruv://world/stats` | Live worldsim snapshot. |
| `pixeleruv://world/zones` | Zone metadata for all maps. |
| `pixeleruv://world/players` | Currently-connected players. |
| `pixeleruv://world/extensions` | Registered extensions and their alive/heartbeat state. |
| `pixeleruv://audit/stats` | 24h severity + type counts, audit service uptime and version. |

**Templated (6):**

| URI template | Returns |
|---|---|
| `pixeleruv://world/maps/{name}` | A single map's stats. |
| `pixeleruv://world/entities/{id}` | A single entity snapshot. |
| `pixeleruv://audit/events/{id}` | A single audit event by integer ID. |
| `pixeleruv://audit/players/{sub}` | Audit timeline for a player by OIDC subject. |
| `pixeleruv://pb/{collection}` | First page (30 items) of a PocketBase collection. |
| `pixeleruv://pb/{collection}/{id}` | A single PocketBase record. |

### Prompts (3)

Pre-baked prompt templates that fetch live data and return it as a user-role
message so the LLM has fresh context.

| Prompt | Args | What it does |
|---|---|---|
| `summarize_recent_audit` | `limit` (default 50, max 500) | Fetch the last N audit events, group by severity and event type, present the summary. |
| `investigate_player` | `sub` (required) | Pull a player's audit timeline, current world state (if online), and PocketBase record in one shot. |
| `world_health_report` | *(none)* | Bundle worldsim stats, extension alive status, and recent warn/error audit events for a quick health assessment. |

## Audit attribution

Admin actions initiated via the MCP server emit audit events stamped with
`actor.extension = <MCP_ACTOR>` (default `"mcp"`) so audit consumers can
filter LLM-initiated actions from client-initiated ones.

`set_world_options` is a special case: the `worldsim.world_options.set`
handler accepts an `actor` field in its request payload. The MCP server
sends `actor = {extension: <MCP_ACTOR>}`; the admin portal sends
`actor = {extension: "admin", sub: <admin email>}`. If the actor is
omitted, worldsim defaults `actor.extension` to `"admin"` for backward
compatibility.

## Storyboard

Open Claude Desktop (or Devin, or Cursor) configured with the Pixel Eruv
MCP server URL and bearer token. Ask the LLM:

- "Who's online right now?" — it calls `get_world_stats` and lists the
  players, their maps, and their IPs.
- "Summarize recent audit activity" — it runs the `summarize_recent_audit`
  prompt and reports that there were 3 kicks and 12 chat messages in the
  last hour.
- "Investigate the player with sub `google-oauth2|12345`" — it runs
  `investigate_player` and shows their timeline, current online state, and
  PB record.
- "Kick client `c_abc123` for being abusive" — it calls `kick_player`,
  confirms the action landed, then queries the audit log to show you the
  `player.kicked` event it just emitted.
- "Disable recording globally" — it calls `get_world_options`, flips
  `recording_enabled` to false, calls `set_world_options` with the full
  object, and confirms the `world_options.updated` audit event tagged
  `actor.extension=mcp`.

Your LLM has the same tools a human admin has. It reads the world, queries
the audit log, takes an action, and verifies its own work — all through one
authenticated endpoint.

## Testing

The MCP server has unit tests in `backend/cmd/mcp/` covering the
WorldsimClient (NATS round-trip against a mock worldsim), AuditClient (HTTP
against a mock server), PocketBaseClient (HTTP against a mock server), the
full `NewMCPServer` registration (catches struct-tag panics like the one
that crashed production on first deploy), and the bearer-token middleware
(valid / wrong / missing / empty token).

Run with:

```bash
cd backend && go test ./cmd/mcp/ -v
```

All tests use an in-process NATS server (no Docker).

## Future work

- **Streamable HTTP transport** (2025-03-26 spec): the SDK supports it; we
  use SSE for now for broader client compatibility. Switch when the
  streamable transport is widely supported.
- **Per-tenant servers**: the `SSEHandler` `getServer` callback can return a
  different `*Server` per request. Currently we return one shared server;
  future multi-tenant deployments could branch on `Authorization` or
  `Host`.
- **Live audit notifications via `ServerSession.SendNotification`**: today
  we log audit events via slog and the SDK forwards log messages to clients
  that subscribed to `notifications/message`. A future version can push
  custom `notifications/audit-event` to subscribed clients.
- **OAuth**: the SDK has auth primitives; we use a static bearer token for
  simplicity. OAuth would let us issue per-client tokens with revocation.
