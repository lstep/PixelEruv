# LiveKit A/V — Positional Audio + Video Tiles

Date: 2026-07-06
Branch: video

## Goal

Add positional audio and billboarded video tiles to PixelEruv.o. Players
within ~2 tiles of each other (outside A/V zones) join an ad-hoc LiveKit
room for proximity A/V. Players inside an `av_enabled` zone join that
zone's LiveKit room instead (zones override proximity).

## Architecture

A new `ext-av` extension bridges the spatial world to LiveKit. The kernel
(worldsim + pusher) stays free of LiveKit dependencies; ext-av handles all
LiveKit SDK calls and token minting.

```
Browser ──WS──> Pusher ──NATS──> WorldSim ──> PocketBase
   │              │                 │
   │              │                 └── zone.enter/exit ──> ext-av ──> LiveKit
   │              │                 └── proximity.join/leave ─┘
   │              │                                          │
   │              <── client.<id>.av_token ──────────────────┘
   │              │
   └── AvTokenFrame (new ServerFrame)
```

### Data flow

1. Player enters an `av_enabled` zone → worldsim publishes `zone.enter`
   (now with `client_id`).
2. ext-av receives it, checks the zone is A/V-enabled (reads map from
   PocketBase, same pattern as ext-walls), mints a LiveKit token for room
   `zone-<zone_id>` with identity = `entity_id`, publishes
   `client.<client_id>.av_token` with `{room, token, url, action:"join"}`.
3. Pusher forwards as `AvTokenFrame` to the browser.
4. Browser's `AvClient` module connects to the LiveKit room, publishes
   mic + camera tracks, subscribes to other participants' tracks.
5. Each tick, client receives replication with positions → updates
   per-participant audio volume based on distance, shows/hides billboarded
   video tiles for nearby avatars.
6. Player exits zone → `zone.exit` → ext-av publishes av_token with
   `action:"leave"` → client disconnects from LiveKit.

Proximity A/V follows the same flow, driven by `proximity.join` /
`proximity.leave` events instead of zone events.

### Zone vs proximity interaction

Zones override proximity. A player inside an `av_enabled` zone is in that
zone's room — proximity A/V is suppressed (worldsim does not emit
`proximity.join` for entities inside A/V zones). Outside zones, proximity
A/V applies.

## Proximity detection (mobile zones + clustering)

### Mobile zones

Activate the dormant `Mobility` field on `Zone`. Each player avatar gets a
mobile circular zone, radius 2 tiles, ID `prox-<entity_id>`. The zone
follows the avatar's position each tick.

Detection reuses the existing zone membership machinery —
`ZonesAtPoint`, `currentZones`, `zone.enter`/`zone.exit`. When player B's
feet enter A's mobile zone, worldsim fires `zone.enter` for B into
`prox-A`. No new detection code — just mobile zone position updates.

Mobile zones are created when a player entity spawns and removed when it
despawns. They are not authored in Tiled — worldsim creates them
programmatically for every player. They fire normal `zone.enter`/
`zone.exit` events; ext-av filters `prox-*` zones by ID prefix and only
acts on `proximity.join`/`proximity.leave`.

### Clustering

Mobile zones solve detection, not room assignment. Three players A, B, C
standing close together each have their own mobile zone (`prox-A`,
`prox-B`, `prox-C`). Mapping each zone to a LiveKit room would produce 3
separate rooms — B can't hear C. For a group to share one audio space,
they need one room.

Clustering runs in worldsim each tick (throttled to ~4Hz) after zone
membership updates:

1. Build a graph: nodes = player avatars not in an `av_enabled` zone
   (zones override proximity). Edge between A and B if B ∈ `prox-A` (or
   A ∈ `prox-B` — symmetric).
2. Connected components → each group gets a stable ID:
   `proxgroup-<hash(sorted member entity IDs)>`.
3. Compare each entity's `currentProximityGroup` against the new
   assignment. On change, publish edge-triggered:
   - `proximity.join` — `{entity_id, client_id, group_id, map_id, members: [entity_id...]}`
   - `proximity.leave` — `{entity_id, client_id, group_id, map_id}`

Scaling note: O(n²) pairwise check is fine for MVP player counts. Spatial
hashing would be the fix for larger worlds — noted as a future concern,
not built now.

## ext-av extension

