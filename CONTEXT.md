# Pixel Eruv

The shared language for the Pixel Eruv virtual office platform — a
server-authoritative, ECS-based multiplayer world where extensions implement all
gameplay behavior and the kernel owns spatial authority and replication.

## Language

### Core simulation

**World Simulator (the kernel):**
The spatial authority and replication gateway. Owns the ECS, the tile grid, the spatial index, the zone registry, the trigger registry, the AOI filter, and the replication encoder. Its only gameplay system is player avatar movement.
_Avoid_: server, game server, sim

**Extension:**
A peer process on the NATS bus, written in any language, that owns and drives all gameplay behavior — entity logic, trigger logic, zone behavior. Communicates with the kernel exclusively via NATS Core pub/sub and JetStream KV.
_Avoid_: plugin, module, addon, script

**Entity:**
A thing in the ECS world — a player avatar, an NPC, a door, an alarm, a popup. All entities live in the same ECS regardless of whether the kernel or an extension drives them.

**Zone:**
A first-class kernel object representing a region on a map. Has a `zone_id`, a **Shape**, a **Mobility**, and metadata. Zones are the primary unit for spatial trigger evaluation — access and event triggers attach to zones, not to individual tiles. Authored in Tiled as objects on a dedicated `Zones` object layer; stored by the kernel as Zone entities. Shape mapping: Tiled Rectangle → `rect`, Tiled Ellipse (with `width == height`) → `circle`, Tiled Polygon → `polygon`. Rotation is ignored (all shapes axis-aligned). See `14-zones-and-interactions.md` §6.
_Avoid_: area, region, room (a room is a zone with specific semantics, not a synonym)

**Tile:**
The movement and rendering unit. Players move tile-by-tile; collision resolves on the tile grid; rendering tiles. Tiles are the substrate zones are painted onto — they are not the decision boundary for triggers.
_Avoid_: cell, square

**Exclusive zone:**
A static zone marked with an `is_exclusive` flag (written to KV by the owning extension). When a zone is exclusive, the AOI filter excludes entities inside the zone from non-members' replication batches — outsiders don't receive `UpdateComponent` or `SpawnEntity` for entities inside the zone. Only **static zones** can be exclusive; mobile circle zones (proximity triggers) are never exclusive. A "privacy bubble" around a player is not an exclusive zone — it's a per-entity replication filter (like the existing `InventorySlot` owner-only filter), not a zone concept.

### Triggers

**Trigger:**
A registered rule-of-firing owned by an extension that tells the kernel how to make a specific decision at a specific location or condition. There are exactly two trigger types: **Zone Triggers** (spatial transitions) and **Input Triggers** (player-initiated actions).
_Avoid_: rule, handler, callback, event listener

#### Zone Triggers

**Zone Trigger:**
A trigger that fires on spatial transitions into or out of a zone's region. Has a **Mode** (the kind of kernel decision) and inherits the zone's **Shape** and **Mobility**.

**Shape:**
The geometry of a zone. One of:
- **Polygon** — arbitrary polygon (rooms, meeting areas, walls as 1×1 rects). Static zones only.
- **Circle** — defined by center + radius. Used for both static and mobile zones.
- **Rect** — axis-aligned rectangle. A specialization of polygon, common enough (1×1 tile zones, wall segments) to name directly.

**Mobility:**
Whether a zone is fixed to map coordinates or follows an entity. One of:
- **Static** — fixed to map coordinates. The kernel pre-rasterizes the shape to a tile set at load time for O(1) point-in-zone lookup.
- **Mobile** — follows an entity's position each tick. Mobile zones are **circle-only** (per-tick polygon rasterization is a performance trap). The kernel evaluates mobile zones via distance checks, not rasterization.

  > **Limitation: no mobile polygons.** A patrolling guard's cone-of-vision can't be expressed as a mobile polygon zone. The accepted workaround: register a mobile circle zone (notify mode) with a radius covering the cone's reach, and let the extension filter by facing direction on `proximity_enter` (the extension reads the guard's `Position.dir` and the player's relative position). Directionality is gameplay semantics, so it belongs in the extension — consistent with the kernel-vs-extension split. Revisit if extension-side filtering proves too coarse for real use cases.

**Mode:**
The kind of decision the kernel makes when a player crosses a zone trigger's boundary. One of:
- **Gate** — gates movement. Returns allow/block. Evaluated on every movement attempt into the zone. Behaves as a persistent constraint.
- **Notify** — observes transitions and publishes events. No gating. Fires `enter` / `exit` (or `proximity_enter` / `proximity_exit` for mobile circle zones).

**Access behavior:**
The response mode of a gate-mode zone trigger. One of `block` (cached locally, kernel refuses without NATS round-trip), `allow` (cached locally, kernel permits), or `ask` (routed to the owning extension at runtime, fails closed to block on timeout).

**Gate resolution (block-wins):**
When a player attempts to move into a tile covered by multiple gate-mode zones, the kernel checks all of them: if any returns `block` (cached or via `ask` reply), the movement is refused. Only if all overlapping gate zones permit is movement allowed. The kernel caches gate decisions per-zone and intersects at evaluation time. For overlapping `ask` zones owned by different extensions, the kernel queries all **in parallel** within a per-tick timeout and blocks if any replies `block`. See ADR 0001.

**Proximity trigger:**
Shorthand for a mobile circle zone trigger in notify mode. The kernel evaluates it per-tick via a distance check and fires `proximity_enter` / `proximity_exit` on transitions. Not a separate trigger type — proximity is a shape + mobility combination, not a kind of decision.
_Avoid_: range trigger, area trigger

#### Input Triggers

**Input Trigger:**
A trigger that fires when a player initiates a specific input type (`click:left`, `click:right`, `key:E`, etc.). The kernel broadcasts the event to all extensions registered for that input type, along with contextual data (target tile, entities on tile, range, line-of-sight, adjacent entities, equipment snapshot). Each extension self-filters and replies asynchronously; all replies are applied. Input triggers are not spatial — a click is an action, not a transition.
_Avoid_: action trigger (use "input trigger"; "action" refers to the result, not the trigger)

### Zone interaction model

**Zone gate trigger:**
A gate-mode zone trigger that fires when a player attempts to cross into the zone. The kernel routes the decision to the owning extension (if `ask`) or resolves locally (if `block`/`allow` cached). This is distinct from any door, button, or lever the player interacts with — the boundary gate and the interaction mechanism are decoupled triggers.

**Zone notify trigger:**
A notify-mode zone trigger that fires on enter/exit transitions as players cross the zone boundary. Used by extensions to track occupancy and ownership.

**Interaction mechanism (door / button / lever):**
An entity with an input trigger that a player clicks/activates to mutate zone state. May be located on the zone boundary, inside the zone, or elsewhere on the map. The extension wires the input trigger (mutates zone state) to the gate trigger (reads zone state and replies allow/block). The kernel does not know they are related.
