# A/V Meeting Recording

Date: 2026-07-18
Branch: `feat/recording` (to be created)

## Goal

Admin-hosted recording of A/V meetings in LiveKit rooms. Two mutually
exclusive targets chosen at start time:

- **MP4** — local file on disk (v1; S3 later).
- **YouTube** — live RTMP stream to a configured YouTube channel.

One recording per room at a time. Host (admin) initiates and stops.
Proximity rooms are out of scope for v1; recording targets zone A/V rooms
only (the rooms `ext-av` already mints tokens for).

## Architecture

New extension `ext-rec`, separate from `ext-av`. `ext-av` owns token
minting (stateless, per-event). Recording has long-lived state (active
Egress IDs, lifecycle, audit, consent), its own env vars (LiveKit creds,
`RECORDINGS_DIR`, `YOUTUBE_RTMP_URL`, `YOUTUBE_STREAM_KEY`), and its own
failure modes. Folding it into `ext-av` would bloat a process that is
currently ~490 lines of pure token minting. A separate extension matches
the existing `ext-demo` / `ext-walls` / `ext-props` / `ext-av` pattern.

```
Browser ──WS──> Pusher ──NATS──> ext-rec ──LiveKit API──> Egress ──┬──> RTMP ──> YouTube (live)
                              │                                   └──> local MP4
                              │
                       worldsim.entity_info (admin check)
                       audit.Emit (start/stop)
                       PocketBase `recordings` collection
```

Egress is the official LiveKit recording service (`livekit/egress`),
added to the Docker stack. A single Room Composite Egress call produces
either a local MP4 or an RTMP stream, depending on the requested target.
One Egress per recording.

## Host authorization

Host = admin for v1. Reuses the existing `is_admin` field on entities
(set from PocketBase `players.is_admin`, replicated via
`DisplayName.is_admin`). No new permission system.

There is no existing `worldsim.entity_info` request-reply. Add one,
mirroring `worldsim.zones.get` / `worldsim.stats.get`:

- Subject: `worldsim.entity_info`
- Request: `{entity_id}`
- Reply: `{entity_id, is_admin, status, display_name, map_id}`

ext-rec calls it on each `recording.start` and rejects non-admins.
Future extensions will want the same lookup.

## Protocol

### New proto frames (`proto/frames.proto`)

```proto
message RecordingRequestFrame {
  string action = 1;    // "start" | "stop"
  string room = 2;      // LiveKit room name (zone-<slug>)
  string target = 3;    // "mp4" | "youtube"
}

message RecordingStateFrame {
  string room = 1;
  string status = 2;    // "active" | "stopped" | "error"
  string target = 3;    // "mp4" | "youtube"
  string egress_id = 4;
  string error = 5;
}

message RecordingActiveFrame {
  string room = 1;
  bool active = 2;
  string target = 3;    // "mp4" | "youtube"
}
```

- `ClientFrame.recording` → `RecordingRequestFrame`
- `ServerFrame.recording_state` → `RecordingStateFrame` (to host)
- `ServerFrame.recording_active` → `RecordingActiveFrame` (to all
  participants in the room)

Run `make proto` to regenerate Go + TypeScript.

### NATS subjects

| Subject | Direction | Payload |
|---|---|---|
| `recording.start` | pusher → ext-rec | `{client_id, entity_id, room, target}` |
| `recording.stop` | pusher → ext-rec | `{client_id, entity_id, room}` |
| `client.<id>.recording_state` | ext-rec → pusher → host | `RecordingStateFrame` |
| `client.<id>.recording_active` | ext-rec → pusher → each participant | `RecordingActiveFrame` |
| `worldsim.entity_info` | ext-rec → worldsim (req-rep) | see above |

Pusher stays pure passthrough. ext-rec already needs the participant
list for the `recordings.participants` snapshot, so it publishes
per-client `recording_active` rather than asking the pusher to track
client→room mappings.

## ext-rec

### State

```go
type activeRec struct {
    EgressID     string
    Room         string
    Target       string   // "mp4" | "youtube"
    StartedBy    string   // entity_id
    StartedAt    time.Time
    Participants []string  // snapshot at start
}
type activeRecs map[string]*activeRec  // keyed by room name
```

One recording per room max. Second `start` for an active room → error
reply.

### Start flow

1. Receive `recording.start`.
2. `nc.Request("worldsim.entity_info", {entity_id}, 2s)` → reject if
   `!is_admin`.
3. Check `activeRecs[room]` — reject if present.
4. Build Egress request via `lksdk.NewRoomServiceClient(...).StartRoomCompositeEgress`:
   - `target == "mp4"`: `EncodedFileOutput{FileType: "mp4", Filepath: "recordings/{room}-{time}.mp4"}` (local disk; `RECORDINGS_DIR` env, default `./recordings`).
   - `target == "youtube"`: `StreamOutput{Protocol: RTMP, Urls: [YOUTUBE_RTMP_URL + "/" + YOUTUBE_STREAM_KEY]}`.
   - Layout: `"speaker"` (LiveKit built-in).
