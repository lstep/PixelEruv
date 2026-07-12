# Admin Mode — Design

**Date:** 2026-07-11
**Status:** Design (not yet implemented)

## Goal

Build an admin mode infrastructure that lets authenticated admin users see
extra information about other players — starting with guest IP addresses in
the name tag pillbox, and extensible to future admin features (hidden entity
info, moderation tools, server stats, etc.).

The core requirement: **admin-only data must never reach non-admin clients.**
This rules out embedding admin data in the replication stream (which broadcasts
to all clients in range). Instead, a dedicated admin-only NATS channel routes
admin data selectively.

## Context

- Guest entities are in-memory only (no PocketBase record). Their IP is
  available at the pusher (TCP peer) and already threaded to worldsim via
  `AuthResultFrame.ip` (implemented in `feat/extension-options` branch).
- Logged-in players already have their IP persisted in the `players`
  collection (`ip` + `last_seen_at` fields, same branch).
- The replication system broadcasts entity components to all clients in range
  via `client.<id>.replication`. This is unsuitable for admin-only data.
- Worldsim already does a PocketBase lookup in `provisionClient` for logged-in
  users — adding an `is_admin` read there is trivial.

## Design

### 1. Admin identification: PocketBase `is_admin` flag

Add `is_admin` (bool) to the `players` collection. Set via the PB admin GUI.

Worldsim reads it during `provisionClient` (it already does a PB lookup by
`user_id`). Guests never have `is_admin` — they have no PB record.

**Why PB flag, not OIDC claims:** PB is already the source of truth for player
data (display_name, sprite_base, position). Adding admin status there is
consistent. No Dex configuration changes needed. The admin GUI already exists.

### 2. Auth flow: thread `is_admin` to the pusher and browser

Current auth flow:
```
Pusher → worldsim:  AuthResultFrame{ClientId, Sub, Ip}     (client.connected)
Worldsim → pusher:  AuthResultFrame{EntityId, MapId}        (reply)
Pusher → browser:   AuthResultFrame{Ok, ClientId, EntityId} (WS frame)
```

Extended:
```
Pusher → worldsim:  AuthResultFrame{ClientId, Sub, Ip}           (unchanged)
Worldsim → pusher:  AuthResultFrame{EntityId, MapId, IsAdmin}    (new field)
Pusher → browser:   AuthResultFrame{Ok, ClientId, EntityId, IsAdmin} (new field)
```

Proto change: add `bool is_admin = 7` to `AuthResultFrame`.

Worldsim sets `IsAdmin` in the `client.connected` reply based on the PB
lookup. The pusher reads it from the reply and includes it in the
browser-bound auth result. The frontend stores `isAdmin` on the `WsClient`.

### 3. In-memory IP on Entity

