# Future Exploration: Client-Side Keep-Warm for A/V Join Latency

## Context

Issue #88 reported that video takes ~2 seconds to open or close. The primary
fix (hysteresis + movement gating + reduced leave debounce) addresses the
leave delay and thrashing. The join delay (~1-2s) is largely inherent to
WebRTC connection setup (WebSocket signaling + ICE/DTLS/SRTP negotiation).

This document explores a client-side keep-warm approach to reduce join
latency further, to try if the primary fix is insufficient.

## Idea

On "leave", instead of disconnecting the LiveKit room immediately:
1. Unpublish mic and camera tracks (stop sending).
2. Hide remote video tiles (stop receiving visually).
3. Keep the Room signaling connection warm.

On "join" to the **same** room:
1. Republish mic and camera tracks.
2. Unhide remote video tiles.

This skips WebSocket + ICE negotiation on rejoin (~1-2s), leaving only track
publish latency (~200-500ms).

## Where It Works

- **Rejoining the same proximity group** — e.g. you step briefly out of range
  and back in. The room is still warm, so rejoin is fast.
- **Zone A/V** — fixed room names, you exit and re-enter the same zone.
  Keeping the connection warm during brief exits is clean.

## Where It Breaks Down

### Privacy

If the room stays alive but tracks are not unpublished, the person you walked
away from can still hear your mic and see your camera. Unpublishing on leave
mitigates this, but it is a critical correctness requirement, not optional.

### Multi-Room Accumulation

Proximity groups are dynamic. Walking from person A to person B means
different LiveKit rooms (`proxgroup-<hashA>` vs `proxgroup-<hashB>`). You
cannot keep both warm without accumulating stale rooms. The "walk past
someone" scenario in issue #88 is exactly this: you join A's room, walk past,
and now you are near no one. If you keep A's room warm, you have leaked a
connection. When you later approach C, that is a third room. A cleanup
timeout (e.g. "disconnect after 30s of no rejoin") would be needed, adding
complexity and a new latency tier.

### The Other Person's Side

Even if you unpublish your tracks, you remain a participant in their LiveKit
room. They see an empty participant slot. When you actually leave for good,
they never get a clean "participant disconnected" event until your cleanup
timeout fires. This affects their UI.

### LiveKit MoveParticipant Is Cloud-Only

The ideal solution — server-side room moves via `MoveParticipant` — is
LiveKit Cloud-only. Confirmed via LiveKit community forum (February 2026)
and GitHub issue livekit/livekit#4203:

> "As the room a participant is moved to could be on a different mode, it is
> not possible to do this in self-hosted version."

This project uses self-hosted LiveKit v1.13.2, so `MoveParticipant` is
unavailable. The client SDK has `RoomEvent.Moved` (a passive listener for
server-initiated moves), but there is no client-side API to initiate a move.

## Complexity vs. Root Cause

The keep-warm approach requires changes to:
- Track publish/unpublish logic on leave/join
- Visual hide/show of tiles
- Cleanup timeouts for stale rooms
- Multi-room lifecycle management

Compare to the primary fix (hysteresis + movement gating): ~30 lines of
backend code, no frontend changes beyond the debounce value.

## Assessment

Worth considering for **zone A/V** (fixed rooms, re-entry is common). For
**proximity A/V** (dynamic groups, multi-room transitions), the privacy risk
and room accumulation are real concerns. The primary fix (hysteresis +
movement gating) is simpler and addresses the root cause rather than masking
it.

If join latency is still insufficient after the primary fix, consider:
1. A short keep-warm window (e.g. 2-3 seconds) for same-room rejoins only.
2. Pre-warming the LiveKit signaling WebSocket (if the SDK supports
   connection pooling).
3. Investigating LiveKit Cloud for MoveParticipant support.
