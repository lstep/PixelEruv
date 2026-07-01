# World Simulator

This document specifies the **World Simulator** — the Go service that is the
**spatial authority** and **replication gateway** for the virtual world. It owns
the ECS, the tile grid, the trigger registry, the zone boundaries, and the
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
  the zone boundary registry. It validates every position change (player or
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
   owner, behavior, tiles/entities), and the zone boundary registry (zone_id →
   boundaries, owner). See §5a.
3. **Player avatar movement** — the **only** gameplay system in the kernel.
   Processes client input, computes target tile, evaluates access triggers on
   the target tile, and updates the entity's Position. This is the in-kernel
   exception: it is latency-critical and deployment-invariant. See §5b.
4. **Trigger evaluation** — when an entity (player or extension) attempts to
   enter a tile, the kernel evaluates access triggers (`block`/`allow`/`ask`)
   on that tile. `block` and `allow` are cached locally (no NATS round-trip).
   `ask` triggers publish a query to the owning extension and defer the move
   until the reply arrives (or timeout). See §5c.
5. **Event trigger dispatch** — after a move succeeds, the kernel fires event
   triggers (`notify`) on the entered/exited tile. Entity-bound `notify`
   triggers are sent to the owning extension (point-to-point). Tile-bound
   `notify` triggers are broadcast to all extensions. See §5d.
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
    heartbeats, and routes client interactions to the owning extension. See
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
├── Trigger registry      (trigger_id → owner_extension, category, behavior, tiles/entities)
├── Zone registry         (zone_id → boundaries, owner_extension)
├── Player movement       (input → target tile → trigger evaluation → Position update)
├── Trigger evaluator     (access: block/allow/ask; event: notify)
├── Replication encoder   (component-based, per-client batches)
├── AOI manager           (per-client area-of-interest filter)
├── NATS subscriber       (client input, connect/disconnect, extension updates, trigger replies)
├── NATS publisher        (per-client replication batches, cross-shard events, trigger queries)
├── JetStream KV client   (player positions, player status; reads zone state)
├── PocketBase client     (HTTP: user lookup/create, world config, audit)
└── Extension manager     (registration, entity hosting, heartbeat, interaction routing)
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
  1. Drain input queue         ← client inputs from NATS subscriber
  2. Drain event queue         ← connect/disconnect events
  3. Drain extension queue     ← entity updates, spawn/despawn from extensions
  4. Drain trigger reply queue ← ask-trigger replies from extensions
  5. Process player avatar movement (in-kernel):
     a. For each player with input:
        - Compute target tile from input direction
        - Evaluate access triggers on target tile (see §5c)
        - Any block trigger? → refuse, don't move
        - All allow or no triggers? → accept, update Position, mark dirty
        - Has ask trigger? → mark move as pending, publish query to extension
  6. Resolve pending moves (ask-trigger replies / timeouts):
     a. For each pending move with all replies received:
        - All approved? → accept, update Position, mark dirty
        - Any refused? → refuse
     b. For each pending move whose timeout expired:
        - Refuse (fail closed, see §5c)
  7. Apply extension entity updates:
     a. Validate each update (position bounds, collision, trigger evaluation)
     b. Apply valid updates, mark dirty
     c. Reject invalid updates, publish error to extension
  8. Fire event triggers for completed moves (see §5d):
     a. For each entity that entered a tile:
        - Entity-bound notify triggers → publish to owning extension
        - Tile-bound notify triggers → broadcast to all extensions
     b. For each entity that exited a tile:
        - Same for exit events
  9. Replication system:
     a. Collect dirty components
     b. Apply AOI filter per client
     c. Encode per-client replication batches
     d. Publish batches to NATS Core (client.<id>.replication)
  10. Clear dirty flags
  11. Persist changed player positions to JetStream KV (if any changed)
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
- **Base zone boundaries** — polygon regions defined in Tiled.

### Trigger registry

Extensions register triggers at init time (via NATS request/reply). The kernel
stores them in the trigger registry and indexes them in the spatial index.

A trigger registration declares:

```
{
  "trigger_id": "wall-lobby-north",
  "owner_extension": "walls-v1",
  "category": "access" | "event",
  "binding": "tile" | "entity",
  "tiles": [...],           // if tile-bound
  "entity_id": "...",       // if entity-bound
  "event": "enter" | "exit" | "interact",  // for event triggers
  "behavior": "block" | "allow" | "ask",   // for access triggers
  "default_on_timeout": "block",           // for ask
  "ttl_ms": 500                            // for ask
}
```

| Category | Types | Gates movement? | Kernel waits? |
|---|---|---|---|
| **Access** | `block`, `allow`, `ask` | Yes | `block`/`allow`: no (cached in spatial index). `ask`: yes (pending, async) |
| **Event** | `notify` | No | No (fire and forget) |

### Spatial index

The spatial index maps each tile to:

- **Access triggers** on that tile (with their behavior: `block`/`allow`/`ask`).
- **Event triggers** on that tile (with their event type and binding).
- **Entities** currently on that tile.

This allows O(1) lookup during movement validation: "what triggers are on the
target tile?" and "what entities are on the target tile?"

### Zone registry

Zones are regions with associated triggers. A zone is created either from Tiled
(base zones) or dynamically by an extension at init time. The kernel stores:

- **Zone boundaries** (polygon or tile set).
- **Zone owner** (the extension that registered the zone, if any).

Zone *behavior* (exclusivity, knock-to-join, timers, access policies) is
implemented by the owning extension via triggers. The kernel does not know what
a "meeting room" or an "exclusive zone" is — it knows "zone Z has trigger T on
its boundary tiles, owned by extension E." When an entity attempts to enter the
zone, the kernel evaluates the trigger like any other tile trigger.

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
3. Evaluates access triggers on the target tile (see §5c).
4. If the move is allowed, updates the entity's `Position` component and marks
   it dirty.
