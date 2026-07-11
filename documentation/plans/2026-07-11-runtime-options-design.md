# Runtime Options System — Design Discussion

**Date:** 2026-07-11
**Status:** Decisions recorded. Multi-map support and PB embedding are being designed as the prerequisites. This document records decisions and open questions for resumption.

## Context

The system needs three categories of runtime options:

1. **World-level options** — policy decisions affecting the whole world: `allow_anonymous`, `day_night_enabled`, and more to be defined.
2. **Player preferences** — per-player settings: `show_own_name`, audio preferences, etc.
3. **Extension-specific options** — configurable behavior per extension: LiveKit settings, prop toggles, etc.

Currently none of these exist as a system. Guest access is always-on (hardcoded), day/night is client-only localStorage, player prefs are localStorage only, extensions don't expose options.

## Decisions Made

### World options: stored as env vars or a singleton config record

- The `worlds` collection was removed — the project has one world with multiple maps.
- World-level options (`allow_anonymous`, `day_night_enabled`) will be stored as typed columns on a singleton config record or env vars (to be decided when options are implemented).
- Worldsim loads options on startup, enforces them (e.g. rejects guest `AuthFrame` with `AuthResultFrame{ok: false}` when `allow_anonymous=false`).
- **Multi-map:** worldsim loads all maps from PocketBase. The default map is configured via the `DEFAULT_MAP` env var.

### Replication to clients: dedicated `WorldOptionsFrame` (Option B)

Two approaches were considered:

**Option A — Singleton entity with `WorldOptions` component:** Reuses existing replication machinery (SpawnEntity/UpdateComponent). Rejected because the world is not an entity — forcing it into the `Entity` struct (which has ~15 fields: Position, NetworkSession, Gid, SpriteBase, mobileZone, currentZones, etc.) creates special cases in every system that iterates `s.entities` (movement, zone detection, proximity clustering, frontend spawn).

**Option B — Dedicated `WorldOptionsFrame` (chosen):** New variant in `ServerFrame` oneof. Worldsim publishes it on `client.<id>.replication` (same NATS subject — pusher forwards raw bytes unchanged regardless of payload type, zero pusher code changes). Sent on client connect (after auth, before first tick) and on change.

Advantages of Option B:
- No fake entity in the ECS.
- No special cases in movement, zone, proximity, or frontend spawn logic.
- Sent immediately on auth, before the first replication tick.
- Doesn't participate in the per-tick replication loop (sent only on connect and on admin change).
- ~15 lines of new worldsim code, zero pusher changes, clean frontend handler.

### Player preferences: columns on existing `players` collection

No separate `user_preferences` collection — the `players` table already holds per-player state (`display_name`, `sprite_base`, `pos_x`, `pos_y`). Adding preference columns there is simpler: one collection, one lookup, one place for the admin to edit.

Three tiers of player preferences:
- **Client-local** (audio devices, UI scale, day/night overlay toggle): stay in `localStorage`. Server has no reason to know.
- **Persisted, not replicated** (`show_own_name`, keybindings, preferred language): columns on `players` in PB, fetched on login, updated via `SetPreferenceFrame`.
- **Replicated** (things that change how others see you): ECS components on the entity, like `DisplayName` already is. Replication system handles propagation automatically.

The question for each new preference: "Does another player's client need this to render correctly?" If yes → component. If no but should survive logout → persisted column. If neither → localStorage.

### Extension options: declared at registration, created by worldsim

Extensions do NOT access PocketBase directly (this was a doc rule that the implementation violated — see Discrepancy below). Instead:

1. Extension declares its options schema in the registration message to worldsim.
2. Worldsim (which owns PB) creates the collection on the extension's behalf with default values.
3. Worldsim reads the current values and sends them back to the extension via NATS.
4. On PB record change (via in-process Go hooks since PB is embedded), worldsim publishes the updated options to the extension via NATS.
5. The admin edits extension options in the PB admin GUI, same as world and player options.

The extension carries its own options schema definition in code (sent in the registration message). Worldsim handles all PB interaction. Extensions never touch PB.

### Admin editability

All option collections are typed columns in PocketBase, so the admin gets a free editor via the PB admin GUI — no custom admin panel to build. This is a key requirement.

## Resolved Questions

### PB embedding: DECIDED — embed PB in worldsim

**Decision:** PocketBase will be embedded in the worldsim server as a Go library, not run as a standalone container.

**Architecture commitment:** One worldsim instance owns multiple maps. Pusher(s) can scale horizontally, all forwarding to the one worldsim via NATS. No multi-worldsim sharding.

