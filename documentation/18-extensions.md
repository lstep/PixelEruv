# Extensions

This document specifies the **extension system** — a mechanism for external
processes (written in any language with a NATS client) to own and drive **all
gameplay behavior** in the virtual world. The World Simulator (the kernel) is
the spatial authority and replication gateway; extensions are everything else.

Extensions make the system **modular**: NPCs, custom zone behaviors,
interactive objects, LLM-driven characters, trigger logic, zone access
policies, and entirely new gameplay systems can be developed, deployed, and
updated independently of the World Simulator core.

> **Status:** design document. The extension protocol is new and has not been
> implemented yet. The architecture (NATS as the bus, World Sim as the
> spatial authority and replication gateway) is compatible with the existing
> design and requires no changes to the Pusher or the replication pipeline.

---

## 1. Motivation

The World Simulator is the **spatial authority**: it owns the tile grid, the
spatial index, the trigger registry, the ECS, and the replication pipeline.
But it does not run gameplay logic for non-player entities. All gameplay
behavior — NPC movement, trigger logic, zone behavior, AI, custom game
mechanics — belongs in extensions:

- An **LLM-controlled NPC** needs Python (LangChain, LlamaIndex) and network
  access to an inference endpoint. This has no place in the kernel.
- A **welcome waitress** that responds to chat interactions with fixed or
  generated sentences is a self-contained feature that a developer should be
  able to add without touching the kernel.
- A **custom zone behavior** (e.g. a meeting room that auto-starts a timer
  when occupied) is deployment-specific logic that shouldn't be hardcoded.
- A **patrol system** with its own pathfinding AI might be complex enough to
  warrant a separate process.
- **Wall collision** is a trigger registered by a "walls extension" — the
  kernel caches it as `block` and refuses movement locally, but the extension
  owns the decision.
- **Zone access policy** (knock-to-join, exclusive, invite-only) is a trigger
  registered by the zone's owning extension — the kernel routes the query, the
  extension decides.

The kernel-vs-extension split follows one criterion:

**Kernel = deployment-invariant infrastructure. Extensions = deployment-specific
behavior.**

Extensions solve this by letting external processes act as peer simulators
through NATS — the same bus the World Sim, Pusher, and Bridge already use.

### Design principles

1. **NATS is the only contract.** Extensions communicate with the World
   Simulator exclusively via NATS Core pub/sub and JetStream KV. No shared
   memory, no direct RPC, no compiled plugins.
2. **Any language.** Anything with a NATS client can be an extension: Go,
   Python, Node, Rust, Java, C#, etc.
3. **Extensions are peers, not subordinates.** An extension can do everything
   the kernel can do for its entities: spawn, update any component directly,
   handle interactions, watch and write KV, register custom component types,
   register triggers and zones. The kernel does not gatekeep — it validates
   (collision, zone access, trigger rules) and replicates.
