# PixelEruv.o — Dashboard

Last updated: 2026-07-09 (session assets-reorg)

## Overview

2D top-down spatial MMO with OIDC authentication, persistent identity,
extensible zone system, and first-party extensions. Kernel architecture
(worldsim + pusher) + extensions communicating via NATS.

Remote deployment note: the `dist/` backend image now bundles `sprites/` so
worldsim can seed the `sprite_bases` catalog on first run. The `docker/dist/`
templates are now tracked (`.gitignore` previously ignored them due to an
overly broad `dist/` pattern).

Default map: `map1` (the `maps/default-map.json` office map). The
`test-map.json` starter file has been removed. Map sources (tmx, json and
tileset PNGs) live in `maps/`; spritesheets live in `spritesheets/`. `make`
copies them to `frontend/public/assets/maps` and `frontend/public/sprites` for
the Vite dev server and the `dist/` build. The frontend loads the map from
PocketBase only — upload `maps/default-map.json` and its tileset PNGs to a
`maps` record named `map1` (or the configured `MAP_ID`/`VITE_MAP_NAME`).

## Current architecture

```
Browser ──WS──> Nginx ──> Pusher ──NATS──> WorldSim ──> PocketBase
                                ↕               ↕
                             ext-demo        ext-walls
                             ext-props       ext-av ──> LiveKit
```

| Service      | Role                                              | Stack           |
|--------------|---------------------------------------------------|-----------------|
| frontend     | Phaser 3 client, OIDC auth, sprite rendering      | TypeScript/Vite |
| pusher       | WebSocket ↔ NATS gateway, JWT validation          | Go              |
| worldsim     | Spatial authority, ECS, zones, replication        | Go              |
| pocketbase   | Maps, players, positions storage                  | PocketBase      |
| dex          | OIDC identity provider (local-password)           | Dex             |
| nats         | Pub/sub message bus + JetStream                   | NATS            |
| ext-demo     | Demo extension (logs zone events)                 | Go              |
| ext-walls    | Walls extension (block gate triggers on zones)    | Go              |
| ext-props    | Props extension (interactive entities, key:E)     | Go              |
| ext-av       | LiveKit A/V bridge (token minting, NATS events)   | Go              |
| livekit      | WebRTC SFU for audio/video                        | LiveKit         |

## Implemented features

### Authentication & Identity
- [x] Dex OIDC with authorization code flow + PKCE
- [x] JWT validation on pusher side (JWKS, iss, aud, sub)
- [x] 2 users: `admin@pixeleruv.local` / `player@pixeleruv.local` (password: `password123`)
- [x] Persistent identity: `oidc_sub` → PocketBase `players` record → `entity_id` + position
- [x] Position saved on disconnect, restored on reconnect
- [x] Guest sessions: empty `id_token` is accepted by pusher as an anonymous, non-persistent session (`sub=""`); a non-empty but invalid/expired token is still rejected
- [x] **Anonymous (no Login) flow** — what happens when a user never clicks Login:
  - *Frontend*: `main.ts` boots straight into the game (no Dex redirect). `isLoggedIn()` is false, so `TopMenu` shows a blue "Login" button. `getIdToken()` returns `null` (empty string in localStorage is treated as null, commit `ca97a6e`). On WebSocket open, `WsClient` sends `AuthFrame{idToken: ""}` (the `?? ""` fallback in `ws.onopen`).
  - *Pusher*: `handleWS` sees `s.auth` configured but `idToken == ""`, so the JWT validation branch is skipped — `sub` stays `""`. A fresh `clientID` is minted and `AuthResult{Ok: true}` is returned. A non-empty-but-invalid token would still be rejected (`TestInvalidTokenRejected`).
  - *Worldsim*: `provisionClient` is called with `sub == ""`, so the PocketBase `FindOrCreateUser` lookup is skipped (`sub != "" && sub != "dev"` is false). The avatar spawns at the map's default spawn point with a non-persistent entity ID `e_<clientID>`. Nothing is written to PocketBase.
  - *Net effect*: guests can play, move, and use A/V (mic/camera) identically to logged-in users, but get a **per-session identity only** — no PocketBase user record, no persisted entity ID, no saved position. Each reconnect mints a new `clientID`/entity. Guests are indistinguishable from each other on the backend (only the ephemeral `clientID` differentiates them); any future per-user feature relying on `sub` (inventory, friends, etc.) would need a separate guest policy.
