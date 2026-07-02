# Use Cases and Workflow Validation

This document traces concrete gameplay scenarios through the PixelEruv
architecture to validate that the design supports them. Each use case includes
the map/extension setup, the runtime sequence of events, and any gaps found.

> **Purpose:** verify feasibility, surface missing pieces, and provide
> implementers with end-to-end traces they can follow when building extensions.

---

## 1. Proximity Alarm

### Requirement

When a player comes within 3 tiles of a specific entity (e.g. a security
camera), an alarm rings. It keeps ringing as long as any player is within the
3-tile radius, and stops when all players leave the radius.

### Map setup (Tiled)

- Place the alarm entity at tile `(10, 5)` with a sprite and an
  `Interactable` component.
- No special zone needed — the alarm is entity-centric, not zone-centric.

### Extension setup (`security-ext`)

```
1. Claim the alarm entity from Tiled (or spawn it).
2. Register a proximity-bound notify trigger:
   - trigger_id: "alarm-1-proximity"
   - category: "event", binding: "proximity"
   - entity_id: "alarm-1"
   - radius: 3 (tiles)
   - events: ["proximity_enter", "proximity_exit"]
3. Maintain an in-memory presence set (players currently in radius).
```

That's it — one trigger registration. The kernel handles the per-tick
proximity evaluation and fires `proximity_enter`/`proximity_exit` events.

### Runtime sequence

```
Player A moves to tile (8, 4) — within 3 tiles of alarm at (10, 5)
  → Tick 10: kernel evaluates proximity triggers (step 10 of tick loop)
  → Kernel: range query around (10, 5) with radius 3 → finds A at (8, 4)
  → Compare with previous tick's set (empty) → A is new
  → Kernel publishes: entity.alarm-1.notify.proximity_enter
      { entity_id: "alarm-1", event: "proximity_enter",
        player_entity_id: "A", distance: 2 }
  → security-ext receives proximity_enter
  → Extension: add A to presence set → set is non-empty
  → Extension sends UpdateComponent on alarm entity:
      AlarmBell { ringing: true }
  → Replication encoder picks up dirty AlarmBell
  → All clients in AOI see the alarm start ringing

Player A moves from (8, 4) to (8, 5) — still within radius
  → Tick 11: kernel re-evaluates proximity
  → Range query finds A at (8, 5) → still in set (was in set last tick)
  → No transition → no event fired
  → Alarm continues (no component update needed — already ringing)

Player A moves from (7, 5) to (6, 5) — leaves the 3-tile radius
  → Tick 12: kernel re-evaluates proximity
  → Range query around (10, 5) with radius 3 → A at (6, 5) is NOT in range
  → Compare with previous tick's set (contained A) → A departed
  → Kernel publishes: entity.alarm-1.notify.proximity_exit
      { entity_id: "alarm-1", event: "proximity_exit",
        player_entity_id: "A", distance: 4 }
  → security-ext receives proximity_exit
  → Extension: remove A from presence set → set is empty
  → Extension sends UpdateComponent:
      AlarmBell { ringing: false }
  → All clients see the alarm stop
```

### Multiple players

The kernel fires `proximity_enter`/`proximity_exit` for each player
transition independently. The extension maintains a presence set: the alarm
rings while the set is non-empty and stops when the last player leaves.

### Moving alarm entity

If the alarm entity itself moves (e.g. a patrol drone with an alarm), the
radius moves with it. The kernel re-evaluates from the entity's new position
each tick. No trigger re-registration needed — the trigger is bound to the
entity, not to fixed tiles.

### Verdict

**Feasible with the current architecture.** The proximity-bound trigger type
handles this cleanly: one trigger registration, no tile enumeration, no
extension-side enter/exit pairing. The kernel evaluates proximity per-tick
and fires transitions only on enter/exit.

---

## 2. Full Isolation Room

### Requirement

A room has a door. When the door is closed:

