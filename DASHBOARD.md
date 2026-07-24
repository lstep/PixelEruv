# Dashboard

## Admin Teleport Buttons — "Teleport to" + "Teleport to me"

**Status:** Implemented — `make proto`, `make build`, `go test ./internal/worldsim/`, `tsc --noEmit`, `vite build` all pass. 4 new admin-teleport tests green. Branch: `feature/admin-teleport-buttons`. Not yet tested on ellipsis.

Added two admin-only buttons to both the avatar dropdown and the Players panel modal:

- **"Teleport to"** — opens a map picker (fetched from new public `GET /api/assets/maps` endpoint) and sends the target player to the selected map via `AdminTeleportFrame` (random spawn zone).
- **"Teleport to me"** — sends the target player to the admin's exact position on the admin's current map via `AdminTeleportFrame` with `exact_position=true`.

### What was built

- **`proto/frames.proto`** — New `AdminTeleportFrame` message (ClientFrame oneof case 15): `entity_id`, `map_id`, `x`, `y`, `exact_position`.
- **`backend/internal/worldsim/entity_teleport.go`** — Extracted `subscribeEntityTeleport()` from the inline subscription in `subscribe()`. Handles `worldsim.entity.teleport` with sender admin auth (non-admins rejected with forbidden audit) + exact-position passthrough. Trusted callers (MCP, extensions) publish without `sender_client_id` and skip the auth check.
- **`backend/internal/worldsim/portal.go`** — `portalTransitionReq` + `transition()` now accept `spawnX/spawnY/exactSpawn`. When `exactSpawn` is true, uses x/y directly instead of beacon/random-spawn resolution. Zone-triggered transitions leave these zero/false (unchanged).
- **`backend/internal/worldsim/asset_http.go`** — New `handleAssetMapsList` serving `GET /api/assets/maps` → `[{name, is_default}]` via `MapStore.ListAllMaps()`.
- **`backend/internal/worldsim/worldsim.go`** — Registered the new route; replaced inline teleport subscription with `subscribeEntityTeleport()` call.
- **`backend/internal/pusher/pusher.go`** — New `ClientFrame_AdminTeleport` case forwarding to `worldsim.entity.teleport` with `sender_client_id`, `entity_id`, `map_id`, `x`, `y`, `exact_position`.
- **`backend/internal/worldsim/admin_teleport_test.go`** — 4 tests: admin exact-position, admin random-spawn, non-admin rejection, trusted-caller (no sender) bypass.
- **`frontend/src/net/WsClient.ts`** — `sendAdminTeleport(entityId, mapId, x, y, exact)`.
- **`frontend/src/scenes/GameScene.ts`** — Avatar dropdown admin buttons + `openMapTeleportMenu` (Phaser submenu, counter-scaled); wired Players panel callbacks (`onAdminTeleportToMap`, `onAdminTeleportToMe`).
- **`frontend/src/ui/TopMenu.ts`** — `PlayersPanelOpts` extended; admin buttons in player rows with inline map picker (`toggleMapPickerForRow` + `fetchMapList`).

### Design decisions

- **Server-side auth is the real gate** — frontend button visibility is cosmetic, consistent with the existing `teleport_to_entity` pattern. The `worldsim.entity.teleport` handler now checks `sender_client_id` → admin; empty sender = trusted caller (MCP/extensions), preserving original behavior.
- **Fire-and-forget** — no ack frame (matches kick/teleport_to_entity). Same-map targets move via replication; cross-map targets get a `MapTransitionFrame`.
- **Map list cached** on both GameScene and TopMenu instances after first fetch from `/api/assets/maps`.

### Next steps

- Test on ellipsis (remote): login as admin → verify both buttons appear only for admin; test teleport-to-map and teleport-to-me on a target player; verify non-admin sees neither button.
- Push + create PR when verified.

## AOI Grid — Phase 2 of MMORPG-Scale World Engine

**Status:** Implemented — `make proto`, `make build`, `go test ./internal/worldsim/` all pass. 7 new AOI tests + all existing tests green.

**Design doc:** `documentation/plans/2026-07-23-mmorpg-scale-and-worldgen-design.md` (section 5.0 for implementation order, section 5 Phase 2 for spec).

Replaces "replicate everything on the same map" with cell-based Area of Interest (AOI) filtering. Each client now only receives replication for entities within their AOI radius, with hysteresis to prevent spawn/despawn storms at cell boundaries. This is the single highest-impact performance change for scaling to large maps — converts O(N*M) replication bandwidth to O(N*k) where k = entities in AOI radius.

### What was built

- **`backend/internal/worldsim/aoi.go`** — `AOIGrid` struct: spatial hash grid with `cellSize=16` tiles, `Insert(entity)`, `EntitiesInRadius(pos, radiusCells)`. One grid per map, rebuilt from scratch each tick before replication (O(M) hash inserts, ~100µs for 1000 entities — avoids incremental maintenance across provision/despawn/portal-transition).
- **`backend/internal/worldsim/world.go`** — Added `aoiGrids map[string]*AOIGrid` field to `World`.
- **`backend/internal/worldsim/worldsim.go`** — Added `rebuildAOIGrids()` method, called in `tick()` after movement/zone/proximity and before replication. Passes grids to `ReplicationInput`.
- **`backend/internal/worldsim/replication.go`** — Added `AOIGrids` to `ReplicationInput`. In `replicateToClient`, replaced the same-map filter with AOI filtering: entities within `aoiUnsubscribeRadius` (4 cells = 64 tiles) stay spawned/updated; entities beyond that get `DestroyEntity` + flag clear; new entities only spawn if within `aoiSubscribeRadius` (3 cells = 48 tiles). The hysteresis band (48-64 tiles) prevents thrashing. Client's own entity always bypasses AOI. Falls back to whole-map replication when no grid is available (nil `AOIGrids`).
- **`backend/internal/worldsim/aoi_test.go`** — 7 tests: `TestAOI_BasicFiltering` (far entity not replicated), `TestAOI_BoundaryCrossing` (spawn on enter, destroy on exit, re-spawn on re-entry), `TestAOI_Hysteresis` (entity in hysteresis band stays spawned, no thrash), `TestAOI_FallbackNoGrid` (nil grids = whole-map replication), `TestAOI_ClientAlwaysSeesSelf`, `TestAOIGrid_BasicInsert`, `TestAOIGrid_NilPosition`.
- **`ROADMAP.md`** — Added MMORPG-Scale World Engine initiative with recommended implementation order table.

### Design decisions

- **Rebuild grids from scratch each tick** instead of incremental maintenance. Simpler, correct, negligible cost. Avoids hooking into provision/despawn/portal-transition/map-reload.
- **Reuse existing `spawnedTo[clientID]` as subscription state** — no new per-client tracking. AOI filter decides spawn/update; entities leaving unsubscribe radius get DestroyEntity + flag clear.
- **Hysteresis**: subscribe radius (3 cells) < unsubscribe radius (4 cells). Entity at 50 tiles (cell 3) spawns; moves to 70 tiles (cell 4) stays spawned; moves to 100 tiles (cell 6) gets destroyed.
- **Fallback to whole-map** when no grid is available — ensures backward compatibility and graceful degradation.
- **Zone indexing by cell** (design doc item 4) deferred — secondary optimization, not needed for the replication win.

### Performance impact

- **Small maps (50x50)**: AOI covers entire map (3x3 cells at 16 tiles = 48 tiles > 50). Degrades to current whole-map behavior. No regression.
- **Medium maps (200x200)**: AOI covers 7x7 = 49 cells out of 144 (~34%). ~3x bandwidth reduction.
- **Large maps (2000x2000)**: AOI covers 49 cells out of 15625 (~0.3%). ~300x bandwidth reduction. This is the scaling win that makes worldgen viable.

### Next phases (per ROADMAP.md)

1. Phase 1: Infinite map support (backend parser for Tiled infinite maps)
2. Phase 4: Worldgen terrain + biomes
3. Phase 3: Delta compression (second-order bandwidth optimization)

## Roadmap (not started)

- **Periodic world snapshot on the Welcome page** — show a 60s-refreshed full-map image (map + props + live players) below the buttons on `/welcome/`. Rendering approach deferred; the two obvious paths (headless browser container, Go server-side render) are rejected for now as respectively heavyweight or divergent. See `documentation/plans/2026-07-18-world-snapshot-on-welcome-roadmap.md` for alternatives to explore (SVG minimap via stats channel, real-client canvas upload, static map + live overlay, off-host headless).

## World Options (server-wide runtime config)

**Status:** Implemented — `make proto`, `go build ./...`, `go test ./internal/worldsim/`, `npm run build` all pass.

A new `world_options` NATS KV bucket (key `current`) is the single source of truth for server-wide runtime config: SMTP (host/port/user/pass/from/sender/TLS), `APP_URL`, YouTube RTMP defaults (`youtube_rtmp_url` / `youtube_stream_key`), and ffmpeg audio-extraction limits (`ffmpeg_concurrency` default 2, `ffmpeg_timeout` default 10m). worldsim owns the bucket, seeds hardcoded defaults on first boot, and broadcasts `world_options.update` on every save so consumers hot-reload without restarting.

### What was built