4. **The World Sim is the spatial authority and replication gateway.** It owns
   the ECS, the tile grid, the spatial index, the trigger registry, the AOI
   filter, the replication encoder, and the connection to the Pusher. It
   validates all entity changes (including its own and extensions') against
   collision, zone access, and trigger rules. It does not decide what
   extensions are allowed to do — it enforces physics and access rules. The
   only gameplay system in the kernel is player avatar movement (the in-kernel
   exception: latency-critical and deployment-invariant).
5. **Extensions are optional and isolated.** A crashed extension does not
   take down the World Sim. Its entities freeze (or despawn after a timeout)
   and resume when the extension reconnects. Its `block`/`allow` triggers
   remain cached in the kernel's spatial index; its `ask` triggers time out
   and fail closed (block).
6. **Replication is transparent.** Extension-owned entities are regular ECS
   entities. The replication encoder, the Pusher, and the client don't know
   or care that an extension is driving the entity.
7. **First-party extensions are real sibling processes.** The "default
   gameplay" pack (walls, doors, base zone behaviors, base triggers) ships as
   sibling processes in Docker Compose, not compiled into the kernel. This
   ensures the kernel is truly free of gameplay logic and that the extension
   API is the same for first-party and third-party extensions.

---

## 2. Architecture

```
                   NATS Core
                   ┌────┴──────────────────────────┐
                   │                                │
          ┌────────┴────────┐              ┌───────┴────────┐
          │  World Simulator│              │   Extension    │
          │  (spatial       │              │  (any language)│
          │   authority +   │◄── updates ──┤  Entity logic  │
          │   replication   │              │  Trigger logic │
          │   gateway)      ├── input ────►│  Zone behavior │
          │                  ├── trigger ──►│  KV read/write │
          │  ECS + spatial  │              │  Heartbeat     │
          │  index + AOI +  │              │                │
          │  replication    │              │                │
          │  + validation    │              └────────────────┘
          └────────┬────────┘
                   │
                   ▼ replication batches
              Pusher → Browser
```

The extension is a **peer process on the NATS bus**, not a plugin loaded into
the World Sim. It can run in its own Docker container, on a different machine,
in a different language, and be restarted independently.

### What the World Sim retains (kernel responsibilities)

The World Sim is the **spatial authority and replication gateway**. It is the
single point that:

- **Owns the ECS.** All entities — whether driven by the kernel's player
  movement or by extensions — live in the same ECS. The World Sim creates them,
  stores them, and removes them.
- **Owns the spatial index.** The tile grid (from Tiled), the trigger registry,
  and the zone boundary registry are all in the World Sim. The spatial index
  maps tiles to triggers and entities for O(1) lookup during movement
  validation.
- **Handles replication.** The AOI filter, the replication encoder, and the
  NATS → Pusher forwarding are all in the World Sim. Extensions never talk to
  the Pusher or the client directly.
- **Validates physics and access.** Collision checks, zone access (via
  triggers), position bounds, and component schema validation apply to all
  entities equally. The World Sim rejects updates that violate these rules —
  whether they come from its own player movement or from an extension.
- **Evaluates triggers.** The kernel checks access triggers (`block`/`allow`/
  `ask`) on target tiles during movement. `block` and `allow` are cached
  locally; `ask` triggers are routed to the owning extension via NATS.
- **Moves player avatars.** The only gameplay system in the kernel. Input →
  target tile → trigger evaluation → Position update, all in one tick.
- **Provisions and persists.** Identity → entity provisioning (PocketBase
  lookup, KV restore) and player position/status persistence are kernel
  responsibilities (deployment-invariant, on the critical connect path).

### What extensions can do (same as kernel systems, plus trigger/zone ownership)

- Spawn and despawn entities.
- Update any component on their entities directly (per-tick or event-driven).
- Request interpolated movement (target-based) or publish positions directly.
- Register custom component types with new protobuf schemas.
- **Register triggers** (access: `block`/`allow`/`ask`; event: `notify`;
  action: input handlers) on tiles, entities, or input types. The kernel caches
  `block`/`allow` triggers locally, routes `ask` triggers to the extension at
  runtime, broadcasts input events to all registered extensions with range/LOS
  data and an equipment snapshot, and evaluates proximity triggers per-tick. See
  §3a.
- **Register zones** (polygon regions with associated triggers). Zone
  boundaries are stored in the kernel; zone behavior is implemented by the
  extension via triggers. See §3b.
- **Claim base entities** from the Tiled map. Base entities exist in the ECS
  without an `ExtensionOwner`; an extension can claim them by registering
  triggers on them or updating their components.
- Handle client interactions asynchronously (via input handlers — register for
  input types like `click:left` or `key:E` and self-filter based on the
  payload).
- Read and write any JetStream KV key.
- Watch KV keys for reactive behavior.

### Relationship to existing services

| Service | Role | Knows about extensions? |
|---|---|---|
| World Simulator | Spatial authority + replication gateway + validator. Hosts all entities in the ECS. Evaluates triggers. Moves player avatars. | Yes |
| Pusher | WebSocket proxy — forwards input, replication, and control frames | No (transparent) |
| LiveKit Bridge | Media sync — watches KV for zone changes, subscribes to `client.provisioned` for token issuance | No |
| Client | Renders entities, sends interactions | No (entities look like any other) |
| Extension | Peer simulator — drives entities, registers triggers and zones, handles interactions via NATS | Knows about the World Sim (NATS subjects) |

---

## 3. Extension lifecycle

### 3.1 Registration

When an extension starts, it connects to NATS and publishes a registration
request. Registration declares the extension's identity and liveness policy,
but **does not restrict what it can do** — there is no entity type whitelist
or capability list.

```
Subject: extension.register
Payload (JSON):
{
  "extension_id": "waitress-npc-v1",
  "heartbeat_interval_ms": 5000,
  "on_death": "freeze",
  "metadata": {
    "description": "Welcome waitress NPC with LLM dialogue",
    "version": "1.0.0"
  }
}
```

The World Simulator responds on a reply subject:

```
{
  "status": "registered",
  "extension_id": "waitress-npc-v1",
  "world_sim_instance": "ws-shard-1",
  "existing_entities": []
}
```

If the extension was previously registered and its entities are still in the
ECS (freeze policy), `existing_entities` contains them so the extension can
resume without re-spawning. See §11.

### 3.2 Custom component registration

If the extension uses component types not already in the World Sim's component
registry, it registers them at startup (after registration):

```
Subject: extension.<extension_id>.register_components
Payload:
{
  "extension_id": "waitress-npc-v1",
  "components": [
    {
      "component_id": 100,
      "name": "SpeechBubble",
      "protobuf_schema": "message SpeechBubbleData { string text = 1; uint32 duration_ms = 2; }"
    },
    {
      "component_id": 101,
      "name": "DialogueState",
      "protobuf_schema": "message DialogueStateData { string current_topic = 1; repeated string history = 2; }"
    }
  ]
}
```

The World Sim adds these to its component registry. From this point:

- The extension can spawn entities with these components.
- The replication encoder will serialize them generically (by component ID +
  raw bytes, as described in `11-replication.md` §4).
- The client needs to know how to render them. This is handled by a
  **client-side extension package** (see §8).

> **[OPEN]** The client-side extension package mechanism (how custom
> component renderers are delivered to the browser) is not specified yet.
> Options: a static asset served by the extension, a Phaser plugin loaded
> dynamically, or a convention-based mapping (component ID → renderer class).

### 3.3 Entity spawning

The extension publishes a spawn command for each entity it wants to create.
There are **no type restrictions** — the extension can spawn any entity with
any components (including custom ones it registered).

```
Subject: extension.<extension_id>.spawn
Payload:
{
  "entity_id": "waitress-1",
  "position": { "map_id": "lobby", "x": 42, "y": 17, "dir": "south" },
  "components": {
    "AvatarAppearance": {
      "sprite_sheet": "assets/sprites/waitress.png",
      "animation_set": "waitress_anim"
    },
    "Interactable": {
      "prompt": "Talk to waitress",
      "interaction_type": "talk"
    },
    "DisplayName": {
      "name": "Alice",
      "title": "Receptionist"
    },
    "SpeechBubble": null
  }
}
```

The World Sim:

1. Validates the position is valid (on the map, not inside a wall).
2. Creates the entity in the ECS with the requested components.
3. Marks the entity with a server-only `ExtensionOwner` component (containing
   the `extension_id`) for cleanup.
4. The entity is now in the ECS and will be replicated to clients whose AOI
   includes it — exactly like any other entity.

### 3.4 Heartbeat and liveness

The extension publishes a heartbeat at the interval declared during
registration:

```
Subject: extension.<extension_id>.heartbeat
Payload:
{
  "extension_id": "waitress-npc-v1",
  "timestamp": "2025-01-15T12:00:00Z",
  "entity_count": 3
}
```

The World Sim tracks heartbeats. If it stops receiving them for
`heartbeat_interval_ms * 3` (configurable), it considers the extension dead
and takes the action declared at registration:

- **freeze** (default): entities stay in the ECS but stop updating. Clients
  see them standing still. When the extension reconnects and re-registers,
  it re-claims its entities and resumes sending updates.
- **despawn**: entities are removed from the ECS. Clients see
  `DestroyEntity` messages. When the extension reconnects, it must re-spawn
  them.

### 3.5 Deregistration

When an extension shuts down gracefully, it publishes:

```
Subject: extension.<extension_id>.deregister
Payload:
{
  "extension_id": "waitress-npc-v1",
  "action": "despawn"
}
```

The World Sim removes the extension from its registry and despawns or freezes
its entities accordingly. The extension's triggers are also unregistered from
the spatial index (or retained if the `freeze` policy is used, so cached
`block`/`allow` triggers continue to function during the freeze).

---

## 3a. Trigger registration

Triggers are the mechanism by which extensions declare **spatial rules** on
tiles and entities. The kernel stores them in its trigger registry and indexes
them in the spatial index. There are three categories:

### Access triggers (gate movement)

| Behavior | Kernel action | NATS round-trip? | Use case |
|---|---|---|---|
| `block` | Cache in spatial index. Refuse any move to that tile. | No — kernel decides locally | Walls, obstacles, closed doors |
| `allow` | Cache in spatial index. Always permit. | No | Open passages, explicit overrides |
| `ask` | Publish query to owning extension each time. Wait for reply. | Yes — but rare | Dynamic access (room full? on guest list?) |

### Event triggers (notifications, do not gate movement)

| Binding | Routing | Use case |
|---|---|---|
| **Entity-bound** | Point-to-point to the extension that owns the entity | NPC notices a player approached |
| **Tile-bound** | Broadcast to all extensions (self-filtered) | Welcome mat, floor switch, meeting room occupancy counter |
| **Proximity-bound** | Point-to-point to the extension that owns the entity | Proximity alarm that rings while a player is within N tiles of an entity; NPC aggro radius |

Proximity-bound triggers fire when a player enters or leaves a radius around
the bound entity. Unlike tile-bound triggers (which require one trigger per
tile in the area), a proximity trigger is a single registration with a
`radius` in tiles. The kernel evaluates proximity every tick after movement:
it checks all players against all proximity triggers using the spatial index,
fires `proximity_enter` for new entries and `proximity_exit` for departures,
and sends point-to-point notifications to the owning extension. The extension
does not need to track a presence counter — the kernel handles enter/exit
transitions.

### Input handlers (player-initiated, broadcast to all registered extensions)

| Binding | Kernel action | NATS round-trip? | Use case |
|---|---|---|---|
| `input` | Compute range + LOS + entities on tile, then broadcast to all extensions that registered for that input type | Yes (broadcast to all registered) | Shooting a bow, pressing E to interact, clicking a button, throwing an object |

Input handlers fire when a player triggers an input event (a click or a key
press, via `ActionFrame`). Unlike access and event triggers (which are bound to
tiles or entities), input handlers are bound to an **input type** — a string
like `click:left`, `click:right`, `click:double`, `key:E`, etc. Extensions
register for the input types they care about; the kernel broadcasts each input
event to **all** extensions that registered for that type.

The kernel does **not** validate range or LOS as a gate — instead, it includes
`range` and `has_los` in the dispatch payload so each extension can decide for
itself whether the action is valid. This keeps the kernel as a data provider,
not a gatekeeper. The dispatch payload also includes a **snapshot of the
player's `Equipment` component**, the entities on the clicked tile (for clicks)
or adjacent to the player (for key presses), and the player's position.

All replies within the timeout window are **applied** — there is no conflict
resolution. If two extensions respond to the same click, both replies are
applied. The kernel collects all replies, applies the updates and
`consume_items`, and sends a single `ActionResultFrame` to the client.

### Registration message

An extension registers triggers at init time (after `extension.register` and
`register_components`):

```
Subject: extension.<extension_id>.register_triggers
Payload:
{
  "triggers": [
    {
      "trigger_id": "wall-lobby-north",
      "category": "access",
      "binding": "tile",
      "tiles": [{"map_id": "lobby", "x": 10, "y": 5}, {"map_id": "lobby", "x": 10, "y": 6}],
      "behavior": "block"
    },
    {
      "trigger_id": "meeting-room-1-entrance",
      "category": "access",
      "binding": "tile",
      "tiles": [{"map_id": "office", "x": 42, "y": 17}],
      "behavior": "ask",
      "default_on_timeout": "block",
      "ttl_ms": 500
    },
    {
      "trigger_id": "welcome-mat-lobby",
      "category": "event",
      "binding": "tile",
      "tiles": [{"map_id": "lobby", "x": 5, "y": 5}],
      "event": "enter"
    },
    {
      "trigger_id": "waitress-1-interact",
      "category": "action",
      "binding": "input",
      "input": "key:E",
      "owner_extension_id": "waitress-npc-v1"
    },
    {
      "trigger_id": "alarm-1-proximity",
      "category": "event",
      "binding": "proximity",
      "entity_id": "alarm-1",
      "radius": 3,
      "events": ["proximity_enter", "proximity_exit"]
    },
    {
      "trigger_id": "combat-click-left",
      "category": "action",
      "binding": "input",
      "input": "click:left",
      "owner_extension_id": "combat-ext"
    },
    {
      "trigger_id": "interact-key-e",
      "category": "action",
      "binding": "input",
      "input": "key:E",
      "owner_extension_id": "office-ext"
    }
  ]
}
```

The kernel adds these to its trigger registry and spatial index. The kernel
responds with a confirmation:

```
{
  "status": "registered",
  "trigger_ids": ["wall-lobby-north", "meeting-room-1-entrance", "welcome-mat-lobby", "waitress-1-interact", "alarm-1-proximity", "combat-click-left", "interact-key-e"]
}
```

### Trigger evaluation at runtime

When any entity (player or extension) attempts to enter a tile, the kernel
evaluates all access triggers on that tile (see `10-world-simulator.md` §5c):

1. **Any `block` trigger?** → refuse immediately (cached, no NATS).
2. **All `allow` or no triggers?** → accept.
3. **Has `ask` trigger?** → publish query to the owning extension, defer move.

For `ask` triggers, the kernel publishes:

```
Subject: trigger.<trigger_id>.query
Payload:
{
  "trigger_id": "meeting-room-1-entrance",
  "entity_id": "user-42",
  "client_id": "abc123",
  "target_tile": {"map_id": "office", "x": 42, "y": 17},
  "reply_to": "trigger.meeting-room-1-entrance.reply"
}
```

The extension replies:

```
Subject: trigger.<trigger_id>.reply
Payload:
{
  "trigger_id": "meeting-room-1-entrance",
  "entity_id": "user-42",
  "decision": "allow" | "block",
  "reason": "Room is full"  // optional, for client feedback
}
```

If the extension does not reply within `ttl_ms`, the kernel applies
`default_on_timeout` (default: `block` — fail closed).

### Multiple triggers per tile: any-refusal-blocks

A tile can have triggers from multiple extensions. The resolution policy is
**any-refusal-blocks**: all access triggers must approve the move. If any
trigger refuses (`block` or `ask` reply with `block`), the move is blocked.

### Event trigger dispatch

After a move succeeds, the kernel fires event triggers on the entered and
exited tiles. Proximity-bound triggers are evaluated separately, once per
tick after all movement is processed (see below).

- **Entity-bound `notify`**: the kernel publishes to the owning extension
  (point-to-point):
  ```
  Subject: entity.<entity_id>.notify.<event>
  Payload:
  {
    "entity_id": "waitress-1",
    "event": "enter",
    "player_entity_id": "user-42"
  }
  ```
  Events are `enter` and `exit` (fired after movement). This is fire-and-forget
  — no reply expected. (Player-initiated interactions like clicking or pressing
  E are handled by input handlers, not by entity-bound notify triggers.)

- **Tile-bound `notify`**: the kernel broadcasts to all extensions:
  ```
  Subject: trigger.notify.tile.<map_id>.<x>.<y>
  Payload:
  {
    "event": "enter",
    "entity_id": "user-42",
    "tile": {"map_id": "lobby", "x": 5, "y": 5}
  }
  ```
  Extensions self-filter: they receive the broadcast and ignore it if they
  don't care about that tile/event. This is fire-and-forget — no reply expected.

- **Proximity-bound `notify`**: the kernel evaluates proximity triggers once
  per tick, after all movement has been processed. For each proximity trigger,
  it computes the set of players within `radius` tiles of the bound entity
  (using the spatial index) and compares with the previous tick's set. For
  each transition, it publishes to the owning extension (point-to-point):
  ```
  Subject: entity.<entity_id>.notify.proximity_enter
  Payload:
  {
    "entity_id": "alarm-1",
    "event": "proximity_enter",
    "player_entity_id": "user-42",
    "distance": 2
  }
  ```
  Same for `proximity_exit`. This is fire-and-forget — no reply expected. The
  kernel handles enter/exit transition detection; the extension does not need
  to track a presence counter.

### Input handler dispatch

When a player triggers an input event (via `ActionFrame`), the kernel:

1. Determines the input type from the `ActionFrame`.
2. Looks up all extensions that registered for that input type.
3. If no extension registered → `ActionResultFrame{ ok: false, reason: "no_handler" }`
   sent to the client immediately (no NATS round-trip).
4. Computes contextual data from the spatial index:
   - For **clicks**: `target_tile`, `entities_on_tile`, `range` (distance from
     player to clicked tile), `has_los` (Bresenham raycast).
   - For **key presses**: `adjacent_entities` (entities on tiles adjacent to
     the player). No target tile, no range, no LOS.
5. Gathers the player's `Equipment` snapshot.
6. Broadcasts the input event to all registered extensions:

```
Subject: input.<input_type_with_dots>
  (e.g. input.click.left, input.click.right, input.key.e)
Payload:
{
  "request_id": "req-123",
  "source_entity_id": "user-42",
  "client_id": "abc123",
  "input": "click:left",
  "target_tile": {"map_id": "arena", "x": 10, "y": 5},  // null for key presses
  "player_position": {"map_id": "arena", "x": 2, "y": 5, "dir": "east"},
  "entities_on_tile": ["door-1"],          // for clicks; null for key presses
  "adjacent_entities": null,               // for key presses; null for clicks
  "has_los": true,                         // for clicks; null for key presses
  "range": 8,                              // for clicks (tile distance); null for key presses
  "equipment": [
    {"slot": "main_hand", "item_entity_id": "bow-7", "item_type": "bow"},
    {"slot": "off_hand", "item_entity_id": null}
  ],
  "reply_to": "input.click.left.reply.<request_id>"
}
```

Each extension processes the input asynchronously (decides what happens based
on equipment, entities on tile, range, LOS, etc.) and replies:

```
Subject: input.<input_type_with_dots>.reply.<request_id>
Payload:
{
  "request_id": "req-123",
  "extension_id": "combat-ext",
  "updates": [
    {"entity_id": "arrow-1", "component": "Position", "data": {"x": 10, "y": 5, "map_id": "arena"}},
    {"entity_id": "user-42", "component": "AvatarAppearance", "data": {"animation": "shoot"}}
  ],
  "consume_items": [{"item_entity_id": "arrow-1"}]
}
```

The kernel collects **all** replies within a timeout window (e.g. 500 ms,
configurable). All replies are applied — updates are applied to the ECS,
`consume_items` removes items from the player's inventory. The kernel then
sends a single `ActionResultFrame{ ok: true }` to the client. If no reply
arrives within the timeout, the kernel sends
`ActionResultFrame{ ok: false, reason: "timeout" }`.

Extensions self-filter: they receive every input event of their registered type
and ignore events they don't care about (e.g. an extension that handles bow
shooting ignores clicks where the player isn't holding a bow, or where
`has_los` is false).

