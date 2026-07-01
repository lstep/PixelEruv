# Zones and Interactions

> **Status:** partial. The area-of-interest (AOI) algorithm is an open
> decision; the zone and knock-to-join models below are specified because
> they are MVP features (see `02-functional-requirements.md` § 1).

This document specifies zones (polygon regions with typed behavior), the
knock-to-join meeting-room flow, and the area-of-interest filter that bounds
per-client bandwidth.

---

## 1. Zones

- Zones are **polygon-defined** regions on a map (authored in Tiled, see
  `15-maps-and-tiled.md`).
- Each zone has typed characteristics: `water`, `exclusive`, `work`,
  `silent`, `meeting`, plus an optional `owner`.
- Zone boundaries are stored by the World Simulator (the spatial authority).
  Zone behavior (exclusivity, knock-to-join, timers, access policies) is
  implemented by extensions via triggers on the zone's boundary tiles (see
  `18-extensions.md` §3a and §3b). The kernel evaluates access triggers and
  routes ask triggers to the owning extension; it does not implement zone
  behavior itself.
- Dynamic zone state lives in JetStream KV (written by the owning extension;
  the kernel and the LiveKit Bridge read it via `kv.Watch`)
  (`zones.<zone_id>.properties`, `zones.<zone_id>.owner` — see
  `06-data-model-and-persistence.md` § 2).

### Exclusive zones (dynamic)

Activated/deactivated at runtime — e.g. a room whose `exclusive` zone follows
its door state (open door → inactive, closed door → active). The owning
extension (e.g. a doors extension) writes the new state to KV and
registers/unregisters block triggers on the zone boundary tiles. `kv.Watch`
fires; the World Sim replicates the visual filter to clients and the LiveKit
Bridge cuts audio/video subscriptions for outsiders.

---

## 2. Knock-to-join meeting rooms

A `meeting`-type zone where the first entrant becomes the **owner** and
subsequent entrants must be admitted.

### Data model

- `zones.<zone_id>.owner` in KV → `{ "entity_id": "...", "since": "..." }`.
- New ECS component **`ZoneAccess`** on the zone entity:
  `{ policy: "open" | "knock" | "invite_only", owner_entity_id }`
  (see `13-ecs-design.md`).

### Flow

```
1. User A enters an empty meeting zone → the zone's owning extension (e.g.
   meeting-v1) receives a notify trigger (enter event) → extension sets
   owner = A (KV).
2. User B walks into the zone boundary.
3. The kernel evaluates the ask trigger on the zone boundary tile → publishes
   query to the meeting extension → extension checks if B needs admission →
   replies block (B cannot cross).
4. Extension emits a "knock" event to owner A (via replication → ControlFrame
   or a dedicated knock message — see 07-network-protocol.md).
5. A's client shows a popup: Allow / Deny.
   - Allow → extension grants B temporary access (updates its internal state
     or KV) → next ask trigger reply for B is allow → B may cross.
   - Deny  → extension keeps replying block → B stays blocked; optional
     notification to B.
6. A can also directly invite another user → same grant mechanism.
```

> **[OPEN]** Exact wire messages for knock / allow / deny / invite — to be
> added to `07-network-protocol.md` once the popup UX is settled. Likely a
> `ControlFrame` variant server→client and an `ActionFrame` client→server
> (the client sends an `ActionFrame` on the meeting room door tile).

---

## 3. Interaction routing

Interactions are routed via NATS subjects (see `07-network-protocol.md` § 2).
Client interactions arrive as `ActionFrame` (tile clicks and keypress
interactions — `InteractFrame` has been deprecated and replaced by
`ActionFrame`). The kernel validates range and line-of-sight, then routes:

1. **Action triggers on the clicked tile?** → dispatch to the owning extension
   with an equipment snapshot (see `18-extensions.md` §3a).
2. **Entity on the clicked tile?** → fallback to entity interaction routing:
   `ExtensionOwner` component or entity-bound `notify` triggers (see
   `18-extensions.md` § 6).
