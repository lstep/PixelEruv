# World Simulator

This document specifies the **World Simulator** — the Go service that is the
**spatial authority** and **replication gateway** for the virtual world. It owns
the ECS, the tile grid, the trigger registry, the zone registry, and the
replication pipeline. It does **not** run gameplay logic for non-player entities
— all such behavior is delegated to extensions (see `18-extensions.md`).

> The World Simulator's counterpart is the **Pusher** (a thin WebSocket proxy),
> specified in `09-pusher.md`. The two services communicate exclusively via
> NATS Core. See `09-pusher.md` §1 for the rationale behind the split and the
> canonical responsibility matrix.

---

## 1. Role in the system

The World Simulator is the **spatial authority** and the **replication gateway**.

- **Spatial authority** — it owns the tile grid (loaded from Tiled), the
  spatial index (tile → triggers, tile → entities), the trigger registry, and
  the zone registry. It validates every position change (player or
  extension entity) against the spatial index. It does not decide *what* an
  entity does — it decides *where* an entity can be.
- **Replication gateway** — it owns the AOI filter, the replication encoder,
  and the per-client batch publishing. Extensions never talk to the Pusher or
  the client directly.
- **Validator** — it enforces collision, zone access, position bounds, and
  component schema for all entities, regardless of who drives them.

It does **not** handle WebSocket connections. It does **not** validate OIDC
tokens. It does **not** talk to the browser directly. It does **not** run
gameplay systems for non-player entities (NPCs, triggers, zone behaviors, AI).
Those are all extension responsibilities, communicated via NATS.

```
                NATS Core
                   ▲ ▼
          ┌────────┴────────┐
          │  World Simulator │
          │                  │
          │  ECS (Ark)       │
          │  Spatial index   │     ┌──────────┐
          │  Trigger registry│────►│ PocketBase│
          │  Zone registry   │     └──────────┘
          │  AOI manager     │
          │  Replication     │     ┌──────────┐
          │  encoder         │────►│ JetStream │
          │  Player movement │     │   KV     │
          │  KV client       │     └──────────┘
          │  PB client       │
          │  Extension mgr   │
          └──────────────────┘
```

### Design principle: kernel vs. extensions

The split between the World Sim (kernel) and extensions follows one criterion:

**Kernel = deployment-invariant infrastructure. Extensions = deployment-specific
behavior.**

- "Every deployment needs this, with the same logic" → kernel.
- "This deployment wants it differently, or not at all" → extension.

This means the kernel holds everything that cannot be delegated without either
breaking the model or adding a critical-path dependency on a non-kernel
component:

| Irreducible kernel job | Why it can't be an extension |
|---|---|
| ECS store | Shared mutable state all extensions write into. If every extension had its own ECS, you'd need a merge layer for AOI/replication. |
| Replication read-path (AOI + encode + publish) | There is one client stream (via the Pusher); exactly one component must own "what this client sees this tick." |
| Neutral validation (collision, zone access, bounds, schema) | The arbiter cannot be an untrusted extension. |
| Spatial index + trigger registry | The kernel needs the spatial index to validate positions and evaluate triggers. It must be local, not a NATS round-trip away. |
| Player avatar movement | Latency-critical (input → position → replication in one tick) and deployment-invariant. |
| Identity → entity provisioning | On the connect path. A downed extension would block all connects. |
| Player position/status persistence | Persisting kernel-owned state. |
| Extension lifecycle (register, heartbeat, freeze/despawn) | Someone must host the entities extensions drive. |

Everything else — NPC behavior, trigger logic, zone behavior, AI, custom game
mechanics — is an extension. The kernel routes trigger queries to extensions; it
does not implement trigger behavior itself.

---

## 2. Responsibilities

1. **ECS host** — owns the authoritative entity-component-system (Ark). See
   `13-ecs-design.md`. All entities — whether driven by the kernel's player
   movement or by extensions — live in the same ECS.
2. **Spatial authority** — owns the tile grid (from Tiled), the spatial index
   (tile → triggers, tile → entities), the trigger registry (trigger_id →
   owner, mode, zone), and the zone registry (zone_id → shape, mobility,
   owner). See §5a.
3. **Player avatar movement** — the **only** gameplay system in the kernel.
   Processes client input, computes target tile, evaluates gate triggers on
   the target tile, and updates the entity's Position. This is the in-kernel
   exception: it is latency-critical and deployment-invariant. See §5b.
4. **Trigger evaluation** — when an entity (player or extension) attempts to
   enter a tile, the kernel evaluates gate-mode zone triggers (`block`/`allow`/
   `ask`) covering that tile. `block` and `allow` are cached locally (no NATS
   round-trip). `ask` gates publish a query to the owning extension and defer
   the move until the reply arrives (or timeout). See §5c.