New Go service `backend/cmd/ext-av`, same shape as ext-walls/ext-props.
Bridges zone + proximity events to LiveKit.

### Subscriptions

- `zone.enter` / `zone.exit` — for A/V-enabled zones (room-per-zone)
- `proximity.join` / `proximity.leave` — for proximity groups
- `map.updated` — refresh A/V zone set
- `extension.av.register` / heartbeat — standard extension protocol
  (observe-only, no gate triggers)

### Behavior

**On `zone.enter`** (entity enters an `av_enabled` zone):
- Mint LiveKit token: identity = `entity_id`, room =
  `zone-<slugify(zone_id)>`, grants = `canPublish + canSubscribe`
- Publish `client.<client_id>.av_token` with
  `{action:"join", room, token, url}`

**On `zone.exit`** (entity leaves an `av_enabled` zone):
- Publish `client.<client_id>.av_token` with `{action:"leave", room}`

**On `proximity.join`:**
- Mint LiveKit token: identity = `entity_id`, room = `proxgroup-<hash>`,
  grants = `canPublish + canSubscribe`
- Publish `client.<client_id>.av_token` with
  `{action:"join", room, token, url, members: [...]}`

**On `proximity.leave`:**
- Publish `client.<client_id>.av_token` with `{action:"leave", room}`

### Zone-vs-proximity override

Worldsim suppresses proximity clustering for entities inside an
`av_enabled` zone, so ext-av never receives a `proximity.join` for a
player already in a zone room. No override logic needed in ext-av.

### Token minting

`github.com/livekit/protocol/auth` (`AccessTokenBuilder`). Env vars:
`LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET`, `LIVEKIT_URL`. No LiveKit server
SDK — just the protocol package for minting.

### Room naming

`zone-<slugify(zone_id)>` for zone rooms, `proxgroup-<hash>` for
proximity rooms. Slugify replaces non-alphanumeric with `-` (LiveKit room
names are restricted). Rooms auto-create on first join.

### Config

`AV_ZONE_PROPERTY=av_enabled` (env, default `av_enabled`) — the Tiled
custom property name that marks a zone as A/V-enabled. Matches the
ext-walls pattern of reading zone properties from the map.

## Protocol changes

### `proto/frames.proto` — new ServerFrame variant

```protobuf
message AvTokenFrame {
  string room = 1;          // LiveKit room name; empty on "leave"
  string token = 2;         // LiveKit JWT; empty on "leave"
  string url = 3;           // LiveKit server URL (ws://...)
  string action = 4;        // "join" or "leave"
  repeated string members = 5;  // entity IDs in the group (proximity only; empty for zones)
}
```

Add to `ServerFrame.oneof payload`:
```protobuf
AvTokenFrame av_token = 6;
```

### `zone.enter` / `zone.exit` NATS payload — add `client_id`

Current:
```json
{"entity_id":"e_1","zone_id":"meeting","map_id":"test-map"}
```

New:
```json
{"entity_id":"e_1","client_id":"c_abc","zone_id":"meeting","map_id":"test-map"}
```

`client_id` is empty for non-player entities (base entities don't have
NetworkSession). ext-av ignores events with empty `client_id`.

### New NATS subjects (worldsim → ext-av)

- `proximity.join` — `{entity_id, client_id, group_id, map_id, members: [...]}`
- `proximity.leave` — `{entity_id, client_id, group_id, map_id}`

### New NATS subject (ext-av → pusher)

- `client.<id>.av_token` — `{action, room, token, url, members}` — pusher
  forwards as `AvTokenFrame` to the client over WS.

### Pusher change

Subscribe to `client.*.av_token` and forward as `AvTokenFrame` in the WS
stream, same pattern as replication forwarding.

### No client→server A/V frames

Mic mute/camera toggle are client-local state — the client
publishes/unpublishes tracks directly to LiveKit, never through worldsim.
The server never sees mic/camera state.

## Frontend

### `frontend/src/net/AvClient.ts`

Wraps the LiveKit browser SDK (`livekit-client`). Manages the connection
lifecycle and track publishing/subscribing.

- On `AvTokenFrame` with `action:"join"`: disconnect from any current
  room, connect to the new room with the token + url, publish mic +
  camera tracks (if not muted/disabled), subscribe to all existing
  participant tracks.
