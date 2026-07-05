# PixelEruv.o — Dashboard

Last updated: 2026-07-05 (session 3)

## Overview

2D top-down spatial MMO with OIDC authentication, persistent identity,
extensible zone system, and first-party extensions. Kernel architecture
(worldsim + pusher) + extensions communicating via NATS.

## Current architecture

```
Browser ──WS──> Nginx ──> Pusher ──NATS──> WorldSim ──> PocketBase
                                ↕               ↕
                             ext-demo        ext-walls
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

## Implemented features

### Authentication & Identity
- [x] Dex OIDC with authorization code flow + PKCE
- [x] JWT validation on pusher side (JWKS, iss, aud, sub)
- [x] 2 users: `admin@pixeleruv.local` / `player@pixeleruv.local` (password: `password123`)
- [x] Persistent identity: `oidc_sub` → PocketBase `players` record → `entity_id` + position
- [x] Position saved on disconnect, restored on reconnect

### Rendering & Movement
- [x] 32x32 character sprites (6 characters, 4 directions, 6 walk frames)
- [x] Walk animation (3fps) + idle animation (2fps, 4 frames)
- [x] Direction mapping: 0=down, 1=left, 2=right, 3=up
- [x] 8-directional movement with wall sliding
- [x] Collision: Walls tile layer (fallback) + extension gate triggers (zones)

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

### Integrity & Documentation
- [x] Map integrity checker: validation at startup, every 5 min, and on demand (`admin.map.integrity` via NATS)
- [x] Map design guide documentation (`documentation/21-map-design-guide.md`): layers, properties, shapes, upload
- [x] SVG diagram of layer structure and data flow (`documentation/map-design-guide.html`)

### Infrastructure
- [x] Docker Compose: nats, pocketbase, dex, pusher, worldsim, frontend, ext-demo, ext-walls
- [x] Nginx proxy: `/dex/` → Dex (same-origin for browser)
- [x] Makefile for local dev (pusher + worldsim as native binaries)
- [x] OpenTelemetry instrumentation (disabled by default)

## Remaining work (MVP)

### High priority
- [ ] **Camera follow**: camera follows local player instead of showing whole map
- [ ] **Zones in Tiled**: add rectangles on Zones object layer with `zone_type=wall` to test ext-walls
- [ ] **Chat**: chat UI + PocketBase collection, messages broadcast via NATS

### Medium priority
- [ ] **LiveKit A/V**: positional audio/video (LiveKit server, bridge, token exchange, WebRTC client)
- [ ] **AOI filter**: only replicate entities within client radius + same zone
- [ ] **Input triggers**: clicks/keys → broadcast to extensions (NPC interactions, objects)
- [ ] **Exclusive zones**: visual + audio isolation for members

### Low priority
- [ ] **Knock-to-join**: meeting rooms with owner and admission
- [ ] **Mobile zones**: circular zones that follow an entity (NPC vision)
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

# Docker (full stack)
docker compose -f docker/docker-compose.yml up --build -d
docker compose -f docker/docker-compose.yml logs -f worldsim
docker compose -f docker/docker-compose.yml restart ext-walls

# PocketBase admin
http://localhost:8090/_/

# Check registered players
curl -s http://localhost:8090/api/collections/players/records | jq

# Zone event logs
docker logs pixeleruv-ext-demo-1 -f

# Map integrity check on demand
nats -s nats://localhost:4222 pub admin.map.integrity ""
docker logs pixeleruv-worldsim-1 2>&1 | grep "integrity"
```

## Notable branches

| Branch                    | Description |
|--------------------------|-------------|
| main                     | Main branch |
| zones                    | Zones + extension protocol (merged into main) |
| netcode-lerp-prediction  | Client prediction + interpolation (not merged) |
