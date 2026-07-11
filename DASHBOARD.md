# Dashboard

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

- Extension options schema declared in registration, worldsim creates PB collections. Hot-reload via PB hooks + NATS.

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
