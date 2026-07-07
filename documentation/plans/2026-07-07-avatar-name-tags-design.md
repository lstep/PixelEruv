# Avatar Name Tags — Design

Date: 2026-07-07
Status: Approved (implementation pending)

## Goal

Display each player's display name as a pixel-art text tag above their avatar,
consistent with the name shown in chat. Names are server-authoritative:
the client uploads a name via `SetNameFrame`, worldsim updates the entity and
replicates the change to all clients.

## Scope

In:
- New `DisplayName` component (ID=4) in the replication protocol.
- New `SetNameFrame` (client→server) for uploading a name.
- Worldsim: validate, update `Entity.DisplayName`, mark dirty for replication,
  persist to PocketBase for logged-in users.
- Frontend: Phaser `BitmapText` name tags above avatars, styled with a
  Press Start 2P bitmap font and dark drop shadow.
- TopMenu: "Save" button sends `SetNameFrame`; localStorage caches the input
  for pre-fill only (no auto-send on boot).

Out (flagged for future tasks):
- Profile/configuration UI for editing display name and other settings.
- Speech bubble above avatar for in-character dialogue (separate from chat
  panel).
- Profanity filtering, rate limiting, retroactive chat message renaming.
- Unicode/CJK font support.

## Source of truth

`Entity.DisplayName` is the single source, used by both chat (`handleChat`
reads it directly) and name tags (replicated as a component). The client-side
`username.ts` (localStorage) becomes an input cache only — it pre-fills the
TopMenu input field but does not determine the displayed name.

## Transport

`SetNameFrame` joins the `ClientFrame` oneof → pusher forwards to NATS
`client.<id>.set_name` → worldsim `handleSetName`. Same pattern as
`ChatFrame`.

## Persistence

- **Logged-in users** (`sub != "" && sub != "dev"`): worldsim calls a new
  `UserStore.UpdateDisplayName(sub, name)` after updating the entity. The
  name is restored from PocketBase `players.display_name` at provision time
  on reconnect.
- **Guests** (`sub == ""` or `"dev"`): session-only. Name is lost on
  reconnect; the entity re-provisions as `Guest <short>`.

## Replication

New component in `components.proto`:

```proto
// Component ID 4 — DisplayName
// Player avatar display name, shown as a name tag above the sprite and
// stamped on chat messages. Only present on player avatars (not props or
// decorations). Set at provision time (PocketBase for logged-in, "Guest
// <last4>" for guests) and updated via SetNameFrame.
message DisplayName {
  string name = 1;
}
```

- **Spawn**: `DisplayName` component included in `SpawnEntity.components`
  alongside Position/Appearance. The frontend creates the `BitmapText` tag
  at spawn time if the component is present.
- **Update**: `dirtyName` flag on `Entity` (mirrors `dirtyPosition`/
  `dirtyState`). On `SetNameFrame`, worldsim sets `DisplayName` and
  `dirtyName = true`. The next replication tick sends `UpdateComponent`
  with `componentId=4` to all clients that have already received the spawn.
- Props and decorations never include this component (same as how they
  omit `EntityState` unless they have state).

## Validation

Server-side, in `handleSetName`:
- **Max 20 characters.** Truncate rune-safe (count runes, slice on rune
  boundary), same pattern as chat's 500-rune truncation.
- **ASCII printable only (32–126).** Strip any character outside that range.
  Matches the bitmap font's glyph set exactly; prevents layout breakage from
  control chars, zero-width chars, RTL overrides, emoji.
- **No profanity filtering.** Consistent with chat. Flagged for future.
- **No rate limiting.** Consistent with the rest of the MVP. Flagged for
  future.

## Frontend rendering

### Bitmap font

Press Start 2P (OFL, Google Fonts), rendered as a 128×48px PNG atlas with
95 ASCII characters (32–126) in a 16×6 grid, plus an AngelCode BMFont XML
descriptor. Files in `frontend/public/fonts/pressstart2p.{png,xml}`.

Loaded in `preload()`:
```ts
this.load.bitmapFont("pressstart2p", "fonts/pressstart2p.png", "fonts/pressstart2p.xml");
```

### Name tag lifecycle