- On `action:"leave"`: disconnect from that room, tear down all tracks.
- Exposes: `setMicMuted(bool)`, `setCameraEnabled(bool)`,
  `getParticipantTracks()` — for the UI to query and render video tiles.
- Emits events: `participantJoined(entityId)`,
  `participantLeft(entityId)`, `trackReady(entityId, track)`.

Identity mapping: LiveKit participant identity = `entity_id` (set in the
token by ext-av). The client maps replication `entity_id` → LiveKit
participant → video track for billboard rendering.

### Spatial audio volume (per-tick)

Each replication tick, the client receives all entity positions. For each
subscribed LiveKit participant:

- Compute Euclidean distance between local avatar and the remote entity.
- If distance > 2 tiles (proximity) or outside the zone boundary (zone
  room): set track volume to 0.
- Otherwise: volume = `1 - (distance / maxDistance)`, clamped to [0, 1].
  LiveKit client SDK supports per-track volume control.

### Video tiles (billboarded)

In `GameScene.ts`, for each participant with a camera track and within
view:

- Render a small `video` DOM element positioned above the avatar sprite,
  billboarded (screen-space, doesn't rotate with camera zoom).
- Show/hide based on whether the participant has an active camera track.
- Tile size: ~64×48px (2 tiles wide, 1.5 tall), anchored above the
  avatar head.
- Phaser DOM element (`this.add.dom`) or an overlay HTML layer positioned
  via camera world-to-screen transform each frame.

### Mic/camera controls (always-on + mute)

- Two buttons in a HUD overlay (top-left or bottom): mic mute toggle,
  camera on/off toggle.
- Default: mic on, camera off (avoids surprise webcam on join).
- State persists in `localStorage` across sessions.

### Reconnect handling

On WS reconnect, the client gets a new `entity_id` (existing behavior —
fresh session). The old LiveKit participant lingers until LiveKit's
timeout. The new session triggers fresh `zone.enter`/`proximity.join`
events → new token → new LiveKit connection. Acceptable for MVP; the
stale participant times out.

### Bandwidth note

No cap on video tiles for MVP (above-avatar for all participants in the
room). If this becomes a problem, capping to N closest is a trivial
follow-up — the distance logic is already there.

## Infrastructure

### `docker/docker-compose.yml` — two new services

```yaml
livekit:
  image: livekit/livekit-server:v1.8
  command: ["--config", "/etc/livekit.yaml"]
  ports:
    - "7880:7880"    # signaling (WS)
    - "7881:7881"    # TCP/UDP media
    - "7882:7882"    # HTTP API
  volumes:
    - ./livekit/livekit.yaml:/etc/livekit.yaml:ro
  restart: unless-stopped

ext-av:
  build:
    context: ..
    dockerfile: docker/backend.Dockerfile
    target: ext-av
  depends_on:
    - nats
    - livekit
  environment:
    NATS_URL: "nats://nats:4222"
    EXTENSION_ID: "av"
    LIVEKIT_URL: "ws://livekit:7880"
    LIVEKIT_API_KEY: "${LIVEKIT_API_KEY}"
    LIVEKIT_API_SECRET: "${LIVEKIT_API_SECRET}"
    POCKETBASE_URL: "http://pocketbase:8090"
    PB_ADMIN_EMAIL: "admin@pixeleruv.local"
    PB_ADMIN_PASSWORD: "password123"
    MAP_ID: "test-map"
  restart: unless-stopped
```

### `docker/livekit/livekit.yaml`

```yaml
port: 7880
rtc:
  tcp_port: 7881
  port_range_start: 50000
  port_range_end: 60000
keys:
  APIkey: <secret>
```

### Frontend nginx

LiveKit signaling goes directly browser→LiveKit (not through
nginx/pusher). The frontend connects to `wss://<host>:7880` or
`ws://localhost:7880` in dev. The `url` field in `AvTokenFrame` tells the
client which endpoint to use.

### Other

- `docker/backend.Dockerfile` — add `ext-av` build target (same pattern
  as ext-walls/ext-props).
- `Makefile` — add `ext-av` to the build target list.
- `.env.example` — `LIVEKIT_API_KEY`, `LIVEKIT_API_SECRET` (generated via
  `livekit-server generate-keys`).

## Testing

### Worldsim (ginkgo, no Docker)

- Mobile zone follows avatar position each tick.
- `proximity.join` fires when two players come within 2 tiles.
- `proximity.leave` fires when they move apart.
- Clustering: 3 players in a line (A-B-C, A and C > 2 tiles apart but
  both near B) → one group with all three.
- Zone override: player in an `av_enabled` zone → no `proximity.join`
  even if near another player.
- `client_id` present in `zone.enter` payload for player entities, empty
  for base entities.

### ext-av (ginkgo, embedded NATS)

- On `zone.enter` for `av_enabled` zone → publishes
  `client.<id>.av_token` with `action:"join"` and a valid JWT.
- On `zone.exit` → publishes `action:"leave"`.
- Ignores `zone.enter` for non-A/V zones.
- On `proximity.join` → publishes token with `action:"join"` and correct
  `members` list.
- Ignores events with empty `client_id`.

### Integration (Docker stack)

- Two clients connect, walk near each other → both receive
  `AvTokenFrame`, both join the same LiveKit room.
- One walks away → both receive `action:"leave"`.
- One enters an `av_enabled` zone → proximity room left, zone room
  joined.

### Frontend

Manual testing (no browser automation in the project). Verify video
tiles appear above avatars, mute/camera toggles work, volume changes with
distance.

## Implementation plan

Ordered so each step is independently verifiable:

1. **Proto changes** → verify: `make proto` generates `AvTokenFrame` in
   Go + TS.
2. **Worldsim: `client_id` in zone events** → verify: ginkgo test
   asserts `client_id` present for player entities.
3. **Worldsim: mobile zones** → verify: ginkgo test asserts mobile zone
   follows avatar position, `zone.enter`/`zone.exit` fire for nearby
   players.
4. **Worldsim: proximity clustering + events** → verify: ginkgo test
   asserts `proximity.join`/`proximity.leave` for 2-player and 3-player
   clusters, zone override suppresses proximity.
5. **ext-av: token minting + NATS bridge** → verify: ginkgo test with
   embedded NATS asserts correct `av_token` payloads on zone/proximity
   events.
6. **Pusher: forward `client.*.av_token`** → verify: integration test
   asserts `AvTokenFrame` reaches the client.
7. **Frontend: AvClient + LiveKit SDK** → verify: manual — connect two
   browsers, walk near each other, confirm shared audio.
8. **Frontend: video tiles + spatial volume** → verify: manual — video
   appears above avatars, volume changes with distance.
9. **Frontend: mic/camera HUD controls** → verify: manual — mute/camera
   toggle works, state persists.
10. **Infrastructure: LiveKit + ext-av in docker-compose** → verify:
    `docker compose up` starts full stack, end-to-end A/V works.
11. **Map: mark a zone `av_enabled` in Tiled** → verify: entering zone
    joins zone room, proximity suppressed.

## Risks

**LiveKit room fragmentation on group changes.** When a player joins or
leaves a proximity group, the group ID changes (hash of members), so
every member gets a `proximity.leave` + `proximity.join` with a new room.
A 3-person group where one walks away causes the remaining 2 to
disconnect and reconnect to a new room. Brief audio drop. Acceptable for
MVP; LiveKit reconnects in ~200ms. Future fix: keep the room stable when
membership shrinks (only re-key when the group splits or merges).

**Mobile zone event noise.** Mobile zones fire `zone.enter`/`zone.exit`
for every player that crosses the 2-tile boundary. ext-av ignores
`prox-*` zones (filters by ID prefix) and only acts on
`proximity.join`/`proximity.leave`. The raw zone events are still
published — extensions like ext-demo will log them. Minor noise, not a
problem.

**Stale participants on reconnect.** WS reconnect mints a new
`entity_id` (existing behavior). The old LiveKit participant lingers
until timeout (~30s default). Other players see a "ghost" video tile
briefly. Acceptable for MVP.

**NATS message volume.** Proximity clustering runs at 4Hz, but events
are edge-triggered (only on membership change), so steady-state produces
zero messages. Mobile zone position updates are internal to worldsim (no
NATS). No volume concern.

**LiveKit WebRTC ports in Docker.** LiveKit needs UDP port range
50000-60000 for media. Docker Desktop on macOS handles this, but
production deployments need the range exposed. Documented in
docker-compose, not a blocker for dev.

**LiveKit SDK bundle size.** `livekit-client` adds ~200KB to the frontend
bundle. Lazy-load it (dynamic `import()`) so it only loads when A/V is
actually used. Minor optimization, can defer.