5. **Zone notify trigger dispatch** — after a move succeeds, the kernel fires
   notify-mode zone triggers for the entered/exited zones. Zone notify triggers
   are sent to the owning extension (point-to-point). Proximity triggers
   (mobile circle zones in notify mode) are evaluated per-tick (see §5d).
6. **Input trigger dispatch** — when a player triggers an input event (via
   `ActionFrame`), the kernel computes range, LOS, and entities on the clicked
   tile (or adjacent entities for key presses), then broadcasts to all
   extensions that registered for that input type. All replies within a timeout
   are applied. See §5f.
7. **Line-of-sight raycasting** — the kernel can raycast through the tile grid
   (Bresenham line algorithm) to determine if a player has line-of-sight to a
   target tile. A tile blocks LOS if it has a `block` gate trigger, a
   non-traversable entity (`Traversable=false`), or is a wall in the map. The
   result is included in the input trigger dispatch payload — the kernel does
   not gate on it. See §5f.
6. **Replication encoder** — encodes dirty components into generic replication
   messages (`SpawnEntity`, `UpdateComponent`, `DestroyEntity`,
   `PlayAnimation`). See `11-replication.md`.
7. **AOI manager** — computes the area-of-interest per client; only entities
   within a client's AOI are replicated.
8. **State-store access** — reads/writes JetStream KV (player positions, player
   status; reads zone state written by extensions) and PocketBase (user
   lookup/create, world config, audit log). These are infrastructure
   dependencies for provisioning and persistence, not gameplay.
9. **Identity → entity provisioning** — on `client.connected` from the Pusher,
   looks up or creates the user in PocketBase, restores position from
   JetStream KV, and registers the entity in the ECS. See
   `08-auth-and-identity.md` §5.
10. **Player position/status persistence** — periodically and on despawn,
    writes the player avatar's position and status to JetStream KV so they
    survive a kernel restart.
11. **Cross-shard communication** — publishes volatile entity state on shared
    NATS Core subjects so other World Sim shards can relay to their interested
    clients.
12. **Extension lifecycle management** — accepts registration from external
    extension processes, hosts their entities in the ECS, validates their
    commands (same rules as its own), applies component updates, tracks
    heartbeats, and broadcasts client input events to registered extensions
    (input trigger model). See
    `18-extensions.md`.
13. **Token revocation execution** — when an admin kick is requested (by an
    admin extension or authorized client), the kernel publishes
    `admin.revoke.<entity_id>` (the Pusher closes the WebSocket) and despawns
    the entity from the ECS. The *policy* (who can kick, under what
    conditions) is deployment-specific and lives in an admin extension; the
    *execution* is in the kernel because it requires ECS mutation.

---

## 3. Internal modules

```
World Simulator process
├── ECS host              (Ark: entities, components)
├── Spatial index         (tile → triggers, tile → entities; from Tiled + extension registrations)
├── Trigger registry      (trigger_id → owner_extension, type, mode, zone/input)
├── Zone registry         (zone_id → shape, mobility, owner_extension)
├── Player movement       (input → target tile → trigger evaluation → Position update)
├── Trigger evaluator     (zone gate: block/allow/ask; zone notify: enter/exit; proximity: mobile circle zones)
├── Input trigger dispatcher (ActionFrame → compute range/LOS/entities → broadcast to registered extensions → collect replies)
├── LOS raycaster         (Bresenham line through tiles; checks walls, block gate triggers, Traversable=false)
├── Proximity evaluator   (per-tick: distance check for each mobile circle zone → enter/exit transitions)
├── Replication encoder   (component-based, per-client batches)
├── AOI manager           (per-client area-of-interest filter)
├── NATS subscriber       (client input, connect/disconnect, extension updates, trigger replies)
├── NATS publisher        (per-client replication batches, cross-shard events, trigger queries)
├── JetStream KV client   (player positions, player status; reads zone state)
├── PocketBase client     (HTTP: user lookup/create, world config, audit)
├── SpriteStore           (auto-seeds sprite_bases from SPRITES_DIR on first run)
├── MapStore              (auto-seeds the default maps record from MAP_DIR on first run)
└── Extension manager     (registration, entity hosting, heartbeat, input trigger dispatch)
```

---

## 4. Goroutine model

- **One goroutine for the replication tick** — the main loop. Drains queues,
  processes player movement, evaluates triggers, applies extension updates,
  runs the replication encoder. This is the hot path.
- **One goroutine for NATS subscription** — receives client input,
  connect/disconnect events, extension updates (spawn/despawn/component
  updates), and trigger replies. Queues them for the next tick.