Add `IP string` to `Entity` (not `NetworkSession` — it's entity-level data).
Set in `provisionClient` from the `ip` parameter (already arriving via
`AuthResultFrame.ip`). No proto change — this is server-internal state.

For logged-in users, the IP is also in PB (`players.ip`). For guests, it's
in-memory only, lost on disconnect. This matches the existing "guests are
ephemeral" design.

### 4. Admin-only NATS channel: `client.<id>.admin`

New per-client subject, same pattern as `client.<id>.replication`,
`client.<id>.chat_inbox`, `client.<id>.av_token`.

**Pusher side:** After the `client.connected` request-reply returns
`IsAdmin=true`, the pusher subscribes to `client.<id>.admin` and forwards
raw bytes to the browser. Non-admin sessions never get this subscription —
admin data never reaches their WebSocket.

**Worldsim side:** Worldsim tracks which clients are admins (a set or flag on
the Entity). When an entity spawns near an admin client (i.e., is added to
the admin's `spawnedTo` set), worldsim publishes an `AdminInfoFrame` to
`client.<admin_client_id>.admin` with that entity's admin-only data.

### 5. AdminInfoFrame proto

New `ServerFrame` variant:

```proto
// AdminInfoFrame carries admin-only data about entities, sent only to
// admin clients via the client.<id>.admin NATS subject. Never sent to
// non-admin clients. Extensible — add fields as admin features grow.
message AdminInfoFrame {
  message EntityAdminInfo {
    string entity_id = 1;
    string ip = 2;           // client IP (guests: in-memory; logged-in: from PB)
    bool is_guest = 3;       // anonymous session?
    string user_id = 4;     // PocketBase user ID (empty for guests)
  }
  repeated EntityAdminInfo entities = 1;
}
```

Sent on two occasions:
1. **Admin client connect:** worldsim sends AdminInfoFrame for all entities
   already visible to the admin (already in `spawnedTo`).
2. **Entity spawns near admin:** when a new entity is spawned to an admin
   client during the replication tick, worldsim sends an AdminInfoFrame for
   that entity alongside the SpawnEntity.

Not sent on despawn — the frontend destroys the name tag (and its admin info)
when it receives DestroyEntity.

### 6. Frontend: admin overlay

**WsClient:** stores `isAdmin` from the auth result. Handles `adminInfo`
ServerFrame variant, passes to `GameScene` via a new handler
`onAdminInfo`.

**GameScene:** maintains a `Map<entityId, EntityAdminInfo>`. When rendering
name tags, if `this.isAdmin` is true and admin info exists for the entity,
render the IP below the name in the pillbox (smaller font, muted color).

For non-admin sessions: `isAdmin` is false, `onAdminInfo` is never called,
the map is empty, name tags render exactly as today. No visual change for
non-admins.

### 7. Privacy guarantee

- Admin data travels only on `client.<admin_id>.admin`.
- The pusher subscribes to this subject only for admin sessions.
- Non-admin browsers never receive `AdminInfoFrame` — not in the replication
  stream, not in any other frame. The data doesn't leave the server.
- A non-admin client inspecting WebSocket frames sees only the same data as
  today. No IP, no OIDC sub, no admin info of any kind.

## Implementation plan

1. **Proto:** add `is_admin` to `AuthResultFrame`, add `AdminInfoFrame` to
   `ServerFrame` oneof. Run `make proto`.
   → verify: `make proto` succeeds, generated code has new fields.

2. **Migration:** add `is_admin` bool to `players` collection (idempotent,
   same pattern as existing migrations).
   → verify: worldsim starts, migration runs, field visible in PB admin.

3. **Worldsim:**
   - Add `IP` and `IsAdmin` to `Entity` struct.
   - `provisionClient`: read `is_admin` from PB user record, set `Entity.IsAdmin`
     and `Entity.IP`.
   - `client.connected` reply: include `IsAdmin` in the `AuthResultFrame`.
   - Track admin clients (e.g. `map[string]bool` of admin client IDs, or check
     `Entity.IsAdmin` on iteration).
   - On entity spawn to an admin client: publish `AdminInfoFrame` to
     `client.<admin_id>.admin`.
   - On admin client connect: send initial AdminInfoFrame for all already-
     visible entities.
   → verify: `go test ./internal/worldsim/ -v` passes. New unit test:
     `TestAdminInfo_SentOnSpawnToAdmin`.

4. **Pusher:**
   - Read `IsAdmin` from `client.connected` reply.
   - If admin, subscribe to `client.<id>.admin` and forward to browser.
   - Include `IsAdmin` in the browser-bound `AuthResultFrame`.
   → verify: `make build` succeeds. Integration test: admin client receives
     AdminInfoFrame, non-admin client does not.

5. **Frontend:**
   - `WsClient`: store `isAdmin`, handle `adminInfo` frame, add `onAdminInfo`
     handler.
   - `GameScene`: maintain admin info map, render IP in name tag for admin
     viewers.
   → verify: manual smoke test — admin sees IPs in name tags, non-admin sees
     no change.

6. **DASHBOARD.md:** update with admin mode decision and status.

## Out of scope (flagged for future)

- **Rate limiting by IP:** can be built on top of the in-memory IP on Entity
  (worldsim) or at the pusher (connection-level). Separate feature.
- **Admin actions (kick, ban, teleport):** the admin channel is one-way
  (server→admin client). Admin actions would need a client→server frame
  (e.g. `AdminActionFrame`) and worldsim handling. Separate feature.
- **Guest session audit log (persistent):** a `guest_sessions` PB collection
  for post-hoc investigation. Separate feature; the in-memory IP covers
  real-time admin visibility for now.
- **Admin role granularity:** single `is_admin` bool. No moderator vs. admin
  vs. super-admin tiers. Can add a `role` text field later if needed.
- **Admin badge on own name tag:** admins seeing themselves as admin. Trivial
  once the frontend has `isAdmin`.
- **Dynamic admin status (no reload):** currently `is_admin` is read once at
  connect time. Flipping it in PB requires a page reload to take effect. To
  make it live: worldsim registers a PB `OnRecordAfterUpdate` hook (in-process,
  since PB is embedded), updates the in-memory `Entity.IsAdmin`, notifies the
  pusher to start/stop the `client.<id>.admin` subscription (new NATS message
  like `client.<id>.admin_status`), and the pusher tells the browser its admin
  status changed so the frontend re-renders name tags. Deferred — "reload to
  become admin" is acceptable for now.

## Files to modify

| File | Change |
|------|--------|
| `proto/frames.proto` | `is_admin` on AuthResultFrame, `AdminInfoFrame` in ServerFrame |
| `backend/migrations/1752800000_add_is_admin_to_players.go` | New migration |
| `backend/internal/worldsim/worldsim.go` | `Entity.IP` + `Entity.IsAdmin`, provisionClient reads PB, admin info publishing |
| `backend/internal/worldsim/userstore.go` | `UserRecord.IsAdmin`, `recordToUser` reads it |
| `backend/internal/pusher/pusher.go` | Read `IsAdmin` from reply, conditional admin subscription, forward to browser |
| `frontend/src/net/WsClient.ts` | Store `isAdmin`, handle `adminInfo` frame |
| `frontend/src/scenes/GameScene.ts` | Admin info map, IP in name tag for admins |
| `frontend/src/proto/frames.ts` | Regenerated by `make proto` |

## Testing

**Worldsim unit tests:**
- `TestAdminInfo_SentOnSpawnToAdmin`: provision an admin entity and a guest
  entity, run a tick, assert the admin client's `client.<id>.admin` subject
  receives an AdminInfoFrame with the guest's IP.
- `TestAdminInfo_NotSentToNonAdmin`: same setup, assert non-admin client
  receives no AdminInfoFrame.
- `TestProvisionClient_AdminFlagFromPB`: (requires mock PB — may skip and
  verify manually).

**Integration tests:**
- Admin client connects, receives `IsAdmin=true` in auth result, receives
  AdminInfoFrame when a guest connects nearby.
- Non-admin client connects, receives `IsAdmin=false`, never receives
  AdminInfoFrame.

**Manual smoke test:**
- Set `is_admin=true` on a player in PB admin GUI.
- Connect as that player (admin) and as a guest.
- Admin sees guest's IP in name tag pillbox.
- Non-admin sees no IP anywhere.
