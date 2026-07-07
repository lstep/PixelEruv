# Chat — Design

Date: 2026-07-07
Status: Approved (implementation in progress)

## Goal

Add text chat to PixelEruv with two channels: **global** (everyone online) and
**proximity** (players in the same proximity A/V group). Messages are ephemeral
(live-only, no persistence). Sender identity is server-stamped.

## Scope

In:
- Two channels: `global`, `proximity`. Active tab determines send target.
- Server-stamped `display_name` (PocketBase for logged-in users; `Guest <short>`
  for guests) and `timestamp`.
- Right-side DOM sidebar with channel tabs, message list, input row.
- Toggle button in the floating `TopMenu`.
- Ephemeral delivery via NATS; no DB writes, no history fetch on reconnect.

Out (flagged for future tasks):
- Message persistence / history fetch / scrollback pagination.
- Mute/ignore, profanity filtering, rate limiting.
- Markdown / rich text / @mentions / edit / delete / unread indicators.

## Architecture decision: worldsim-mediated (Option B)

Worldsim routes chat. It already owns every entity↔client map and computes
proximity groups each tick, so there is zero mapping problem and no new
service. Chat *routing* is plumbing (like replication routing, which already
lives in worldsim), not gameplay logic — so this does not violate the
"kernel stays clean of gameplay" principle.

Rejected alternatives:
- **Option A — `ext-chat` extension**: architecturally purer, but adds a whole
  service whose only job is looking up data worldsim already has, plus a
  proximity payload schema change. ~300-400 lines of Go for a thin wrapper.
- **Option C — pusher-stamped hybrid**: simplest for global chat (no NATS hop),
  but pusher would need a PocketBase dependency for display_name lookups and a
  worldsim request-reply for proximity membership — awkward, and puts
  communication logic in the pusher.

## Protocol

New messages in `proto/frames.proto`:

```proto
// Client → Server
message ChatFrame {
  string channel = 1;    // "global" or "proximity"
  string text = 2;        // max ~500 chars, server truncates (rune-safe)
  string traceparent = 3;
}

// Server → Client (one per delivered message, including echo to sender)
message ChatMessageFrame {
  string channel = 1;     // "global" or "proximity"
  string entity_id = 2;   // sender's entity id
  string display_name = 3; // server-stamped; "Guest <short>" for guests
  string text = 4;
  uint64 timestamp = 5;   // unix millis, server-stamped
}
```

`ChatFrame` joins `ClientFrame.payload`; `ChatMessageFrame` joins
`ServerFrame.payload`. Regenerate via `make proto`.

## Data flow

```
Browser ──WS──> Pusher ──NATS client.<id>.chat──> Worldsim
                                                  │
                                                  ├─ stamps display_name + timestamp
                                                  ├─ global:    publish chat.broadcast
                                                  └─ proximity: publish client.<recipientID>.chat_inbox per group member (incl. sender echo)
                                                  │
Pusher <──NATS chat.broadcast (1 sub, fan-out to all sessions)────┤
Pusher <──NATS client.<id>.chat_inbox (per-session sub)───────────┘
Pusher ──WS──> Browser (as ChatMessageFrame)
```

Worldsim marshals a full `ServerFrame` protobuf and publishes raw bytes —
matching the existing replication path. Pusher's chat handlers are pure
pass-through (`c.Write(m.Data)`), no chat-specific logic, no marshaling.

## Backend

### Worldsim (`worldsim.go`)

- Subscribe to `client.<id>.chat` in `NewSimulator` (alongside `.input` /
  `.action`). Handler calls `s.handleChat(ctx, clientID, chat)`.
- `handleChat`:
  1. Look up sender via `s.clients[clientID]`. Drop silently if missing.
  2. Read `display_name` from `e.DisplayName` (cached on entity, set at
     provision time — no per-message PocketBase lookup).
  3. Truncate `text` to 500 runes (count runes, slice on rune boundary).
  4. Build `ChatMessageFrame{channel, entity_id, display_name, text, timestamp}`.
  5. `global`: publish `ServerFrame` bytes to `chat.broadcast`.
  6. `proximity`: read `e.currentProximityGroup`; if empty, drop silently.
     Otherwise iterate `groupMembers[groupID]`, map each member entity_id →
     client_id via `s.entityIDToClient`, publish to
     `client.<recipientClientID>.chat_inbox` per member (including sender echo).
- New `Simulator.entityIDToClient map[string]string` — set in `provisionClient`,
  delete in `despawnClient`. O(1) lookup.
- New `Entity.DisplayName string` — set in `provisionClient`:
  - Logged-in (`sub != "" && sub != "dev"`): `user.DisplayName` from
    `FindOrCreateUser`; fall back to `entityID` if empty.
  - Guest: `"Guest " + entityID[len-4:]` (last 4 chars — stable within session,
    no PII).

### Pusher (`pusher.go`)

