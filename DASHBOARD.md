# Dashboard

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