- **One goroutine for JetStream KV watcher** — receives zone-state change
  notifications (for cross-shard visibility and zone reactivity).
- **One goroutine for PocketBase lookups** — handles user provisioning on
  connect (async, does not block the tick).
- **One goroutine for replication publishing** — encodes per-client batches
  and publishes to NATS Core (can run in parallel with the next tick's
  processing if needed).
- **One goroutine for graceful shutdown** — listens for SIGTERM, drains
  connections.

The replication tick goroutine owns the ECS. Other goroutines communicate with
it via channels (input queue, event queue, extension update queue, trigger
reply queue, KV event queue).

---

## 5. Tick loop

```
Each tick (fixed interval, e.g. 50 ms for 20 Hz):
  1. Drain input queue         ← client inputs (InputFrame + ActionFrame) from NATS subscriber
  2. Drain event queue         ← connect/disconnect events
  3. Drain extension queue     ← entity updates, spawn/despawn from extensions
  4. Drain trigger reply queue ← ask-gate replies + input-trigger replies from extensions
  5. Process player avatar movement (in-kernel):
     a. For each player with input:
        - Compute target tile from input direction
        - Evaluate gate triggers on target tile (see §5c)
        - Any block gate? → refuse, don't move
        - All allow or no gate triggers? → accept, update Position, mark dirty
        - Has ask gate? → mark move as pending, publish query to extension
  6. Process action frames (in-kernel, see §5f):
     a. For each ActionFrame:
        - Determine input type from the frame
        - Look up extensions registered for that input type
        - If no extension registered → ActionResultFrame{ ok: false, reason: "no_handler" }
        - Compute contextual data:
          · clicks: target_tile, entities_on_tile, range, has_los (Bresenham)
          · key presses: adjacent_entities (no target tile, no range, no LOS)
        - Gather equipment snapshot
        - Broadcast to all registered extensions
        - Collect replies (until timeout or next tick) → apply all
  7. Resolve pending moves (ask-gate replies / timeouts):
     a. For each pending move with all replies received:
        - All approved? → accept, update Position, mark dirty
        - Any refused? → refuse
     b. For each pending move whose timeout expired:
        - Refuse (fail closed, see §5c)
  8. Apply extension entity updates:
     a. Validate each update (position bounds, collision, trigger evaluation)
     b. Apply valid updates, mark dirty
     c. Reject invalid updates, publish error to extension
  9. Fire zone notify triggers for completed moves (see §5d):
     a. For each entity that entered a zone:
        - Zone notify triggers → publish to owning extension
     b. For each entity that exited a zone:
        - Same for exit events
  10. Evaluate proximity triggers (mobile circle zones, see §5d):
      a. For each proximity trigger (mobile circle zone in notify mode):
         - Get the followed entity's current Position
         - Find all players within `radius` tiles (spatial index range query)
         - Compare with previous tick's set
         - For each new entry → publish proximity_enter to owning extension
         - For each departure → publish proximity_exit to owning extension
  11. Replication system:
      a. Collect dirty components
      b. Apply AOI filter per client
      c. Encode per-client replication batches
      d. Publish batches to NATS Core (client.<id>.replication)
  12. Clear dirty flags
  13. Persist changed player positions to JetStream KV (if any changed)
```

The tick rate is a trade-off:

- **Higher tick rate** (30 Hz) → smoother movement, lower input latency, but
  more CPU per shard and more NATS traffic.
- **Lower tick rate** (20 Hz) → more headroom for entities per shard, but
  clients must interpolate more aggressively.

> **[OPEN]** The exact tick rate will be tuned during load testing. The
> architecture supports changing it without code changes (configurable
> interval).

---

## 5a. Spatial authority: tile grid, triggers, and zones

### Tile grid

The kernel loads the Tiled map (see `15-maps-and-tiled.md`) at startup. The tile
grid provides:

- **Walkable tiles** — which tiles an entity can occupy.
- **Base entities** — entities defined in the Tiled map (doors, decorations,
  furniture) with their initial components and positions. These entities exist
  in the ECS but have no `ExtensionOwner` component until an extension claims
  them (via trigger registration or entity update).
- **Base zone shapes** — polygon, circle, and rect regions defined in Tiled.

### Trigger registry

Extensions register triggers at init time (via NATS request/reply). The kernel
stores them in the trigger registry and indexes them in the spatial index.

A trigger registration declares:

```
{
  "trigger_id": "wall-lobby-north",
  "owner_extension": "walls-v1",
  "type": "zone" | "input",
  "zone_id": "...",         // if type: "zone"
  "mode": "gate" | "notify", // if type: "zone"
  "shape": "polygon" | "circle" | "rect",  // if type: "zone"
  "mobility": "static" | "mobile",          // if type: "zone"
  "follows_entity_id": "...",               // if mobility: "mobile"
  "radius": 3,                              // if shape: "circle" (tiles)
  "input": "click:left",    // if type: "input"
  "events": ["enter", "exit"],              // if mode: "notify" (zone)
  "events": ["proximity_enter", "proximity_exit"],  // if mode: "notify" (mobile circle)
  "access_behavior": "block" | "allow" | "ask",  // if mode: "gate"
  "default_on_timeout": "block",           // if access_behavior: "ask"
  "ttl_ms": 500                            // if access_behavior: "ask"
}
```

| Type | Mode | Gates movement? | Kernel waits? |
|---|---|---|---|
| **Zone** | `gate` (block/allow/ask) | Yes | `block`/`allow`: no (cached in spatial index). `ask`: yes (pending, async) |
| **Zone** | `notify` (static or proximity) | No | No (fire and forget). Proximity triggers (mobile circle zones) are evaluated per-tick by the kernel. |
| **Input** | — (player-initiated) | No (player-initiated) | No gating — kernel broadcasts with range/LOS data; extensions self-filter |

### Spatial index

The spatial index maps each tile to:

- **Gate triggers** covering that tile (with their access behavior: `block`/`allow`/`ask`).
- **Zone notify triggers** covering that tile (with their event type).
- **Entities** currently on that tile.

This allows O(1) lookup during movement validation: "what gate triggers cover
the target tile?" and "what entities are on the target tile?" The same lookup
is used for input trigger dispatch — the kernel queries entities on the clicked
tile (for clicks) or adjacent tiles (for key presses) to include in the
broadcast payload.

The kernel also maintains a **proximity trigger list** — all registered mobile
circle zones in notify mode with their followed entity ID, radius, and events.
This is separate from the tile-based spatial index because proximity triggers
are entity-centric and mobile, not tile-centric. The kernel evaluates them
per-tick by querying the spatial index for players within each trigger's radius
(a range query over (2×radius+1)² tiles around the followed entity's current
position).

### Zone registry

Zones are first-class kernel objects with shapes and mobility. A zone is
created either from Tiled (base zones) or dynamically by an extension at init
time. The kernel stores:

- **Zone shape** (polygon, circle, or rect) and **mobility** (static or mobile).
- **Pre-rasterized tile set** (for static zones — O(1) point-in-zone lookup).
- **Zone owner** (the extension that registered the zone, if any).

Zone *behavior* (exclusivity, knock-to-join, timers, access policies) is
implemented by the owning extension via zone triggers (gate mode for access
control, notify mode for enter/exit observations). The kernel does not know
what a "meeting room" or an "exclusive zone" is — it knows "zone Z has gate
trigger T, owned by extension E." When an entity attempts to enter the zone,
the kernel evaluates the gate trigger covering the target tile.

---

## 5b. Player avatar movement (in-kernel)

Player avatar movement is the **only** gameplay system in the kernel. It is the
in-kernel exception because:

1. **Latency-critical** — input → position → replication must happen in one
   tick, with no NATS round-trip. Adding a NATS hop to an extension for player
   movement would add ~2 ms × 2 hops of latency on the most frequent action.
2. **Deployment-invariant** — every deployment moves player avatars the same
   way (input direction → target tile → validate → update position).

The movement system:

1. Reads the player's `InputFrame` from the input queue.
2. Computes the target tile from the current position + input direction.
3. Evaluates gate triggers on the target tile (see §5c).
4. If the move is allowed, updates the entity's `Position` component and marks
   it dirty.
5. If the move is refused, the entity stays at its current position. The
   refusal is implicit — the client's prediction is reconciled by the
   unchanged authoritative position (see `12-netcode.md`).

Extension-driven entities (NPCs, objects) do **not** use this system. They
publish position updates via NATS (`entity.<entity_id>.update`), which the
kernel validates and applies in step 7 of the tick loop.

---

## 5c. Gate trigger evaluation

When any entity (player or extension) attempts to enter a tile, the kernel
evaluates all gate-mode zone triggers covering that tile:

### Multiple gate zones per tile: block-wins

A tile can be covered by gate zones from multiple extensions (e.g. a wall gate
from the walls extension AND a zone gate from the meeting extension). The
resolution policy is **block-wins** (AND logic): all gate triggers must approve
the move. If any gate refuses, the move is blocked. For overlapping `ask` gates
owned by different extensions, the kernel queries all in parallel within a
per-tick timeout and blocks if any replies `block`. See ADR 0001.