5. Store EgressID in `activeRecs[room]`.
6. Insert row into PocketBase `recordings` collection.
7. `audit.Emit("recording.start", severity_info, actor={entity_id}, details={room, target, egress_id}, traceID)`.
8. Publish `client.<id>.recording_state` (status=active) to host.
9. For each participant in the room, publish
   `client.<participant_client_id>.recording_active` (active=true).

### Stop flow

v1: host-initiated stop only. Auto-stop on empty room is v2.

1. Receive `recording.stop` from the host.
2. `client.StopEgress(ctx, egress_id)`.
3. Update PocketBase row: `end_time = now`, `status = "completed"`.
4. `audit.Emit("recording.stop", ...)`.
5. Publish `recording_state` (status=stopped) to host.
6. Publish `recording_active` (active=false) to each participant.

### Env vars

| Var | Default | Purpose |
|---|---|---|
| `NATS_URL` | `nats://localhost:4222` | NATS connect |
| `EXTENSION_ID` | `rec` | Extension registration id |
| `LIVEKIT_URL` | `ws://localhost:7880` | LiveKit API |
| `LIVEKIT_API_KEY` | (required) | Same as ext-av |
| `LIVEKIT_API_SECRET` | (required) | Same as ext-av |
| `RECORDINGS_DIR` | `./recordings` | Local MP4 output |
| `YOUTUBE_RTMP_URL` | (optional) | RTMP base URL |
| `YOUTUBE_STREAM_KEY` | (optional) | YouTube stream key |

YouTube env vars are only required when `target=youtube` is used; ext-rec
still starts without them so MP4 recording works in dev.

## PocketBase `recordings` collection

New migration
`backend/migrations/1753700000_create_recordings.go`, modeled on
`1752900000_create_bans.go` (server-side access only, no public API
rules).

| Field | Type | Notes |
|---|---|---|
| `meeting_id` | text | UUID generated by ext-rec |
| `room` | text | LiveKit room name |
| `zone_id` | text, optional | if zone room, else empty |
| `map_id` | text | from `entity_info` |
| `target` | text | "mp4" \| "youtube" |
| `egress_id` | text | LiveKit Egress ID |
| `started_by` | text | entity_id (admin) |
| `participants` | json | array of entity_ids at start |
| `start_time` | date | |
| `end_time` | date, optional | |
| `status` | text | "active" \| "completed" \| "error" |
| `file_url` | text, optional | local path or YouTube URL |
| `consent_state` | json | `{notified_participants: [...], consented: [...]}` — v1 records who was in the room at start |

Frontend listing of recordings per zone/meeting is later work.

## Consent UX (v1)

Admin-triggered recording, like Zoom host recording. No opt-in dialog in
v1. When `recording_active` flips to true, all clients in that room
show:

- A persistent "REC ●" DOM pill near the VideoBar.
- A one-time toast: "This meeting is being recorded to {target}".

The participant list is captured in `consent_state` for audit. v2 can
add per-participant consent gating.

## Pusher deltas

### Outbound (browser → NATS)

Add a `ClientFrame_Recording` case to the read-loop switch in
`backend/internal/pusher/pusher.go` (~L544+), mirroring `SetStatus`:

```go
case *pb.ClientFrame_Recording:
    subject := "recording." + p.Recording.Action  // "recording.start" | "recording.stop"
    payload := {client_id, entity_id, room: p.Recording.Room, target: p.Recording.Target}
    s.nc.PublishMsg(&nats.Msg{Subject: subject, Data: payloadBytes, ...})
```

Pusher adds `client_id` and `entity_id` from the session.

### Inbound (NATS → browser)

Add two subscriptions next to the `av_token` sub (~L337):

- `client.<id>.recording_state` → wrap in `ServerFrame_RecordingState`, forward to host.
- `client.<id>.recording_active` → wrap in `ServerFrame_RecordingActive`, forward to participant.

No new pusher state. Pusher stays pure passthrough.

## Worldsim delta

Add `worldsim.entity_info` request-reply handler near
`worldsim.zones.get` (~L787). Implementation ~30 lines:

```go
s.nc.Subscribe("worldsim.entity_info", func(msg *nats.Msg) {
    var req struct{ EntityID string `json:"entity_id"` }
    json.Unmarshal(msg.Data, &req)
    s.mu.RLock()
    e, ok := s.entities[req.EntityID]
    s.mu.RUnlock()
    if !ok { msg.Respond(nil); return }
    resp, _ := json.Marshal(struct {
        EntityID    string `json:"entity_id"`
        IsAdmin     bool   `json:"is_admin"`
        Status      uint32 `json:"status"`
        DisplayName string `json:"display_name"`
        MapID       string `json:"map_id"`
    }{e.ID, e.IsAdmin, e.Status, e.DisplayName, e.MapID})
    msg.Respond(resp)
})
```

Unit test next to `worldsim_status_test.go` covering: admin returns
`is_admin:true`, unknown entity returns empty, fields populate
correctly.