5. If the move is refused, the entity stays at its current position. The
   refusal is implicit — the client's prediction is reconciled by the
   unchanged authoritative position (see `12-netcode.md`).

Extension-driven entities (NPCs, objects) do **not** use this system. They
publish position updates via NATS (`entity.<entity_id>.update`), which the
kernel validates and applies in step 7 of the tick loop.

---

## 5c. Access trigger evaluation

When any entity (player or extension) attempts to enter a tile, the kernel
evaluates all access triggers on that tile:

### Multiple triggers per tile: any-refusal-blocks

A tile can have triggers from multiple extensions (e.g. a wall trigger from the
walls extension AND a zone trigger from the meeting extension). The resolution
policy is **any-refusal-blocks** (AND logic): all access triggers must approve
the move. If any trigger refuses, the move is blocked.

### Evaluation algorithm

```
For each access trigger on the target tile:
  ├── behavior = "block"  → refuse immediately (cached, no NATS)
  ├── behavior = "allow"  → continue to next trigger
  └── behavior = "ask"    → publish query to owning extension, mark pending

After all triggers evaluated:
  ├── Any trigger refused (block)? → move refused
  ├── All triggers approved (allow or no triggers)? → move allowed
  └── Has pending ask triggers? → move deferred until replies arrive
```

### `ask` trigger resolution

`ask` triggers are asynchronous. The kernel publishes a query to the owning
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

## 5d. Event trigger dispatch (`notify`)

After a move succeeds (all access triggers approved), the kernel fires event
triggers on the entered and exited tiles. `notify` triggers do not gate
movement — they are notifications, fire-and-forget.

| Binding | Routing | When it fires |
|---|---|---|
| **Entity-bound** | Kernel sends to the extension that owns the entity (point-to-point, via `ExtensionOwner` or trigger owner) | When the registered event involves that entity |
| **Tile-bound** | Kernel publishes to a broadcast subject; all extensions receive it and self-filter | When the registered event happens on that tile |

### Events

| Event | Fires when | Entity-bound example | Tile-bound example |
|---|---|---|---|
| `enter` | Entity arrived at a tile | NPC notices a player approached | Welcome mat plays a sound |
| `exit` | Entity left a tile | NPC stops following | Meeting room decrements occupancy |
| `interact` | Client pressed interact key targeting an entity | NPC starts dialogue | Floor switch activates |

The `interact` event for entity-bound triggers unifies with the existing
interaction routing (see `18-extensions.md` §6). When a client interacts with an
entity, the kernel checks for entity-bound `notify` triggers with event
`interact` and forwards the interaction to the owning extension. This is the
same mechanism, named consistently.

### Broadcast scaling note

Tile-bound `notify` broadcasts to all extensions on every event. This is fine
for a small number of extensions (MVP: 5–10). If extension count grows, switch
to a subject-based subscription model where extensions subscribe only to trigger
IDs they care about. This is a known scaling limit, not an issue for the MVP.

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
3. Re-registers all entities in the ECS.
4. Resumes the tick loop.
5. Sends a fresh initial snapshot to each connected client (via NATS → Pusher
   → client).
6. Publishes `world_sim.restarted` on NATS Core so extensions can re-register
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
| `entity.<entity_id>.interact.reply.<req_id>` | Extension | Interaction response | Async |
| `trigger.<trigger_id>.reply` | Extension | Access trigger reply (for `ask`) | Async |

### Outbound (published by the World Sim)

| Subject | Subscriber | Payload | Frequency |
|---|---|---|---|
| `client.<client_id>.replication` | Pusher (owning the client) | `ReplicationBatch` (protobuf) | Per tick |
| `client.provisioned` | LiveKit Bridge | `{client_id, entity_id, zone_id}` | On connect (after provisioning) |
| `world.<shard_id>.volatile` | Other World Sim shards | Entity position/state updates | Per tick (cross-shard) |
| `world_sim.restarted` | Extensions | `{shard_id, timestamp}` | On restart |
| `extension.<ext_id>.registered` | Extension | Registration response | On registration |
| `entity.<entity_id>.interact` | Extension (owning the entity) | Forwarded client interaction | Event-driven |
| `entity.<entity_id>.arrived` | Extension | Reached movement target | On arrival |
| `entity.<entity_id>.despawned` | Extension | Entity removed | Event-driven |
| `trigger.<trigger_id>.query` | Extension (owning the trigger) | Access trigger query (for `ask`) | On move attempt |
| `trigger.notify.tile.<map_id>.<x>.<y>` | All extensions (broadcast) | Tile-bound event trigger notification | On enter/exit |
| `extension.<ext_id>.error` | Extension | Validation error | On error |
| `admin.revoke.<entity_id>` | Pusher (all instances) | Force-disconnect a user | On admin kick |

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
  trigger logic, zone behavior, custom game mechanics). These are extension
  responsibilities, communicated via NATS. The kernel's only gameplay system
  is player avatar movement.
- ❌ Does not decide what a trigger does — it routes trigger queries to the
  owning extension and caches the result for `block`/`allow` triggers.
- ❌ Does not decide zone behavior (exclusivity, knock-to-join, timers) — it
  stores zone boundaries and routes zone-entry triggers to the owning
  extension.

The World Simulator is the **spatial authority and replication gateway**: it
owns the ECS, the tile grid, the trigger registry, the zone boundaries, the AOI
filter, and the replication encoder. It validates all entity changes (including
its own and extensions') against collision, zone access, and trigger rules. It
moves player avatars. Entity behavior itself — for all non-player entities —
comes from peer extensions via NATS.
