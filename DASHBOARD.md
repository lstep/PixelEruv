# PixelEruv.o — Dashboard

Last updated: 2026-07-07 (session 12)

## Overview

2D top-down spatial MMO with OIDC authentication, persistent identity,
extensible zone system, and first-party extensions. Kernel architecture
(worldsim + pusher) + extensions communicating via NATS.

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
- [x] Floating top-right menu (`frontend/src/ui/TopMenu.ts`): Login/Logout button (drives Dex redirect / `logout()`) + a Menu dropdown to set a display name, stored client-side only (`localStorage["display_name"]`, `frontend/src/username.ts`) — not yet wired into the replication protocol or shown as an avatar name tag

### Rendering & Movement
- [x] 32x32 character sprites (6 characters, 4 directions, 6 walk frames)
- [x] Walk animation (3fps) + idle animation (2fps, 4 frames)
- [x] Run animation used as default movement (frame-row 2 of limezu sheet; visual-only, speed unchanged)
- [x] Direction mapping: 0=down, 1=left, 2=right, 3=up
- [x] 8-directional movement with wall sliding
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
- [x] Map: `meeting-room-1` zone marked `av_enabled=true` in `assets/map1.json`
- [x] Fix: AvClient retry (3 attempts) on transient ICE failure + unhandled promise rejection fix
- [x] Fix: upgrade LiveKit server v1.9.8 → v1.13.2 (protocol 17, `/rtc/v1` path, data tracks enabled)
- [x] Fix: reduce UDP ICE port range 50000-50100 → 50000-50020 (Docker Desktop UDP forwarding reliability)

### Infrastructure
- [x] Docker Compose: nats, pocketbase, dex, pusher, worldsim, frontend, ext-demo, ext-walls, ext-props, ext-av, livekit
- [x] Nginx proxy: `/dex/` → Dex (same-origin for browser)
- [x] Makefile for local dev (pusher + worldsim as native binaries)
- [x] **Self-contained `dist/`**: `make dist-x86` (linux/amd64, for Docker deployment) or `make dist-macos` (darwin/arm64, for native host execution) builds binaries + web assets and stages binary-based Dockerfiles, compose, nginx.conf, livekit.yaml, dex config, and pb_migrations into `dist/`. The entire `dist/` directory can be copied to another machine and run with `docker compose -f dist/docker-compose.yml up --build` — no source code needed.
- [x] OpenTelemetry instrumentation (disabled by default)
- [x] WebSocket keepalive: pusher sends protocol-level pings every 30s (browser auto-responds with pong) so idle connections don't die
- [x] Frontend auto-reconnect: exponential backoff (1s→30s cap), re-auths on reconnect, "Reconnecting…" overlay, re-sends current input state
- [x] **HTTPS for remote access**: nginx now also listens on 443 (exposed as `4043:443`) with a self-signed cert generated at container start from `TLS_HOSTS`. Required because browsers only expose `crypto.subtle` (used by the PKCE auth flow in `frontend/src/auth.ts`) in secure contexts (HTTPS or localhost) — accessing the app remotely over plain HTTP produced a black screen + `TypeError: undefined is not an object (evaluating 'crypto.subtle.digest')`. Localhost dev over `http://localhost:4080` still works unchanged. To enable remote access: set `TLS_HOSTS=localhost,127.0.0.1,<host-lan-ip>` in compose env, add `https://<host-lan-ip>:4043/auth/callback` to `docker/dex/config.yaml` `redirectURIs`, rebuild, then open `https://<host-lan-ip>:4043` and accept the self-signed cert warning once.

## Remaining work (MVP)

### High priority
- [x] **Camera follow**: camera follows local player (centered) with mouse-wheel zoom (1x–4x, default 2x)
- [x] **Zones in Tiled**: wall zones on Zones object layer tested with ext-walls; client-side prediction matches server
- [x] **Canvas fills browser window**: Scale.RESIZE mode (was hardcoded 640x640)
- [x] **Entity ID fix**: server returns real entity ID via request-reply (was derived client-side, wrong for persistent identities)
- [ ] **Chat**: chat UI + PocketBase collection, messages broadcast via NATS

### Medium priority
- [x] **LiveKit A/V**: positional audio/video (LiveKit server, bridge, token exchange, WebRTC client)
- [ ] **AOI filter**: only replicate entities within client radius + same zone
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
| 2026-07-06 | Re-upload map to PocketBase after editing assets/map1.json | ext-av reads the map from PocketBase, not the repo. The committed `av_enabled` property on `meeting-room-1` was invisible until the map was re-uploaded (superuser PATCH on the `maps` record). Workflow: edit in Tiled → save to `assets/` → re-upload file to PocketBase → worldsim hot-reloads within 30s → ext-av/ext-walls re-read. |
| 2026-07-07 | Despawn queues a DestroyEntity (not just deletes from ECS) | `despawnClient` deleted the entity from `s.entities` but never told other clients, so avatars lingered on screen after a player closed their browser. The existing `destroyedBaseEntities` queue (map-reload destroys) is generalized to `destroyedEntities` and reused for player despawns — drained each tick after replication. |
| 2026-07-07 | Frontend no longer force-redirects to Dex login before boot | Needed for guest browsing. `main.ts` now always boots the game; a floating `TopMenu` shows Login/Logout based on `isLoggedIn()`. |
| 2026-07-07 | Guest = empty `id_token`, not the `"dev"` sentinel | `"dev"` was already used to mean "no Dex configured" (pusher ignores the token value entirely in that branch). Reusing it for guests would be ambiguous. `WsClient` now sends `""` when there's no stored token; pusher treats an empty token as a guest only when Dex *is* configured, and still rejects a non-empty-but-invalid token. |
| 2026-07-07 | Username is client-side only (`localStorage`), no protocol change | Requested as a minimal stub for now — no `display_name` field on the wire, no name tags over avatars yet. |

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
