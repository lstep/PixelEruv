# Zones and Interactions

> **Status:** partial. The area-of-interest (AOI) algorithm is an open
> decision; the zone and knock-to-join models below are specified because
> they are MVP features (see `02-functional-requirements.md` § 1).

This document specifies zones (first-class kernel regions with shapes and
mobility), the knock-to-join meeting-room flow, and the area-of-interest
filter that bounds per-client bandwidth.

> **Vocabulary note:** The trigger model has been unified. There are two
> trigger types: **Zone Triggers** (spatial transitions, with a mode: gate or
> notify) and **Input Triggers** (player-initiated actions). See
> `CONTEXT.md` for the canonical glossary and `docs/adr/0001` for the
> block-wins gate resolution decision.

---

## 1. Zones

- Zones are **first-class kernel objects** representing regions on a map
  (authored in Tiled, see `15-maps-and-tiled.md`). Each zone has a `zone_id`,
  a **Shape** (polygon, circle, or rect), a **Mobility** (static or mobile),
  and metadata.
- Each zone has typed characteristics: `water`, `exclusive`, `work`,
  `silent`, `meeting`, plus an optional `owner`.
- Zone boundaries are stored by the World Simulator (the spatial authority).
  Zone behavior (exclusivity, knock-to-join, timers, access policies) is
  implemented by extensions via **zone triggers** registered on the zone
  (see `18-extensions.md` §3). The kernel evaluates gate triggers and routes
  ask-behavior gates to the owning extension; it does not implement zone
  behavior itself.
- Static zones are pre-rasterized to a tile set at load time for O(1)
  point-in-zone lookup. Mobile zones (circle-only) are evaluated per-tick via
  distance checks.
- Dynamic zone state lives in JetStream KV (written by the owning extension;
  the kernel and the LiveKit Bridge read it via `kv.Watch`)
  (`zones.<zone_id>.properties`, `zones.<zone_id>.owner` — see
  `06-data-model-and-persistence.md` § 2).

### Exclusive zones (dynamic)

Activated/deactivated at runtime — e.g. a room whose `exclusive` zone follows
its door state (open door → inactive, closed door → active). The owning
extension (e.g. a doors extension) writes the new state to KV and switches the
zone's gate trigger behavior between `block` and `allow` (or `ask`).
`kv.Watch` fires; the World Sim replicates the visual filter to clients and
the LiveKit Bridge cuts audio/video subscriptions for outsiders.

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
   meeting-v1) receives a zone notify trigger (enter event) → extension sets
   owner = A (KV).
2. User B walks into the zone boundary.
3. The kernel evaluates the gate trigger on the zone boundary → publishes
   query to the meeting extension → extension checks if B needs admission →
   replies block (B cannot cross).
4. Extension emits a "knock" event to owner A (via replication → ControlFrame
   or a dedicated knock message — see 07-network-protocol.md).
5. A's client shows a popup: Allow / Deny.
   - Allow → extension grants B temporary access (updates its internal state
     or KV) → next gate trigger reply for B is allow → B may cross.
   - Deny  → extension keeps replying block → B stays blocked; optional
     notification to B.