- Nobody outside can enter (movement blocked).
- Nobody outside can hear or see inside (audio/video cut).
- Nobody outside can see inside (visual replication cut — not just a
  client-side dark filter, but a server-side exclusion from replication
  batches).

Players inside the room can still see and hear each other normally.

### Map setup (Tiled)

- A room with walls (block triggers from the walls extension).
- A door entity at the entrance tile `(42, 17)`.
- A zone polygon (`meeting-room-1`) covering the room interior.
- Boundary tiles at the entrance.

### Extension setup (`doors-ext`)

```
1. Claim the door entity from Tiled.
2. Register the zone (meeting-room-1) with the room's boundary tiles.
3. Register an ask trigger on the door tile:
   - behavior: "ask", default_on_timeout: "block", ttl_ms: 500
   (When the door is open, the extension replies "allow".
    When closed, it replies "block".)
4. Register notify triggers on the room interior tiles:
   - event: "enter" / "exit" (to track who is inside).
5. Watch KV: zones.meeting-room-1.properties
```

### Door close sequence

```
Player A (inside the room) clicks the door tile → ActionFrame
  → No action trigger on door tile → fallback to entity interaction routing
  → doors-ext receives interact (entity.<door_id>.interact)
  → doors-ext: door state = closed
     a. Update internal state: reply "block" to future ask queries on the
        door tile.
        (Optionally: register a block trigger on the door tile so the
        kernel refuses locally without a NATS round-trip. Faster, but
        requires unregistering when the door reopens.)
     b. Write KV: zones.meeting-room-1.properties
        { is_exclusive: true, tint_color: "#1a1a2e" }
  → kv.Watch fires:
     a. LiveKit Bridge reads is_exclusive = true
        → cuts A/V subscriptions for all participants outside the zone
        → participants inside keep their subscriptions (they can still
          hear/see each other)
     b. World Sim reads zone state change
        → replicates zone properties to all clients in AOI
        → clients outside apply the visual filter (dark tint, halo)
        → clients inside see no change
```

### Isolation layers

| Layer | Mechanism | Hard isolation? |
|---|---|---|
| **Movement** (no entry) | `block` access trigger on door tile (cached in spatial index, kernel refuses locally) | ✅ Yes — kernel refuses the move, no NATS round-trip |
| **Audio/video** (no hear/see) | LiveKit Bridge cuts subscriptions based on `is_exclusive` in KV | ✅ Yes — Bridge revokes LiveKit track subscriptions for outsiders |
| **Visual** (no see inside) | Zone-aware AOI filter (see below) | ⚠️ **Gap — needs implementation** |

### Gap: visual isolation is not hard isolation

The current AOI filter (`11-replication.md` §3.3) includes entities based on
**same map + distance radius**. It does **not** exclude entities inside an
exclusive zone from non-members' replication batches. Without a fix, an
outsider standing next to the wall would still receive `UpdateComponent`
messages for entities inside the room (player positions, animations). The
"visual filter" (dark tint) is a client-side rendering effect — a modified
client could remove it and see through the wall.

**Fix: zone-aware AOI filter.** The replication encoder must exclude entities
inside an exclusive zone from non-members' replication batches:

```
AOI filter (updated):
  For each entity E, for each client C:
    1. Is E on the same map as C? → if no, skip
    2. Is E within AOI radius of C? → if no, skip
    3. Is E inside an exclusive zone? → if yes:
       a. Is C's avatar also inside that zone? → if no, skip E
       b. (zone members see each other; outsiders don't)
    4. Does E have an InventorySlot/Equipped component owned by another
       player? → if yes, skip (existing owner-only filter)
    5. Include E in C's replication batch.
```

The kernel already knows:

- Zone boundaries (zone registry, from Tiled + extension registrations).
- Entity positions (ECS).
- Zone exclusivity state (read from KV, or cached in a zone-state map
  updated via `kv.Watch`).

This is a kernel-side change to the AOI filter, consistent with the kernel's
role as replication gateway. The extension writes `is_exclusive` to KV; the
kernel reads it and applies the filter. No extension change needed.

