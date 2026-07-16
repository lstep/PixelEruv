# Replication

This document specifies how authoritative game state is transmitted from the
Pusher to connected clients. It covers the message model, the replication
pipeline, delta encoding, and the area-of-interest (AOI) filter.

> **Design principle:** replication is **component-based**, not
> entity-type-based. The protocol is generic: it replicates *components on
> entities*, not "player moved" or "desk updated" events. This means new
> entity types, new components, and new behaviours can be added without
> touching the replication layer or the network protocol.

---

## 1. Why component-based, not entity-type-based

A naive replication scheme defines a message type per entity type per action:

```
PlayerMoved, PlayerStopped, PlayerSatDown
DeskUpdated, DoorOpened, DoorClosed
NPCMoved, NPCStateChanged
```

This approach has three problems:

1. **Proliferation.** Every new entity type or behaviour requires a new message
   type, a new protobuf definition, and new handler code on both sides.
2. **Coupling.** The replication layer must know about every entity type. Adding
   a feature means touching the network protocol, the serializer, and the
   client deserializer.
3. **Waste.** `PlayerMoved` and `NPCMoved` carry the same data (entity ID +
   position). Defining them separately duplicates effort for no benefit.

The component-based model defines a **small, fixed set of generic messages**
that operate on any entity and any component. New entity types and new
components are added by registering them in the component registry — the
replication code and the wire protocol do not change.

---

## 2. Message types

The replication protocol uses exactly **four message types**:

| Message | Direction | Purpose |
|---|---|---|
| `SpawnEntity` | server → client | An entity has entered the client's AOI |
| `UpdateComponent` | server → client | One component's data has changed on an entity |
| `DestroyEntity` | server → client | An entity has left the client's AOI or been removed |
| `PlayAnimation` | server → client | Trigger a visual animation on an entity (transient, not state) |

### 2.1 `SpawnEntity`

Sent when an entity enters the client's area of interest. Carries the entity's
**initial component set** — the full state of all replicated components at
spawn time. The client creates the entity and attaches the components.

```protobuf
message SpawnEntity {
  string entity_id = 1;
  repeated ComponentData components = 2;
  uint32 snapshot_seq = 3;         // server snapshot sequence number
}

message ComponentData {
  uint32 component_id = 1;         // registered component type ID
  bytes data = 2;                  // protobuf-encoded component payload
}
```

> **No `entity_type` field.** Earlier drafts carried a fixed `entity_type`
> enum (Player / NPC / Object / Decoration). That was removed because it
> contradicts the component-based model and cannot represent extension-defined
> entities (see `18-extensions.md`). **Rendering is driven entirely by
> components**: the client picks a renderer from the components present on the
> entity (e.g. an `AvatarAppearance` component → render as an avatar with the
> given sprite sheet; a decoration carries its own appearance component). For
> custom components registered by extensions, the extension ships a
> client-side renderer (see `18-extensions.md` § 8). The authoritative state
> is always the component set, never an entity-type tag.

### 2.2 `UpdateComponent`

Sent when a single component's data has changed on an entity the client is
already tracking. This is the **high-frequency message** — most replication
traffic is `UpdateComponent` messages for `Position`, `Velocity`, and other
frequently-changing components.

```protobuf
message UpdateComponent {
  string entity_id = 1;
  uint32 component_id = 2;
  bytes data = 3;                  // new full component payload
  uint32 snapshot_seq = 4;
}
```

> **Design choice: full component payload, not field-level delta.** The unit
> of delta is the *component*, not the *field*. A `Position` component update
> sends the full `{x, y, map_id, dir}` even if only `x` changed. This keeps
> the encoder and decoder trivial: no per-field diffing, no field masks, no
> partial-merge logic on the client. The bandwidth cost is small because most
> components are tiny (Position is ~20 bytes). Components that are genuinely
> large (e.g. an `Equipment` component with many slots) can be split into
> smaller sub-components if needed.

### 2.3 `DestroyEntity`

Sent when an entity leaves the client's AOI or is removed from the world. The
client tears down the entity and all its components.

```protobuf
message DestroyEntity {
  string entity_id = 1;
  uint32 snapshot_seq = 2;
}
```

### 2.4 `PlayAnimation`

A **transient** message: it triggers a visual animation on the client without
changing replicated component state. Used for one-shot effects: a door
opening, a fire starting, an avatar emote, a screen flicker.

```protobuf
message PlayAnimation {
  string entity_id = 1;
  uint32 animation_id = 2;         // registered animation ID
  bytes params = 3;                // optional animation-specific parameters
}
```