### Evaluation algorithm

```
For each gate trigger covering the target tile:
  ├── access_behavior = "block"  → refuse immediately (cached, no NATS)
  ├── access_behavior = "allow"  → continue to next trigger
  └── access_behavior = "ask"    → publish query to owning extension, mark pending

After all triggers evaluated:
  ├── Any trigger refused (block)? → move refused
  ├── All triggers approved (allow or no triggers)? → move allowed
  └── Has pending ask gates? → move deferred until replies arrive
```

### `ask` gate resolution

`ask` gates are asynchronous. The kernel publishes a query to the owning
extension and marks the move as pending. The extension replies via NATS. The
kernel processes the reply in the next tick's trigger reply queue drain (step 4
and step 6 of the tick loop).

**Timeout and default policy:** if the extension does not reply within
`ttl_ms` (configurable per trigger, default 500 ms), the kernel applies the
trigger's `default_on_timeout` policy. The default is **block** (fail closed) —
a unreachable extension does not open a security hole. The player experiences a
brief hesitation, not a bypass.

### Why `block` and `allow` are cached

Walls, static obstacles, and open passages register as `block` or `allow`. The
kernel caches them in the spatial index at registration time. Walking into a
wall is a local cache lookup — zero NATS round-trips, smooth movement. The
extension *owns the decision* (it registered the trigger), but it pre-declared
the answer as "always block," so the kernel doesn't need to ask.

`ask` is for dynamic decisions (is the room full? is the player on the guest
list?). These are rare and a one-tick delay is acceptable.

---

## 5d. Zone notify trigger dispatch

After a move succeeds (all gate triggers approved), the kernel fires notify-
mode zone triggers for the entered and exited zones. Notify triggers do not
gate movement — they are notifications, fire-and-forget.

| Binding | Routing | When it fires |
|---|---|---|
| **Zone-bound** (static) | Kernel sends to the extension that owns the zone (point-to-point) | When a player crosses the zone boundary (enter/exit) |
| **Proximity** (mobile circle) | Kernel sends to the extension that owns the zone (point-to-point) | When a player enters or leaves the radius around the followed entity (evaluated per-tick) |

### Events

| Event | Fires when | Zone-bound example | Proximity example |
|---|---|---|---|
| `enter` | Player crossed into a zone | Meeting room increments occupancy | — |
| `exit` | Player crossed out of a zone | Meeting room decrements occupancy | — |
| `proximity_enter` | Player entered the radius around the followed entity | — | Proximity alarm starts ringing; guard enters ALERT |
| `proximity_exit` | Player left the radius around the followed entity | — | Proximity alarm stops (if no other players in radius); guard returns to IDLE |

Player-initiated interactions (clicks, key presses) are handled by input
triggers (see §5f), not by zone notify triggers. Zone notify triggers only
fire on movement (zone enter/exit) and proximity transitions.

### Proximity evaluation

Proximity triggers (mobile circle zones in notify mode) are evaluated once per
tick (step 10 of the tick loop), after all movement has been processed. The
kernel maintains the previous tick's player set for each proximity trigger.
For each trigger:

1. Get the followed entity's current `Position` from the ECS.
2. Query the spatial index for all player entities within `radius` tiles
   (Chebyshev distance: (2×radius+1)² tile lookups).
3. Compare with the previous tick's set.
4. For each player that is in the new set but not the old set → fire
   `proximity_enter` (point-to-point to the owning extension).
5. For each player that is in the old set but not the new set → fire
   `proximity_exit` (point-to-point to the owning extension).

If the followed entity itself moves (e.g. a patrolling guard), the radius
moves with it — the kernel re-evaluates from the entity's new position each
tick.

**Performance:** each proximity trigger costs (2×radius+1)² spatial index
lookups per tick. For radius=3, that's 49 lookups. With a small number of
proximity triggers (typical: a few per map), this is negligible. If proximity
triggers become numerous, the kernel can optimize by maintaining a separate
entity-centric spatial structure (e.g. a uniform grid with cell size = max
radius) for O(1) range queries.

### Broadcast scaling note

Zone notify triggers are point-to-point (sent only to the owning extension),
so there is no broadcast scaling concern. The kernel routes notifications
directly to the extension that registered the zone trigger.

---

## 5e. Session and entity lifecycle

### Connect

On `client.connected` (from the Pusher), the World Sim provisions the entity
(PocketBase lookup, KV position restore), registers it in the ECS, and sends
the initial snapshot (see §5 and `06-data-model-and-persistence.md` §5).

After provisioning, the World Sim publishes a `client.provisioned` event to
NATS Core (with `client_id`, `entity_id`, and initial `zone_id`). The LiveKit
Bridge subscribes to this event to issue a LiveKit room token (see
`08-auth-and-identity.md` §6).

