# Roadmap (future features)

## MMORPG-Scale World Engine + Procedural World Generation

**Design doc:** `documentation/plans/2026-07-23-mmorpg-scale-and-worldgen-design.md` (762 lines, research-backed, with file:line citations to current architecture).

Two coupled initiatives: (A) scale the engine to huge maps with hundreds of concurrent players, and (B) procedurally generate those worlds (terrain, biomes, settlements, NPCs, history). Research covers AOI algorithms (nine-grid), snapshot interpolation + delta compression (Gaffer On Games, Overwatch), chunk streaming (Minecraft, Roblox, Tiled infinite maps), Eve Online sharding, and Dwarf Fortress world generation (phases, history simulation, emergent narrative, personality model).

### Recommended implementation order

| Order | Phase | Status | Why this order |
|---|---|---|---|
| 1st | **Phase 2: AOI grid** | In progress | Highest-impact performance change. Zero prerequisites — works on existing finite maps. Converts O(N*M) replication to O(N_local*M). Foundation that makes large maps viable. |
| 2nd | Phase 1: Infinite maps | Not started | Small change, unblocks worldgen output. Quick once AOI is in place. |
| 3rd | Phase 4: Worldgen terrain + biomes | Not started | Generate huge maps AND populate them without hitting the replication ceiling. The visible deliverable. |
| 4th | Phase 3: Delta compression | Not started | Second-order bandwidth optimization. Protocol change — ship when there's time for migration. |
| 5th | Phase 5: Chunk streaming | Not started | Needed only when generated maps are large enough that full-load is too slow. |
| 6th | Phase 6: NPCs, history, quests | Not started | Ambitious story layer. Builds on worldgen + chunk streaming. |
| 7th | Phase 7: Sharding | Not started | Only if exceeding hundreds of concurrent players. Explicitly deferred. |

**Key decision:** Phase 2 (AOI) before Phase 1 (infinite maps) because AOI has zero prerequisites and addresses the actual scaling bottleneck. Building worldgen before AOI would produce maps that choke at ~50 concurrent players.

## Service Worker push notifications for player pings

The current player ping feature (see `documentation/plans/2026-07-22-player-ping-design.md`) plays a notification sound + browser Notification when a player is pinged. This works for visible tabs and recently-backgrounded tabs, but Chrome freezes deeply backgrounded tabs after ~5 minutes — in that state no JS runs, so neither the WebSocket handler nor the audio/Notification code executes. The ping is silently lost.

A Service Worker with Push API would deliver pings even to frozen tabs, since push events wake the service worker independently of page JS execution.

### Sketch

- Register a service worker (`frontend/sw.js`) scoped to the frontend.
- On connect, the client subscribes to the Push API (VAPID) and sends the subscription endpoint to worldsim via a new `PushSubscriptionFrame` (client→server).
- Worldsim stores the subscription per-client (in-memory, cleared on disconnect — same lifecycle as the entity).
- When a ping is delivered (worldsim `subscribeClientPing`), in addition to publishing `PlayerPingFrame` to `client.<id>.ping_inbox`, worldsim sends a push notification to the target's subscription endpoint via the Web Push protocol (VAPID-signed JWT + encrypted payload).
- The service worker's `push` event handler shows a Notification with the sender's display name. Clicking the notification focuses the tab.
- VAPID key pair generated once and stored in env vars (`VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`, `VAPID_SUBJECT`).

### Open questions

- Push delivery requires a server-side Web Push library (e.g. `github.com/webpush-go/webpush-go` for Go). Adds a dependency to worldsim.
- VAPID keys need to be generated and managed. Store in env vars or PocketBase settings?
- Should push notifications be opt-in (prompt for permission separately from the in-page Notification permission)?
- Battery/CPU cost of waking a frozen tab vs. just showing a system notification without focusing.
- Fallback for browsers without Push API support (Safari iOS has partial support).

## AI meeting transcription

Plug an AI service to read a recording's MP4 and produce a transcript, displayed alongside the video on the recordings management page (`/admin/recordings`).

### Sketch

- Background worker picks up completed recordings (status=`completed`, no transcript yet).
- Worker calls a speech-to-text API (e.g. OpenAI Whisper) on the MP4 file from the `recordings` volume.
- Transcript stored in a new `transcript` field on the `recordings` PocketBase collection (JSON: segments with start/end timestamps + text).
- Recordings page renders the transcript next to the `<video>` element in the modal player, with click-to-seek on each segment.
- Transcript is downloadable (plain text + JSON + SRT formats) from the recordings page.
- Re-transcribe button on the recordings row (admin only) for when the model improves or the original run failed.

### Open questions

- Hosted API (Whisper, Deepgram) vs. self-hosted (faster-whisper on GPU)?
- Speaker diarization (who said what) — requires participant audio tracks, not just the room composite mix. May need a separate Egress with per-track outputs.
- Cost/latency budget for long meetings.
- Language detection vs. per-meeting language selection.
