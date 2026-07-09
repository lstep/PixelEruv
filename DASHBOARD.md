# Dashboard

## Current Focus: Remote Audio Not Working (PAUSED)

**Branch:** `fix/av-audio-autoplay`
**PR:** https://github.com/lstep/PixelEruv/pull/16
**Status:** Video works, audio does not. User paused to come back later.

## What's Been Done (6 commits on the branch)

1. **`12bd048` — Fix remote audio playback blocked by browser autoplay policy**
   - Added `AvAudioBlockedHandler` + `onAudioBlocked` property
   - Added `startAudio()` method
   - Modified `setMicMuted` to call `startAudio()` on unmute
   - Modified `handleTokenFrame` to skip re-joining same room + debounce "leave" events (1.5s)
   - Added `RoomEvent.AudioPlaybackStatusChanged` listener
   - Added "Enable Audio" button to TopMenu

2. **`5bc971a` — Enable LiveKit use_external_ip for remote WebRTC access**
   - `livekit.yaml`: `use_external_ip: true` (was `false`)
   - `enable_loopback_candidate: true` (kept for local dev)
   - Updated docker-compose comments about UDP port range 50000-50020

3. **`7050aaf` — Document-level click listener to unlock audio after room connect**
   - (Superseded by silent-audio approach in next commit)

4. **`d287d48` — Unlock audio on first page click via silent audio element**
   - Constructor installs one-time `document.click` listener
   - Plays a silent 1-byte WAV to unlock Safari autoplay
   - Removed the document-level click listener from `connect()`

5. **`1efc8ae` — Add mic/camera device selection in the menu dropdown**
   - `AvClient.getDevices(kind)` — enumerates via `navigator.mediaDevices.enumerateDevices`
   - `AvClient.switchDevice(kind, deviceId)` — calls `room.switchActiveDevice`
   - TopMenu dropdown has mic + camera `<select>` dropdowns
   - Lists refresh each time the dropdown opens

6. **`33e0e27` — Persist selected device IDs across room reconnects**
   - Added `selectedMicId` / `selectedCamId` fields
   - `switchDevice` stores the ID even when no room is connected
   - `connect()` passes them to `Room` constructor via `audioCaptureDefaults` / `videoCaptureDefaults`

## What Works (confirmed from server logs)

- LiveKit signaling connects: `connected to Livekit Server edition: 0, version: 1.13.2`
- WebRTC ICE succeeds: both participants active with UDP, `connectionType: "udp"`
- Public IP advertised: `nodeIP: "195.154.221.137"` (use_external_ip fix works)
- Both participants publish audio + video tracks
- Video renders correctly on both sides
- ICE candidate pairs are established (srflx candidates, selected pairs)

## What Doesn't Work

- **No audio heard** on either side, despite tracks being published and subscribed

## Uninvestigated Leads (pick up here)

### 1. Spatial volume might be 0
`GameScene.ts` line 909: `maxDist = 10` tiles. Volume = `1 - dist/10`.
If players are >10 tiles apart when audio first subscribes, volume is 0.
**Check:** Add a `console.log` in `setParticipantVolume` to see what volume is being set.
**Quick test:** Try setting volume to 1.0 unconditionally to rule this out.

### 2. `audioTrack.setVolume` may not exist or may not work on Safari
`AvClient.ts` line 391-393: `audioTrack.setVolume(volume)` — this is a LiveKit SDK method
on `RemoteAudioTrack`. Safari may not support `setVolume` on `WebAudio` audio elements.
**Check:** Log whether `audioTrack.setVolume` is defined. Log the audio track type.
**Quick test:** Comment out the `setVolume` call entirely and see if audio works at full volume.

### 3. Audio track may be attached but not playing
LiveKit attaches remote audio to a hidden `<audio>` element. Safari may not auto-play
these even after the silent-audio unlock (the unlock may only apply to the specific
element that was played, not all future audio elements).
**Check:** In Safari Web Inspector, look for `<audio>` elements in the DOM after joining.
Check if they have `paused: true` or `playNotAllowedError`.
**Possible fix:** Listen for `RoomEvent.TrackSubscribed` and manually call `.play()` on
the audio element within a user gesture, or use `room.startAudio()` after each track
subscription (not just once).

### 4. `audio/red` codec (RED redundancy) may not decode on Safari
Server logs show `"mimeType": "audio/red"` for published audio tracks. RED is an
Opus redundancy codec. Safari may not support decoding `audio/red` — it might need
the plain `audio/opus` codec instead.
**Check:** Look at the SDP negotiation in the LiveKit logs — does Safari accept `audio/red`?
**Possible fix:** Configure LiveKit or the LiveKit client SDK to prefer `audio/opus` over
`audio/red`, or disable RED in the publish options.

### 5. `Room.startAudio()` may need to be called AFTER tracks are subscribed
The silent-audio unlock plays a data: URI WAV on a separate `<audio>` element.
Safari's autoplay policy may scope the unlock to that specific element, not to
audio elements created later by LiveKit. `room.startAudio()` is LiveKit's method
to resume all audio elements — but it's only called in `setMicMuted`/`setCameraEnabled`
(which fire before the room exists) and in the constructor's one-time click (which
also fires before the room exists).
**Check:** Call `room.startAudio()` inside the `TrackSubscribed` event handler and log
the result. Also check `room.canPlaybackAudio` after track subscription.

### 6. Mic device selection
The user suspected the wrong mic device. Device selectors were added, but the user
reports it still doesn't work. Could still be a device issue on the *receiving* side
(speaker/output device) rather than the mic.

## Files Changed

| File | Changes |
|------|---------|
| `frontend/src/net/AvClient.ts` | Audio unlock, device selection, same-room skip, leave debounce, startAudio |
| `frontend/src/ui/TopMenu.ts` | Enable Audio button, mic/camera device selectors in dropdown |
| `docker/livekit.yaml` | `use_external_ip: true` |
| `docker/docker-compose.yml` | Comments about UDP ports + node IP |
| `docker/dist/docker-compose.yml` | Same comments |

## How to Resume

```bash
git checkout fix/av-audio-autoplay
# Rebuild frontend after changes:
docker compose up -d --build frontend
# Check LiveKit server logs:
docker compose logs -f livekit
```

Start with lead #2 (setVolume) and #4 (audio/red codec) — they're the most likely
culprits given that ICE, signaling, and track publishing all work.