**Transition handling:** when the door closes and the zone becomes exclusive,
the kernel must send `DestroyEntity` to outsiders for all entities inside the
zone (they leave the outsiders' replication). When the door reopens, the
kernel sends `SpawnEntity` to outsiders for all entities inside the zone
(they re-enter the outsiders' replication). This happens naturally on the
next tick after the AOI filter changes — the replication encoder detects
that entities previously in a client's batch are no longer in the filtered
set and emits `DestroyEntity`.

### Verdict

**Partially feasible.** Movement and audio/video isolation work with the
current architecture. **Visual isolation requires a zone-aware AOI filter**
in the replication encoder — a kernel change, not an extension change. This
should be added to `11-replication.md` §3.3 before implementation.

---

## 3. Knock-to-Join with Owner

### Requirement

- A user enters an empty zone → becomes the owner.
- Another user tries to enter → denied access. A "toc-toc" sound plays for
  the owner, and a popup appears asking whether to accept the new user.
- If the owner accepts → the user is allowed to enter.
- The owner can also proactively invite a user → the user is automatically
  allowed to enter.

### Map setup (Tiled)

- A meeting room with a zone polygon (`meeting-room-1`).
- Door/boundary tiles at the entrance.

### Extension setup (`meeting-ext`)

```
1. Register zone: meeting-room-1, boundary tiles = entrance tiles.
2. Register ask trigger on boundary tiles:
   - behavior: "ask", default_on_timeout: "block", ttl_ms: 500
3. Register notify triggers on interior tiles:
   - event: "enter" (detect first occupant → set owner)
   - event: "exit" (detect last occupant → clear owner)
4. Watch KV: zones.meeting-room-1.owner
5. Maintain in-memory guest list: Set<entity_id> (cleared on owner change).
```

### Sequence: A enters, B knocks, A accepts

```
Step 1: A enters the empty meeting zone
  → Kernel fires notify enter on interior tile
  → meeting-ext receives: trigger.notify.tile.office.42.18
      { event: "enter", entity_id: "A" }
  → Extension: occupancy = 1, first occupant → set owner = A
  → Write KV: zones.meeting-room-1.owner
      { entity_id: "A", since: "2026-07-01T12:00:00Z" }
  → A's entity gets ZoneAccess component
      { policy: "knock", owner_entity_id: "A" }
  → Replicated to all in AOI (others see A is the owner)

Step 2: B walks toward the zone boundary
  → Kernel evaluates ask trigger on boundary tile
  → Kernel publishes: trigger.meeting-room-1-entrance.query
      { entity_id: "B", target_tile: {x: 42, y: 17},
        reply_to: "trigger.meeting-room-1-entrance.reply" }
  → meeting-ext receives query
  → Extension checks: zone has owner A, policy = "knock",
      B is not on guest list
  → Extension replies: { decision: "block", reason: "knock" }
  → B is blocked at the boundary (position doesn't change —
    client prediction reconciled by unchanged authoritative position)

Step 3: Extension sends knock notification to owner A
  → meeting-ext spawns a transient "knock-popup" entity on A's tile
    (or near A) with:
      Interactable { type: "knock_response" }
      KnockPopup { knocker: "B", popup_id: "knock-123" }
  → Replication encoder sends SpawnEntity for the popup to A
    (popup is on A's tile, in A's AOI)
  → A's client:
      a. Plays "toc-toc" sound (from KnockPopup component)
      b. Shows popup: "B wants to enter. [Allow] [Deny]"

Step 4: A clicks "Allow" on the popup
  → A's client sends ActionFrame targeting the popup entity's tile
  → Kernel: no action trigger on tile → fallback to entity interaction
  → meeting-ext receives: entity.<popup_id>.interact
      { interaction_type: "knock_response",
        params: { popup_id: "knock-123", response: "allow" } }
  → meeting-ext:
      a. Add B to guest list
      b. Despawn the popup entity (DestroyEntity to A)
      c. Optionally: send a notification to B
         (entity.B.update → NotifyComponent { text: "You may enter." })

Step 5: B walks toward the boundary again
  → ask trigger fires → meeting-ext: B is on guest list → reply: allow
  → B enters the zone
  → Kernel fires notify enter on interior tile
  → meeting-ext: occupancy = 2

Step 6: B leaves the zone (later)
  → Kernel fires notify exit on interior tile
  → meeting-ext: occupancy = 1
  → B removed from guest list (optional — guest list could persist
    until owner leaves)
```

### Sequence: A invites B (without B knocking)

```
Step 1: A opens the zone menu
  → A clicks a UI element associated with the zone (e.g. a "manage zone"
    button rendered from the ZoneAccess component on A's entity)
  → A's client sends ActionFrame targeting A's own tile (or a nearby
    "zone-control" entity spawned by the extension)
  → meeting-ext receives the interaction
  → A selects "Invite B" from the menu
  → A's client sends another ActionFrame with params:
      { action: "invite", target: "B" }

Step 2: meeting-ext processes the invitation
  → Add B to guest list
  → Send notification to B:
      entity.B.update → InviteNotification
        { zone: "meeting-room-1", from: "A", expires_at: "..." }
  → B's client shows: "A invited you to meeting-room-1. Click to enter."

Step 3: B walks toward the boundary
  → ask trigger fires → meeting-ext: B is on guest list → allow
  → B enters the zone
```

### Gap: popup response wire protocol

The knock-to-join flow is designed in `14-zones-and-interactions.md` §2, but
the wire protocol for the popup response is an open question. The approach
above uses **transient entities + ActionFrame**:

1. The extension spawns a transient "knock-popup" entity on the owner's tile.
2. The owner clicks it → `ActionFrame` → entity interaction routing →
   extension processes the response.
3. The extension despawns the popup entity after the response or a timeout.

This requires no new frame types. The existing `ActionFrame` → entity
interaction fallback handles it. The popup entity is a regular ECS entity
with an `Interactable` component and a custom `KnockPopup` component — the
client renders it as a popup UI element.

**Alternative considered: dedicated ControlResponseFrame.** A new
client→server frame type that carries a popup ID + response. This is cleaner
conceptually (popups are not tiles) but adds a new frame type and a new
routing path in the kernel. Not worth the complexity for the MVP.

**Recommendation:** use transient entities + ActionFrame. Document this in
`14-zones-and-interactions.md` §2 as the resolution of the open question.

### Edge cases

- **Owner disconnects:** the grace period (`10-world-simulator.md` §5e)
  applies. The owner's avatar stays for 30s. If the owner reconnects, the
  popup is still pending (the extension holds the knock state). If the owner
  despawns, the extension clears the owner and the zone becomes open (or
  closed, depending on policy).

- **Multiple knockers:** the extension can spawn multiple popup entities
  (one per knocker) or queue them. The owner sees them one at a time or in a
  list. This is extension-side UX logic — the architecture supports either.

- **Guest list lifetime:** the extension decides when to clear the guest
  list. Options: clear when the guest enters (one-time pass), clear when the
  owner leaves, or persist until explicitly revoked. This is extension
  policy, not architecture.

### Verdict

**Feasible with the current architecture.** The knock-to-join flow is
already designed. The popup response wire protocol has a viable solution
(transient entity + ActionFrame) that requires no new frame types. The open
question in `14-zones-and-interactions.md` §2 should be resolved with this
approach.

---

## Summary

| Use case | Feasible? | Gap | Fix location |
|---|---|---|---|
| 1. Proximity alarm | Yes | None — proximity-bound trigger handles it natively | — |
| 2. Full isolation room | Partial | Visual isolation is soft only — AOI filter doesn't exclude entities in exclusive zones from non-members | `11-replication.md` §3.3 — add zone-aware AOI filter |
| 3. Knock-to-join | Yes | Popup response wire protocol is open | `14-zones-and-interactions.md` §2 — resolve with transient entity + ActionFrame |
