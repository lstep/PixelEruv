# Player Presence Status — Design

**Date:** 2026-07-15
**Status:** Implemented. Status is persisted to PocketBase (`players.status`) and restored on connect, so it survives page reloads.

## Context

Players can set a presence status — **Available** (0), **Busy** (1), or **Do Not Disturb** (2) — via the TopMenu dropdown. The status is rendered as the name-tag pill color and used to gate A/V: DND fully excludes the player from A/V rooms (worldsim skips proximity clustering for DND players; ext-av skips zone token minting; the client AvClient disconnects and refuses joins; mic/camera/screen buttons are disabled in the UI as visual feedback).

The status rides on the existing `DisplayName` replication component (`uint32 status = 4`), so no new component or replication path was needed.

## Decision: persist to `players.status`

The first implementation was session-only: `Entity.Status` in worldsim memory, reset to Available on every connect. This caused sync problems — a page reload lost the value, the TopMenu hardcoded `applyStatus(0)` on init, and the server/client could disagree about the current status.

Status is now persisted to a `status` NumberField on the `players` PocketBase collection, mirroring the pattern used for `display_name`, `sprite_base`, `options`, and `hide_admin_badge`:

1. **Migration** adds the field (idempotent, default 0 = Available, no backfill needed).
2. **UserStore** gains `UpdateStatus(entityID, status)` and reads `status` in `recordToUser`.
3. **Worldsim `provisionClient`** restores `user.Status` onto `Entity.Status` at provision time. The status then reaches all clients (including the local player) via the existing DisplayName component in `SpawnEntity`.
4. **Worldsim `handleSetStatus`** persists via `userStore.UpdateStatus` after the in-memory update. Errors are logged but do not fail the request — the in-memory status is already live and replicated.
5. **Frontend** `TopMenu.syncStatusFromServer(value)` reflects the server-confirmed status in the dropdown (button highlight + A/V control enablement) without re-firing `setStatusHandler`, so there is no echo loop. `GameScene` calls it on both initial spawn and DisplayName updates for the local player.

## Reconnect behavior

**Restore as-is.** A player who sets DND and closes the browser reconnects as DND (mic/cam disabled until they change it). This is intentional — the user asked for the persisted value to come back on reload. A "reset to Available on connect" alternative was considered and rejected.

## Guests

Guests have no PocketBase record. `UpdateStatus` is a no-op for them (the `findByEntityIDRecord` lookup returns nil). Their status remains session-only, consistent with all other player fields.

## Save timing

Save-on-change (mirrors `UpdateDisplayName`), not save-on-disconnect like position. Status is discrete and user-driven, so persisting immediately on `handleSetStatus` is correct — no double-write or debounce needed.

## Files

| File | Changes |
|---|---|
| `backend/migrations/1753600000_add_status_to_players.go` | New — `status` NumberField on `players` (default 0) |
| `backend/internal/worldsim/userstore.go` | `UserRecord.Status`; `UpdateStatus` method; read in `recordToUser` |
| `backend/internal/worldsim/worldsim.go` | Restore `user.Status` in `provisionClient`; persist in `handleSetStatus`; updated comments |
| `backend/internal/pusher/pusher.go` | Comment updated (was "Session-only — not persisted to PocketBase") |
| `proto/components.proto` | `DisplayName.status` comment updated |
| `frontend/src/ui/TopMenu.ts` | `applyStatusFn` field; `syncStatusFromServer(value)` method |
| `frontend/src/scenes/GameScene.ts` | Call `avClient.setStatus` + `topMenu.syncStatusFromServer` for local player on spawn and DisplayName update |