### Disconnect — grace period then despawn

**[DECISION] On `client.disconnected`, the avatar is not removed immediately.
It enters a grace period (e.g. 30 s, configurable) during which it is shown to
others as "away / disconnected".**

```
client.disconnected received
  → mark entity status = "disconnected" (replicated, so others see "away")
  → start grace timer (default 30 s)
  → if the same user reconnects (new client.connected for the same sub/entity)
       before the timer fires → cancel timer, clear "away" status, resume
  → if the timer fires → despawn the entity (DestroyEntity to all),
       persist final position/status to JetStream KV
```

Rationale: a brief network blip (the common case) should not make the avatar
vanish and reappear. The grace period absorbs reconnects (sticky session to
the same Pusher) without disrupting other clients' view.

> The user's last position and status are persisted to JetStream KV on despawn
> (and periodically during the session), so a later login restores them (see
> `06-data-model-and-persistence.md` § 2; KV TTL is 90 days).

> **[OPEN]** Grace-period duration (30 s default) to tune. Whether to show a
> distinct "reconnecting" vs "away" state.

---

## 5f. Input trigger dispatch

When a player triggers an input event (via `ActionFrame`), the kernel broadcasts
to all extensions that registered for that input type. The kernel does **not**
gate on range or LOS — instead, it computes them and includes them in the
payload so each extension can decide for itself.

### Contextual data computation

For **clicks** (input types like `click:left`, `click:right`, `click:double`):

1. **Target tile**: from the `ActionFrame`.
2. **Entities on tile**: looked up via the spatial index.
3. **Range**: Chebyshev distance from the player's `Position` to the clicked
   tile.