- `chat.broadcast` subscription in `New()` (one-time): on message, write
  `m.Data` raw to every session in `s.sessions`. ~6 lines.
- `client.<clientID>.chat_inbox` subscription per session (alongside `avSub`):
  write `m.Data` raw to the WS. Store as `sess.chatSub`, unsubscribe on
  disconnect. ~10 lines.

No PocketBase migration. `display_name` already exists on the `players`
collection and is read (not written) by `FindOrCreateUser`.

## Frontend

### `frontend/src/ui/ChatPanel.ts` (new, ~150 lines)

DOM sidebar, like `TopMenu` (no Phaser dependency):
- Fixed right edge, full height, ~320px wide, `z-index: 15` (below TopMenu's 20).
- Header: channel tabs (`Global` / `Nearby`) + close button.
- Messages list (`overflow-y:auto`); each message:
  `<div><b>{display_name}</b> <span class=time>{HH:MM}</span><br>{text}</div>`.
  Auto-scroll to bottom on new message unless user has scrolled up
  (`scrollTop + clientHeight >= scrollHeight - 50`).
- Input row: `<input type=text>` + send button. Enter sends; empty messages
  not sent.
- Hidden by default; `show()` / `hide()` toggle.
- Per-channel in-memory message arrays (lost on refresh — matches ephemeral
  contract).
- `addMessage({channel, entityId, displayName, text, timestamp})` — appends to
  the right channel's array; re-renders only if that tab is active.

### `TopMenu.ts`

Add a "Chat" pill button (between A/V controls and auth button) that toggles
`ChatPanel.show()` / `hide()`.

### `main.ts`

Create `ChatPanel` instance, store on `game.registry` as `"chatPanel"` (same
pattern as `topMenu`).

### `WsClient.ts`

- `sendChat(channel: "global" | "proximity", text: string)` — builds
  `ClientFrame{payload: {case: "chat", value: ChatFrame{channel, text}}}`,
  sends it. Fire-and-forget; echo confirms delivery.
- `onChatMessage` callback in connect options, dispatched on
  `ServerFrame.payload.case === "chatMessage"`. Decoded into
  `{channel, entityId, displayName, text, timestamp}`.

### `GameScene.ts`

In `create()`, grab `chatPanel` from registry, wire `WsClient.onChatMessage`
to `chatPanel.addMessage(msg)`. No special shutdown cleanup — `ChatPanel` is
page-level (survives scene restarts); the `WsClient` callback is per-connection.

### Channel UX

`Global` tab active by default. Switching to `Nearby` shows proximity messages
received so far. Sending while on `Nearby` → proximity; on `Global` → global.
Active tab determines send channel — no separate "send to" dropdown.

Guests see the same panel; their own messages echo back stamped
`"Guest <short>"` (server-stamped, consistent with what others see).

## Testing (ginkgo)

`backend/internal/worldsim/worldsim_chat_test.go`:
- `Describe("Chat routing")`:
  - `Context("global channel")`: two clients; one sends global; assert
    `chat.broadcast` receives one `ChatMessageFrame` with stamped name +
    truncated text. Verify 500-rune truncation on 600-rune input.
  - `Context("proximity channel")`: two clients in same group (pre-seed
    `currentProximityGroup` + `groupMembers`); sender emits proximity chat;
    assert each member's `client.<id>.chat_inbox` receives exactly one
    message, including sender echo. Solo player (no group) → no publishes.
  - `Context("guest display name")`: provision guest (`sub == ""`); assert
    `display_name == "Guest " + last4(entityID)`.
  - `Context("logged-in display name")`: provision with `UserRecord{DisplayName: "Alice"}`;
    assert chat carries `"Alice"`.
  - `Context("unknown client")`: publish `client.<bogus>.chat`; assert no
    panic, no publishes.

Reuses the in-process NATS + simulator + sync-subscriber harness from
`worldsim_proximity_test.go`.

No pusher integration test for chat — pusher's job is pure pass-through
(raw bytes → WS), already covered by existing WS plumbing tests.

## Implementation plan

1. **Proto** → `make proto`; verify Go + TS regenerate cleanly.
2. **Worldsim** `handleChat` + `entityIDToClient` + `DisplayName` on `Entity`
   → `go build ./...` compiles.
3. **Worldsim tests** (red-first) → `go test ./internal/worldsim/ -v -run Chat`
   passes.
4. **Pusher** `chat.broadcast` + per-session `chat_inbox` subs → `go build`;
   manual smoke test (two browsers, global + proximity delivery).
5. **Frontend** `WsClient.sendChat` + `onChatMessage` → `npm run build`.
6. **Frontend** `ChatPanel.ts` + `TopMenu` button + `main.ts`/`GameScene`
   wiring → `npm run build`; manual smoke (open panel, send global, see echo;
   walk two avatars together, switch to Nearby, send, see both receive).
7. **DASHBOARD.md** — mark Chat `[x]`, add architectural decision row, bump
   session to 14.
8. **Commit** locally (not pushed).