## Frontend

### TopMenu record button

Add a "Record" pill button to `frontend/src/ui/TopMenu.ts`, visible
only when `ws.isAdmin()` and an A/V room is joined. Two-state: idle →
"Record"; active → "Stop REC" (red dot prefix). Click when idle opens
a tiny inline menu with two choices: "Record to MP4" / "Stream to
YouTube".

Wire-up:

- `TopMenu.setRecordingStateHandler({onStart, onStop, onState})`.
- `onStart(target)` → `ws.sendRecording({action:"start", room: avClient.currentRoomName(), target})`.
- `onStop()` → `ws.sendRecording({action:"stop", room: avClient.currentRoomName()})`.
- `onState(state)` updates button label + toggles a red dot.

`AvClient.currentRoomName()` is a tiny new getter exposing
`this.currentRoom` (already tracked at `AvClient.ts` L41).

### GameScene REC indicator + toast

In `frontend/src/scenes/GameScene.ts`, add a `recordingActiveByRoom:
Map<string, boolean>` (mirrors `isAdminByEntity`). On
`onRecordingActive` from WsClient:

- If active and not previously shown: display a one-time toast "This
  meeting is being recorded" (DOM div with auto-fade).
- Render a small "REC ●" DOM pill near the VideoBar while active. DOM
  element, not Phaser world-space, so no zoom counter-scaling needed.
- Hide on inactive.

### WsClient deltas (`frontend/src/net/WsClient.ts`)

- `sendRecording(req: {action, room, target})` → marshals
  `ClientFrame_Recording`.
- `onRecordingState` and `onRecordingActive` handlers, dispatched from
  the `recordingState` / `recordingActive` ServerFrame cases (mirror
  the `avToken` case at L280).

## Docker stack

Add `livekit/egress` service to `docker/docker-compose.yml` and
`docker/dist/docker-compose.yml`. Egress needs `LIVEKIT_API_KEY`,
`LIVEKIT_API_SECRET`, `LIVEKIT_URL`, and a volume mount for
`./recordings`. Add `ext-rec` service with the same LiveKit creds plus
`RECORDINGS_DIR`, `YOUTUBE_RTMP_URL`, `YOUTUBE_STREAM_KEY` (optional).

## Implementation order

1. **Proto frames** → verify: `make proto` then `go build ./...` and
   `npm run typecheck` pass.
2. **Worldsim `worldsim.entity_info` handler + test** → verify:
   `cd backend && go test ./internal/worldsim/ -v -run EntityInfo`.
3. **ext-rec skeleton** (NATS connect, register, no-op handlers) →
   verify: starts, registers with worldsim, heartbeats.
4. **PocketBase `recordings` migration** → verify: `make up` then
   `recordings` collection visible in PB admin GUI.
5. **ext-rec start/stop with MP4 Egress** → verify: integration test
   starts a LiveKit room, calls start, asserts file appears in
   `RECORDINGS_DIR`, asserts PB row.
6. **ext-rec YouTube RTMP path** → verify: same test with
   `target=youtube` against a mock RTMP sink (or skip in CI, manual
   verify against a real YouTube test stream).
7. **Pusher inbound/outbound recording frames** → verify: integration
   test sends `ClientFrame_Recording`, asserts NATS subject published.
8. **Frontend TopMenu button + WsClient handlers + GameScene REC
   indicator** → verify: manual `make up` + browser test as admin.

## Risks / considerations

- **Proximity rooms unrecorded in v1.** Recording only zone A/V rooms.
  Proximity rooms are ad-hoc and short-lived; recording them is noise.
- **One recording per room.** Concurrent recording attempts by two
  admins race; ext-rec rejects the second. No "take over" flow in v1.
- **No auto-stop on empty room in v1.** If the host forgets to stop,
  the recording runs until the host stops it or ext-rec restarts. v2
  should poll `ListParticipants` and auto-stop at N participants == 0
  for M seconds.
- **YouTube target is one global channel in v1.** Env vars
  `YOUTUBE_RTMP_URL` + `YOUTUBE_STREAM_KEY`. Per-map / per-zone
  YouTube targets are v2 (PocketBase `recording_targets` collection).
- **Consent is indicator + toast + audit capture, no opt-in dialog.**
  Admin-triggered, like Zoom host recording. Per-participant consent
  gating is v2.
- **Egress is a new stateful service in the stack.** It needs CPU for
  transcoding and disk for MP4 output. Resource limits should be set
  in compose. Restart policy `unless-stopped`.
- **ext-rec restart loses `activeRecs` state.** A recording started
  before the restart keeps running in Egress but ext-rec forgets it.
  v2 should rehydrate by calling `ListEgress` on startup. v1 mitigation:
  on startup, stop any egress we don't recognize (orphan cleanup) —
  decide during implementation.
- **Local MP4 storage grows unbounded.** No retention policy in v1.
  Operator must clean `RECORDINGS_DIR` manually. v2 should add a
  retention sweep.
