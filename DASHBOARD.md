# Dashboard

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