4. **Line-of-sight**: Bresenham raycast from the player's tile to the clicked
   tile. For each tile along the ray:
   - **Wall check**: is the tile a wall in the Tiled map? If yes, LOS blocked.
   - **Block gate check**: does the tile have a `block` gate trigger? If
     yes, LOS blocked.
   - **Entity check**: is there a non-traversable entity on the tile
     (`Traversable=false`)? If yes, LOS blocked.
   The ray starts from the tile adjacent to the player (the player's own tile
   doesn't block). The result (`true`/`false`) is included in the payload.

For **key presses** (input types like `key:E`):

1. **Adjacent entities**: entities on all tiles adjacent to the player (up,
   down, left, right), looked up via the spatial index.
2. No target tile, no range, no LOS.

### Equipment snapshot

The kernel includes a snapshot of the player's `Equipment` component in every
dispatch payload, so extensions know what the player is holding.

### Broadcast

The kernel publishes to all extensions that registered for the input type:

```
Subject: input.<input_type_with_dots>
  (e.g. input.click.left, input.key.e)
Payload:
{
  "request_id": "req-123",
  "source_entity_id": "user-42",
  "client_id": "abc123",
  "input": "click:left",
  "target_tile": {"map_id": "arena", "x": 10, "y": 5},
  "player_position": {"map_id": "arena", "x": 2, "y": 5, "dir": "east"},
  "entities_on_tile": ["door-1"],
  "adjacent_entities": null,
  "has_los": true,
  "range": 8,
  "equipment": [
    {"slot": "main_hand", "item_entity_id": "bow-7", "item_type": "bow"},
    {"slot": "off_hand", "item_entity_id": null}
  ],
  "reply_to": "input.click.left.reply.<request_id>"
}
```

### Reply collection

Each extension replies asynchronously:

```
Subject: input.<input_type_with_dots>.reply.<request_id>
Payload:
{
  "request_id": "req-123",
  "extension_id": "combat-ext",
  "updates": [...],
  "consume_items": [...]
}
```

The kernel collects **all** replies within a timeout window (e.g. 500 ms). All
replies are applied — updates are applied to the ECS, `consume_items` removes
items from the player's inventory. The kernel then sends a single
`ActionResultFrame{ ok: true }` to the client. If no reply arrives within the
timeout, the kernel sends `ActionResultFrame{ ok: false, reason: "timeout" }`.

If no extension registered for the input type, the kernel sends
`ActionResultFrame{ ok: false, reason: "no_handler" }` immediately (no NATS
round-trip).

### Performance

Raycasting is O(ray length) — at most the distance to the clicked tile. The
spatial index lookup for entities on tile / adjacent tiles is O(1). The
broadcast is one NATS message per input event per registered extension. NATS
Core handles this volume easily — the kernel does not filter or gate.

---

## 6. Sharding

Multiple World Simulator instances can run in parallel, each owning a shard
of the world:

- **Per-map sharding** (simplest): each World Sim instance owns one or more
  maps. A client on map A is served by the World Sim that owns map A.
- **Per-region sharding** (future): a single large map is split into regions,
  each owned by a different World Sim instance.

### Cross-shard entity visibility

When an entity on shard A is visible to a client on shard B (e.g. near a map
boundary), shard A publishes the entity's volatile state on a shared NATS
Core subject (`world.<shard_id>.volatile`). Shard B subscribes, creates a
"shadow" entity in its ECS, and includes it in its clients' replication
batches.

Shadow entities are read-only in the receiving shard — they reflect state
published by the owning shard. They are not simulated (no movement, no
triggers); they are only rendered by clients.

### Shard assignment

When a client connects, the Pusher publishes `client.connected` to NATS Core.
The World Sim instance that owns the map the user's saved position is on
processes the event. If the user's position is on a map owned by a different
shard, the receiving shard forwards the event to the correct shard via NATS.

> **[OPEN]** Sharding strategy (per-map vs. per-region), the shadow-entity
> mechanism, and shard assignment are to be detailed in a future sharding
> document. The MVP may ship with a single World Sim instance.

---

## 7. Failure recovery

### World Sim crash

The ECS state is lost, but all semi-persistent state (zone properties, user
positions, user status) is in JetStream KV. On restart, the World Sim:

1. Reads all JetStream KV keys for its shard(s) to reconstruct zone state and
   user positions.
2. Reads world configuration from PocketBase.
3. Auto-seeds the default `maps` record from `MAP_DIR` if no record named
   `MAP_ID` exists (idempotent; retries for 30s while PocketBase is booting).
   Mirrors the `SpriteStore.SeedIfEmpty` pattern for `sprite_bases`.
4. Re-registers all entities in the ECS.
5. Resumes the tick loop.
6. Sends a fresh initial snapshot to each connected client (via NATS → Pusher
   → client).
7. Publishes `world_sim.restarted` on NATS Core so extensions can re-register
   their triggers and re-spawn their entities.

Connected clients experience a brief replication pause but do not disconnect
(the Pusher is still running). The pause duration is the restart time plus
one tick.

### NATS disconnect

The World Sim cannot receive input or publish replication. It continues
running the tick loop on the last known input (dead reckoning for a
configurable timeout, e.g. 5 seconds). If NATS is still down after the
timeout, it pauses the tick loop and waits for NATS to recover.

### PocketBase unreachable

User provisioning on connect fails. The World Sim rejects the connection by
publishing an error to the Pusher via NATS (subject
`client.<client_id>.replication` with an error frame), which closes the
WebSocket with `4401 Unauthorized`.

For already-connected clients, PocketBase unavailability does not affect the
replication tick (simulation state is in the ECS and JetStream KV). Only audit
log writes and new user provisioning are impacted.

### Extension crash

The kernel is unaffected. Extension-owned entities freeze (or despawn after a
timeout, per the extension's `on_death` policy). The kernel's trigger
registry retains the crashed extension's `block`/`allow` triggers (they are
cached and don't require the extension to be alive). `ask` triggers from the
crashed extension will time out and fail closed (block). `notify` triggers
from the crashed extension are silently dropped (no one is listening).

When the extension reconnects and re-registers, it re-claims its entities and
re-registers its triggers. See `18-extensions.md` §11.

---

## 8. Communication contract

All communication with other services is via NATS Core. No direct RPC, no
shared memory.

### Inbound (subscribed by the World Sim)

| Subject | Publisher | Payload | Frequency |
|---|---|---|---|
| `client.<client_id>.input` | Pusher | Protobuf input frame | Per client input |
| `client.connected` | Pusher | `{client_id, sub, pusher_instance}` | On connect |
| `client.disconnected` | Pusher | `{client_id, reason}` | On disconnect |
| `world.<shard_id>.volatile` | Other World Sim shards | Entity position/state updates | Per tick (cross-shard) |
| `extension.register` | Extension | Registration request | On extension startup |
| `extension.<ext_id>.register_components` | Extension | Custom component schemas | On extension startup |
| `extension.<ext_id>.deregister` | Extension | Graceful shutdown | On extension shutdown |
| `extension.<ext_id>.heartbeat` | Extension | Liveness signal | Per interval |
| `extension.<ext_id>.spawn` | Extension | Spawn entity | On demand |
| `extension.<ext_id>.despawn` | Extension | Despawn entity | On demand |
| `extension.<ext_id>.batch_update` | Extension | Batched component updates | Per tick or event-driven |
| `extension.<ext_id>.register_triggers` | Extension | Trigger registrations | At init time |
| `extension.<ext_id>.unregister_triggers` | Extension | Remove triggers | On demand |
| `extension.<ext_id>.register_zone` | Extension | Zone registration | At init time |
| `entity.<entity_id>.update` | Extension | Direct component update | Per tick or event-driven |
| `entity.<entity_id>.move` | Extension | Movement target | On demand |
| `trigger.<trigger_id>.reply` | Extension | Gate trigger reply (for `ask`) | Async |
| `input.<input_type>.reply.<req_id>` | Extension | Input trigger response (updates, consume_items) | Async |

### Outbound (published by the World Sim)

| Subject | Subscriber | Payload | Frequency |
|---|---|---|---|
| `client.<client_id>.replication` | Pusher (owning the client) | `ReplicationBatch` (protobuf) | Per tick |
| `client.provisioned` | LiveKit Bridge | `{client_id, entity_id, zone_id}` | On connect (after provisioning) |
| `world.<shard_id>.volatile` | Other World Sim shards | Entity position/state updates | Per tick (cross-shard) |
| `world_sim.restarted` | Extensions | `{shard_id, timestamp}` | On restart |
| `extension.<ext_id>.registered` | Extension | Registration response | On registration |
| `entity.<entity_id>.notify.<event>` | Extension (owning the entity) | Proximity notify dispatch (proximity_enter/proximity_exit for mobile circle zones) | Per-tick |
| `zone.<zone_id>.notify.<event>` | Extension (owning the zone) | Zone notify dispatch (enter/exit for static zones) | On zone boundary crossing |
| `entity.<entity_id>.arrived` | Extension | Reached movement target | On arrival |
| `entity.<entity_id>.despawned` | Extension | Entity removed | Event-driven |
| `trigger.<trigger_id>.query` | Extension (owning the trigger) | Gate trigger query (for `ask`) | On move attempt |
| `input.<input_type>` | All extensions registered for that input type | Input trigger dispatch (player clicked or pressed a key; includes equipment snapshot, range, LOS, entities) | On ActionFrame |
| `extension.<ext_id>.error` | Extension | Validation error | On error |
| `admin.revoke.<entity_id>` | Pusher (all instances) | Force-disconnect a user | On admin kick |
| `healthz` | Pusher (health aggregator) | Health JSON: `{service, status, version, uptime, extras}` with `entity_count`, `connected_players`, `running_extensions` | Every 10s |

> The subject naming convention is illustrative. The final convention will be
> defined in `07-network-protocol.md`.

---

## 9. What the World Simulator does NOT do

This section is explicit because the World Simulator's responsibilities were
previously overloaded (see `09-pusher.md` §1 for the history).

- ❌ Does not handle WebSocket connections or talk to the browser directly.
- ❌ Does not validate OIDC tokens (the Pusher does that; the World Sim
  receives the already-validated `sub`).
- ❌ Does not manage WebSocket sessions (the Pusher does that; the World Sim
  identifies clients by `client_id` from NATS events).
- ❌ Does not proxy media (WebRTC goes directly to LiveKit via Traefik).
- ❌ Does not coordinate with the LiveKit Bridge directly — the Bridge watches
  KV for zone state (written by owning extensions) and subscribes to
  `client.provisioned` for token issuance. The World Sim publishes
  `client.provisioned` (after provisioning); it does not mediate token delivery.
- ❌ Does not run gameplay systems for non-player entities (NPC movement, AI,
  trigger logic, zone behavior, custom game mechanics, inventory, equipment,
  item effects). These are extension responsibilities, communicated via NATS.
  The kernel's only gameplay systems are player avatar movement, input trigger
  dispatch (computing range/LOS/entities and broadcasting to registered
  extensions), and proximity trigger evaluation — all latency-critical and
  deployment-invariant.
- ❌ Does not decide what a trigger does — it routes gate queries to the
  owning extension and caches the result for `block`/`allow` gates. For
  input triggers, it computes range/LOS/entities and broadcasts to all
  registered extensions with an equipment snapshot; each extension self-filters
  and decides what to do. For proximity triggers, it detects enter/exit
  transitions and notifies the owning extension. The extension decides what
  happens in all cases.
- ❌ Does not decide zone behavior (exclusivity, knock-to-join, timers) — it
  stores zone shapes and routes zone-entry triggers to the owning
  extension.

The World Simulator is the **spatial authority and replication gateway**: it
owns the ECS, the tile grid, the trigger registry, the zone registry, the AOI
filter, and the replication encoder. It validates all entity changes (including
its own and extensions') against collision, zone access, and trigger rules. It
moves player avatars. Entity behavior itself — for all non-player entities —
comes from peer extensions via NATS.