### Unregistering triggers

An extension can unregister triggers (e.g. when a door is removed):

```
Subject: extension.<extension_id>.unregister_triggers
Payload:
{
  "trigger_ids": ["wall-lobby-north"]
}
```

The kernel removes them from the trigger registry and spatial index.

### Trigger persistence across extension crashes

When an extension crashes:
- **`block`/`allow` triggers** remain cached in the spatial index. They continue
  to function without the extension being alive (the decision was pre-declared).
- **`ask` triggers** will time out and fail closed (`default_on_timeout: block`).
  The extension is unreachable, so no reply arrives.
- **`notify` triggers** (tile-bound, entity-bound, proximity-bound) are
  silently dropped (no one is listening on the extension's subscriber). The
  kernel stops firing proximity evaluations for triggers owned by the crashed
  extension.
- **`input` handlers** from the crashed extension are removed from the input
  registry. Input events of that type are no longer broadcast to the crashed
  extension. If no other extension registered for that input type, the kernel
  sends `ActionResultFrame{ ok: false, reason: "no_handler" }`.

When the extension reconnects and re-registers, it re-registers its triggers.
If the `freeze` policy was used, the kernel may have retained the old trigger
registrations — the extension can update or replace them.

---

## 3b. Zone registration

Zones are polygon-defined regions on a map. They can be created from Tiled (base
zones) or dynamically by extensions at init time.

A zone by itself is just a boundary. Zone **behavior** (exclusivity,
knock-to-join, timers, access policies) is implemented by the owning extension
via triggers on the zone's boundary tiles. The kernel does not know what a
"meeting room" or an "exclusive zone" is — it knows "zone Z has trigger T on
its boundary tiles, owned by extension E."

### Registration message

```
Subject: extension.<extension_id>.register_zone
Payload:
{
  "zone_id": "meeting-room-1",
  "map_id": "office",
  "boundary_tiles": [{"x": 42, "y": 17}, {"x": 43, "y": 17}, ...],
  "properties": {
    "is_exclusive": false,
    "tint_color": null
  }
}
```

The kernel stores the zone boundaries in its zone registry. The extension is
expected to also register triggers on the zone's boundary tiles (via
`register_triggers`) if it wants to control access to the zone.