> `PlayAnimation` is deliberately separate from `UpdateComponent`. Animations
> are visual-only and non-authoritative — if the client misses the message
> (packet loss), the world state is still correct. Component updates, by
> contrast, are authoritative and must be reliable.

---

## 3. The replication pipeline

Replication is split across two services: the **World Simulator** (spatial
authority and replication gateway) encodes per-client replication batches, and
the **Pusher** forwards them to clients over WebSocket. The World Simulator has
no WebSocket connections; the Pusher has no ECS knowledge. Extensions drive
entity behavior via NATS; the replication encoder picks up their component
updates the same way it picks up the kernel's player movement updates.

```
World Simulator                          NATS Core                    Pusher
─────────────────                        ─────────                    ──────

Entity
    │
Component changed ──► Dirty flag set
    │
Replication scheduler (per tick)
    │
AOI filter ─── is this entity in the client's area of interest?
    │                ├── no  → skip
    │                └── yes → continue
    │
Delta encoder
    │                ├── entity not yet known to client → SpawnEntity
    │                ├── component changed              → UpdateComponent
    │                └── entity left AOI                → DestroyEntity
    │
Component serializer (protobuf)
    │
Per-client batch ──► publish to          ──► subscribe     ──► WebSocket
   subject client.<client_id>.replication     (per client)     ──► Browser
```

### 3.1 Dirty flags

Each replicated component carries a **dirty flag**. When a system mutates a
component (e.g. the kernel's `PlayerMovementSystem` updates `Position`, or an
extension's update is applied to the ECS), the ECS marks that component dirty.
The replication scheduler (in the World Simulator) checks dirty flags once per
tick and clears them after the update has been encoded.

- Dirty flags are per-component, not per-entity. If `Position` and
  `AvatarAppearance` both change on the same entity in the same tick, two
  `UpdateComponent` messages are sent.
- The `ReplicationSystem` (see `13-ecs-design.md` §5) is the only
  system that reads and clears dirty flags.

### 3.2 Replication scheduler

Runs once per World Simulator tick, after player movement, trigger evaluation,
and extension update processing have completed. For each connected client
(known via `client.connected` events from the Pusher):

1. Collect all dirty components on all entities.
2. Apply the AOI filter (§ 3.3).
3. For each surviving (entity, component) pair:
   - If the client doesn't know this entity yet → emit `SpawnEntity` with the
     full component set, mark the entity as known.
   - If the component is dirty → emit `UpdateComponent`.
4. For entities that have left the client's AOI → emit `DestroyEntity`, mark
   the entity as unknown.

Messages are batched per client per tick into a single `ReplicationBatch`
(see §7). The World Simulator publishes each batch to NATS Core on subject
`client.<client_id>.replication`. The Pusher subscribes to this subject and
forwards the batch to the client's WebSocket as a single frame, minimizing
framing overhead.

### 3.3 AOI filter

The **area of interest** filter determines which entities a client needs to
know about. An entity is in a client's AOI if it is on the same map and within
a configurable radius of the client's avatar (or in the same zone, depending
on the algorithm chosen — see `14-zones-and-interactions.md`).

The AOI filter is applied **before** encoding in the World Simulator, so
entities outside the AOI consume zero serialization bandwidth. This is the
primary scalability mechanism: each client only receives updates for the
slice of the world it can see.

### 3.3a Owner-only replication (inventory and equipment)

Items in a player's inventory or equipment are **not visible to other players**
— they are replicated only to the owning player. The replication encoder
applies a second filter for entities with `InventorySlot` or `Equipped`
components:

- If the entity has an `InventorySlot` or `Equipped` component, it is only
  included in a client's replication batch if that client's avatar entity ID
  matches the `owner_entity_id` field.
- This filter is applied **after** the AOI filter. An item on the ground (has
  `Position`, no `InventorySlot`/`Equipped`) passes the AOI filter normally
  and is replicated to all clients in range.

**Pickup/drop transitions:**

When an extension updates an item from ground → inventory (removes `Position`,
adds `InventorySlot`):

1. The item leaves all other clients' AOI naturally — no `Position` means it's
   not in the spatial index. The next tick sends `DestroyEntity` to everyone
   who had it.
2. The item enters the owner's replication — the encoder sees
   `InventorySlot{owner: "user-42"}` and includes it as `SpawnEntity` in
   `user-42`'s batch.

