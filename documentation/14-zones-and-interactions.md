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
> (the client sends an `ActionFrame` with `input_type: "click:left"` on the
> popup entity's tile; the meeting extension self-filters based on
> `entities_on_tile`).

---

## 3. Interaction routing

Interactions are handled by the input handler model (see `18-extensions.md`
§3a). Client interactions arrive as `ActionFrame` (tile clicks and key presses
— `InteractFrame` has been deprecated and replaced by `ActionFrame`). The
kernel broadcasts each input event to all extensions that registered for that
input type. There is no fallback routing:

1. **Extensions registered for the input type?** → broadcast to all of them.
   Each extension self-filters based on the payload (entities on tile,
   adjacent entities, range, LOS, equipment) and replies asynchronously. All
   replies are applied.
2. **No extension registered?** → `ActionResultFrame{ ok: false, reason: "no_handler" }`.

The kernel does not have a `TriggerSystem` or `ZoneSystem` — all trigger and
zone behavior is implemented by extensions. The kernel only computes spatial
data (range, LOS, entities) and broadcasts.

## 3a. Input handlers

Input handlers are a third trigger category (alongside access and event
triggers). They are registered by extensions for specific input types
(`click:left`, `click:right`, `key:E`, etc.) and fire when a player triggers
that input. Unlike access triggers (which gate movement) and event triggers
(which notify on enter/exit), input handlers are **player-initiated** and
**broadcast-based** — the kernel does not gate, it provides data (range, LOS,
entities, equipment) and lets each extension decide.

### Use cases

- **Ranged actions**: shooting a bow, throwing an object at a distant tile.
  The extension checks `range` and `has_los` from the payload.
- **Adjacent interactions**: clicking a tile next to the player to interact
  with whatever is on it. The extension checks `entities_on_tile`.
- **Key-based interactions**: pressing E to interact with adjacent entities.
  The extension checks `adjacent_entities`.
- **Equipment-dependent actions**: the action depends on what the player is
  holding (e.g. bow → spawn arrow, empty-handed → no action). The kernel
  includes an equipment snapshot in the dispatch payload.

### Registration

See `18-extensions.md` §3a for the full registration protocol. An input
handler declares:

```
{
  "trigger_id": "combat-click-left",
  "category": "action",
  "binding": "input",
  "input": "click:left",
  "owner_extension_id": "combat-ext"
}
```

### Kernel dispatch

When a player triggers an input event:

1. **Compute contextual data**: for clicks — `target_tile`, `entities_on_tile`,
   `range` (tile distance), `has_los` (Bresenham raycast). For key presses —
   `adjacent_entities`. Always — `equipment` snapshot.
2. **Broadcast** to all extensions registered for that input type.
3. **Collect replies** within a timeout. All replies are applied (updates +
   `consume_items`). Single `ActionResultFrame` sent to the client.
4. **No extension registered** → `ActionResultFrame{ ok: false, reason: "no_handler" }`.
5. **No reply within timeout** → `ActionResultFrame{ ok: false, reason: "timeout" }`.

See `18-extensions.md` §3a and `10-world-simulator.md` §5f for details.

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