- **Created at spawn time** in `handleReplication` when a new avatar appears
  with a `DisplayName` component. `this.add.bitmapText(x, y, "pressstart2p",
  name, 8)`.
- **Position**: `(sprite.x, sprite.y - 52)`, `setOrigin(0.5, 1)` (bottom-
  center anchor, so text grows upward above the head). The sprite origin is
  (0.5, 0.75) on a 64px frame; the head is ~48px above the feet anchor, so
  52px clears the top of the frame with a small gap.
- **Depth**: `setDepth(sprite.depth + 0.01)` — just above the sprite in the
  Y-sort so it doesn't hide behind tall decorations.
- **Style**: white text, dark drop shadow `(1, 1, 0x000000, 1)` for
  readability on both light and dark backgrounds.
- **Local player**: `setVisible(entityId !== this.myEntityId)` — hidden for
  the local player's own avatar. Other players see it.
- **Update on `UpdateComponent` (componentId=4)**: `nameTag.setText(newName)`.
  If the avatar somehow doesn't have a tag yet (edge case), create it then.
- **Repositioned each frame** in `update()`: `nameTag.x = sprite.x;
  nameTag.y = sprite.y - 52; nameTag.setDepth(sprite.depth + 0.01)`. The
  existing `update()` loop already iterates all avatars for animation/depth,
  so this adds ~3 lines per avatar.

### TopMenu integration

- The existing "Your name" Save button now also calls `ws.setName(name)`.
- `username.ts` (localStorage) stays as-is — it caches the input for pre-fill
  on next page load. It does **not** auto-send on boot. The user must click
  Save each session to apply their name.
- `WsClient` gains a `setName(name: string)` method that builds and sends a
  `SetNameFrame`.

### Chat interaction

`handleChat` reads `e.DisplayName` directly, so chat automatically uses the
new name after a `SetNameFrame`. Old chat messages keep their original
stamped name — `ChatMessageFrame.display_name` is a snapshot at send time,
immutable. This matches how Discord/Slack work (name changes don't
retroactively rename old messages).

## Testing (worldsim unit tests)

`worldsim_nametag_test.go`:

1. **`TestSetName_UpdatesEntityAndReplicates`**: provision a guest, send
   `SetNameFrame{name: "Alice"}`, assert `Entity.DisplayName == "Alice"` and
   `dirtyName == true`. After a tick, assert the replication batch for
   another client contains an `UpdateComponent` with `componentId=4` and
   the name "Alice".

2. **`TestSetName_SanitizesInput`**: send name with control chars + over-
   length, assert stripped to ASCII printable and truncated to 20 chars.

3. **`TestSetName_GuestNotPersisted`**: provision a guest, set name, despawn,
   re-provision with same clientID, assert `DisplayName` is the default
   `Guest <short>`, not the previously set name.

4. **`TestReplication_SpawnIncludesDisplayName`**: provision two guests, set
   one's name, assert the other client's `SpawnEntity` for that entity
   includes a `DisplayName` component with the right name.

Skip: logged-in PocketBase persistence test (no mock infra; the
`UserStore.UpdateDisplayName` method is a thin HTTP PUT wrapper, same shape
as the existing untested `SavePosition`). Verified manually via smoke test.

## Implementation plan

1. **Font asset**: generate `pressstart2p.{png,xml}` in
   `frontend/public/fonts/` (already done during design).
2. **Proto**: add `DisplayName` component to `components.proto`, add
   `SetNameFrame` to `frames.proto`, regenerate via `make proto`.
3. **Worldsim**: `handleSetName` + `dirtyName` flag + `compDisplayName`
   constant + include `DisplayName` in spawn/update replication +
   `UserStore.UpdateDisplayName` for logged-in users.
4. **Worldsim tests**: 4 unit tests as above.
5. **Pusher**: forward `ClientFrame_SetName` to `client.<id>.set_name` on
   NATS (same pattern as chat).
6. **Frontend**: load bitmap font in `preload()`, create/update/position
   `BitmapText` name tags in `handleReplication` + `update()`, add
   `WsClient.setName()`, wire TopMenu Save button to `ws.setName()`.
7. **DASHBOARD.md**: mark name tags done, add architectural decision row,
   bump session.
8. **Commit** locally (not pushed).