When an extension drops an item (removes `InventorySlot`, adds `Position`):

1. The item leaves the owner's replication (`DestroyEntity` to owner).
2. The item enters the spatial index — `SpawnEntity` to all clients in AOI on
   the next tick.

The `Equipment` component on the player entity is replicated to **all clients
in AOI** (normal replication), so other clients can render equipped items
(e.g. a bow in the player's hand). The item entities themselves are only
visible to the owner.

### 3.4 Reliability

Replication crosses two transport hops with different guarantees:

| Hop | Transport | Guarantee |
|---|---|---|
| World Sim → Pusher | NATS Core | **at-most-once** (fire-and-forget, no persistence) |
| Pusher → client | WebSocket over TCP | reliable, ordered |

The first hop (NATS Core) can drop a batch — e.g. if the Pusher's
subscription briefly stalls or reconnects. The design **tolerates loss by
construction** rather than relying on a reliable transport:

- **`UpdateComponent` carries the full current component payload**, not a
  delta (see § 2.2). A dropped update is self-correcting: the next tick's
  update for the same component carries the complete, current state. At worst
  the client renders one stale tick, which snapshot interpolation already
  smooths over (see `12-netcode.md`).
- **`snapshot_seq` lets the client detect gaps.** Each `ReplicationBatch`
  carries a monotonically increasing sequence number (see § 7). If the client
  sees a gap, it can request a fresh full snapshot.
- **`SpawnEntity` and `DestroyEntity` are the loss-sensitive cases** — a lost
  `SpawnEntity` means the client never learns about an entity; a lost
  `DestroyEntity` leaves a stale ghost. The World Simulator mitigates this by
  **re-sending `SpawnEntity`/`DestroyEntity` for any entity whose
  known/unknown state the client has not yet confirmed**, until the next
  acknowledged snapshot. On a detected gap or reconnect, the client requests
  a full snapshot, which reconciles spawns and destroys authoritatively.
- **On reconnect** (sticky session to the same Pusher), the Pusher publishes a
  new `client.connected` event and the World Simulator sends a fresh full
  snapshot.

> **[OPEN]** Whether to upgrade the World Sim → Pusher hop to JetStream
> (at-least-once, persisted) for `SpawnEntity`/`DestroyEntity` specifically,
> versus the re-send-until-confirmed approach above. JetStream adds latency
> and storage overhead; the re-send approach keeps the hot path on Core NATS.
> Decide during load testing (see `03-non-functional-requirements.md`).

`PlayAnimation` is **best-effort** — if the message is lost, the world state
is still correct (it is visual-only, non-authoritative). The client simply
ignores animations it never received.

---

## 4. Component registry

Components are identified by a **numeric ID** (`uint32`) on the wire. The
mapping between component IDs and protobuf types is defined in a shared
registry, generated at build time from the component definitions.

| Component ID | Component | Protobuf message |
|---|---|---|
| 1 | `Position` | `PositionData { float x; float y; string map_id; uint32 dir; }` |
| 2 | `EntityState` | `EntityStateData { string state; }` — generic state for interactive props ("on", "off", "locked", ...). Set by extensions via action replies. See `documentation/plans/2026-07-15-interaction-system-design.md`. |
| 3 | `Appearance` | `AppearanceData { uint32 gid; string sprite_base; bool interactable; }` — Tiled GID for base entities, sprite_base for avatars, interactable flag for client-side sparks. `gid` is swapped to `gid_on` when state changes. |
| 4 | `DisplayName` | `DisplayNameData { string name; bool is_guest; bool is_admin; uint32 status; }` — player avatar name tag |
| 5 | `Velocity` | *(reserved — not currently used)* |
| 6 | `Interactable` | *(server-only — not replicated; the `interactable` bool in Appearance replaces it for client-side rendering)* |
| 7 | `Traversable` | `TraversableData { bool traversable; }` |
| 8 | `AvatarAppearance` | *(deprecated — replaced by Appearance for props, sprite_base for avatars)* |
| 9 | `ZoneMembership` | `ZoneMembershipData { string zone_id; string joined_at; }` |
| 10 | `NetworkSession` | *(not replicated — server-only)* |
| 11 | `Item` | `ItemData { string item_type; string display_name; string icon; bool stackable; uint32 quantity; }` |
| 12 | `InventorySlot` | `InventorySlotData { string owner_entity_id; }` *(replicated to owner only)* |
| 13 | `Equipped` | `EquippedData { string owner_entity_id; string slot; }` *(replicated to owner only)* |
| 14 | `Equipment` | `EquipmentData { repeated EquipmentSlot slots; }` *(replicated to all in AOI)* |
| ... | ... | ... |

> `NetworkSession` is never replicated — it is a World-Simulator-only
> component that links an entity to its client session (identified by
> `client_id`). It has no wire representation. The Pusher does not use the
> ECS at all; it tracks sessions in its own in-memory map.

Adding a new component:

1. Define the protobuf message for its data.
2. Register it in the component registry with the next available ID.
3. Add it to the entity in the ECS.

The replication layer, the wire protocol, and the client deserializer require
**no changes** — they already handle any `component_id` generically.

---

## 5. Initial snapshot (on connect / reconnect)

When a client connects (or reconnects after a drop), the flow is:

1. The **Pusher** validates the token, assigns a `client_id`, and publishes a
   `client.connected` event to NATS Core.
2. The **World Simulator** receives the event, provisions the entity (PocketBase
   lookup, JetStream KV position restore), registers it in the ECS, and
   computes the initial snapshot for the client's AOI.
3. The **World Simulator** publishes the initial snapshot as a
   `ReplicationBatch` of `SpawnEntity` messages (one per entity in the AOI,
   each carrying the full component set) to NATS Core on subject
   `client.<client_id>.replication`.
4. The **Pusher** receives the batch and forwards it to the client's
   WebSocket.

After the initial snapshot, the client is in steady-state and receives
`UpdateComponent` / `DestroyEntity` / `PlayAnimation` messages per tick
(World Sim → NATS → Pusher → client).

---

## 6. Client-side handling

The client maintains a **local entity store** mirroring the server's
replicated state:

| Message | Client action |
|---|---|
| `SpawnEntity` | Create entity, instantiate components from `ComponentData` payloads |
| `UpdateComponent` | Replace the component's data on the entity |
| `DestroyEntity` | Remove the entity and all its components |
| `PlayAnimation` | Trigger the visual animation (non-authoritative, may be dropped) |

The client does **not** interpret component data based on entity type. A
`Position` update is handled identically whether the entity is a player, an
NPC, or a moving platform. This is the core benefit of the component-based
model: the client replication code is fixed and generic.

> **Local avatar exception:** the client's own avatar is **not** driven by
  `UpdateComponent` for `Position` — it uses client-side prediction with
  server reconciliation (see `12-netcode.md`). The server still
  sends `UpdateComponent` for the local avatar's other components (appearance,
  zone membership, etc.).

