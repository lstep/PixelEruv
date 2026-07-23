# AFK State — Design

**Date:** 2026-07-22
**Status:** Implemented.

## Context

Players have a manual presence status — Available (0), Busy (1), Do Not Disturb (2) — set via the TopMenu dropdown and persisted to PocketBase (`players.status`). See `2026-07-15-player-status-design.md`.

This adds an automated **AFK overlay** that activates when the player is inactive and deactivates when they return, preserving the manual status underneath (DND -> AFK -> DND). It also adds **tab-visibility A/V gating** so a tabbed-away player stops broadcasting immediately.

## Two features

These share detection signals but serve different purposes:

1. **Tab-visibility A/V gating** (immediate, client-side only): when the tab is hidden or the window blurred, AvClient mutes mic/cam/screen tracks. When the tab returns, tracks restore. No server involvement — the player stays in the room, tracks just stop flowing. Debounced 3s both directions to avoid WebRTC churn on rapid tab switching.

2. **AFK overlay** (timer-based, server-replicated): a two-tier timer (tab hidden 2min, or input idle 10min) sets an AFK boolean sent to the server, replicated via `DisplayName.afk`, and rendered to others as a dimmed nametag pill + AFK badge. AFK also auto-mutes A/V tracks (same mechanism as tab-hide). When the user returns, AFK clears and the manual status restores.

They compose: tab-hidden mutes A/V immediately; if the tab stays hidden for 2min, AFK status also activates. When the user returns, both clear. If the tab is visible but the user is input-idle for 10min, only AFK activates.

## Design decisions

### AFK is an overlay, not a 4th status enum value

The manual status (0/1/2) is preserved underneath. AFK is a separate `bool` on `Entity` and `DisplayName`, not a 4th value in the status enum. Reasons:

- Persisting "AFK" to `players.status` would lose the manual state on reload.
- ext-av only cares about DND (A/V exclusion). AFK doesn't change A/V exclusion — it mutes tracks client-side but doesn't eject from rooms. No ext-av changes needed.
- The wire format stays simple: `DisplayName.afk` is a new bool field, `status` stays 0-2.

### Hybrid: client signals, server authoritative

The client runs AFK detection (AfkDetector module) and sends `SetAfkFrame` on transitions. The server applies the flag and replicates it. The server does not auto-clear AFK on activity — the client is responsible (self-healing: any activity sends `SetAfkFrame(false)`).

### Two-tier thresholds

- Tab hidden / window blurred -> AFK after 2 min.
- Tab visible but input idle -> AFK after 10 min.

Constants in `AfkDetector.ts`, easily tuned. Could later be configurable via world_options.

### Meeting exemption

AFK does NOT activate if the player is in an A/V room with at least one other participant (`avClient.isInMeeting()` = `room !== null && remoteParticipants.size > 0`). This prevents false-positiving someone in a long video meeting who doesn't move the mouse. A solo user in a room (no remote participants) is NOT exempt — they can still go AFK.

The exemption prevents activation only — it does NOT clear existing AFK. This matters for proximity/zone A/V: when a second player walks up to an already-AFK player, a LiveKit room forms and `isInMeeting()` flips true, but the AFK player has not actually returned. Clearing AFK there would wake them with no input. Existing AFK clears only on genuine user activity (mouse/keyboard/touch/wheel handlers), which is immediate and does not wait for the next check tick.

### Tab-visibility A/V gating is client-side only, debounced both directions

When the tab is hidden or window blurred, AvClient mutes all tracks (mic, cam, screen) without disconnecting from the room. When the tab returns, tracks restore to the user's manual preferences. Debounced 3s in both directions to avoid WebRTC track churn on rapid tab switching. Applies even in meetings — no A/V broadcast while the tab is not in focus.

### AFK mutes A/V tracks but does not disconnect

Unlike DND (which disconnects from A/V rooms entirely), AFK mutes tracks but the player stays in the proximity/zone room. When AFK clears, tracks restore. This avoids disruptive eject/rejoin cycles.

### AFK is not persisted

On page reload, AFK starts false. The client's AfkDetector starts fresh. The manual status is restored from PocketBase as before.

## Files

| File | Changes |
|---|---|
| `proto/components.proto` | `bool afk = 5` on `DisplayName` |
| `proto/frames.proto` | `SetAfkFrame` message; `set_afk = 12` on `ClientFrame` |
| `backend/internal/worldsim/worldsim.go` | `Entity.AFK` field; `handleSetAfk`; `client.*.set_afk` subscription |
| `backend/internal/worldsim/replication.go` | `Afk: e.AFK` added to 3 `DisplayName` marshal sites |
| `backend/internal/worldsim/worldsim_afk_test.go` | New — tests `handleSetAfk` |
| `backend/internal/pusher/pusher.go` | Forward `SetAfkFrame` to worldsim on `client.<id>.set_afk` |
| `frontend/src/net/WsClient.ts` | `setAfk(afk)` method |
| `frontend/src/net/AfkDetector.ts` | New — two-tier AFK detection + meeting exemption + debounced tab events |
| `frontend/src/net/AvClient.ts` | `isInMeeting()`, `setTabHidden()`, `setAfkMuted()`, `applyAutoMute()` |
| `frontend/src/scenes/GameScene.ts` | Wire `AfkDetector`; `afkByEntity` map; pass `afk` to `createNameTag`; cleanup on shutdown |
| `frontend/src/ui/TopMenu.ts` | `setLocalAfk()` method; AFK badge; disable A/V buttons while AFK |

## Risks / known limitations

- **Multi-tab conflict:** Two tabs each run AfkDetector independently; active tab sends `SetAfkFrame(false)` while AFK tab sends `SetAfkFrame(true)`. Existing limitation (multi-tab already conflicts on status/position). Not solving in v1.
- **Reconnect edge case:** Worldsim re-provisions entity with `AFK=false` on reconnect. AfkDetector re-evaluates and re-sends `SetAfkFrame(true)` if still idle. Self-healing.
- **Meeting exemption false negative:** A user in a proximity zone with others who all walked away won't go AFK. Accepted — safer than false-positiving meeting participants. Room tears down on disconnect.
- **No server-side auto-clear:** If `SetAfkFrame(false)` is lost on activity, user appears stuck AFK until next activity sends another frame. Self-healing within seconds.
- **Threshold tuning:** 2min/10min/3s are initial guesses. Constants in AfkDetector, easy to adjust.