3. **No trigger, no entity** → `ActionResultFrame{ ok: false, reason: "no_target" }`.

The kernel does not have a `TriggerSystem` or `ZoneSystem` — all trigger and
zone behavior is implemented by extensions. The kernel only validates spatial
reachability (range, LOS) and routes.

## 3a. Action triggers

Action triggers are a third trigger category (alongside access and event
triggers). They are registered by extensions on tiles and fire when a player
clicks that tile. Unlike access triggers (which gate movement) and event
triggers (which notify on enter/exit), action triggers are **player-initiated**
and support **range and line-of-sight validation** by the kernel.

### Use cases

- **Ranged actions**: shooting a bow, throwing an object at a distant tile
  (with `max_range` and `require_los`).
- **Adjacent interactions**: clicking a tile next to the player to interact
  with whatever is on it (the kernel falls back to entity interaction routing
  if no action trigger is registered on the tile).
- **Equipment-dependent actions**: the action depends on what the player is
  holding (e.g. bow → spawn arrow, empty-handed → no action). The kernel
  includes an equipment snapshot in the dispatch payload.

### Registration

See `18-extensions.md` §3a for the full registration protocol. An action
trigger declares:

```
{
  "trigger_id": "bow-shot-zone",
  "category": "action",
  "binding": "tile",
  "tiles": [{"map_id": "arena", "x": 10, "y": 5}],
  "event": "click",
  "max_range": 8,
  "require_los": true,
  "los_through_walls": false,
  "default_on_timeout": "drop"
}
```

### Kernel validation

When a player clicks a tile with an action trigger:

1. **Range check**: distance from player's `Position` to the clicked tile ≤
   `max_range`. If no `max_range` is set, defaults to adjacent-only (distance
   ≤ 1).
2. **Line-of-sight check**: if `require_los` is true, the kernel raycasts
   (Bresenham line through tiles) from the player to the clicked tile. A tile
   blocks LOS if it has a `block` access trigger, a non-traversable entity
   (`Traversable=false`), or is a wall in the map. If `los_through_walls` is
   false, walls block.
3. **If validation fails**: `ActionResultFrame{ ok: false, reason }` sent to
   the client immediately (no NATS round-trip).
4. **If validation passes**: dispatch to the owning extension with the
   player's equipment snapshot. See `18-extensions.md` §3a.

---

## 4. Area of Interest (AOI)

The AOI filter bounds per-client bandwidth: a client only receives state for
entities within its area of interest (see `11-replication.md` § 3.3 and
`03-non-functional-requirements.md` § 4).

An entity is in a client's AOI if it is on the same map and within a
configurable radius of the client's avatar (or in the same zone).

### Candidate algorithms

| Algorithm | Pros | Cons |
|---|---|---|
| **Distance radius** | Simplest | O(n²) naive; needs spatial index at scale |
| **Uniform grid / buckets** | Cheap neighbor lookup; maps to NATS subjects | Tuning cell size |
| **Quadtree** | Adapts to density | More complex; rebuild cost |

> **[OPEN] AOI algorithm decision.** This affects how NATS subjects are
> partitioned and how cross-shard visibility works (see `10-world-simulator.md`
> § 6). Recommended starting point: **uniform grid** (cell ≈ AOI radius),
> revisited if profiling shows hotspots. Decide alongside load testing.

---

## Open questions

- **[OPEN] AOI algorithm** (grid / quadtree / distance).
- **[OPEN] Knock/invite wire protocol** in `07-network-protocol.md`.
- **[OPEN] Silent / water / work zone semantics** — full behavior table.
- **[OPEN] Zone polygon authoring** — Tiled object layer conventions
  (`15-maps-and-tiled.md`).
- **[OPEN] Zone trigger registration timing** — when an extension registers a
  zone and its boundary triggers at init time, what happens if a player is
  already standing on a boundary tile? Does the trigger apply retroactively?