---

## 7. Wire format summary

All replication messages are sent as a batch per tick:

```protobuf
message SpawnEntity {
  string entity_id = 1;
  repeated ComponentData components = 2;
  uint32 snapshot_seq = 3;
}

message UpdateComponent {
  string entity_id = 1;
  uint32 component_id = 2;
  bytes data = 3;                  // protobuf-encoded component payload
  uint32 snapshot_seq = 4;
}

message DestroyEntity {
  string entity_id = 1;
  uint32 snapshot_seq = 2;
}

message PlayAnimation {
  string entity_id = 1;
  uint32 animation_id = 2;
}

message ReplicationBatch {
  uint32 last_input_seq = 1;       // last processed input seq (for reconciliation)
  repeated SpawnEntity spawns = 2;
  repeated UpdateComponent updates = 3;
  repeated DestroyEntity destroys = 4;
  repeated PlayAnimation animations = 5;
}

message ComponentData {
  uint32 component_id = 1;
  bytes data = 2;
}
```
```

One `ReplicationBatch` is sent per client per tick. The `snapshot_seq` allows
the client to detect gaps (if the WebSocket was briefly interrupted) and
request a fresh full snapshot if needed.

---

## 8. Open questions

- **[OPEN] AOI algorithm**: grid-based, quadtree, or distance-based? This
  determines how the AOI filter computes the entity set per client. See
  `14-zones-and-interactions.md`.
- **[OPEN] Component-level vs. field-level delta**: the current design sends
  the full component payload on every `UpdateComponent`. If bandwidth becomes
  a bottleneck for large components, consider field-level delta encoding with
  field masks. Defer until profiling shows a need.
- **[OPEN] Interpolation buffer**: how many snapshots does the client buffer
  before rendering? Typically 2–3 ticks. To be specified in `12-netcode.md`
 .
- **[OPEN] Component compression**: protobuf `bytes` fields could be
  compressed with a lightweight scheme (e.g. delta encoding for Position
  sequences). Defer until profiling shows a need.