Zone properties (exclusivity, tint, etc.) are written to JetStream KV by the
extension (see §7), so the LiveKit Bridge can react to zone-state changes via
`kv.Watch`.

---

## 4. Entity ownership: peers, not proxies

Extension-owned entities are **real entities** in the World Sim's ECS — not
"proxy entities" with limited capabilities. They have the same status as
entities driven by the kernel's player movement system.

| Aspect | Kernel-driven entity (player avatar) | Extension-driven entity |
|---|---|---|
| Behavior driven by | Kernel's player movement system (in-process) | Extension via NATS updates |
| Component updates | Direct (kernel mutates components in the ECS) | Direct (extension publishes, kernel applies) |
| Movement | Kernel computes positions from client input | Extension publishes positions OR requests interpolation |
| Validated by | Kernel (same rules: collision, triggers, bounds) | Kernel (same rules) |
| Components | Any in the registry | Any in the registry (including custom-registered) |
| KV access | Kernel reads/writes player position and status | Read/write any key |
| Triggers | Can be on tiles the player enters | Can register triggers on tiles/entities |
| `ExtensionOwner` component | Absent | Present (for cleanup) |

The `ExtensionOwner` component is server-only (never replicated). Its sole
purpose is:

1. **Cleanup** — when an extension dies, the World Sim knows which entities
   to freeze or despawn.

(Interaction routing is no longer based on `ExtensionOwner` — extensions
register for input types and self-filter. See §6.)

### What the World Sim validates (for all entities, equally)

The World Sim enforces the same rules for all entities, regardless of who
drives them:

- **Collision.** No entity (kernel-driven or extension-driven) can move through
  tiles with a `block` access trigger.
- **Zone access.** No entity can enter a zone whose boundary triggers refuse
  the entry (via `block` or `ask` with a refusal reply).
- **Position bounds.** Positions must be on the map and within valid bounds.
- **Component schema.** Component data must match its registered protobuf
  schema.

If validation fails, the World Sim rejects the update and publishes an error
(see §10). The entity's state is not modified.

---

## 5. Component updates and movement

Extensions update entity components by publishing to NATS. There are two
modes, and extensions can use both interchangeably:

### 5.1 Direct component updates (primary mode)

The extension publishes a component update, and the World Sim applies it
directly to the ECS on the next tick. This is the same mechanism the World
Sim's kernel uses — they mutate components, mark them dirty, and the
replication encoder picks them up.

```
Subject: entity.<entity_id>.update
Payload:
{
  "entity_id": "waitress-1",
  "component": "Position",
  "data": { "x": 43, "y": 17, "map_id": "lobby", "dir": "east" }
}
```

The World Sim validates the update (is the position valid? is the entity
still alive?) and applies it. The replication encoder picks up the dirty
component on the next tick.

Extensions can publish direct updates at **any rate** — per-tick for smooth
movement, or event-driven for discrete changes (animations, speech bubbles,
status changes). The World Sim does not impose a rate limit.

### 5.2 Target-based movement (convenience mode)

For extensions that don't want to compute per-tick positions (e.g. an LLM
that decides "go to the lobby entrance" but doesn't want to compute the
trajectory), the World Sim offers interpolated movement:

```
Subject: entity.<entity_id>.move
Payload:
{
  "entity_id": "waitress-1",
  "target": { "x": 50, "y": 20, "map_id": "lobby" },
  "speed": 2.0
}
```

The World Sim sets a `MovementTarget` component on the entity. The kernel's
interpolation logic (part of the tick loop) interpolates the position toward
the target each tick, at the specified speed. Access triggers on each tile
along the path are evaluated (block/allow cached, ask routed).

When the entity reaches the target, the World Sim publishes:

```
Subject: entity.<entity_id>.arrived
Payload:
{
  "entity_id": "waitress-1",
  "position": { "x": 50, "y": 20, "map_id": "lobby" }
}
```

### When to use which mode

| Mode | Best for |
|---|---|
| Direct updates | Extensions that compute their own movement (pathfinding AI, physics, per-tick AI). Full control. |
| Target-based | Extensions that think in destinations (LLM decisions, simple state machines). The World Sim handles the trajectory. |
| Both | An extension can use target-based for long-distance movement and direct updates for fine-grained positioning (e.g. a dance animation that offsets the position frame by frame). |

### Batch updates

An extension can update multiple components on multiple entities in a single
message:

```
Subject: extension.<extension_id>.batch_update
Payload:
{
  "updates": [
    { "entity_id": "waitress-1", "component": "Position", "data": { "x": 43, "y": 17 } },
    { "entity_id": "waitress-1", "component": "AvatarAppearance", "data": { "animation": "wave" } },
    { "entity_id": "waitress-2", "component": "SpeechBubble", "data": { "text": "Hello!" } }
  ]
}
```

The World Sim applies all updates atomically (within one tick). If any update
fails validation, the entire batch is rejected.

---

## 6. Interactions

Interactions are handled by the input handler model (see §3a). The client
sends `ActionFrame` for all tile clicks and key presses (`InteractFrame` has
been deprecated — `ActionFrame` replaces it for all player-initiated input).
The kernel broadcasts each input event to all extensions that registered for
that input type. There is no fallback routing — extensions must explicitly
register for the input types they want to handle.

### Routing flow

When a client sends an `ActionFrame`:

1. The kernel determines the input type (e.g. `click:left`, `key:E`).
2. The kernel looks up all extensions that registered for that input type.
3. If no extension registered → `ActionResultFrame{ ok: false, reason: "no_handler" }`.
4. The kernel computes contextual data (target tile, entities on tile, adjacent
   entities, range, LOS, equipment snapshot) and broadcasts to all registered
   extensions.
5. Each extension self-filters: it decides whether to act based on the payload
   (e.g. "is there an entity I own on the clicked tile?", "is the player holding
   a bow?", "is `has_los` true?").
6. All replies within the timeout are applied. The kernel sends a single
   `ActionResultFrame` to the client.

### What replaced entity interaction routing

Previously, the kernel had a fallback path: if no action trigger was registered
on a clicked tile but an entity was present, the kernel would forward the
interaction to the entity's owning extension via `entity.<entity_id>.interact`.
This is no longer needed. In the input handler model:

- An extension that owns an NPC and wants to handle clicks on it registers for
  `click:left`. When a player clicks the NPC's tile, the extension receives the
  broadcast, checks `entities_on_tile` for its NPC, and responds.
- An extension that wants to handle the interact key (`E`) registers for
  `key:E`. When a player presses E, the extension receives the broadcast,
  checks `adjacent_entities` for entities it owns, and responds.
- The `entity.<entity_id>.interact` subject is no longer used. All interaction
  routing goes through the input handler broadcast.

### Interaction flow

```
Client → Pusher → NATS → World Sim (ActionFrame: click tile where waitress-1 is)
                                    │
                                    │  Kernel broadcasts to all extensions
                                    │  registered for "click:left"
                                    ▼
input.click.left → All registered extensions (via NATS)
  {
    "request_id": "req-125",
    "source_entity_id": "user-42",
    "input": "click:left",
    "target_tile": {"map_id": "lobby", "x": 42, "y": 17},
    "entities_on_tile": ["waitress-1"],
    "has_los": true,
    "range": 1,
    "equipment": [...],
    "reply_to": "input.click.left.reply.req-125"
  }
                                    │
                            npc-ext (registered for click:left):
                            • Checks entities_on_tile → sees waitress-1 (owns it)
                            • LLM call (or fixed response)
                            • Decides response + animation
                            • Other extensions self-filter (ignore)
                                    │
                                    ▼
input.click.left.reply.req-125 → World Sim (via NATS)
  {
    "request_id": "req-125",
    "extension_id": "npc-ext",
    "updates": [
      {"entity_id": "waitress-1", "component": "AvatarAppearance", "data": {"animation": "talk"}},
      {"entity_id": "waitress-1", "component": "SpeechBubble", "data": {"text": "Welcome!"}}
    ]
  }
                                    │
                            World Sim applies updates
                            Replication encoder → NATS → Pusher → Client
                                    │
                                    ▼
                            Client sees waitress talk
                            and displays speech bubble
```

### Async replies

The input handler dispatch includes a `reply_to` subject so each extension can
respond asynchronously. The World Sim does **not** block the tick loop waiting
for replies. Extensions respond whenever they're ready (immediately for fixed
responses, after an LLM call for generated responses). The World Sim applies
all received replies on subsequent ticks after receiving them.

If no extension replies within the timeout (e.g. 500 ms), the World Sim sends
`ActionResultFrame{ ok: false, reason: "timeout" }` to the client.

---

## 6a. Inventory and equipment extension

Inventory and equipment are gameplay behavior, owned by a first-party extension
(e.g. `inventory-ext`). The kernel handles only the spatial parts (items on the
ground have `Position`; picking up removes `Position` and adds `InventorySlot`).
The extension handles all gameplay semantics (equipment slots, item effects,
use actions, persistence).

### Items as ECS entities

Items are full ECS entities (see `13-ecs-design.md` §4). They transition
between three spatial states:

- **On the ground**: has `Position`, visible to all players via normal AOI
  replication.
- **In inventory**: loses `Position`, gains `InventorySlot{owner_entity_id}`.
  Replicated only to the owning player.
- **Equipped**: loses `InventorySlot`, gains `Equipped{owner_entity_id, slot}`.
  The extension updates the player's `AvatarAppearance` to reflect the equipped
  item. Replicated only to the owning player.

The player entity has an `Equipment` component (replicated to all in AOI) so
other clients can render equipped items.

### Kernel's spatial responsibilities

- An item on the ground has `Position` — in the ECS, replicated via normal AOI.
- When an extension removes `Position` and adds `InventorySlot` to an item, the
  kernel validates the owner exists and applies the change. The item disappears
  from the world.
- When an extension removes `InventorySlot` and adds `Equipped`, the kernel
  applies it.
- The kernel replicates `InventorySlot` and `Equipped` only to the owning
  player (see `11-replication.md` §3.3).
- The kernel replicates the `Equipment` component on the player entity to all
  clients in AOI.

### Extension's gameplay responsibilities

- **Pickup**: receives an input event (click or key press) on an item tile,
  validates rules, sends component updates (ground → inventory).
- **Equip/unequip**: receives a client action, validates slot compatibility,
  swaps `InventorySlot` ↔ `Equipped`, updates the player's `Equipment`
  component.
- **Drop**: sends updates to remove `InventorySlot`/`Equipped`, add `Position`
  (kernel validates the tile).
- **Use/consume**: handles item effects and sends `consume_items` in action
  trigger replies.
- **Equipment in input handlers**: reads the equipment snapshot from the
  input handler payload to decide the action.
- **Persistence**: item definitions and player inventory state are persisted by
  the extension to JetStream KV (extensions have unrestricted KV access, see
  §7). For durable relational storage in PocketBase, the extension coordinates
  with the kernel (extensions do not access PocketBase directly, see
  `06-data-model-and-persistence.md` §1).

### Custom components

The inventory extension registers custom components for item-specific data:

```
Subject: extension.inventory-ext.register_components
Payload:
{
  "components": [
    {"component_id": 200, "name": "WeaponStats", "protobuf_schema": "..."},
    {"component_id": 201, "name": "ConsumableEffect", "protobuf_schema": "..."},
    {"component_id": 202, "name": "Cooldown", "protobuf_schema": "..."}
  ]
}
```

These are gameplay-specific. The kernel replicates them (to the owning player
only) without understanding their semantics.

---

## 7. JetStream KV: read and write

Extensions have **unrestricted access** to JetStream KV — they can read and
write any key, same as the World Sim. This is intentional: extensions are
peers, and some extensions need to manage their own persistent state or
influence shared world state directly.

### Reading (KV watch)

Extensions can subscribe to KV watches for reactive behavior, exactly like
the LiveKit Bridge does:

```
Extension subscribes to: kv.Watch("zones.<zone_id>.properties")

Zone becomes exclusive (door closed):
  → KV watch fires with { "is_exclusive": true, "tint_color": "#222244" }
  → Extension decides: "waitress should move away from the door"
  → Extension publishes: entity.waitress-1.move → target: { x: 60, y: 30 }
  → World Sim interpolates waitress to new position
  → Clients see waitress walk away
```

### Writing

Extensions can write to any KV key directly. Common patterns:

| Pattern | Example | Notes |
|---|---|---|
| Extension-private state | `ext.<ext_id>.npc_memory.<entity_id>` | NPC conversation history, AI state |
| Custom world state | `ext.<ext_id>.weather` | A weather extension publishing current conditions |
| Shared world state | `world.time` | A world-clock extension updating the virtual time |
| Zone state | `zones.<zone_id>.properties` | A zone-behavior extension modifying zone properties |

> **[OPEN]** Unrestricted KV writes mean an extension could overwrite zone
> state or user positions that the World Sim also writes. The MVP accepts
> this risk (extensions run in the trusted Docker network). For production,
> per-key ACLs or a write-through-World-Sim pattern can be added without
> changing the extension API.

### KV keys extensions typically watch

| Key pattern | What it gives the extension |
|---|---|
| `zones.<zone_id>.properties` | Zone exclusivity, tint, access policy |
| `world.time` | Virtual time of day |
| `users.<entity_id>.position` | Player positions (for NPC awareness) |
| `ext.<other_ext_id>.*` | Other extensions' published state (for cross-extension coordination) |
| `world.<shard_id>.volatile` | Cross-shard entity positions |

---

## 8. Custom components and client-side rendering

### Registering custom components

Extensions can register new component types at startup (see §3.2). The World
Sim adds them to the component registry, and the replication encoder handles
them generically — it serializes by component ID + raw protobuf bytes, as
described in `11-replication.md` §4.

### Client-side rendering

The client needs to know how to render custom components. The mechanism:

1. The extension ships a **client-side package** containing:
   - Protobuf definitions for its custom components.
   - A Phaser renderer (or renderer configuration) for each custom component.
   - Any required sprite sheets or assets (served via SeaweedFS/RustFS).
2. The client loads the package at startup (or dynamically when it first
   encounters an entity with an unknown component ID).
3. The client's replication decoder deserializes the component bytes using
   the extension's protobuf definition and passes the data to the extension's
   renderer.

> **[OPEN]** The client-side package delivery mechanism is not specified yet.
> Options:
> - **Static serving**: the extension serves a `.js` + `.proto` bundle from
>   its own HTTP endpoint, loaded by the client at startup.
> - **Asset registry**: the extension registers its package URL in JetStream
>   KV; the client discovers it and loads it.
> - **Phaser plugin system**: the extension ships a Phaser plugin that
>   registers custom renderers.
>
> The MVP may ship with predefined components only and defer custom component
> rendering to a later phase.

---

## 9. NATS subject contract

### Extension → World Sim

| Subject | Purpose | Frequency |
|---|---|---|
| `extension.register` | Register a new extension | On startup |
| `extension.<ext_id>.register_components` | Register custom component types | On startup |
| `extension.<ext_id>.register_triggers` | Register access/event triggers on tiles/entities, or input handlers for input types | At init time |
| `extension.<ext_id>.unregister_triggers` | Remove triggers | On demand |
| `extension.<ext_id>.register_zone` | Register a zone (boundary + properties) | At init time |
| `extension.<ext_id>.deregister` | Graceful shutdown | On shutdown |
| `extension.<ext_id>.heartbeat` | Liveness signal | Per interval (e.g. 5s) |
| `extension.<ext_id>.spawn` | Spawn an entity | On demand |
| `extension.<ext_id>.despawn` | Despawn an entity | On demand |
| `extension.<ext_id>.batch_update` | Batch component updates (multiple entities/components) | Per tick or event-driven |
| `entity.<entity_id>.update` | Direct component update | Per tick or event-driven |
| `entity.<entity_id>.move` | Request interpolated movement to a target | On demand |
| `trigger.<trigger_id>.reply` | Reply to an `ask` access trigger query | Async (within `ttl_ms`) |
| `input.<input_type>.reply.<req_id>` | Reply to an input handler dispatch (updates, consume_items) | Async |

### World Sim → Extension

| Subject | Purpose | Frequency |
|---|---|---|
| `extension.<ext_id>.registered` | Registration response (with existing entities) | On registration |
| `entity.<entity_id>.notify.<event>` | Entity-bound `notify` trigger dispatch (enter/exit) and proximity-bound `notify` dispatch (proximity_enter/proximity_exit) | Event-driven |
| `entity.<entity_id>.arrived` | Entity reached movement target | On arrival |
| `entity.<entity_id>.despawned` | Entity was despawned (by World Sim or admin) | Event-driven |
| `trigger.<trigger_id>.query` | `ask` access trigger query (does the kernel allow this move?) | On move attempt to a tile with an `ask` trigger |
| `input.<input_type>` | Input handler dispatch (player clicked or pressed a key; includes equipment snapshot, range, LOS, entities on tile) | On ActionFrame with a matching input type |
| `trigger.notify.tile.<map_id>.<x>.<y>` | Tile-bound `notify` trigger broadcast (all extensions self-filter) | On enter/exit |
| `world_sim.restarted` | World Sim restarted (extensions should re-register triggers and re-spawn) | On restart |
| `extension.<ext_id>.error` | Validation error for a command | On error |

### Extension → JetStream KV

| Direction | Key pattern | Purpose |
|---|---|---|
| Watch (read) | `zones.<zone_id>.properties` | React to zone state changes |
| Watch (read) | `world.time` | React to time of day |
| Watch (read) | `users.<entity_id>.position` | Track player positions |
| Watch (read) | Any key | Reactive world awareness |
| Write | `ext.<ext_id>.*` | Extension-private persistent state |
| Write | Any key | Influence shared world state (e.g. `world.time`, `zones.*`) |

> The subject naming convention is illustrative. The final convention will be
> defined in `07-network-protocol.md`.

---

## 10. Validation and error handling

The World Sim validates every command from an extension — the same validation
it applies to its own player movement. If validation fails, the World Sim publishes
an error on `extension.<ext_id>.error` and does not apply the command.