6. A can also directly invite another user → same grant mechanism.
```

> **[RESOLVED]** See §8 for the exact wire protocol. Uses transient popup
> entities + `ActionFrame` (input trigger) — no new frame types.

---

## 3. Interaction routing

Interactions are handled by the **input trigger** model (see `18-extensions.md`
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

## 3a. Input triggers

Input triggers are one of two trigger types (the other being **zone
triggers**). They are registered by extensions for specific input types
(`click:left`, `click:right`, `key:E`, etc.) and fire when a player triggers
that input. Unlike zone triggers (which fire on spatial transitions — gate
mode gates movement, notify mode observes enter/exit), input triggers are
**player-initiated** and **broadcast-based** — the kernel does not gate, it
provides data (range, LOS, entities, equipment) and lets each extension
decide.

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
trigger declares:

```
{
  "trigger_id": "combat-click-left",
  "type": "input",
  "input": "click:left",
  "owner_extension": "combat-ext"
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

## 6. Zone shape authoring in Tiled

Zones are authored as objects on a dedicated Tiled object layer named
**`Zones`**. The kernel scans this layer at map load time and registers each
object as a zone.

### Shape mapping

| Our shape | Tiled shape | How the kernel reads it |
|---|---|---|
| `rect` | Rectangle | `x`, `y`, `width`, `height` → axis-aligned rect in tile coords |
| `circle` | Ellipse | Requires `width == height` (in pixels). `radius = width / 2 / tile_width`. If `width != height`, the map load **errors** — use a polygon approximation for elliptical zones. |
| `polygon` | Polygon | `polygon` array of points → polygon in tile coords |

**Rotation is ignored.** The `rotation` field on Tiled objects is not read —
all shapes are treated as axis-aligned. If a rotated rect is needed, draw a
polygon with the rotated corners. This avoids oriented-rectangle math and
rotated polygon rasterization for the MVP.

### Zone identification

- **Layer name**: `Zones`. The kernel scans all objects on this layer. Zones
  cannot be on other layers.
- **Object `name`** → `zone_id`. Must be unique within the map. Example:
  `meeting-room-1`.
- **Object `class`** (formerly `type`): optional, set to `"zone"` for
  editor color-coding and filtering. The kernel does not require it — layer
  membership is the identifier.

### Zone properties (custom properties on the object)

| Property | Type | Required | Default | Notes |
|---|---|---|---|---|
| `is_exclusive` | bool | no | false | If true, AOI filter excludes entities inside from non-members' replication. Static zones only. |
| `zone_type` | string | no | (none) | Hint for the owning extension: `"meeting"`, `"water"`, `"work"`, `"silent"`, etc. The kernel interprets `"spawn"` (player spawn point) and `"portal"` (map transition with `target_map`/`target_entity`) directly; other values are passed to extensions. |
| `mobility` | string | no | `"static"` | `"static"` or `"mobile"`. Mobile zones must be circles (ellipses with `width == height`). |
| `follows_entity_id` | string | only if `mobility: "mobile"` | — | The entity ID (or entity name from Tiled) the zone follows. |
| `owner_ext` | string | no | (derived at runtime) | Which extension owns this zone. Can be left unset and claimed at init time. |

### Mobile zone constraints

- Mobile zones **must** be circles (Tiled ellipses with `width == height`).
- The `follows_entity_id` must reference an entity that exists in the ECS
  (either a base entity from Tiled or one spawned by an extension at init
  time).
- The kernel evaluates mobile circle zones per-tick via distance checks, not
  rasterization.

### Example Tiled JSON

```json
{
  "name": "Zones",
  "type": "objectgroup",
  "objects": [
    {
      "name": "meeting-room-1",
      "class": "zone",
      "x": 320, "y": 256,
      "width": 192, "height": 128,
      "type": "rectangle",
      "properties": [
        { "name": "is_exclusive", "type": "bool", "value": true },
        { "name": "zone_type", "type": "string", "value": "meeting" }
      ]
    },
    {
      "name": "guard-vision",
      "class": "zone",
      "x": 480, "y": 480,
      "width": 96, "height": 96,
      "ellipse": true,
      "properties": [
        { "name": "mobility", "type": "string", "value": "mobile" },
        { "name": "follows_entity_id", "type": "string", "value": "guard-1" }
      ]
    }
  ]
}
```

### Map load errors

The kernel rejects the map at load time if:
- An object on the `Zones` layer has an ellipse with `width != height` (use a
  polygon for elliptical zones).
- A mobile zone is not a circle (mobile polygons are not supported).
- A mobile zone has no `follows_entity_id` property.
- Two objects on the `Zones` layer share the same `name` (zone_id collision).

---

## Open questions

- **[OPEN] AOI algorithm** (grid / quadtree / distance).
- **[RESOLVED] Knock/invite wire protocol** — see §8 below.
- **[OPEN] Silent / water / work zone semantics** — full behavior table.
- **[RESOLVED] Zone shape authoring** — see §6 below.
- **[RESOLVED] Zone trigger registration timing** — see §7 below.

---

## 7. Zone trigger registration timing

When the kernel starts up, it loads the map and restores player positions from
JetStream KV. Extensions register at init time, including zone triggers. At
the moment a trigger is registered, players may already be standing inside the
zone. The resolution:

### Gate triggers: no retroactive evaluation

A gate fires when a player **attempts to move to** a target tile covered by
the gate. If a player is already on a tile when a gate is registered on that
tile, there is no move attempt to evaluate — the player stays. The gate
applies to their **next** move.

Consequences:
- A player already inside a `block` zone can leave (moving to a non-gated
  tile outside the zone), but cannot move to another tile inside the zone
  (the gate blocks the target tile), and cannot re-enter after leaving.
- There is no "eject" — the gate controls movement, not position. If an
  extension needs to eject players from a zone at init time, it must do so
  explicitly by sending entity move commands (the kernel validates them
  against any existing gates on the target tile).

### Notify triggers: no synthetic events

Notify triggers only fire on actual zone transitions (a player crosses the
zone boundary **after** registration). No synthetic `enter` events are fired
for players already inside at registration time.

### Initial occupancy: `query_occupancy` API

Extensions that need to know who is inside a zone at init time use a
request/reply query:

| Subject | Pattern | Reply |
|---|---|---|
| `zone.<zone_id>.query_occupancy` | Extension → World Sim (request/reply) | `entity_ids: [...]` — list of entity IDs currently inside the zone |

The kernel computes this by checking each entity's tile against the zone's
pre-rasterized tile set (for static zones) or distance check (for mobile
circle zones). This is O(entities × zone_tiles) but only called once at init,
so the cost is acceptable. For very large zones, the kernel can optimize by
iterating the zone's tile set and looking up entities on each tile via the
spatial index.

Extensions call this after registering their zone triggers, during the init
sequence:

```
1. Extension registers zone + gate/notify triggers
2. Extension calls zone.<zone_id>.query_occupancy
3. Kernel replies with current entity IDs inside the zone
4. Extension initializes its occupancy state from the reply
5. From this point, the extension tracks enter/exit events from notify triggers
```

---

## 8. Knock/invite wire protocol

The knock-to-join flow uses **transient popup entities + `ActionFrame` (input
trigger)**. No new frame types are introduced — the existing input trigger
broadcast handles everything.

### Knock flow

```
1. B walks into the zone's gate
   → Kernel evaluates gate trigger (ask) → publishes query to meeting-ext
   → meeting-ext: B is not on guest list, policy = "knock"
   → meeting-ext replies: { decision: "block", reason: "knock" }
   → B is blocked at the boundary

2. meeting-ext spawns a transient "knock-popup" entity on owner A's tile
   → Entity components:
     · Position { x: A.x, y: A.y }
     · Interactable { type: "knock_response" }
     · KnockPopup { knocker: "B", popup_id: "knock-123" }
     · ExtensionOwner { ext_id: "meeting-v1" }
     · (no AvatarAppearance — client renders as UI overlay)
   → Replication encoder sends SpawnEntity to A (popup is in A's AOI)
   → A's client: plays "toc-toc" sound, shows popup: "B wants to enter. [Allow] [Deny]"

3. A clicks "Allow" (or "Deny")
   → A's client sends ActionFrame { input_type: "click:left" } targeting popup's tile
   → Kernel broadcasts to all extensions registered for "click:left"
   → meeting-ext self-filters: sees popup entity in entities_on_tile
   → meeting-ext processes response:
     · Allow: add B to guest list → despawn popup (DestroyEntity to A)
       → optionally: send NotifyComponent to B { text: "You may enter." }
     · Deny: despawn popup → B stays blocked

4. B walks toward the boundary again
   → gate trigger fires → meeting-ext: B is on guest list → reply: allow
   → B enters the zone
```

### Invite flow (without knocking)

```
1. A clicks a "zone-control" entity on A's tile (spawned by meeting-ext)
   → ActionFrame { input_type: "click:left" } → kernel broadcasts
   → meeting-ext self-filters: sees zone-control entity in entities_on_tile
   → A selects "Invite B" from the menu
   → A's client sends another ActionFrame with params:
     { action: "invite", target: "B" }

2. meeting-ext processes the invitation
   → Add B to guest list
   → Send component update to B:
     InviteNotification { zone: "meeting-room-1", from: "A", expires_at: "..." }
   → B's client shows: "A invited you to meeting-room-1. Click to enter."

3. B walks toward the boundary
   → gate trigger fires → meeting-ext: B is on guest list → allow
   → B enters the zone
```

### Popup entity

The popup is a regular ECS entity owned by the meeting extension. It has:

| Component | Purpose |
|---|---|
| `Position` | On the owner's tile (so it's in the owner's AOI) |
| `Interactable` | Tells the client to render a popup UI overlay |
| `KnockPopup` | Popup-specific data: `knocker`, `popup_id` |
| `ExtensionOwner` | The meeting extension owns this entity |

No `AvatarAppearance` — the client's extension component renderer handles the
UI. The popup is invisible to other players (it's only on A's tile, only
replicated to A).

### Popup timeout

The popup entity despawns after a timeout (e.g. 30s) if the owner doesn't
respond. This is **extension-side logic** — the meeting-ext sets a timer and
despawns the popup if no `ActionFrame` response arrives. The knocker stays
blocked. The kernel does not need to know about popup timeouts.

### Edge cases

- **Owner disconnects:** the grace period (`10-world-simulator.md` §5e)
  applies. The owner's avatar stays for 30s. If the owner reconnects, the
  popup is still pending (the extension holds the knock state). If the owner
  despawns, the extension clears the owner and the zone becomes open (or
  closed, depending on policy).
- **Multiple knockers:** the extension can spawn multiple popup entities (one
  per knocker) or queue them. The owner sees them one at a time or in a list.
  This is extension-side UX logic.
- **Guest list lifetime:** the extension decides when to clear the guest list
  (on entry, on owner leave, or on explicit revoke). This is extension policy.