- [x] Floating top-right menu (`frontend/src/ui/TopMenu.ts`): mic/camera A/V controls (moved from `AvOverlay`'s old bottom-right HUD), a Login/Logout button (drives Dex redirect / `logout()`), and a Menu dropdown to set a display name, stored client-side only (`localStorage["display_name"]`, `frontend/src/username.ts`) — not yet wired into the replication protocol or shown as an avatar name tag. `TopMenu` is created once in `main.ts` and stored on `game.registry`; `GameScene` attaches/detaches the A/V buttons to its per-scene `AvClient` via `attachAvControls`/`detachAvControls`

### Rendering & Movement
- [x] 32x32 character sprites (6 characters, 4 directions, 6 walk frames)
- [x] Walk animation (3fps) + idle animation (2fps, 4 frames)
- [x] Run animation used as default movement (frame-row 2 of limezu sheet; visual-only, speed unchanged)
- [x] Direction mapping: 0=down, 1=left, 2=right, 3=up
- [x] **Server-authoritative avatar sprites**: `SpriteIndex` assigned at provision time (FNV-1a hash of entity ID % 5), replicated via `Appearance.sprite_index` (component ID 3). All clients now render the same character sprite for the same player — previously each client picked a sprite via a local counter, causing two anonymous players to see each other as different characters. The client-side fallback also uses the same FNV-1a hash (not a counter), so even if the Appearance component doesn't arrive, all clients still agree. On WebSocket reconnect, the `avatars` map is fully cleared so the server re-spawns all entities with correct sprites. 3 unit tests in `worldsim_sprite_test.go`.
- [x] Collision: Walls tile layer (fallback) + extension gate triggers (zones)
- [x] Collision evaluated at avatar feet (Position.Y + feet offset), not sprite origin — fixes wall collision being off by ~1 tile
- [x] Swept (segment-vs-shape) collision: walls of any thickness work, no tunneling through sub-tile walls

### Zones & Extensions
- [x] Parse Zones object layer from Tiled (rect, circle, polygon)
- [x] Continuous-space zone checks (no tile rasterization) for collision and enter/exit
- [x] Enter/exit detection → NATS `zone.enter` / `zone.exit` events
- [x] Extension protocol: `extension.<id>.register`, `.heartbeat`, `.register_triggers`
- [x] Gate triggers: `block` / `allow` (cached locally, evaluated during movement)
- [x] Stale extension detection (3× heartbeat interval)
- [x] ext-walls: reads map, finds `zone_type=wall`, registers block triggers
- [x] ext-demo: logs zone enter/exit events
- [x] Walls migrated to extension system (Walls tile layer = fallback only)
- [x] Map hot-reload: worldsim detects map changes in PocketBase every 30s, publishes `map.updated` NATS event
- [x] ext-walls subscribes to `map.updated`, re-reads map and re-registers triggers

### Decoration Layers & Interactive Entities
- [x] Decoration layers identified by `layer_type=decoration` custom property (tile layers + object layers with `gid`)
- [x] Layer altitude = Tiled layer stack order (no numeric property)
- [x] Per-layer `sort_mode` (`static` = fixed depth band, `dynamic` = Y-sort with avatars)
- [x] Unified depth space: `BAND_BASE(layer) + (feetY_pixels / mapHeightPixels)`
- [x] Frontend renders multiple decoration layers with static/dynamic sort modes (GameScene.ts)
- [x] Interactive entities authored as Tiled objects with `entity_id`, `sprite`, `interactable`, `trigger_radius`
- [x] `ext-props` extension claims entities by `entity_type=prop`, registers `key:E` input trigger
- [x] `ActionFrame` / `ActionResultFrame` protos for client→worldsim→extension→worldsim→client flow
- [x] Pusher forwards `ActionFrame` to worldsim via `client.<id>.action` NATS subject
- [x] Worldsim dispatches actions to registered extensions, applies replies (state + animation)
- [x] `TriggerOwner` proto for extension ownership claims (by `entity_type` or `owner_extension`)
- [x] Unit tests: entity parsing, adjacency filtering, action reply, input trigger registration/coexistence/staleness

### Integrity & Documentation
- [x] Map integrity checker: validation at startup, every 5 min, and on demand (`admin.map.integrity` via NATS)
- [x] Map design guide documentation (`documentation/21-map-design-guide.md`): layers, properties, shapes, upload
- [x] SVG diagram of layer structure and data flow (`documentation/map-design-guide.html`)
- [x] Design doc: decoration layers, Y-sort depth bands, and interactive map-authored entities — `documentation/plans/2026-07-05-decoration-layers-and-interactive-entities-design.md` + `documentation/depth-layers-diagram.svg`

### LiveKit A/V (positional audio + video)
- [x] `AvTokenFrame` proto: carries LiveKit room, token, URL, action (join/leave), members
- [x] Worldsim: `client_id` in zone event payloads (so ext-av can address token replies)
- [x] Worldsim: mobile proximity zones (2-tile radius circles that follow each player)
- [x] Worldsim: proximity clustering via connected components (BFS on near-neighbor graph, ~4Hz)
- [x] Worldsim: `proximity.join` / `proximity.leave` NATS events (edge-triggered on group changes)
- [x] Worldsim: `Zone.AvEnabled` field + `av_enabled` Tiled property parsing (zones override proximity A/V)
- [x] ext-av: mints LiveKit JWTs, subscribes to zone + proximity events, publishes `client.<id>.av_token`
- [x] Pusher: forwards `client.*.av_token` as `AvTokenFrame` over WebSocket
- [x] Frontend: `AvClient` (LiveKit SDK wrapper, lazy-loaded), `AvOverlay` (DOM video tiles + HUD)
- [x] Frontend: spatial volume (linear falloff, 0 at 10 tiles), billboarded video tiles, mic/camera HUD
- [x] Infrastructure: LiveKit + ext-av in docker-compose, `docker/livekit.yaml` config
- [x] Map: `meeting-room-1` zone marked `av_enabled=true` in `maps/default-map.json`
- [x] Fix: AvClient retry (3 attempts) on transient ICE failure + unhandled promise rejection fix
- [x] Fix: upgrade LiveKit server v1.9.8 → v1.13.2 (protocol 17, `/rtc/v1` path, data tracks enabled)
- [x] Fix: reduce UDP ICE port range 50000-50100 → 50000-50020 (Docker Desktop UDP forwarding reliability)

### Chat
- [x] `ChatFrame` (client→server) + `ChatMessageFrame` (server→client) protos in `frames.proto`
- [x] Worldsim `handleChat`: stamps `display_name` (PocketBase for logged-in, `Guest <last4>` for guests) + `timestamp`, truncates text to 500 runes, routes global → `chat.broadcast`, proximity → per-recipient `client.<id>.chat_inbox` (including sender echo)
- [x] Worldsim `entityIDToClient` map + `DisplayName` on `Entity` (set at provision time, no per-message PocketBase lookup)
- [x] Pusher: `chat.broadcast` subscription (fans out to all sessions) + per-session `chat_inbox` subscription (raw bytes pass-through)
- [x] Frontend: `ChatPanel.ts` (right-side DOM sidebar, Global/Nearby tabs, message list, input row), `TopMenu` Chat toggle button, `WsClient.sendChat` + `onChatMessage`
- [x] 8 unit tests in `worldsim_chat_test.go`: global broadcast, text truncation (rune-safe), proximity delivery + isolation, solo drop, guest display name, unknown client, unknown channel

### Avatar Name Tags
- [x] `DisplayName` component (ID=4) in `components.proto` + `SetNameFrame` (client→server) in `frames.proto`
- [x] Worldsim `handleSetName`: sanitizes (ASCII printable 32–126, max 20 runes), updates `Entity.DisplayName`, marks `dirtyName`, replicates via `UpdateComponent`. Persists to PocketBase for logged-in users via `UserStore.UpdateDisplayName`; guests are session-only (no PocketBase record)
- [x] Replication: `DisplayName` included in `SpawnEntity.components` at spawn + `UpdateComponent` on name change (mirrors `dirtyPosition`/`dirtyState` pattern)
- [x] Pusher: `ClientFrame_SetName` forwarded to `client.<id>.set_name` on NATS
- [x] Frontend: Press Start 2P bitmap font (8×8, CC0/OFL, generated as PNG+XML atlas), `BitmapText` name tags above avatars with dark drop shadow, hidden for local player, repositioned each frame in `update()`
- [x] TopMenu: Save button calls `ws.setName()` (localStorage caches input only, no auto-send on boot)
- [x] 4 unit tests in `worldsim_nametag_test.go`: name update + replication, sanitization/truncation, guest non-persistence, spawn includes DisplayName component

### Spawn Points
- [x] First-time/guest users spawn on a random walkable tile inside a random `zone_type=spawn` zone on the Zones layer; returning users keep their saved last position. Falls back to the existing center-spiral `FindSpawn()` when no spawn zones exist or none have walkable tiles.
- [x] `MapData.SpawnZones` populated in `loadMapData` by filtering zones with `zone_type=spawn` (still registered as ordinary zones).
- [x] `FindSpawnPoint(rng)`: enumerates walkable tiles in a random spawn zone's bbox (clamped to map) filtered by `Zone.Contains` (rect/circle/polygon all work), picks one uniformly at random; tries zones in shuffled order; falls back to `FindSpawn()` if none yield a tile. Shared `walkableTilesInZone` helper used by both spawn selection and the integrity check.
- [x] Integrity check warns (LevelWarning) when a spawn zone has no walkable tiles; `"spawn"` added to `knownZoneTypes`.
- [x] `Simulator.rng` (`math/rand/v2`, seeded from `time.Now().UnixNano()`) wired into `provisionClient` (one-line call-site change; saved-position restore unchanged).
- [x] 8 unit tests in `mapdata_spawn_test.go`: fallback (no zones), rect-zone pick, all-blocked fallback, distribution coverage, circle zone, JSON parsing of spawn zones, integrity warn/no-warn.
- [x] Design doc: `docs/plans/2026-07-07-spawn-points-design.md`

### Sprite Selection (phase 1: base sheet) — implemented
- [x] Design doc: `docs/plans/2026-07-07-sprite-selection-design.md` (branch `feat/sprite-selection`)
- [x] PB `sprite_bases` collection (admin-managed catalog of base sheets) + `players.sprite_base` field
- [x] Proto: `Appearance.sprite_base` replaces `sprite_index` (field 2 reserved); new `SetSpriteBaseFrame` (ClientFrame tag 7)
- [x] Server: `Entity.SpriteBase` + `dirtyAppearance`; `handleSetSpriteBase` mirrors `handleSetName`; `SpriteStore` (ListBases, BaseExists, SeedIfEmpty, Seed)
- [x] Auto-seed `sprite_bases` from `SPRITES_DIR` on worldsim startup (idempotent); `cmd/seed-sprites` CLI with `-force` for adding sheets later
- [x] Frontend: `spriteLoader.ts` catalog fetch; `CharacterSelectScene` pre-join chooser; hot-swap on `UpdateComponent` for appearance; TopMenu "Character sheet" field
- [x] 4 new tests: SetSpriteBase updates+replicates, guest updates, unknown client rejected, empty reverts to fallback
- [x] Updated sprite tests for SpriteBase; updated addPlayer helper
- [x] Docs: quick-start.md section 8 (spritesheets)
- [ ] Phase 2 (deferred): in-game mirror, palette recolor + accessory overlays (recipe), per-player pixel customization

### Infrastructure
- [x] Docker Compose: nats, pocketbase, dex, pusher, worldsim, frontend, ext-demo, ext-walls, ext-props, ext-av, livekit
- [x] Nginx proxy: `/dex/` → Dex (same-origin for browser)
- [x] Makefile for local dev (pusher + worldsim as native binaries)
- [x] **Self-contained `dist/`**: `make dist-x86` (linux/amd64, for Docker deployment) or `make dist-macos` (darwin/arm64, for native host execution) builds binaries + web assets and stages binary-based Dockerfiles, compose, nginx.conf, livekit.yaml, dex config, and pb_migrations into `dist/`. The entire `dist/` directory can be copied to another machine and run with `docker compose -f dist/docker-compose.yml up --build` — no source code needed.
- [x] OpenTelemetry instrumentation (disabled by default)
- [x] WebSocket keepalive: pusher sends protocol-level pings every 30s (browser auto-responds with pong) so idle connections don't die
- [x] Frontend auto-reconnect: exponential backoff (1s→30s cap), re-auths on reconnect, "Reconnecting…" overlay, re-sends current input state
- [x] **HTTPS for remote access**: nginx now also listens on 443 (exposed as `4043:443`) with a self-signed cert generated at container start from `TLS_HOSTS`. Required because browsers only expose `crypto.subtle` (used by the PKCE auth flow in `frontend/src/auth.ts`) in secure contexts (HTTPS or localhost) — accessing the app remotely over plain HTTP produced a black screen + `TypeError: undefined is not an object (evaluating 'crypto.subtle.digest')`. Localhost dev over `http://localhost:4080` still works unchanged. To enable remote access: set `PUBLIC_HOST=<host-lan-ip>` in compose env (or `.env` file), rebuild, then open `https://<host-lan-ip>:4043` and accept the self-signed cert warning once. `PUBLIC_HOST` drives three things automatically: (1) the TLS cert SAN (auto-appended to `TLS_HOSTS` by `frontend-entrypoint.sh`), (2) the Dex `redirectURIs` entry (templated by `dex-entrypoint.sh` at startup), and (3) `LIVEKIT_PUBLIC_URL` for remote A/V. No need to manually edit `docker/dex/config.yaml` or `TLS_HOSTS` anymore.
- [x] **PocketBase same-origin proxy**: nginx now proxies `/api/` → `pocketbase:8090/api/` (both HTTP and HTTPS servers), and `frontend/src/mapLoader.ts` derives its PocketBase URL from `window.location.origin` at runtime (matching the existing pattern in `auth.ts` for Dex). Previously `mapLoader.ts` hardcoded `http://localhost:8090`, so a remote browser tried to fetch the map from the viewer's own localhost — the map failed to load. Now the browser reaches PocketBase through the same nginx proxy as Dex and the WebSocket, working for both localhost and remote access with no hardcoded addresses.

## Remaining work (MVP)

### High priority
- [x] **Camera follow**: camera follows local player (centered) with mouse-wheel zoom (1x–4x, default 2x)
- [x] **Zones in Tiled**: wall zones on Zones object layer tested with ext-walls; client-side prediction matches server
- [x] **Canvas fills browser window**: Scale.RESIZE mode (was hardcoded 640x640)
- [x] **Entity ID fix**: server returns real entity ID via request-reply (was derived client-side, wrong for persistent identities)
- [x] **Chat**: global + proximity chat, ephemeral (no persistence), server-stamped display_name + timestamp. Right-side DOM sidebar with Global/Nearby tabs, toggled from TopMenu. Worldsim-mediated routing: global → `chat.broadcast` (pusher fans out to all sessions); proximity → per-recipient `client.<id>.chat_inbox` (including sender echo). 8 unit tests in `worldsim_chat_test.go`
- [x] **Avatar name tags**: server-authoritative display names via `SetNameFrame`, new `DisplayName` component (ID=4) in replication, Phaser `BitmapText` tags above avatars (Press Start 2P bitmap font, drop shadow), hidden for local player. Logged-in names persist to PocketBase; guests are session-only. TopMenu Save sends `SetNameFrame`; localStorage caches input only. 4 unit tests in `worldsim_nametag_test.go`

### Medium priority
- [x] **LiveKit A/V**: positional audio/video (LiveKit server, bridge, token exchange, WebRTC client)
- [ ] **AOI filter**: only replicate entities within client radius + same zone
- [ ] **Speech bubbles**: in-character dialogue above avatar (separate from chat panel)
- [ ] **Input triggers (broader)**: inventory/equipment action triggers — basic `key:E` interaction implemented via ext-props; broader design ready, see `documentation/plans/2026-07-01-inventory-equipment-action-triggers-design.md`
- [ ] **Exclusive zones**: visual + audio isolation for members

### Low priority
- [ ] **Knock-to-join**: meeting rooms with owner and admission
- [x] **Mobile zones**: circular zones that follow an entity (implemented for proximity A/V)
- [ ] **Full extension pack**: walls, doors, base zone behaviors, base triggers
- [ ] **Client-side prediction + reconciliation** (netcode-lerp-prediction branch exists)

## Architectural decisions

| Date       | Decision | Rationale |
|------------|----------|-----------|
| 2026-07-05 | Authorization code flow + PKCE (not implicit) | Dex doesn't support `response_type=id_token` |
| 2026-07-05 | Collection `players` (not `users`) | PocketBase has a built-in `users` collection |
| 2026-07-05 | Superuser auth for PocketBase API | `null` rules = superuser only for create/update |
| 2026-07-05 | Separate `DEX_ISSUER` from `DEX_JWKS_URL` | Token `iss` = `localhost:5556`, but pusher reaches Dex via `dex:5556` in Docker |
| 2026-07-05 | Zones = Tiled object layer (not tile layer) | Zones are vector shapes with metadata, not tiles |
| 2026-07-05 | Gate triggers cached locally (no NATS round-trip) | `block`/`allow` are deterministic, no need to query extension on every move |
| 2026-07-05 | Walls tile layer kept as fallback | Prevents breaking collision if no wall zones are defined |
| 2026-07-05 | Periodic extension re-registration | NATS Core is fire-and-forget; first publish may be lost |
| 2026-07-05 | Walls migrated to extensions (gate triggers) | Kernel architecture with no gameplay logic; Walls tile layer kept as fallback |
| 2026-07-05 | Integrity checker at startup + periodic + on demand | Detects map corruption/inconsistencies early and during runtime |
| 2026-07-05 | Continuous-space zone checks (no tile rasterization) | Tile rasterization produces gaps for shapes thinner than a tile; direct point-in-shape tests are exact |
| 2026-07-05 | Map hot-reload via filename comparison | PocketBase generates new filenames on re-upload; worldsim polls every 30s and publishes `map.updated` to extensions |
| 2026-07-05 | Decoration layers recognized by `layer_type` property, not name | Removes the hardcoded `"Ground"` name; supports multiple decoration layers |
| 2026-07-05 | Layer altitude = Tiled layer stack order (no explicit numeric property) | Simplest mental model: reordering layers in Tiled changes altitude |
| 2026-07-05 | Per-layer `sort_mode` (`static`/`dynamic`) for Y-sort | Most decorations never need to interleave with the player; only tall/walkable-around objects do |
| 2026-07-05 | Entity ownership via `owner_extension` property + `TriggerOwner` | Lets a generic extension and dedicated extensions claim map-authored props without colliding |
| 2026-07-05 | Server-side WebSocket pings (not app-level ping frames) for keepalive | `coder/websocket` doesn't auto-ping; protocol-level pings get auto-pong from the browser with no client code. App-level `ClientFrame_Ping` already existed but was unused — protocol pings are simpler and keep the connection alive at the transport layer |
| 2026-07-05 | Reconnect mints a fresh session (new entity id, spawn-point teleport) | True session resumption would need worldsim to reattach the entity to the new session; out of MVP scope. Player teleports to spawn on reconnect — flagged for a future task |
| 2026-07-05 | `worldsim.ready` broadcast for extension registration | Extensions published registration before worldsim subscribed (NATS Core drops no-subscriber publishes), then waited up to 30s for the clock-phase-gated re-register window. worldsim now publishes `worldsim.ready` (with Flush) after its subscriptions are live; extensions wait for it (10s timeout fallback) and re-register on every fire, including worldsim restarts. Registration latency dropped from ~30s to ~2ms. |
| 2026-07-06 | Mobile zones for proximity detection (not distance calc) | Each player gets a 2-tile radius circle zone that follows them. Zone enter/exit events drive proximity adjacency — no per-tick O(n²) distance check. Clustering runs at ~4Hz via connected components (BFS) on the adjacency graph. |
| 2026-07-06 | Connected components for proximity groups (not fixed clusters) | Players A-B-C in a line (A near B, B near C, A far from C) form one group via BFS, not three separate pairs. Group ID = FNV-1a hash of sorted member entity IDs (stable across membership order). |
| 2026-07-06 | `av_enabled` zones override proximity A/V | Players inside an av_enabled zone get zone-based A/V (room = `zone-<slug>`), not proximity A/V. They're excluded from proximity clustering and leave any existing proximity group. |
| 2026-07-06 | LiveKit tokens via NATS → pusher → WS (not direct browser→ext-av) | ext-av mints JWTs and publishes `client.<id>.av_token` to NATS. Pusher subscribes per-session and forwards as `AvTokenFrame` over the existing WS. No new transport — reuses the pusher's auth + session management. |
| 2026-07-06 | `LIVEKIT_PUBLIC_URL` separate from `LIVEKIT_URL` | ext-av (in Docker) connects to `ws://livekit:7880` (Docker-internal), but the browser needs `ws://localhost:7880` (host-exposed). ext-av sends `LIVEKIT_PUBLIC_URL` in the token frame so the browser can reach the SFU. |
| 2026-07-06 | LiveKit SDK lazy-loaded in frontend | `livekit-client` is dynamically imported on first A/V token, keeping the main bundle small (~1.5MB vs +2MB if statically imported). |
| 2026-07-06 | Pin livekit image to v1.9.8 (not `latest`) + fixed config schema | `livekit/livekit-server:latest` drifted to a config schema that rejects top-level `tcp_port`/`udp_port`; the SFU crash-looped silently. `tcp_port` now lives under `rtc:`; top-level `udp_port` removed. Pinning prevents future drift. |
| 2026-07-06 | Re-upload map to PocketBase after editing maps/default-map.json | ext-av reads the map from PocketBase, not the repo. The committed `av_enabled` property on `meeting-room-1` was invisible until the map was re-uploaded (superuser PATCH on the `maps` record). Workflow: edit in Tiled → save to `maps/` → re-upload file to PocketBase → worldsim hot-reloads within 30s → ext-av/ext-walls re-read. |
| 2026-07-07 | Despawn queues a DestroyEntity (not just deletes from ECS) | `despawnClient` deleted the entity from `s.entities` but never told other clients, so avatars lingered on screen after a player closed their browser. The existing `destroyedBaseEntities` queue (map-reload destroys) is generalized to `destroyedEntities` and reused for player despawns — drained each tick after replication. |
| 2026-07-07 | Frontend no longer force-redirects to Dex login before boot | Needed for guest browsing. `main.ts` now always boots the game; a floating `TopMenu` shows Login/Logout based on `isLoggedIn()`. |
| 2026-07-07 | Guest = empty `id_token`, not the `"dev"` sentinel | `"dev"` was already used to mean "no Dex configured" (pusher ignores the token value entirely in that branch). Reusing it for guests would be ambiguous. `WsClient` now sends `""` when there's no stored token; pusher treats an empty token as a guest only when Dex *is* configured, and still rejects a non-empty-but-invalid token. |
| 2026-07-07 | ~~Username is client-side only (`localStorage`), no protocol change~~ **Superceded by avatar name tags (session 15)**: `SetNameFrame` sends to server, `DisplayName` component replicates, `localStorage` is now just an input cache | Requested as a minimal stub for now — no `display_name` field on the wire, no name tags over avatars yet. |
| 2026-07-07 | Avatar name tags: server-authoritative via `SetNameFrame` + `DisplayName` component (ID=4) | Anti-spoofing (consistent with chat's server-stamped design). One source of truth (`Entity.DisplayName`) for both chat and name tags. Client uploads; server sanitizes (ASCII printable, max 20 chars) + persists to PocketBase for logged-in users. |
| 2026-07-07 | Name tags use Phaser `BitmapText` with Press Start 2P font (not DOM overlay or `Text`) | `BitmapText` shares one texture across all tags (cheaper than `Text`'s per-object Canvas textures). Press Start 2P is CC0/OFL, 8×8 pixel art, matches the tileset style. DOM overlay breaks depth-sorting. |
| 2026-07-07 | Local player's own name tag is hidden | You know who you are; the name is in the TopMenu dropdown. Showing it adds center-screen clutter. Standard convention (WoW, most MMOs). |
| 2026-07-07 | No auto-send of cached name on boot; user must click Save each session | Safest — no risk of overriding a PocketBase-set name with a stale cache. The input is pre-filled from localStorage, so it's one click. |
| 2026-07-07 | Chat routed by worldsim (not a separate ext-chat extension) | Worldsim already owns every entity↔client map and computes proximity groups each tick, so there's zero mapping problem and no new service. Chat *routing* is plumbing (like replication routing), not gameplay logic — doesn't violate "kernel stays clean of gameplay". ext-chat would have been a thin wrapper looking up data worldsim already has. |
| 2026-07-07 | Chat is ephemeral (no PocketBase persistence) | Matches how A/V and movement work (live state only). No migration needed. A player who joins later or refreshes sees an empty chat. Scrollback/history fetch flagged for a future task. |
| 2026-07-07 | Worldsim marshals full ServerFrame; pusher writes raw bytes for chat | Matches the existing replication path (worldsim marshals, pusher passes through). Keeps pusher free of chat-specific logic. The av_token path is the exception (JSON→proto in pusher) because ext-av publishes JSON; chat is worldsim-to-pusher so we stay in protobuf end-to-end. |
| 2026-07-07 | Reset movement input on window blur / visibilitychange→hidden | Safari suspends DOM event delivery to the page while the native camera/mic permission popup (triggered by `getUserMedia` in AvClient) is shown. The `keyup` for a held arrow key is never delivered, so `inputState.<dir>` stays `true` and the avatar keeps walking after the popup appears. `clearMovementInput` on `blur` + `visibilitychange` is the standard Phaser fix; listeners are torn down on scene `SHUTDOWN`. |
| 2026-07-07 | Single `PUBLIC_HOST` env var drives all remote-access config | Previously remote access required manually editing `TLS_HOSTS`, `docker/dex/config.yaml` redirectURIs, and `LIVEKIT_PUBLIC_URL` separately. Now `PUBLIC_HOST` (default: `localhost`) is the single knob: `frontend-entrypoint.sh` auto-appends it to the TLS cert SANs, `dex-entrypoint.sh` templates it into the redirect URI, and docker-compose interpolates it into `LIVEKIT_PUBLIC_URL`. One variable, rebuild, done. |
| 2026-07-07 | nginx proxies `/api/` to PocketBase (same-origin) | `mapLoader.ts` hardcoded `http://localhost:8090`, so remote browsers fetched the map from the viewer's own machine. Adding the `/api/` proxy (mirroring the existing `/dex/` and `/ws/` proxies) and making `mapLoader.ts` use `window.location.origin` means the browser reaches PocketBase same-origin through nginx — no hardcoded address, works for localhost and remote alike. |
| 2026-07-07 | Server-authoritative avatar sprite index (FNV-1a hash of entity ID) | Each client independently picked a sprite via a local `colorIndex` counter, so two anonymous players saw each other as different characters (each client received its own spawn first → char_0, then the other → char_1). The server now assigns `SpriteIndex = hash(entityID) % 5` at provision time and replicates it via `Appearance.sprite_index`. Deterministic from entity ID → stable across reconnects for logged-in users, and identical on all clients. |
| 2026-07-07 | Spawn points = `zone_type=spawn` zones on the Zones layer (not a new layer or entity type) | Reuses the existing zone parser, `Zone.Contains` (rect/circle/polygon), and zone registry. Spawn zones are invisible to clients (server-side selection only, never replicated). One zone = random walkable tile within it; multiple zones = random zone then random tile. Falls back to `FindSpawn()` center-spiral when absent or unwalkable, so every existing map keeps working. |
| 2026-07-07 | Enumerate walkable tiles in zone bbox (not rejection sampling) for spawn pick | Rejection sampling degrades badly on small or mostly-walled zones and needs a retry cap. Enumeration is O(bbox area), bounded by zone size, gives a true uniform distribution, and runs once per session (cost irrelevant). Shared `walkableTilesInZone` helper used by both `FindSpawnPoint` and the integrity check. |

## Test accounts

| Service      | Username                   | Password        |
|--------------|----------------------------|-----------------|
| Dex admin    | `admin@pixeleruv.local`    | `password123`   |
| Dex player   | `player@pixeleruv.local`   | `password123`   |
| PB superuser | `admin@pixeleruv.local`    | `password123`   |

## Useful commands

```bash
# Local dev
make up                    # starts nats + pocketbase + dex + pusher + worldsim
make down                  # stops everything

# Docker (full stack, builds from source)
docker compose -f docker/docker-compose.yml up --build -d
docker compose -f docker/docker-compose.yml logs -f worldsim
docker compose -f docker/docker-compose.yml restart ext-walls

# Self-contained dist/ (pre-built binaries, no source needed)
make dist-x86                          # linux/amd64 — for Docker deployment
make dist-macos                        # darwin/arm64 — for native macOS execution
docker compose -f dist/docker-compose.yml up --build

# PocketBase admin
http://localhost:8090/_/

# Check registered players
curl -s http://localhost:8090/api/collections/players/records | jq

# Zone event logs
docker logs pixeleruv-ext-demo-1 -f

# Map integrity check on demand
nats -s nats://localhost:4222 pub admin.map.integrity ""
docker logs pixeleruv-worldsim-1 2>&1 | grep "integrity"

# Unit tests (worldsim — no Docker needed)
cd backend && go test ./internal/worldsim/ -v

# Integration tests (requires Docker stack running: worldsim, nats)
# TestMain starts an in-process pusher with no Dex so IdToken="dev" works.
cd backend && go test ./test/integration/ -v
```

## Notable branches

| Branch                    | Description |
|--------------------------|-------------|
| main                     | Main branch |
| zones                    | Zones + extension protocol (merged into main) |
| netcode-lerp-prediction  | Client prediction + interpolation (not merged) |
| feat/sprite-selection    | Sprite selection phase 1 (design doc only so far) |