- **`backend/internal/worldsim/worldoptions.go`** — `WorldOptionsManager`: creates/binds the KV bucket, seeds defaults if absent, `Get`/`Set` (validates, puts to KV, publishes update), `PublishUpdate` (called on `worldsim.ready` so late subscribers catch up). `PUBLIC_HOST` and `LIVEKIT_PUBLIC_URL` mirrored read-only from env on every boot (not editable — TLS cert / LiveKit tokens are startup-baked).
- **`backend/internal/worldsim/worldoptions_sub.go`** — NATS request-reply `worldsim.world_options.get` / `.set` (admin-gated by the admin portal's signed-cookie session; worldsim trusts the admin service as a NATS peer, same pattern as `recording.*`). Plus `GET /api/world-options` on the embedded PocketBase for the frontend (admin-gated via users JWT + `players.is_admin`; returns only YouTube fields + `public_host`, not SMTP password).
- **`backend/cmd/worldsim/main.go`** — `configureSMTP` replaced by `applySMTPFromOptions(app, opts)`; called once after `worldsim.New()` and again on every `world_options.update` via `sim.OnWorldOptionsUpdate(...)`. SMTP/APP_URL env vars removed.
- **`backend/cmd/ext-rec/main.go`** — fetches world_options via `worldsim.world_options.get` at startup, subscribes to `world_options.update` for hot-reload. Replaces `YOUTUBE_RTMP_URL` / `YOUTUBE_STREAM_KEY` / `PUBLIC_HOST` env reads and the hardcoded `audioSem := make(chan struct{}, 2)` + `10*time.Minute` timeout. New `dynamicSemaphore` (sync.Cond-based) so capacity hot-reloads. `buildEgressRequest` now accepts per-recording YouTube override fields.
- **`proto/frames.proto`** — `RecordingRequestFrame` gains optional `youtube_rtmp_url` (field 5) and `youtube_stream_key` (field 6) for per-recording override. Pusher passes them through unchanged.
- **`frontend/src/net/WorldOptions.ts`** — fetches the YouTube subset from `/api/world-options` (admin-gated), cached.
- **`frontend/src/ui/TopMenu.ts`** — "Stream to YouTube" now opens a confirm modal pre-filled from world_options; the host can edit RTMP URL / stream key for this recording only. Cancel sends nothing.
- **`backend/cmd/admin/`** — new `handleWorldOptions` GET/POST + `/admin/world-options` route. POST sends the form to `worldsim.world_options.set` via NATS request-reply. New template `templates/world_options.html` (grouped sections, read-only PUBLIC_HOST/LIVEKIT_PUBLIC_URL). New landing tile "World Options".
- **Docker compose** (`docker/docker-compose.yml` + `docker/dist/docker-compose.yml`) — removed `SMTP_*`, `APP_URL`, `YOUTUBE_*` env from `worldsim` and `ext-rec`. Kept `PUBLIC_HOST` (TLS cert SAN) and `LIVEKIT_PUBLIC_URL` (ext-av tokens).
- **Tests** — `backend/internal/worldsim/worldoptions_test.go` covers seed defaults, persistence across manager instances, `Set` broadcasting `world_options.update`, and validation rejecting invalid values. Uses a new `startEmbeddedNATSWithJetStream` helper (the existing `startEmbeddedNATS` doesn't enable JetStream).

### Notes / migration

- **No .env migration.** Operators with `SMTP_HOST=…` in `.env` will have those vars ignored after this change. worldsim seeds hardcoded defaults on first boot; edit them via Admin > World Options.
- **NATS KV is a new pattern in this repo** — the codebase previously used only NATS Core pub/sub. The `nats:2.10-alpine` image already runs with `-js`, so KV works without config changes. KV is semi-persistent (survives NATS restart, lost on volume wipe); worldsim re-seeds defaults if the bucket is empty.
- **Secrets in NATS KV** — SMTP password and YouTube stream key stored in the KV bucket. NATS is inside the stack; the write endpoint is admin-gated. Acceptable for this deployment.

### Additions: world king, error emails, recording gate

Three new world_options fields added after the initial world_options PR:

- **World king** (`king_name`, `king_email`) — display-only metadata. The king's name is shown on the welcome page footer (public `GET /api/world-king` endpoint, no auth); the email is visible only on the admin World Options page and is the default recipient for error emails when mode = "king". No special permissions.
- **Error email notifications** (`error_email_recipients_mode` ∈ {none, king, all_admins, custom}, `error_email_custom_addresses`) — the audit service emails recipients on `SeverityError` audit events. SMTP config + recipient mode are fetched from worldsim via `worldsim.world_options.get` at startup and hot-reloaded on `world_options.update`. For `all_admins` mode, admin emails are resolved via a new `worldsim.admin_emails.get` NATS request-reply (worldsim owns PocketBase; the audit service has no PB access). Emails sent in a goroutine so audit persistence is never blocked on SMTP. New `backend/cmd/audit/notifier.go`.
- **Recording gate** (`recording_enabled` bool, default true) — when false, ext-rec refuses `recording.start` (emits `recording.start_denied` audit event with reason `globally_disabled`) and the frontend disables the Record button with a "Recording is disabled globally" tooltip. Hot-reloaded on `world_options.update`.

**Deviation from the option label:** the king's email is shown only on the admin World Options page, not on the public welcome page footer (spam risk). The welcome footer shows the king's name only. The email is still stored and used for error emails.

## MCP server (admin LLM tooling)

**Status:** Implemented — `make proto`, `make build`, `go test ./internal/worldsim/`, and `go test ./cmd/mcp/` all pass. Not yet exercised end-to-end with a real MCP client (Claude Desktop / Devin / Cursor).

A new `backend/cmd/mcp` binary exposes PixelEruv internals to MCP clients (Claude Desktop, Devin, Cursor, etc.) over HTTP/SSE on `:8085/mcp`. Bearer-token auth (`MCP_AUTH_TOKEN`, required). Separate binary from worldsim to isolate MCP load from the game loop. Talks to worldsim over NATS, audit over HTTP, PocketBase over REST.

### What was built

- **New worldsim NATS subjects** (request-reply, all reply with JSON):
  - Read: `worldsim.entities.query` (filter by map/type/owner/zone, limit ≤500), `worldsim.entity.get` (single by ID). `backend/internal/worldsim/entities_query.go`.
  - Control: `worldsim.client.kick` (despawn + audit), `worldsim.client.ban` (BanStore.Add + kick matching). `backend/internal/worldsim/admin_actions.go`.
  - Admin overrides (bypass client-ID validation, use entity_id): `worldsim.admin.chat`, `worldsim.admin.set_name`, `worldsim.admin.set_status`, `worldsim.admin.set_sprite`, `worldsim.admin.set_player_options`. All reply `{"ok":bool,"error":"..."}`. `backend/internal/worldsim/admin_actions.go`.
- **BanStore.Add** method (`backend/internal/worldsim/banstore.go`) — inserts a ban record into the `bans` PocketBase collection with target_type / target_value / reason / banned_until / banned_by.
- **Audit JSON API** (`backend/cmd/audit/server.go`) — new endpoints `GET /audit/api/events`, `/audit/api/events/{id}`, `/audit/api/players/{sub}`, `/audit/api/stats` for historical queries (previously HTML-only).
- **MCP binary** (`backend/cmd/mcp/`): `main.go` (env config + NATS connect), `server.go` (HTTP/SSE + bearer auth), `worldsim_client.go` / `audit_client.go` / `pb_client.go` (NATS + HTTP wrappers), `tools.go` (18 tools — 16 original + `get_world_options` / `set_world_options`), `resources.go` (5 static + 6 templated resources), `prompts.go` (3 prompts: summarize_recent_audit, investigate_player, world_health_report).
- **Docker**: `mcp` service in both `docker/docker-compose.yml` (builds from source) and `dist/docker-compose.yml` (uses pre-built binary). `backend.Dockerfile` `mcp` target. nginx proxies `/mcp` to `http://mcp:8085` with `proxy_buffering off` + 1h read timeout (SSE). NOT behind admin cookie auth_request — bearer-token auth at the app layer.
- **Makefile**: `make build` now produces `dist/bin/mcp`.

### Tools exposed

- Read: `get_world_stats`, `get_zones`, `query_entities`, `get_entity`, `query_audit_events`, `get_audit_event`, `player_timeline`, `list_pb_records`, `get_pb_record`, `get_world_options`.
- Control: `teleport_entity`, `kick_player`, `ban_player`.
- Admin overrides: `send_chat_as`, `set_player_name`, `set_player_status`, `set_player_sprite`, `set_player_options`, `set_world_options`, `dispatch_extension_action`.

### PII

The MCP server exposes full PII (IP, device_id, client_id) — necessary for moderation (ban by IP, correlate by device_id). Access control is the bearer token. Do NOT expose on the public internet without a strong token + network-level restrictions.

### Design doc

`documentation/plans/2026-07-19-mcp-server-design.md`

### Verification

```
cd backend && go test ./internal/worldsim/ ./cmd/mcp/
```

Tests: `entities_query_test.go`, `admin_actions_test.go` (worldsim); `server_test.go` (mcp — WorldsimClient against mock NATS, AuditClient + PocketBaseClient against mock HTTP, bearerAuth middleware).

### Future work

- Switch from SSE to streamable HTTP transport (2025-03-26 spec) once widely supported by clients.
- Per-tenant servers via the `SSEHandler` getServer callback.
- Live audit notifications via `ServerSession.SendNotification` (currently routed through slog → `notifications/message`).
- OAuth for per-client tokens with revocation (currently a static bearer token).

## Players page (audit leaderboard + detail)

**Status:** Implemented — `go build ./cmd/audit/ ./cmd/mcp/`, `go test ./cmd/audit/ ./cmd/mcp/`, `go test ./internal/worldsim/` all pass. Smoke-tested with empty DB (templates render, API returns empty arrays, SVG renders offline segment).

New `/audit/players` leaderboard + rich `/audit/players/{sub}` detail page in the audit service. Two new MCP tools (`list_players`, `player_activity`) expose the same data to LLM clients.

### What was built

- **Store layer** (`backend/cmd/audit/store.go`) — 4 new `SQLiteStore` methods + 2 types:
  - `ListPlayers()` — per-`actor_sub` aggregates (excluding guests/`dev`): display name, first/last seen, event count, connect count, total session time (pairs `client.connected`/`client.disconnected` by `(sub, client_id)`; open sessions count to now). Sorted by total session time desc.
  - `PlayerSessions(sub)` — connect/disconnect pairs for one sub, ordered by connected_at desc. Open sessions flagged.
  - `PlayerEvents(sub, limit)` — events where `actor_sub = ?` OR `actor_entity` belongs to this player (captures `player.set_status`/`player.set_afk` which carry entity_id but not sub).
  - `PlayerActivityEvents(sub, since)` — only `client.connected`, `client.disconnected`, `player.set_status`, `player.set_afk` (same sub-or-entity join), ordered ASC. Used for the SVG timeline.
  - Types: `PlayerSummary`, `Session`.
- **Audit handlers** (`backend/cmd/audit/server.go`):
  - `handlePlayersList` at `/audit/players` — leaderboard.
  - `handlePlayerDetail` at `/audit/players/{sub}` — replaces old `handlePlayerTimeline`. Builds activity timeline segments in Go (walks events chronologically, maintains `{connected, status, afk}`, emits segments `{start, end, state}` where state ∈ {offline, present, busy, dnd, afk}).
  - JSON API: `GET /audit/api/players` (list), `GET /audit/api/players/{sub}` (detail with sessions + events + activity_events). Supports `?since_hours=N` query param for activity window.
- **Templates** (`backend/cmd/audit/templates/`):
  - `players.html` — leaderboard table: rank, name (link to detail), total time, first/last seen, sessions, events.
  - `player_detail.html` — summary cards + inline SVG 7-day activity timeline (colored `<rect>` segments with `<title>` tooltips) + legend (present/busy/dnd/afk/offline) + session history table + full event list. Replaces `player_timeline.html` (deleted).
  - `base.html` — Players nav link added.
  - `embed.go` — new template funcs: `add`, `durationStr` (accepts `time.Duration` or `int64`), `segmentClass`, `segmentX`, `segmentW` (SVG coordinate helpers).
- **CSS** (`backend/cmd/audit/static/style.css`) — `.players-table`, `.activity-svg` (responsive SVG with segment fill colors), `.activity-legend`, `.session-table`.
- **MCP** (`backend/cmd/mcp/`):
  - `audit_client.go` — `ListPlayers()` + `PlayerActivity(sub, sinceHours)` methods. `PlayerTimeline` updated to parse the new detail response format (with backward-compat fallback for older audit servers).
  - `tools.go` — 2 new tools: `list_players` (no args), `player_activity` (sub + optional since_hours). Additive — existing `player_timeline` tool kept.
- **Tests**:
  - `backend/cmd/audit/store_test.go` — `TestListPlayers` (guest/dev exclusion, session duration computation, open session, sorting), `TestPlayerSessions` (pairing, open vs closed, ordering), `TestPlayerEvents` (entity-id join captures status events), `TestPlayerActivityEvents` (only 4 event types, ASC ordering, time window filtering).
  - `backend/cmd/mcp/server_test.go` — extended `TestAuditClient_HTTP` with `ListPlayers` + `PlayerActivity` assertions; mock updated to new detail response format.

### Design decisions

- **Entity-id join**: `player.set_status`/`player.set_afk` events lack `actor_sub` (only carry `entity_id` + `client_id`). The `PlayerEvents`/`PlayerActivityEvents` queries join via `actor_entity IN (SELECT DISTINCT actor_entity FROM audit_events WHERE actor_sub = ? AND actor_entity != '')`. Safe because registered users have stable entity_ids derived from PocketBase records.
- **Session pairing by client_id**: `client_id` is `c_` + 16 random hex chars (unique per WebSocket session). Each `(sub, client_id)` group has at most one connect + one disconnect. Missing disconnects (server crash) count as open sessions to now — overestimates duration for crashed sessions. Acceptable for an audit view.
- **SVG timeline**: server-rendered static SVG `<rect>` elements with native `<title>` tooltips. No JS charting dependency. 1000-unit viewBox, segments positioned by time offset within the 7-day window. Minimum width of 1 unit ensures very short segments are visible. Day grid lines + labels (e.g. "Jul 17") at each midnight boundary.
- **`durationStr` template func**: accepts `any` (handles both `time.Duration` from `Session.Duration` and `int64` from `PlayerSummary.TotalSessionNs`) because Go templates can't convert named types automatically.
- **PocketBase merge**: the audit service fetches all registered players from PocketBase's `players` collection via REST (`PB_BASE_URL` env var) and merges with audit stats. Players who registered but never connected (or whose events were purged by retention) still appear with zero stats. Falls back to audit-only data if PB is not configured. Sort: active players first (by total session time desc), then inactive players (by registration date desc).

## A/V meeting recording (ext-rec + LiveKit Egress)

**Branch:** `feat/recording`
**Status:** Implemented — `make proto`, `make build`, `go test ./internal/worldsim/`, and `tsc --noEmit` all pass. Not yet tested end-to-end with a running Docker stack (needs `make up` + manual browser test as admin).

Admin-hosted recording of A/V meetings in LiveKit zone rooms via a new `ext-rec` extension + the LiveKit Egress service. Two mutually exclusive targets per recording: local MP4 (v1) or YouTube RTMP live stream. One recording per room at a time. Host = admin for v1. Proximity rooms unrecorded in v1.

### What was built

- **Proto:** `RecordingRequestFrame` (client→server), `RecordingStateFrame` + `RecordingActiveFrame` (server→client) in `proto/frames.proto`.
- **Worldsim:** `worldsim.entity_info` request-reply handler (returns `is_admin`, `status`, `display_name`, `map_id`, `client_id` for a given entity). `RecordingStore` + `worldsim.recording.create` / `worldsim.recording.update` NATS handlers (extensions don't have direct PocketBase access). `recordings` PB collection migration (`1753700000`).
- **ext-rec** (`backend/cmd/ext-rec`): New extension. Subscribes to `recording.start` / `recording.stop`. Authorizes via `worldsim.entity_info` (admin only). Starts/stops a LiveKit Room Composite Egress via `lksdk` (`livekit/server-sdk-go/v2`). MP4 target writes to `RECORDINGS_DIR`; YouTube target streams RTMP to `YOUTUBE_RTMP_URL/YOUTUBE_STREAM_KEY`. Inserts/updates PB rows via worldsim NATS. Fans out `recording_active` to all participants. Audit emits on start/stop/denied.
- **Pusher:** Forwards `ClientFrame_Recording` to `recording.<action>` NATS subject (adds `client_id` + `entity_id` from session). Forwards `client.<id>.recording_state` + `client.<id>.recording_active` back to the browser as `ServerFrame_RecordingState` / `ServerFrame_RecordingActive`.
- **Frontend:** TopMenu record button (admin-only, visible when in an A/V room). Click → menu with "Record to MP4" / "Stream to YouTube". Active state → "● Stop REC" (red). GameScene shows a blinking "REC ●" DOM pill + one-time toast when a recording is active. `WsClient.sendRecording()` + `onRecordingState` / `onRecordingActive` handlers.
- **Docker:** `livekit-egress` service (v1.9.0) + `ext-rec` service + `recordings` volume in both `docker/docker-compose.yml` and `docker/dist/docker-compose.yml`. `backend.Dockerfile` `ext-rec` target added.

### Design doc

`docs/plans/2026-07-18-recording-design.md`

### Known v1 limitations (documented in design doc)

- No auto-stop on empty room (host must stop manually).
- ext-rec restart loses `activeRecs` state (orphan Egress cleanup is v2).
- YouTube target is one global channel (env vars).
- Consent = indicator + toast + audit capture; no opt-in dialog.
- No frontend listing of past recordings (PB `recordings` collection exists; UI is later work).

## Periodic position save + zoom persistence

**Branch:** `feat/periodic-position-and-zoom-persist`
**Status:** Implemented — `go build ./...`, `go test ./internal/worldsim/`, and `npm run build` all pass.

Two persistence gaps fixed:

1. **Player position was only saved on clean disconnect** (`despawnClient`). A worldsim or pusher crash (where `client.disconnected` never fires) lost the player's position back to their last spawn/restore point. Added `startPositionPersister` — a 30s ticker launched in `Run` that saves each connected player's position+map_id to PocketBase, skipping entities whose position hasn't changed since the last save (zero idle write load). `lastSavedPos` map on `Simulator` tracks the last persisted x/y/map per entity; cleared in `despawnClient`.
2. **Camera zoom reset to `ZOOM_DEFAULT` on every reload.** Zoom is now stored in the existing `player_options` JSON (alongside `show_own_name_tag`), so it rides the existing `SetPlayerOptionsFrame` → PocketBase `players.options` path — no proto or PB schema changes. The wheel handler debounces (500ms) a `setPlayerOptions` call so a stream of wheel notches coalesces into one server write. `applyPlayerOptions` restores the saved zoom (clamped to `[ZOOM_MIN, ZOOM_MAX]`) on connect. Guests have no PB record and reset to default on reload, consistent with `show_own_name_tag`.

### Files

| File | Changes |
|---|---|
| `backend/internal/worldsim/worldsim.go` | `savedPos` type; `Simulator.lastSavedPos` field; init in `New`; `startPositionPersister`/`collectChangedPositionsLocked`/`persistChangedPositions`; `positionPersistInterval` const; launched in `Run`; cleanup in `despawnClient` |
| `backend/internal/worldsim/worldsim_persist_test.go` | New — `TestCollectChangedPositionsLocked_SkipsUnchanged` |
| `frontend/src/scenes/GameScene.ts` | `zoomPersistTimer` field; `scheduleZoomPersist` debounced send; wheel handler calls it; `applyPlayerOptions` restores saved zoom |
| `frontend/src/ui/TopMenu.ts` | `parsePlayerOptions` return type widened to include `zoom?: number` |

## Persist player presence status to PocketBase

**Status:** Implemented — `make proto`, `go build ./...`, `go test ./internal/worldsim/`, and `tsc --noEmit` all pass.

Player presence status (Available / Busy / Do Not Disturb) was session-only — stored in `Entity.Status` in worldsim memory, reset to Available on every connect. This caused sync problems: a page reload lost the value, the TopMenu hardcoded `applyStatus(0)` on init, and server/client could disagree.

Status is now persisted to a `status` NumberField on the `players` PocketBase collection and restored on connect, mirroring the pattern used for `display_name`, `sprite_base`, `options`, and `hide_admin_badge`. Reconnect restores as-is (DND stays DND across sessions). Guests have no PB record and remain session-only. Save-on-change (mirrors `UpdateDisplayName`), not save-on-disconnect.

Design doc: `documentation/plans/2026-07-15-player-status-design.md`.

### Files

| File | Changes |
|---|---|
| `backend/migrations/1753600000_add_status_to_players.go` | New — `status` NumberField on `players` (default 0) |
| `backend/internal/worldsim/userstore.go` | `UserRecord.Status`; `UpdateStatus` method; read in `recordToUser` |
| `backend/internal/worldsim/worldsim.go` | Restore `user.Status` in `provisionClient`; persist in `handleSetStatus`; updated comments |
| `backend/internal/pusher/pusher.go` | Comment updated (was "Session-only — not persisted to PocketBase") |
| `proto/components.proto` | `DisplayName.status` comment updated |
| `frontend/src/ui/TopMenu.ts` | `applyStatusFn` field; `syncStatusFromServer(value)` method (updates UI without echoing a SetStatusFrame) |
| `frontend/src/scenes/GameScene.ts` | Call `avClient.setStatus` + `topMenu.syncStatusFromServer` for local player on spawn and DisplayName update |
| `documentation/plans/2026-07-15-player-status-design.md` | New — design doc |

## Default map selection via `is_default` field

**Status:** Implemented — `make proto`, `make build`, `go test ./internal/worldsim/`, and `tsc --noEmit` all pass.

The default map (where new players spawn and the map the frontend loads on first paint) is now chosen via an `is_default` boolean field on the `maps` collection in the PocketBase admin UI, replacing the `DEFAULT_MAP` (backend) and `VITE_MAP_NAME` (frontend) env vars. To change the spawn map, set `is_default=true` on exactly one map in the PB admin UI and restart worldsim. If maps exist but none has `is_default=true`, worldsim fails fast at startup with a clear error.

### What changed

- **Migration:** `1753600000_add_is_default_to_maps.go` — adds `is_default` bool field to `maps`; backfills `main` (or first record) as default if no record has it set, so existing deployments keep working.
- **MapStore:** `MapRecordInfo.IsDefault` field; read in `ListAllMaps`/`FetchMapRecordInfo`; `SeedMapIfMissing` sets `is_default=true` on the seeded record.
- **Worldsim:** `New` signature drops `defaultMap` param. New `selectDefaultMap(records)` helper returns the `is_default` map or errors if maps exist but none is default. Seeding now triggers only when zero maps exist (was: when the named default was absent).
- **main.go:** `DEFAULT_MAP` env var removed.
- **Frontend:** `mapLoader.ts` queries `is_default=true` map when no name given; `MapAssets.name` carries the resolved record name; `VITE_MAP_NAME` removed. `main.ts` uses `mapAssets.name` for `loadedMapName`.
- **Docker:** `DEFAULT_MAP` removed from both `docker/docker-compose.yml` and `docker/dist/docker-compose.yml`.
- **Test:** `defaultmap_test.go` — unit tests for `selectDefaultMap` (is_default present, multiple flagged, none flagged, empty).

### Files

| File | Changes |
|---|---|
| `backend/migrations/1753600000_add_is_default_to_maps.go` | New — `is_default` field + backfill |
| `backend/internal/worldsim/mapdata.go` | `MapRecordInfo.IsDefault` field |
| `backend/internal/worldsim/mapstore.go` | Read `is_default` in list/fetch; seed sets it |
| `backend/internal/worldsim/worldsim.go` | `New` signature, `selectDefaultMap`, fail-fast, conditional seeding |
| `backend/internal/worldsim/defaultmap_test.go` | New — `selectDefaultMap` unit tests |
| `backend/cmd/worldsim/main.go` | Drop `DEFAULT_MAP` env var |
| `frontend/src/mapLoader.ts` | Query `is_default` map, `MapAssets.name`, drop `VITE_MAP_NAME` |
| `frontend/src/main.ts` | Use `mapAssets.name` |
| `docker/docker-compose.yml`, `docker/dist/docker-compose.yml` | Remove `DEFAULT_MAP` |
| `documentation/*.md` | Updated operational docs |

## Client Reaper — orphaned player entities (ghost avatars / inflated player count)

**Branch:** `feat/interaction-system` (uncommitted)
**Status:** Implemented — `make proto`, `go build ./...`, `go test ./internal/worldsim/` all pass.

The `/audit/world` dashboard counted more players than were actually connected. Root cause: worldsim had no stale-client reaper. When the pusher crashed/restarted or a `client.disconnected` NATS message was lost, the player avatar stayed in `s.clients` forever — inflating the count and leaving a ghost avatar on other players' screens. (The admin portal `backend/cmd/admin` does not touch worldsim; entities only enter `s.clients` via `provisionClient` on `client.connected` from the pusher.)

Fix mirrors the existing extension stale-checker pattern:
1. **Pusher** publishes `client.<id>.heartbeat` to NATS on each successful WS keepalive ping (every `PingInterval`, 30s).
2. **Worldsim** subscribes to `client.*.heartbeat`, updates `Entity.lastHeartbeat`.
3. **Worldsim** `startClientReaper` goroutine (started in `Run`) calls `reapStaleClients` every 10s; any client whose `lastHeartbeat` is older than `clientHeartbeatTimeout` (90s = 3 missed pings) is despawned via the existing `despawnClient` (queues DestroyEntity, saves position to PB, emits `client.reaped` audit).

### Files

| File | Changes |
|---|---|
| `backend/internal/worldsim/worldsim.go` | `Entity.lastHeartbeat` field; set in `provisionClient`; `client.*.heartbeat` subscription; `clientHeartbeatTimeout` const; `startClientReaper`/`reapStaleClients`; launched in `Run` |
| `backend/internal/pusher/pusher.go` | Publish `client.<id>.heartbeat` on successful keepalive ping |
| `backend/internal/worldsim/worldsim_reaper_test.go` | New `TestReapStaleClients_DespawnsOrphans` |

## A/V Proximity Latency Fix (Issue #88)

**Branch:** `main` (uncommitted)
**Status:** Implemented — `go build ./...`, `go test ./internal/worldsim/` (49 tests), and `tsc --noEmit` all pass.

Three changes to fix the ~2s video open/close delay and A/V thrashing when
walking past another player:

1. **Hysteresis on proximity radius** — enter at 2.0 tiles, exit at 3.0 tiles.
   Eliminates boundary oscillation that caused the 1.5s leave debounce to be
   necessary. In `worldsim.go` zone exit detection: `prox-*` zones that are no
   longer in `ZonesAtPoint` are kept if the player is still within
   `proximityExitRadius` of the zone owner's feet.
2. **Movement-gated proximity join** — `proximity.join` is suppressed until
   the player has been stationary for `proximityStationaryThreshold` ticks
   (~500ms at 20Hz). Walking past without stopping triggers no A/V at all.
   New groups require ALL members stationary; existing groups gaining a new
   member require only the joining player stationary.
3. **Reduced leave debounce** — `AvClient.ts` leave debounce reduced from
   1500ms to 200ms (safe now that hysteresis + movement gating eliminate
   thrashing).

The join latency from WebRTC connection setup (~1-2s) is inherent and not
addressed by this fix. A client-side keep-warm alternative is documented in
`documentation/plans/2026-07-14-av-keep-warm-future-exploration.md` for future
evaluation if join latency is still insufficient.

LiveKit `MoveParticipant` (server-side room switching without reconnect) was
investigated but is LiveKit Cloud-only — not available on self-hosted LiveKit
v1.13.2 which this project uses.

### Files

| File | Changes |
|---|---|
| `backend/internal/worldsim/worldsim.go` | `stationaryTicks` field on Entity; `proximityExitRadius`/`proximityStationaryThreshold` constants; stationary tick tracking in `tick()`; hysteresis in zone exit detection; movement gating in `runProximityClustering()` |
| `frontend/src/net/AvClient.ts` | Leave debounce reduced from 1500ms to 200ms; updated comments |
| `backend/internal/worldsim/worldsim_proximity_test.go` | New `TestProximityClustering_Hysteresis` and `TestProximityClustering_MovementGating`; existing tests updated with `stationaryTicks` |
| `documentation/plans/2026-07-14-av-keep-warm-future-exploration.md` | New — documents keep-warm alternative for future evaluation |

## Admin Badge on Name Tags

**Branch:** `main` (uncommitted)
**Status:** Implemented — `make proto`, `go build ./...`, `go test ./internal/worldsim/` (nametag tests), and `tsc --noEmit` all pass.

Admins now show a red "admin" badge to the right of their name in the name-tag pillbox, visible to all clients (public, like the GUEST badge) and on the admin's own self-view tag. Admins can opt out per-player via a new `hide_admin_badge` bool field on the PocketBase `players` collection (default false = badge shown; only takes effect when `is_admin=true`).

The replicated `DisplayName.is_admin` flag is computed server-side as `Entity.IsAdmin && !Entity.HideAdminBadge`, so the client never decides admin status — it just renders the server-provided flag. Normal and guest users always have `IsAdmin=false` (no PB record or `is_admin=false`), so they never get the badge. The toggle is purely cosmetic; it never grants or revokes admin capabilities (admin features like IP info / ban button remain gated on the admin-only NATS subject, separately).


### Files

| File | Changes |
|---|---|
| `proto/components.proto` | Added `bool is_admin = 3` to `DisplayName` |
| `backend/migrations/1753500000_add_hide_admin_badge_to_players.go` | New — adds `hide_admin_badge` BoolField to `players` (default false) |
| `backend/internal/worldsim/userstore.go` | `UserRecord.HideAdminBadge`; `recordToUser` reads it |
| `backend/internal/worldsim/worldsim.go` | `Entity.HideAdminBadge`; set in `provisionClient`; `IsAdmin: e.IsAdmin && !e.HideAdminBadge` at the 2 DisplayName marshal sites |
| `frontend/src/scenes/GameScene.ts` | `isAdminByEntity` map; `createNameTag` renders red "admin" badge + layout |
| `backend/internal/worldsim/worldsim_nametag_test.go` | New `TestReplication_SpawnIncludesIsAdmin` |
| `backend/internal/pb/components.pb.go`, `frontend/src/proto/components_pb.ts` | Regenerated by `make proto` |

## Map & Player Options System

**Branch:** `main` (uncommitted)
**Status:** Implemented — `make build`, `go test`, `tsc --noEmit`, and `vite build` all pass.

A general options system with two dimensions, mirroring the `extension_options` pattern (JSON field on a PB collection, defaults applied in code):

### Map options (per-map, admin-edited, hot-reload)
- `options` JSON field on the `maps` PB collection.
- First option: `day_night_enabled` (bool, default true). Controls whether the day/night overlay is active by default on that map. The player can always override locally via their own toggle (localStorage preference takes precedence).
- Admin edits the JSON in the PB GUI → PB hook fires → `checkMapReload` detects options-only change → updates in-memory `MapData.Options` → pushes `MapOptionsUpdateFrame` to each connected client on that map. No full map reload needed.
- Also sent on auth (`AuthResultFrame.map_options`) and map transitions (`MapTransitionFrame.map_options`).

### Player options (per-player, user-edited, persisted)
- `options` JSON field on the `players` PB collection.
- First option: `show_own_name_tag` (bool, default false). Controls whether the local player's name tag is visible above their own avatar.
- Player toggles via TopMenu dropdown checkbox → `SetPlayerOptionsFrame` → pusher forwards to worldsim on `client.<id>.set_player_options` → `handleSetPlayerOptions` updates `Entity.PlayerOptions` in memory + persists to PB. Guests have no PB record — session-only.
- Sent on auth (`AuthResultFrame.player_options`).

### Bug fix included
The pusher was not forwarding `map_id` in the `AuthResultFrame` to the browser (only `EntityId` + `IsAdmin`). This is now fixed — the pusher forwards `MapId`, `MapOptions`, and `PlayerOptions` from worldsim's reply.

### Files

| File | Changes |
|---|---|
| `proto/frames.proto` | `map_options`/`player_options` on AuthResultFrame, `map_options` on MapTransitionFrame, new `MapOptionsUpdateFrame`, new `SetPlayerOptionsFrame`, new ClientFrame variant |
| `backend/migrations/1753000000_add_options_to_maps_and_players.go` | New — `options` JSON field on `maps` + `players` |
| `backend/internal/worldsim/mapdata.go` | `Options` field on `MapData` + `MapRecordInfo` |
| `backend/internal/worldsim/mapstore.go` | Read `options` in `ListAllMaps`, `FetchMapRecordInfo`, `loadMapOnce` |
| `backend/internal/worldsim/userstore.go` | `Options` on `UserRecord`, `recordToUser`, new `UpdateOptions` method |
| `backend/internal/worldsim/worldsim.go` | `PlayerOptions` on Entity, `provisionResult` fields, `provisionClient` reads options, `client.connected` reply includes options, `transitionEntity` includes map options, `set_player_options` sub+handler, `checkMapReload` options hot-reload |
| `backend/internal/pusher/pusher.go` | Forward `MapId`/`MapOptions`/`PlayerOptions` in AuthResultFrame, forward `SetPlayerOptions` to NATS |
| `frontend/src/net/WsClient.ts` | Store/get options, `setPlayerOptions()`, `mapOptionsUpdate` handler, `onMapOptionsUpdate` callback |
| `frontend/src/scenes/GameScene.ts` | `applyMapOptions`/`applyPlayerOptions` helpers, apply on ready/transition/hot-reload, `showOwnNameTag` field, name tag visibility logic |
| `frontend/src/ui/DayNightOverlay.ts` | `applyDefault()` method, `loadEnabled()` returns `null` when no preference |
| `frontend/src/ui/TopMenu.ts` | "Show my name tag" checkbox, `setPlayerOptions`/`setSetPlayerOptionsHandler` |

## pb-collections export/import tool

**Branch:** `fix/av-duplicate-identity-stuck` (uncommitted)
**Status:** Implemented + smoke-tested. `make build` produces `dist/bin/pb-collections`.

A standalone Go binary (`backend/cmd/pb-collections`) that exports and imports
all application PocketBase collections — schema, records, and file fields —
between a PB data directory and a portable JSON + files directory. Works
offline by bootstrapping PB directly on `PB_DATA_DIR` (same pattern as
`seed-sprites`). Do not run while worldsim is using the same data dir (SQLite
is single-writer).

System collections (`_superusers`, `_externalAuths`, `_migrations`) are
skipped — only app collections are exported (maps, players, sprite_bases,
extension_options, bans, plus PB's default `users` auth collection).

### Usage

```bash
# Export all app collections into <dir>:
PB_DATA_DIR=./pb_data ./dist/bin/pb-collections -export ./pb_backup

# Import into a (possibly fresh) PB_DATA_DIR:
PB_DATA_DIR=./pb_data_fresh ./dist/bin/pb-collections -import ./pb_backup

# -force: overwrite a non-empty export dir, or delete existing records before import
./dist/bin/pb-collections -export ./pb_backup -force
./dist/bin/pb-collections -import ./pb_backup -force
```

Export layout: `<dir>/collections.json` + `<dir>/files/<collection>/<recordId>/<filename>`.

### Behavior notes

- **Schema import** uses `app.ImportCollectionsByMarshaledJSON(..., false)` —
  upserts exported collection definitions without deleting unrelated collections.
- **Record IDs** are preserved on import for idempotency (re-imports skip
  records that already exist by ID). In `-force` mode, fresh IDs are minted
  instead, because PB deletes the old record storage dirs on delete and
  re-uploading to the same path races on the removed directory.
- **Records** are saved with `app.SaveNoValidate` — the export is trusted as a
  valid PB snapshot, so field validations aren't re-run and non-standard record
  IDs are preserved. File-upload interceptors still run.
- **File fields** are re-uploaded via `filesystem.NewFileFromBytes`; PB's
  `normalizeName` always appends a random suffix, so restored filenames differ
  from the export but record references stay internally consistent and content
  is byte-identical.
- `created`/`updated` autodate timestamps are reset to import time (PB's
  autodate hook fires on create). Not preserved.

### Smoke test performed

Seeded `sprite_bases` (4 records with PNG files) + a `players` + a `bans`
record into a source data dir, exported, imported into a fresh dir, and
verified: record counts match per collection, sprite PNG content is
byte-identical (md5), and all plain fields round-trip. Idempotent re-import
skips existing records; `-force` import wipes and restores cleanly.

### Files

| File | Changes |
|---|---|
| `backend/cmd/pb-collections/main.go` | New — export/import CLI binary |
| `Makefile` | Added `pb-collections` to the `build` target |

## Name Tag Info Dropdown

**Branch:** `main` (uncommitted)
**Status:** Implemented — admin pillboxes replaced by a clickable status dot that opens a dropdown panel. tsc + Vite build pass.

The fixed secondary pillboxes (IP, device_id) below name tags have been
removed. The green status dot on the left of the name is now clickable
and opens a small dropdown panel. Regular users see "Hello world";
admins see the player's IP and short device ID. Both see an "Invite"
button; admins also see a "Ban" button. The buttons are stubs — they
show "Not implemented yet" when clicked. Wiring the ban button to a
server-side ban command (proto `BanFrame`, `BanStore.AddBan`, worldsim
handler) is a planned future task.

Only one dropdown is open at a time. Clicking another dot switches,
clicking elsewhere closes it. The dropdown follows the avatar each
frame, counter-scaled like the name tag.

### Files

| File | Changes |
|---|---|
| `frontend/src/scenes/GameScene.ts` | Removed admin pillboxes + `ipText` field; made status dot interactive; added `openDropdown`/`toggleDropdown`/`closeDropdown`/`showDropdownStub`/`refreshDropdownIfOpen` methods; per-frame dropdown positioning; click-outside-to-close listener; cleanup on destroy/reconnect/shutdown |
| `documentation/features.md` | §1.5 updated — dropdown description + storyboard |

## Extension Options System (Phase 3, Part B complete)

**Branch:** `feat/extension-options`
**Status:** Part B complete — extensions declare options schema at registration, worldsim creates PB rows with defaults, admin edits options in PB GUI, changes hot-reload to extensions via NATS. Build and tests pass.

Extensions declare their options as a JSON schema (`options_schema` field) in the `extension.<id>.register` message. Each schema entry has `name`, `type` ("bool", "number", "text"), and `default`. Worldsim's `ExtensionOptionsManager` ensures a row exists in the `extension_options` PocketBase collection for each extension, creating one with default values if missing and backfilling new fields on schema changes. The current options JSON is published back to the extension via NATS on `extension.<id>.options`.

When the admin edits an extension's options in the PB admin GUI, an in-process PB hook (`OnRecordAfterUpdateSuccess("extension_options")`) fires and worldsim republishes the updated options to the extension. The extension receives the update and adjusts its behavior at runtime — no restart needed.

### What changed

- **Migration:** `1752700000_create_extension_options.go` — `extension_options` collection with `extension_id` (text, required) and `options` (JSON) fields. Full CRUD rules for admin access.
- **Worldsim:** New `extensionoptions.go` — `ExtensionOptionsManager` with `EnsureOptions` (create/backfill PB row), `PublishOptions` (NATS publish to `extension.<id>.options`), `PublishAllOptions`. Wired into `New()` after PB+NATS init. PB hooks for `OnRecordAfterUpdateSuccess` and `OnRecordAfterCreateSuccess` on `extension_options` relay changes to extensions.
- **ExtensionManager:** `registerMsg` extended with `OptionsSchema` field. `Register()` calls `EnsureOptions` + `PublishOptions` after registration. New `SetOptionsManager()` method for wiring.
- **ext-av:** Declares `proximity_audio_enabled` (bool, default true) and `zone_audio_enabled` (bool, default true). Subscribes to `extension.av.options`. Zone A/V and proximity A/V gated by respective options.
- **ext-walls:** Declares `enabled` (bool, default true). Subscribes to `extension.walls.options`. When disabled, re-registers with no gate triggers (walls stop blocking).
- **ext-demo:** Declares `log_zone_events` (bool, default true). Subscribes to `extension.demo.options`. Zone enter/exit logging gated by option.
- **ext-props:** Declares `interaction_radius` (number, default 1.5). Subscribes to `extension.props.options`. Logs updated radius on change.

### Files

| File | Changes |
|---|---|
| `backend/migrations/1752700000_create_extension_options.go` | New — extension_options collection |
| `backend/internal/worldsim/extensionoptions.go` | New — ExtensionOptionsManager (PB + NATS) |
| `backend/internal/worldsim/extensionoptions_test.go` | New — tests for defaults, registration, nil app |
| `backend/internal/worldsim/extensions.go` | Options schema in registerMsg, SetOptionsManager, Register calls EnsureOptions+PublishOptions |
| `backend/internal/worldsim/worldsim.go` | Wire ExtensionOptionsManager, PB hooks for option changes |
| `backend/cmd/ext-av/main.go` | Options schema, subscription, zone/proximity gating |
| `backend/cmd/ext-walls/main.go` | Options schema, subscription, enabled toggle |
| `backend/cmd/ext-demo/main.go` | Options schema, subscription, log gating |
| `backend/cmd/ext-props/main.go` | Options schema, subscription |

### How it works

```
Extension startup:
  1. Extension publishes extension.<id>.register with {extension_id, heartbeat_interval_s, options_schema: [{name, type, default}]}
  2. Worldsim ExtensionManager.Register() parses the schema
  3. ExtensionOptionsManager.EnsureOptions() creates/updates PB row with defaults
  4. ExtensionOptionsManager.PublishOptions() sends current options via NATS on extension.<id>.options
  5. Extension receives options, applies them

Admin edits options in PB GUI:
  1. Admin updates the options JSON in the extension_options collection
  2. PB hook (OnRecordAfterUpdateSuccess) fires in-process
  3. Worldsim publishes updated options on extension.<id>.options
  4. Extension receives update, adjusts behavior at runtime
```

## Extension NATS Zone Metadata (Phase 3, Part A complete)

**Branch:** `feat/extension-nats`
**Status:** Part A complete — extensions receive zone metadata from worldsim via NATS instead of hitting PocketBase's HTTP API directly. Build and tests pass.

Extensions (ext-walls, ext-av) no longer read the Tiled map from PocketBase to find wall zones and A/V zones. Instead, worldsim broadcasts zone metadata (zone IDs + properties) via two NATS subjects:
- `worldsim.zones.get` — request-reply: extensions fetch zone metadata on startup/reconnect.
- `worldsim.zones` — broadcast: worldsim publishes updated zone metadata after a map reload so extensions can refresh without a request.

The `POCKETBASE_URL` env var and `MAP_ID` env var are removed from ext-walls and ext-av Docker configs. The `findWallZones` and `findAVZones` functions (which fetched and parsed Tiled JSON from PB's HTTP API) are deleted. The "wait for PocketBase" startup loops are removed — extensions now wait for `worldsim.ready` and then request zone metadata via NATS.

### What changed

- **Worldsim:** New `zonemeta.go` — `buildZoneMetadata()` serializes all zones from all maps into JSON (`zoneMetadataMsg` with per-map zone arrays: id, zone_type, av_enabled, is_exclusive, mobility, portal fields). `subscribeZoneMetadata()` sets up the `worldsim.zones.get` request-reply handler. `broadcastZoneMetadata()` publishes on `worldsim.zones` (called after map reload in `checkMapReload`).
- **ext-walls:** Rewritten — subscribes to `worldsim.zones` for live updates, requests `worldsim.zones.get` on startup/`worldsim.ready`. Filters for `zone_type == "wall"`. Removed `findWallZones()`, `tiledMapJSON` struct, `POCKETBASE_URL`/`MAP_ID` env vars, PB wait loop, `map.updated` subscription, `net/http`/`io`/`strings` imports.
- **ext-av:** Rewritten — same NATS zone metadata pattern. Filters for `av_enabled == true`. Removed `findAVZones()`, `tiledMapJSON` struct, `POCKETBASE_URL`/`MAP_ID` env vars, PB wait loop, `map.updated` subscription, `net/http`/`io`/`strings` imports.
- **Docker:** `POCKETBASE_URL` and `MAP_ID` removed from ext-walls and ext-av in both `docker-compose.yml` and `dist/docker-compose.yml`.
- **Tests:** `zonemeta_test.go` — tests for request-reply and broadcast.

### Files

| File | Changes |
|---|---|
| `backend/internal/worldsim/zonemeta.go` | New — zone metadata serialization, request-reply handler, broadcast |
| `backend/internal/worldsim/zonemeta_test.go` | New — tests for request-reply and broadcast |
| `backend/internal/worldsim/worldsim.go` | `subscribeZoneMetadata()` call in `subscribe()`, `broadcastZoneMetadata()` in `checkMapReload()` |
| `backend/cmd/ext-walls/main.go` | Rewritten — NATS zone metadata instead of PB HTTP |
| `backend/cmd/ext-av/main.go` | Rewritten — NATS zone metadata instead of PB HTTP |
| `docker/docker-compose.yml` | Removed `POCKETBASE_URL`/`MAP_ID` from ext-walls and ext-av |
| `docker/dist/docker-compose.yml` | Same |

### Next steps (Part B — not in this branch)

- ✅ Complete — see "Extension Options System (Phase 3, Part B complete)" above.

## Multi-Map Support (Phase 2 complete)

**Branch:** `feat/multi-map`
**Status:** Phase 2 complete — worldsim manages multiple maps, portal zones trigger map transitions, frontend handles dynamic map loading. Build and tests pass.

The `Simulator` loads all maps from PocketBase on startup and manages per-map `MapData`/`ZoneRegistry` instances. The default map is configured via the `DEFAULT_MAP` env var (default `main`). Entities carry a `Position.MapId` field; movement, collision, zone detection, and replication are all per-map. Portal zones (Tiled `zone_type=portal` with `target_map`/`target_entity` properties) trigger automatic map transitions. Extensions can teleport entities via the `worldsim.entity.teleport` NATS subject.

### What changed

- **Migrations:** 1 new Go migration — `map_id` on `players`. The `worlds` collection and `world_id` on maps were removed (one world, multiple maps — no grouping needed).
- **Proto:** `MapTransitionFrame` message added; `map_id` field added to `AuthResultFrame`.
- **Simulator struct:** `mapID`/`mapData`/`zoneReg`/`mapFilename` replaced with `defaultMap`/`maps map[string]*MapData`/`zones map[string]*ZoneRegistry`/`mapFilenames map[string]string`.
- **MapStore:** `ListAllMaps` added. `SeedMapIfMissing` simplified (no `worldID` param). `WorldConfig`/`LoadWorld`/`ListMapsForWorld`/`SetWorldDefaultMap` removed.
- **UserStore:** `SaveMapID` added. `UserRecord.MapID` field added.
- **Movement/collision:** `isMoveBlocked` takes `zr`/`md` params. `runMovementSystem` looks up per-map data via `e.Position.MapId`.
- **Zone detection:** Per-map `ZoneRegistry` lookup. Portal zones trigger `transitionEntity`.
- **Replication:** Entities filtered by map — clients only see entities on their map.
- **Map reload:** Per-map reload checker; PB hook checks all loaded maps.
- **Portal zones:** `Zone` struct extended with `PortalTargetMap`/`PortalTargetEntity`. Parsed from Tiled `target_map`/`target_entity` properties. No-position transitions use `FindSpawnPoint`; beacon transitions use `FindEntityByName`.
- **Extension teleport:** `worldsim.entity.teleport` NATS subject — extensions can teleport players across maps with `target_entity` or random spawn.
- **main.go:** `DEFAULT_MAP` env var (default `main`).
- **Docker:** `DEFAULT_MAP: "main"` for worldsim; `MAP_ID: "main"` for extensions.
- **Frontend:** `onMapTransition` handler in WsClient; `handleMapTransition` in GameScene loads new map assets and restarts scene. `mapLoader.ts` accepts optional `mapName` param. `AuthResultFrame.map_id` checked on ready to detect saved player map.
- **Map files:** `map1.json`/`.tmx`/etc renamed to `main.*`. Seed file is `default-map.json` (uploaded to PB as record named `main`).

### Files

| File | Changes |
|---|---|
| `backend/migrations/1752400000_add_map_id_to_players.go` | New — map_id on players |
| `proto/frames.proto` | `MapTransitionFrame` message, `map_id` on `AuthResultFrame` |
| `backend/internal/worldsim/mapstore.go` | `ListAllMaps`, simplified `SeedMapIfMissing`, removed world methods |
| `backend/internal/worldsim/userstore.go` | `SaveMapID`, `UserRecord.MapID` |
| `backend/internal/worldsim/zones.go` | `Zone` portal fields (`PortalTargetMap`/`PortalTargetEntity`) |
| `backend/internal/worldsim/mapdata.go` | Portal property parsing, `FindEntityByName`, `MapRecordInfo` |
| `backend/internal/worldsim/worldsim.go` | Multi-map struct, per-map systems, portal transitions, extension teleport |
| `backend/internal/worldsim/*_test.go` | Updated for new struct (5 files) |
| `backend/cmd/worldsim/main.go` | `DEFAULT_MAP` env var |
| `docker/docker-compose.yml` | `DEFAULT_MAP` for worldsim, `MAP_ID: "main"` for extensions |
| `docker/dist/docker-compose.yml` | Same |
| `frontend/src/net/WsClient.ts` | `onMapTransition` handler, `mapId` field, `getMapId()` |
| `frontend/src/scenes/GameScene.ts` | `handleMapTransition`, map_id check on ready |
| `frontend/src/mapLoader.ts` | Accepts optional `mapName` param |
| `frontend/src/main.ts` | Sets `loadedMapName` in registry |

### Next phases

- **Phase 3 Part A (`feat/extension-nats`):** ✅ Complete — extensions receive zone metadata via NATS instead of hitting PB.
- **Phase 3 Part B:** Extension options schema declared in registration, worldsim creates PB collections. Hot-reload via PB hooks + NATS.

## PocketBase Embedding (Phase 1 complete)

**Branch:** `feat/pb-embedding`
**Status:** Phase 1 complete — PB embedded in worldsim, standalone container removed, full stack verified.

PocketBase now runs as a Go library inside worldsim instead of as a separate container. The worldsim process calls `app.Bootstrap()` + `app.RunAllMigrations()` to initialize the DB and run Go migrations, then `app.Start()` in a goroutine to serve the admin GUI and file API on port 8090.

### What changed

- **Migrations:** JS migrations in `pb_migrations/` replaced by Go migrations in `backend/migrations/` (compiled into the binary). `Bootstrap()` only runs system migrations, so `app.RunAllMigrations()` is called explicitly after bootstrap.
- **Stores:** `MapStore`, `UserStore`, `SpriteStore` rewritten from HTTP API calls to PB Go SDK DAO calls (`app.FindFirstRecordByData`, `app.Save`, `app.NewFilesystem`, etc.).
- **Docker:** `pocketbase` service removed from both `docker-compose.yml` and `dist/docker-compose.yml`. The `worldsim` container now mounts `pb_data` and exposes port 8090. Nginx proxies `/api/` to `worldsim:8090`. Extensions (ext-walls, ext-av) previously pointed `POCKETBASE_URL` at `http://worldsim:8090` — removed in Phase 3 Part A.
- **Map reload:** PB `OnRecordAfterUpdateSuccess("maps")` hook triggers instant map reload instead of the 30-second polling checker.
- **Makefile:** `debug-pocketbase` target removed; `debug` target now passes `PB_DATA_DIR`/`PB_HTTP_ADDR` env vars to worldsim.

## Day/Night Overlay

**Branch:** `feat/day-night-overlay`
**Status:** Implemented. Overlay active by default. Toggle UI not yet wired into TopMenu — controllable via `DayNightOverlay.setEnabled()` and localStorage key `daynight.enabled` for now.

A purely cosmetic, 100% client-side full-screen rectangle tints the game world based on the browser's local clock. Color and alpha are interpolated between 8 time-of-day keyframes and recalculated once per minute. Alpha is capped at 0.44 so the map stays readable.

**Keyframes:**

| Hour | Phase | Color | Alpha |
|------|-------|-------|-------|
| 00:00 | Deep night | `#0a0a2e` | 0.38 |
| 03:00 | Night | `#0a0a2e` | 0.38 |
| 06:00 | Dawn | `#ff8c42` | 0.20 |
| 09:00 | Morning | `#fff4e6` | 0.05 |
| 12:00 | Noon | `#ffffff` | 0.00 |
| 15:00 | Afternoon | `#fff4e6` | 0.05 |
| 18:00 | Dusk | `#ff6b35` | 0.25 |
| 21:00 | Evening | `#1a1a4e` | 0.35 |

**Files:**

| File | Changes |
|------|---------|
| `frontend/src/ui/DayNightOverlay.ts` | New — overlay class with keyframes, linear interpolation, per-minute timer, alpha cap, localStorage persistence |
| `frontend/src/scenes/GameScene.ts` | Instantiate overlay after disconnect overlay, resize handler |

**TODO:** Add a toggle checkbox to the TopMenu settings dropdown (the `setEnabled()` API is ready for it).

**TODO:** Add a keyframe editor to the TopMenu settings dropdown (the `setKeyframes()` / `getKeyframes()` API and `DEFAULT_KEYFRAMES` export are ready for it). Custom keyframes persist in localStorage key `daynight.keyframes`.

## Remote Audio: FIXED

**Branch:** `fix/av-audio-autoplay`
**PR:** https://github.com/lstep/PixelEruv/pull/16
**Status:** Audio works across mixed browsers (Safari + Chrome, different machines). Two fixes were needed.

## Root Causes & Fixes

### Fix 1: RED codec incompatibility (Safari)
The LiveKit SDK enables `audio/red` (Redundant Audio Data) by default for mono audio tracks. Safari cannot decode `audio/red` — only `audio/opus`. Chrome-published audio was silent on Safari.

**Fix:** `publishDefaults: { red: false }` in the Room constructor (`AvClient.connect()`). Forces `audio/opus` for all published audio tracks.

### Fix 2: Remote audio tracks never attached (the real blocker)
LiveKit does NOT auto-attach remote audio tracks to `<audio>` elements. `addSubscribedMediaTrack` creates a `RemoteAudioTrack` but never calls `attach()`. Without an attached element:
- `setVolume()` was a no-op (iterates `attachedElements` which was empty)
- `startAudio()` was a no-op (plays `attachedElements` which was empty)
- `isSpeaking` worked (analyzed from RTP packets) but no sound played

For video, `VideoTile.attachTrack()` manually calls `track.attach(videoElement)`. Nobody did the equivalent for audio.

**Fix:** In the `TrackSubscribed` handler, when an audio track arrives, call `audioTrack.attach()` to create a hidden `<audio>` element with `autoplay=true`, then call `room.startAudio()` to start playback.

## What Works (confirmed)

- LiveKit signaling connects
- WebRTC ICE succeeds (UDP, public IP via `use_external_ip`)
- Both participants publish audio + video tracks
- Video renders correctly on both sides
- Audio plays with spatial distance-based volume
- Green speaking border appears on both local and remote tiles
- Cross-browser: Safari + Chrome, different machines

## What's Been Done (commits on the branch)

1. **Fix remote audio playback blocked by browser autoplay policy**
   - `AvAudioBlockedHandler` + `onAudioBlocked` property
   - `startAudio()` method, "Enable Audio" button in TopMenu
   - Same-room skip + leave debounce (1.5s) in `handleTokenFrame`

2. **Enable LiveKit use_external_ip for remote WebRTC access**
   - `livekit.yaml`: `use_external_ip: true`

3. **Unlock audio on first page click via silent audio element**
   - Constructor installs one-time `document.click` listener
   - Plays a silent 1-byte WAV to unlock Safari autoplay

4. **Add mic/camera device selection in the menu dropdown**
   - `AvClient.getDevices(kind)`, `AvClient.switchDevice(kind, deviceId)`
   - TopMenu dropdown has mic + camera `<select>` dropdowns

5. **Persist selected device IDs across room reconnects**

6. **Disable RED codec** (`publishDefaults: { red: false }`)

7. **Attach remote audio tracks on TrackSubscribed** (the actual fix for no sound)

## Files Changed

| File | Changes |
|------|---------|
| `frontend/src/net/AvClient.ts` | Audio unlock, device selection, RED disable, audio track attach on subscribe, same-room skip, leave debounce, startAudio, noise cancellation option |
| `frontend/src/ui/TopMenu.ts` | Enable Audio button, mic/camera device selectors in dropdown |
| `docker/livekit.yaml` | `use_external_ip: true` |
| `docker/docker-compose.yml` | Comments about UDP ports + node IP |
| `docker/dist/docker-compose.yml` | Same comments |

## Noise Cancellation Option

**Branch:** `fix/remote-audio-attach-and-red-codec`
**Status:** Implemented, activated by default. Not yet wired to the TopMenu — toggle via `AvClient.setNoiseCancellation()` for now.

WebRTC client-side noise cancellation (`noiseSuppression` + `echoCancellation` + `autoGainControl`) is now an explicit, persisted option in `AvClient` (localStorage key `av.noiseCancellation`, defaults on). Previously these flags were only set implicitly via the LiveKit SDK's built-in `audioDefaults`.

- `isNoiseCancellationEnabled()` / `setNoiseCancellation(enabled)` — getter/setter, persisted across reconnects.
- `buildAudioCaptureOptions()` — merges the selected mic device ID with the noise-cancellation flags into `AudioCaptureOptions`, applied via `audioCaptureDefaults` at room connect time.
- Mid-call toggle: `setNoiseCancellation` restarts the mic track (`LocalAudioTrack.restartTrack`) so the change takes effect without reconnecting, when the mic is published and unmuted.
- When disabled, the three flags are explicitly set to `false` to override the SDK's `true` defaults.

## AV: Fix video sometimes not appearing (DUPLICATE_IDENTITY + stuck state)

**Branch:** `feat/name-tag-info-dropdown`
**Status:** Implemented, build passes.

Two bugs in `AvClient` combined to cause video to sometimes not appear and
stay broken until page reload:

1. **Concurrent `handleTokenFrame("join")` calls bypassed the guard.**
   `handleTokenFrame` was async and not serialized. The "already connected?"
   guard checked `this.currentRoom`, but it was set inside `connect()` after
   the first `await`. When a player oscillated on a proximity edge after a
   disconnect, multiple "join" frames arrived before the first `connect()` set
   `this.currentRoom`, so all passed the guard. Each created a `Room` object
   connecting to the same LiveKit room with the same identity → server kicked
   one with `DUPLICATE_IDENTITY` (reason 2).

2. **No `RoomEvent.Disconnected` listener → permanent stuck state.**
   When the room died (server kick, network drop), `this.room` and
   `this.currentRoom` stayed set. All future "join" frames for the same room
   were skipped by the guard → player stuck with no A/V until page reload.

**Fix:**
- `handleTokenFrame` now serializes calls via a `frameQueue` promise chain.
  Each frame waits for the previous one to finish before processing.
- Added `RoomEvent.Disconnected` listener that clears state on unexpected
  disconnect. A `disconnecting` flag suppresses it during client-initiated
  `disconnect()` (which already cleans up).

Note: only WebRTC client-side cancellation applies (self-hosted LiveKit). The enhanced Krisp/ai-coustics models in the LiveKit docs require LiveKit Cloud and target voice AI agents, not browser conferencing clients.

### TODO: split into individual toggles in the options menu

Currently all three flags are controlled by a single `noiseCancellation` boolean. In the future options menu, each should be independently changeable:

- **noiseSuppression** — removes background noise (fans, traffic, etc.)
- **echoCancellation** — removes echo from speakers feeding back into the mic
- **autoGainControl** — normalizes voice volume automatically

This means splitting `noiseCancellation` into three separate persisted booleans (with their own localStorage keys), three getters/setters, and three checkboxes in the TopMenu dropdown.

### TODO: Safari echo cancellation not working (unresolved)

**Symptom:** Safari user's mic captures speaker audio and echo cancellation fails to remove it. The Chrome remote hears echo from the Safari user. Chrome→Chrome works fine. Safari user hears no echo themselves (their own AEC for remote audio works).

**Status:** Two attempted fixes did NOT resolve the issue:
1. `voiceIsolation: false` in `buildAudioCaptureOptions()` — overrides SDK default `true`. No improvement.
2. `navigator.audioSession.type = 'play-and-record'` in constructor — sets the W3C Audio Session API (Safari-only, experimental). No improvement.

**What was tried and ruled out:**
- Explicit `echoCancellation: true` in `audioCaptureDefaults` — was already implicit via SDK defaults, making it explicit changed nothing.
- `voiceIsolation: false` — the SDK default `true` is experimental and was suspected of interfering with Safari's CoreAudio VPIO path. Disabling it had no effect.
- `navigator.audioSession.type = 'play-and-record'` — the Audio Session API (W3C draft, Safari-supported) is supposed to tell macOS/iOS to use the VPIO unit for hardware AEC. Setting it before any `getUserMedia` call had no effect on the echo.

**Key research findings (to avoid redoing):**
- This is a known, long-standing Safari/WebKit limitation. See:
  - WebKit bug 213723: "Echo cancellation doesn't work in WebRTC calls when using external microphone" — still OPEN as of 2022. Safari's AEC is weaker than Chrome's, especially with external mics + built-in speakers.
  - WebKit bug 235544: "macOS Safari 15.2 Audio Echo Issue after camera pause/unpause" — FIXED in Safari 15.5. Was a different bug (audio loopback outside WebRTC), not our issue.
  - WebKit bug 179411: "getUserMedia echoCancellation constraint has no effect" — RESOLVED FIXED, but Safari's AEC remains less effective than Chrome's even when the constraint is honored.
  - Twilio issue #1433: same echo problem in Safari, commenters note `noiseSuppression` and `echoCancellation` are not fully supported in Safari.
  - LiveKit client-sdk-js PR #1159: `webAudioMix` was disabled by default due to "various issues around echo cancellation and sound duplication." Our code doesn't use `webAudioMix` (we use `track.attach()` directly), so this is not our issue.
  - LiveKit client-sdk-js issue #1541: `echoCancellation` capture option regression in 2.9.2+ (can't disable it). We're on 2.20.0 and want it ON, so this is not our issue.

- **How AEC works:** The echo canceller needs a reference signal (the far-end audio being played out the speakers) to subtract it from the mic input. Safari's AEC may fail to get the correct reference signal, or its VPIO unit may not be properly initialized. Chrome uses its own software AEC (AEC3) that doesn't depend on the platform audio session.

- **Gather.town:** Has a "Reduce echo" toggle in audio settings (user-facing), suggesting they also expose this as a user-controllable option rather than fully fixing it programmatically.

**Things to try next:**
1. **Verify the audioSession API is actually being set:** Check `navigator.audioSession.type` in Safari DevTools console after page load. It may silently fail or be overridden.
2. **Set audioSession type right before `room.connect()`** instead of in the constructor — timing may matter (before the first `getUserMedia` call, which happens at connect time, not constructor time).
3. **Try `webAudioMix: true`** in Room options — pipes all audio through Web Audio API. LiveKit disabled it by default due to echo issues, but it changes the audio output path which may help Safari's AEC get the reference signal. Test carefully (may cause other issues).
4. **Check if `setParticipantVolume` interferes:** Our code calls `audioTrack.setVolume(volume)` every tick for spatial audio. On Safari, setting `el.volume` on the `<audio>` element may interfere with AEC (the reference signal level changes constantly). Try disabling spatial volume as a test.
5. **Test with headphones** to confirm the issue is acoustic echo (speaker→mic loop) and not a WebRTC loopback bug.
6. **Check Safari version** — Safari 15.5+ fixed several AEC bugs. If the user is on an older version, that may be the issue.
7. **File a WebKit bug** if none of the above helps — include a minimal repro (LiveKit room, Safari + Chrome, no headphones).
8. **Consider server-side AEC** — if LiveKit Cloud is ever adopted, Krisp NC runs server-side and doesn't depend on Safari's client-side AEC.

## Health Endpoint & Version Badge

**Branch:** `feat/mobile-joystick`
**Status:** Implemented.

A distributed `/healthz` system where every backend service (pusher, worldsim, ext-demo, ext-walls, ext-props, ext-av) publishes a health JSON to the `healthz` NATS subject every 10 seconds. The pusher subscribes, aggregates the responses into an in-memory map, and serves them via an HTTP `/healthz` endpoint. The frontend polls this endpoint every 10 seconds and displays the kernel's version (git tag or commit hash) in a tiny bottom-left badge.

### Health JSON format

Each service publishes:
```json
{"service":"kernel","status":"OK","version":"v1.2.3","uptime":"4h32m","extras":{...}}
```

| Service | Extras |
|---|---|
| `pusher` | `nats_connected`, `active_sessions` |
| `kernel` | `entity_count`, `connected_players`, `running_extensions` |
| `ext-*` | `{}` (empty for now) |

Services not heard from in 30s are marked `"stale"` in the HTTP response.

### Version injection

Version is baked into Go binaries at compile time via ldflags:
- `git describe --tags --exact-match` (tag if HEAD is on a tag)
- `git rev-parse --short HEAD` (short commit hash)
- `"dev"` fallback (no git available)

Shared via `backend/internal/version/version.go`. The Makefile and Dockerfile both inject it.

### Files

| File | Changes |
|---|---|
| `backend/internal/version/version.go` | New — shared `Version` variable, set via ldflags |
| `backend/internal/worldsim/worldsim.go` | `startTime`, `startHealthPublisher` goroutine, `publishHealth` with kernel extras |
| `backend/internal/worldsim/extensions.go` | `ActiveCount()` method for non-stale extension count |
| `backend/internal/pusher/pusher.go` | `startTime`, `healthMap`, `healthz` NATS subscriber, `handleHealthz` HTTP handler, `startHealthPublisher`, `publishHealth` |
| `backend/cmd/ext-{demo,walls,props,av}/main.go` | `startTime`, `publishHealth` in existing 10s ticker |
| `Makefile` | `VERSION` + `LDFLAGS` variables, ldflags on all `go build` |
| `docker/backend.Dockerfile` | `ARG VERSION=dev`, ldflags on all `go build` |
| `docker/nginx.conf` | `/healthz` proxy in both HTTP and HTTPS server blocks |
| `frontend/vite.config.ts` | `/healthz` dev proxy to `localhost:8081` |
| `frontend/index.html` | `#version-badge` div (fixed bottom-left, 10px monospace, semi-transparent, pointer-events:none) |
| `frontend/src/main.ts` | `pollVersion()` — fetch `/healthz` every 10s, display kernel version |

### Documentation updated

- `documentation/09-pusher.md` — §9: Health endpoint (`/healthz`) section, `healthz` NATS subject in communication contract, health aggregator in internal modules
- `documentation/10-world-simulator.md` — `healthz` in outbound NATS subjects table
- `documentation/18-extensions.md` — `healthz` in extension NATS subject contract

## Audit & Observability System

**Branch:** `feat/audit-world-status-and-auth`
**Status:** Implemented — `make build` and all tests pass.

A two-pillar system for auditing system health and browsing event history.

### Pillar 1 — OpenTelemetry traces (motel / OpenObserve)

OpenObserve was removed from the Docker stack because its x86 build requires
AES-NI CPU instructions (not available on older Xeons). For dev tracing,
use `make debug` with motel. To add OpenObserve on a compatible CPU, see
[Quick Start §10b](documentation/quick-start.md#10b-opentelemetry-traces-motel--openobserve).
`OTEL_ENABLED` defaults to `false`.

All 4 extensions call `otel.Init()` on startup.

### Pillar 2 — Audit log service

Standalone Go service (`backend/cmd/audit`) that subscribes to
`audit.event` on NATS, persists events to its own SQLite database
(independent of worldsim/PocketBase), and serves a Go templates + HTMX web
UI at `/audit/` (basic auth via `AUDIT_AUTH_USER`/`AUDIT_AUTH_PASS`).
Templates and static files are embedded via `go:embed`. Serves under
`AUDIT_BASE_PATH=/audit` when proxied by nginx.

**Event types (~25):** client.connected/disconnected, auth.failed/banned,
ws.keepalive_timeout, player.provisioned/despawned/banned/set_name/
set_sprite_base/set_player_options/teleport/map_transition, chat.message,
map.reloaded/integrity_check, extension.registered/stale, zone.enter/exit,
props.action_triggered, av.token_minted/revoked.

**UI pages:** dashboard (health + severity counts + event type counts +
recent), events (filterable table with HTMX partial reload, filter by
type/severity/actor/entity), event detail (with trace deep-link),
player timeline, **world status** (per-map overview, zone occupancy,
connected players linked to their events, extension alive/dead status),
health.

**Storage:** SQLite now, `EventStore` interface designed to upgrade to
ClickHouse (preferred) or TimescaleDB. 30-day retention (configurable via
`AUDIT_RETENTION_HOURS`).

### Files

| File | Changes |
|---|---|
| `backend/internal/audit/audit.go` | New — Event/Actor/Details types + Emit helper |
| `backend/internal/audit/audit_test.go` | New — unit tests |
| `backend/cmd/audit/{main,server,store,embed}.go` | New — audit service (basic auth, base path, world page handler) |
| `backend/cmd/audit/templates/*.html` | New — Go templates (dashboard, events, detail, player timeline, world, health) |
| `backend/cmd/audit/static/{style.css,htmx.min.js}` | New — static assets |
| `backend/internal/pusher/pusher.go` | 5 audit.Emit calls |
| `backend/internal/worldsim/worldsim.go` | ~15 audit.Emit calls + subscribeStats() |
| `backend/internal/worldsim/stats.go` | New — worldsim.stats.get NATS request-reply handler |
| `backend/internal/worldsim/extensions.go` | 2 audit.Emit calls |
| `backend/cmd/ext-{demo,walls,props,av}/main.go` | otel.Init() + audit.Emit (props, av) |
| `docker/docker-compose.yml` | audit service, OTel endpoints, AUDIT_AUTH_USER/PASS, AUDIT_BASE_PATH |
| `docker/dist/docker-compose.yml` | Same for dist layout |
| `docker/backend.Dockerfile` | audit build target + image |
| `docker/dist/backend.Dockerfile` | audit in BINARY arg comment |
| `docker/nginx.conf` | /audit/ proxy route (no rewrite, base path aware) |
| `Makefile` | audit in build target |
| `documentation/features.md` | §0.8, §5.3 updated, §5.7 added, Arc D updated |
| `documentation/quick-start.md` | env vars table, admin backends, stack list, §10 added |
| `README.md` | features list, debugging section, project layout, audit section |
| `documentation/plans/2026-07-12-audit-observability-design.md` | New — full design doc |

## Light system (LightEmitter component + procedural masked glow)

**Branch:** `feat/light-emitter-component`
**Status:** Phase 1 implemented (rendering) — `make proto`, `go build ./...`, `go test ./internal/worldsim/`, and `npm run build` all pass. Phase 2 (trigger extension) pending.

Replaces the old additive `lightGlow` PNG (a flat decal that didn't make the scene brighter and ignored occlusion) with a procedural, per-frame-masked `CanvasTexture` glow cut out by `collisionGrid` wall cells and raw `wallZone` shapes (rect/circle/polygon, sub-tile precise). Sprite brightening via Phaser 4 `enableFilters()` + `filters.internal.addGlow()`, gated by a per-sprite segment occlusion test (DDA grid + raw zone shapes, no Minkowski expansion).

New `LightEmitter` proto component (ID 5: intensity 0-100, color 0xRRGGBB, radius in tiles). Worldsim replicates it on spawn (when intensity > 0) and on delta (dirty flag). Frontend maintains an `activeLights: Set<entityId>` iterated each frame — no full-entity scan.

Map option `lights_enabled` (default `true`) in the PB `maps.options` JSON disables all light rendering and sprite brightening when `false`. No PB schema migration needed — `options` field already exists.

No backward compat: the old `state === "on"` glow path is removed. Existing maps must add `light_intensity` (Phase 2) to show glow.

### Files (Phase 1)

| File | Changes |
|---|---|
| `proto/components.proto` | New `LightEmitter` message (component ID 5) |
| `backend/internal/worldsim/worldsim.go` | `compLightEmitter=5`; entity `LightIntensity`/`LightColor`/`LightRadius`/`dirtyLightEmitter` fields; replication on spawn + delta; dirty flag reset |
| `frontend/src/scenes/GameScene.ts` | `LightEmitterSchema` import; `Avatar` light fields + `preFXGlowActive`; `lightsEnabled` + `activeLights` fields; `applyMapOptions` parses `lights_enabled`; spawn/update handlers for component ID 5; removed `state === "on"` glow path; removed `lightGlow` PNG loader; `updateLights`/`showLightGlow`/`hideLightGlow`/`redrawLightGlowMask`/`applyLightSpriteBrightening`/`isLightOccluded`/`effectiveLightRadius`/`effectiveLightColor` methods; per-frame `updateLights()` call in `update()` |
| `frontend/public/assets/sprites/light-glow.png` | Deleted |

## Roadmap (future features)

See `ROADMAP.md`.

## Players panel + teleport-to-player

**Status:** Implemented (branch `feat/players-panel`) — `make proto`, `go build ./...`, `go test ./internal/worldsim/`, `npm run build`, `tsc --noEmit` all pass.

A "👥 Players" button in the floating TopMenu opens a centered DOM modal listing all connected players on the **current map** (replication is same-map only, so no cross-map roster is needed). Each row shows: presence status dot (green/yellow/red, gray when AFK), name + `GUEST`/`admin` badges, AFK label with live duration ("AFK 3m"), a 🔔 Ping button (disabled with tooltip on DND targets — server drops those pings), and a 📍 Teleport button (admin-gated, or registered non-guest when the world option is on). The self row is pinned to the top with no action buttons. The list re-renders every 1s while open so AFK durations tick up and join/leave reflects without reopening.

### What was built

- **`proto/components.proto`** — `uint64 afk_since = 6` on `DisplayName` (unix ms, 0 = not AFK). Server-stamped so all clients see the same duration regardless of join time.
- **`proto/frames.proto`** — `TeleportToFrame { string entity_id = 1; }` + `teleport_to = 14` on `ClientFrame`.
- **`backend/internal/worldsim/worldsim.go`** — `Entity.AfkSince time.Time`; `handleSetAfk` stamps `time.Now()` on true→false transitions (zero on clear); new route `GET /api/world-options/player-teleport` (auth-required via users JWT, **non-admin OK** — registered non-admins need to learn the flag; guests get 401 → frontend hides the button).
- **`backend/internal/worldsim/replication.go`** — `afkSinceMs()` helper; `AfkSince` added at the 3 `DisplayName` marshal sites.
- **`backend/internal/worldsim/teleport_to_entity.go`** (NEW) — `worldsim.entity.teleport_to_entity` handler. Same-map only. Enforces server-side: admin always; registered non-guest only when `allow_player_teleport` is on; guests never. Applies the position mutation **after** the auth check (initial bug moved the sender before checking). Audits `player.teleport_to_entity` with result `delivered`/`forbidden`/`cross_map`/`not_connected`.
- **`backend/internal/worldsim/worldoptions.go`** — `AllowPlayerTeleport bool` field (default false).
- **`backend/internal/worldsim/worldoptions_sub.go`** — audit detail includes `allow_player_teleport`; new `handlePlayerTeleportOptionHTTP`.
- **`backend/internal/pusher/pusher.go`** — `case *pb.ClientFrame_TeleportTo` forwards to `worldsim.entity.teleport_to_entity` (fire-and-forget, mirrors kick/ping).
- **`backend/cmd/admin/`** — `allow_player_teleport` checkbox in World Options (server.go form fields + template).
- **`frontend/src/ui/TopMenu.ts`** — Players button + `attachPlayersPanel` + modal (DOM, dark theme). `ConnectedPlayer`/`PlayersPanelOpts` interfaces exported. `formatAfkDuration` helper. `allowPlayerTeleportCached` fetched on modal open via `fetchAllowPlayerTeleport`.
- **`frontend/src/scenes/GameScene.ts`** — `afkSinceByEntity` map (parsed at spawn + update, cleared/deleted alongside `afkByEntity`); `getConnectedPlayers()` returns current-map players incl. self, sorted self-first then alphabetical; `attachPlayersPanel` wired in `create()`.
- **`frontend/src/net/WsClient.ts`** — `sendTeleportTo(entityId)`.
- **`frontend/src/net/WorldOptions.ts`** — `fetchAllowPlayerTeleport()` hitting the new non-admin endpoint, cached separately from the admin-gated `fetchWorldOptions`.
- **Tests** — `worldsim_afk_test.go` asserts `AfkSince` set on true / reset on false; `teleport_to_entity_test.go` covers admin ok, registered allowed when on, registered rejected when off, guest rejected, cross-map rejected.

### Notes

- **Server-side enforcement is authoritative** — the frontend button visibility is cosmetic. A non-admin with the button hidden can't craft a TeleportToFrame that works; worldsim rejects it.
- **AFK duration uses client `Date.now() - afkSinceMs`** (server-stamped). Minor clock skew acceptable for a UI hint.
- **No new persisted fields** — `afk_since` is transient like `afk`; `allow_player_teleport` lives in the existing `world_options` KV bucket.
- **Modal is DOM** (not Phaser world-space) → unaffected by camera zoom.