**What this gives us:**
- Realtime hooks are in-process Go function calls (`app.OnRecordAfterUpdate()`). No NATS relay, no SSE, no custom PB binary. Hot-reload is trivial.
- No HTTP overhead for PB reads/writes. All store operations (MapStore, UserStore, SpriteStore, WorldStore) become in-process calls.
- Single binary deployment for worldsim.
- PB admin GUI served by worldsim (worldsim exposes PB's HTTP routes at `/api/` and `/_/`).

**Accepted tradeoffs:**
- **Lifecycle coupling:** PB goes down when worldsim restarts. Accepted — with one worldsim, PB is only used by worldsim (extensions go through NATS). Admin GUI has brief downtime on restart.
- **No sharding:** Embedding means no multiple worldsim instances sharing one PB. This is an architectural commitment to one worldsim owning multiple maps.
- **Extensions don't touch PB:** Extensions communicate only via NATS. Worldsim is the sole PB accessor. This was the original doc intent and is now enforced by design.

**What changes:**
- `docker/pocketbase.Dockerfile` and `docker/pocketbase-entrypoint.sh` are removed. PB is no longer a separate container.
- `docker/docker-compose.yml` removes the `pocketbase` service. Worldsim's container mounts the PB data volume.
- `backend/cmd/worldsim/main.go` initializes PB (`pocketbase.New()`) with migrations and hooks, then starts the worldsim tick loop.
- All stores (`MapStore`, `UserStore`, `SpriteStore`) switch from HTTP API calls to PB Go SDK calls (`app.Dao().FindRecordByData()` etc.).
- Worldsim exposes PB's HTTP routes for the admin GUI (PB's `app` serves HTTP on a configurable port).

### Extension PB access: DECIDED — removed, NATS only

**Current state (to be fixed):** ext-walls and ext-av both hit PB's HTTP API directly to read the Tiled map JSON and extract zone properties (wall zones, AV zones). This duplicates work worldsim has already done (worldsim loads and parses the same map via `LoadMap()`).

**Fix:** Worldsim already has parsed zone data in memory. It provides zone metadata to extensions via NATS:
- On `worldsim.ready`, worldsim broadcasts zone metadata (zone IDs + properties) for all maps in the world.
- Extensions use this to determine which zones they care about (wall zones, AV-enabled zones, etc.) without reading PB.
- On map reload (`map.updated`), worldsim re-broadcasts updated zone metadata.

Extensions never touch PB. The `POCKETBASE_URL` env var is removed from extension configs.

### Multi-map support (PREREQUISITE — being designed now)

Multi-map support must be designed and implemented BEFORE the options system. See the multi-map design document (to be created).

## Discrepancy Found and Resolved

Documentation (`06-data-model-and-persistence.md` line 38) says "Extensions do not access PocketBase directly." But ext-walls and ext-av both hit PB's HTTP API directly to read map data. **Resolution:** This is a bug in the extensions, not the docs. Extensions will be refactored to get zone data via NATS from worldsim, and PB will be embedded in worldsim so extensions cannot access it even if they tried.

## Revised Implementation Order

1. **Design multi-map support + PB embedding** — how `worlds` relates to `maps`, how worldsim boots with a world (not a map), how players move between maps, how PB is embedded, how extensions get data via NATS.
2. **Implement PB embedding** — embed PB in worldsim, convert stores to Go SDK, remove standalone PB container, expose admin GUI.
3. **Implement multi-map support** — `DEFAULT_MAP` env var, worldsim loading all maps from PocketBase, player map transitions via portal zones.
4. **Refactor extensions to use NATS for map data** — remove direct PB access, get zone metadata from worldsim via NATS.
5. **Layer options on top** — world options as env vars or singleton config, player preferences as columns on `players`, extension options declared at registration and created by worldsim.
6. **Realtime hot-reload** — via PB in-process Go hooks (world options, extension options).

## Files to Modify (when implementation proceeds)

- `proto/frames.proto` — `WorldOptionsFrame`, `SetPreferenceFrame`, new ServerFrame/ClientFrame variants
- `backend/internal/worldsim/worldsim.go` — world options loading, enforcement, publish, preference handling
- `backend/internal/worldsim/worldstore.go` — new file, WorldStore (PB Go SDK reads + in-process hooks)
- `backend/cmd/worldsim/main.go` — `DEFAULT_MAP` env var, PB initialization
- `backend/cmd/ext-*/main.go` — remove PB access, get zone data via NATS, declare options schema in registration
- `frontend/src/net/WsClient.ts` — `onWorldOptions` handler, `SetPreferenceFrame` send
- `frontend/src/scenes/GameScene.ts` — apply world options + player preferences
- `frontend/src/ui/TopMenu.ts` — preference toggle UI
- `pb_migrations/` — preference columns on players (now embedded Go migrations, run by worldsim)
- `docker/pocketbase.Dockerfile` — REMOVED (PB embedded in worldsim)
- `docker/pocketbase-entrypoint.sh` — REMOVED
- `docker/docker-compose.yml` — remove pocketbase service, mount PB data volume on worldsim
- `backend/internal/worldsim/mapstore.go` — convert from HTTP API to PB Go SDK
- `backend/internal/worldsim/userstore.go` — convert from HTTP API to PB Go SDK
- `backend/internal/worldsim/spritestore.go` — convert from HTTP API to PB Go SDK