| Command | Validation rules |
|---|---|
| `spawn` | Position must be valid (on map, not on a `block` trigger tile). `entity_id` must not already exist. Components must be in the registry. |
| `despawn` | Entity must exist and have `ExtensionOwner` matching this extension. |
| `update` | Entity must exist and be owned by this extension. Component data must match its registered schema. Position updates must pass collision and trigger checks (access triggers on the target tile). |
| `batch_update` | All updates must pass validation. If any fails, the entire batch is rejected. |
| `move` | Entity must exist and be owned by this extension. Target position must be reachable (access triggers on the target tile must allow). Speed must be within configured bounds. |
| `register_components` | Component IDs must not collide with existing IDs. Protobuf schema must be valid. |
| `register_triggers` | `trigger_id` must not already exist. Tiles must be on a valid map. Entity-bound and proximity-bound triggers must reference an existing entity. `behavior` must be `block`, `allow`, `ask` (for access), `notify` (for event), or `input` (for action). Input handlers must specify `input` (a non-empty string, e.g. `click:left`, `key:E`). Proximity-bound triggers must specify `radius` (in tiles, ≥ 1) and `events` (a list of `proximity_enter` and/or `proximity_exit`). |
| `unregister_triggers` | `trigger_id` must exist and be owned by this extension. |
| `register_zone` | `zone_id` must not already exist. Boundary tiles must be on a valid map. |

### Error responses

```
{
  "status": "error",
  "error_code": "INVALID_POSITION",
  "message": "Position (43, 17) is inside a wall on map 'lobby'",
  "original_subject": "entity.waitress-1.update"
}
```

Common error codes:

| Code | Meaning |
|---|---|
| `ENTITY_NOT_FOUND` | The referenced entity does not exist in the ECS. |
| `NOT_AUTHORIZED` | The extension does not own the referenced entity. |
| `INVALID_POSITION` | The position is invalid (off-map, inside wall, zone-restricted). |
| `ENTITY_ALREADY_EXISTS` | A spawn command used an `entity_id` that already exists. |
| `COMPONENT_NOT_REGISTERED` | The component ID is not in the registry. |
| `VALIDATION_FAILED` | Component data failed schema validation. |
| `BATCH_PARTIAL_FAILURE` | One or more updates in a batch failed validation (entire batch rejected). |
| `HEARTBEAT_TIMEOUT` | The extension's heartbeat timed out (sent before freeze/despawn). |

---

## 11. Failure handling

### Extension crash

1. Heartbeats stop arriving.
2. After `heartbeat_interval_ms * 3`, the World Sim marks the extension as
   dead.
3. Action depends on the `on_death` policy declared at registration:
   - **freeze** (default): entities stay in the ECS but stop updating. Clients
     see them standing still. When the extension reconnects and re-registers,
     it re-claims its entities and resumes sending updates.
   - **despawn**: entities are removed from the ECS. Clients see
     `DestroyEntity`. The extension must re-spawn them on reconnect.

### Extension reconnect

1. Extension starts up, publishes `extension.register`.
2. World Sim responds with `registered`, including `existing_entities` if the
   freeze policy kept them alive.
3. If `existing_entities` is non-empty, the extension resumes sending updates
   for those entities. No re-spawn needed.
4. If `existing_entities` is empty (despawn policy, or World Sim restarted),
   the extension re-spawns its entities.
5. The extension re-registers its custom components (if any).
6. The extension re-registers its triggers and zones (if any).

### World Sim crash

1. The ECS is lost, including all extension-driven entities. The trigger
   registry and spatial index are also lost.
2. On restart, the World Sim reconstructs its own state from JetStream KV
   (player positions, zone state).
3. Extension entities and triggers are **not** in JetStream KV (they're
   ECS/registry-only). The World Sim publishes a `world_sim.restarted` event
   on NATS Core.
4. Extensions subscribe to `world_sim.restarted`. On receiving it, they
   re-register their custom components, re-register their triggers and zones,
   and re-spawn their entities. The World Sim recreates the entities in the
   ECS and rebuilds the spatial index from the trigger registrations.

> **[OPEN]** Whether extension entity state (position, components) should be
> persisted to JetStream KV so the World Sim can restore extension entities
> on restart without requiring extensions to re-spawn. Trade-off: more KV
> writes vs. simpler recovery. For the MVP, re-spawn on World Sim restart is
> acceptable.

---

## 12. Examples

### Example 0: Walls extension (first-party, trigger-based collision)

**Language:** Go (sibling process, part of the default gameplay pack)

```
Extension: walls-v1
  ├── On startup:
  │     ├── Register with World Sim (on_death: freeze)
  │     ├── Read Tiled map → identify wall tiles
  │     └── Register triggers: block on every wall tile
  │         (behavior: "block", binding: "tile")
  ├── On notify (tile-bound, event: "enter") for tiles adjacent to walls:
  │     └── (optional) play a "bump" animation on the entity
  ├── No entities to spawn (walls are tile properties, not entities)
  ├── No interactions
  └── Heartbeat every 30s

Note: the kernel caches all block triggers locally. Walking into a wall is
a local cache lookup — zero NATS round-trips. The walls extension owns the
decision (it registered the triggers), but the kernel enforces it.
```

### Example 1: Welcome waitress (LLM-driven NPC)

**Language:** Python (LangChain + NATS client)

```
Extension: waitress-npc-v1
  ├── On startup:
  │     ├── Register with World Sim (on_death: freeze)
  │     ├── Register custom component: SpeechBubble (id: 100)
  │     ├── Spawn waitress-1 at lobby entrance
  │     └── Register input handler: key:E (to handle interact key)
  ├── Watch KV: world.time → change behavior (greeting vs. closing)
  ├── On key:E (via input handler, self-filter for adjacent_entities):
  │     ├── Check adjacent_entities for waitress-1
  │     ├── Send message + context to LLM
  │     ├── Receive response
  │     ├── Batch update: AvatarAppearance (animation: "talk") + SpeechBubble (text: LLM response)
  │     └── After 5s: update SpeechBubble (text: "") + AvatarAppearance (animation: "idle")
  ├── On arrived at target: decide next action (walk to another spot, idle)
  ├── Direct position updates for fine-grained animation (e.g. waving offset)
  └── Heartbeat every 5s
```

### Example 2: Meeting room (zone + access trigger + timer)

**Language:** Go (sibling process, part of the default gameplay pack)

```
Extension: meeting-v1
  ├── On startup:
  │     ├── Register with World Sim (on_death: freeze)
  │     ├── Register custom component: TimerDisplay (id: 200)
  │     ├── Register zone: meeting-room-1 (boundary tiles from Tiled or dynamic)
  │     ├── Register triggers:
  │     │     ├── ask trigger on meeting-room-1 boundary tiles (behavior: "ask",
  │     │     │   default_on_timeout: "block", ttl_ms: 500)
  │     │     └── notify trigger on meeting-room-1 interior tiles (event: "enter",
  │     │         binding: "tile" — broadcast for occupancy counting)
  │     └── Watch KV: zones.meeting-room-1.properties
  ├── On ask trigger query (can entity enter?):
  │     ├── Check room capacity and access policy
  │     └── Reply: allow or block (with reason for client feedback)
  ├── On notify (enter): increment occupancy counter
  │     └── If first occupant: spawn timer display entity, set zone owner in KV
  ├── On notify (exit): decrement occupancy counter
  │     └── If last occupant: despawn timer, clear zone owner
  ├── Every minute while occupied: update TimerDisplay component
  ├── Write KV: zones.meeting-room-1.properties (is_exclusive, tint_color)
  │     → Bridge watches this KV key and cuts A/V for outsiders
  └── Heartbeat every 10s
```

### Example 3: Patrol guard NPC (own pathfinding)

**Language:** Rust (async NATS client)

```
Extension: patrol-guard-v1
  ├── On startup:
  │     ├── Register with World Sim (on_death: freeze)
  │     ├── Spawn guard-1 at security desk
  │     └── Register triggers:
  │           ├── input handler for key:E (to handle interact key)
  │           └── proximity-bound notify on guard-1 (radius: 4,
  │               events: proximity_enter, proximity_exit)
  ├── Own pathfinding AI computes per-tick positions
  │     └── Direct update: entity.guard-1.update → Position (every tick)
  │         (kernel validates each position against trigger registry)
  ├── On proximity_enter (player entered 4-tile radius):
  │     └── If player is not on guard's guest list → ALERT
  ├── State machine:
  │     ├── IDLE → wait 30s → PATROL
  │     ├── PATROL → path to waypoint A → arrived → path to waypoint B
  │     │            → arrived → path to waypoint C → arrived → IDLE
  │     └── ALERT (on key:E interact with "report" type, or proximity_enter)
  │           → path to reporting client → talk → IDLE
  ├── Watch KV: zones.<zone_id>.properties
  │     └── If zone becomes exclusive unexpectedly → ALERT
  ├── Write KV: ext.patrol-guard-v1.alerts (alert log)
  └── Heartbeat every 3s
```

### Example 4: Weather system (world state)

**Language:** Node.js

```
Extension: weather-v1
  ├── On startup:
  │     ├── Register with World Sim (on_death: freeze)
  │     ├── Register custom component: WeatherOverlay (id: 300)
  │     └── Spawn a weather overlay entity (covers entire map)
  ├── Every 5 minutes: change weather state
  │     ├── Update WeatherOverlay component (rain, snow, clear, fog)
  │     └── Write KV: ext.weather-v1.current (for other extensions to read)
  ├── Watch KV: world.time
  │     └── Night → foggy, Day → clear (example logic)
  ├── No interactions (ambient effect)
  └── Heartbeat every 30s
```

### Example 5: Full NPC system (multiple NPCs, shared intelligence)

**Language:** Python

```
Extension: npc-system-v1
  ├── On startup:
  │     ├── Register with World Sim (on_death: freeze)
  │     ├── Register custom components: DialogueState (id: 101), AIBehavior (id: 102)
  │     ├── Spawn 10 NPCs across the map
  │     └── Write KV: ext.npc-system-v1.population (NPC roster)
  ├── Each NPC has its own DialogueState (conversation history)
  ├── On key:E or click:left (via input handler, self-filter for adjacent_entities / entities_on_tile):
  │     ├── Check if an NPC the extension owns is adjacent / on the clicked tile
  │     ├── Route to shared LLM with NPC-specific personality prompt
  │     ├── Update DialogueState (append to history)
  │     └── Update SpeechBubble + AvatarAppearance
  ├── NPCs wander using target-based movement (LLM decides destinations)
  ├── Watch KV: ext.weather-v1.current
  │     └── If raining: NPCs move to sheltered positions
  ├── Cross-NPC coordination:
  │     └── If one NPC is interacted with, nearby NPCs may react (look at the speaker)
  └── Heartbeat every 5s
```

---

## 13. Deployment

Extensions run as separate Docker Compose services alongside the World Sim,
Pusher, and other components. They share the same Docker network and connect
to the same NATS instance.

```yaml
# docker-compose.yml (excerpt)
services:
  waitress-npc:
    build: ./extensions/waitress-npc
    environment:
      - NATS_URL=nats://nats:4222
      - LLM_API_KEY=${LLM_API_KEY}
    depends_on:
      - nats
      - world-sim
    restart: unless-stopped

  meeting-timer:
    build: ./extensions/meeting-timer
    environment:
      - NATS_URL=nats://nats:4222
    depends_on:
      - nats
      - world-sim
    restart: unless-stopped

  weather:
    build: ./extensions/weather
    environment:
      - NATS_URL=nats://nats:4222
    depends_on:
      - nats
      - world-sim
    restart: unless-stopped
```

Extensions can be added or removed by starting or stopping their containers.
The World Sim handles registration and deregistration automatically. No
restart of the World Sim or Pusher is needed.

---

## 14. Security considerations

- **MVP: trusted network.** Extensions run inside the Docker Compose network.
  NATS has no authentication. Any process on the network can register as an
  extension and write to any KV key. This is acceptable for a self-hosted MVP.
- **Production: NATS auth.** For production deployments, NATS should be
  configured with user authentication or JWT-based accounts. Each extension
  gets its own NATS credentials.
- **KV write ACLs.** In production, extensions' KV write access can be
  restricted to their own namespace (`ext.<ext_id>.*`) plus specific shared
  keys they're authorized to write. The MVP allows unrestricted writes.
- **LLM API keys.** Extensions that call external LLM APIs should receive
  their API keys via Docker secrets or environment variables, never committed
  to the repository.
- **Custom component validation.** The World Sim validates custom component
  data against the registered protobuf schema. An extension cannot inject
  arbitrary bytes — the data must deserialize correctly.

> **[OPEN]** A formal extension permission model (which KV keys an extension
> can write, which zones it can affect, rate limits) is deferred to a future
> security document.

---

## Open questions

- **[OPEN] Client-side extension package delivery.** How custom component
  renderers are delivered to the browser (static serving, asset registry, or
  Phaser plugin system). The MVP may ship with predefined components only.
- **[OPEN] Extension entity persistence.** Should extension entity state be
  persisted to JetStream KV so the World Sim can restore extension entities
  on restart without requiring extensions to re-spawn? Trade-off: more KV
  writes vs. simpler recovery.
- **[OPEN] KV write ACLs.** The MVP allows unrestricted KV writes. Production
  should restrict extensions to their own namespace plus authorized shared
  keys.
- **[OPEN] Extension permission model.** A formal model for rate limits,
  entity count limits, trigger count limits, and zone influence. Deferred to
  a future security document.
- **[OPEN] Extension discovery.** Should the World Sim expose a list of
  registered extensions (via NATS or an HTTP endpoint) for debugging and
  monitoring?
- **[OPEN] Hot-reload.** Can extensions be updated without despawning their
  entities? If the extension publishes a `deregister` with `action: freeze`,
  then restarts with a new version and re-registers, the World Sim could
  re-claim the frozen entities. This should work with the current design but
  needs testing.
- **[OPEN] Cross-extension communication.** Should extensions be able to
  communicate directly with each other via NATS (e.g. the weather extension
  and the NPC extension coordinating)? The current design allows this
  implicitly (both can read each other's KV keys), but a formal subject
  convention would help.
- **[OPEN] Trigger conflict resolution.** When two extensions register
  conflicting triggers on the same tile (e.g. one says `block`, the other
  says `allow`), the current `any-refusal-blocks` policy means `block` wins.
  Should there be a priority mechanism for more nuanced conflict resolution?
- **[OPEN] Tile-bound notify broadcast scaling.** The current design
  broadcasts tile-bound `notify` triggers to all extensions. This is fine for
  a small number of extensions (MVP: 5–10). If extension count grows, switch
  to a subject-based subscription model where extensions subscribe only to
  trigger IDs they care about.
- **[OPEN] First-party extension pack.** The "default gameplay" pack (walls,
  doors, base zone behaviors, base triggers) ships as sibling processes. The
  exact composition and configuration of this pack needs to be defined.
