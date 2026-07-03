# Inventory, Equipment, and Action Triggers

**Date:** 2026-07-01
**Status:** Design — validated

## Overview

Three related features:

1. **Player inventory** — players can hold items.
2. **Equipment** — players can equip items that change available actions.
3. **Input handlers** — a trigger category fired by player input (clicks, key presses). Extensions register for input types; the kernel broadcasts each input event to all registered extensions with range, LOS, entities, and equipment data. All replies are applied.

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Items are ECS entities | Items have components, Position, etc. | Unified model; items can have complex state |
| Inventory logic ownership | Split: kernel spatial, extension gameplay | Consistent with "kernel = spatial authority only" |
| Range/LOS | Kernel computes, includes in payload | Kernel is data provider, not gatekeeper; extensions self-filter |
| Trigger model | "action" category, binding: "input" | Player-initiated, broadcast-based, not tile-bound |
| Client frame | Always ActionFrame (InteractFrame deprecated) | One frame type for all player-initiated input |
| Conflict resolution | All replies applied | No conflict — extensions self-filter, multiple can respond |
| Entity interaction routing | Removed — replaced by input handlers | Extensions register for input types and self-filter |

## 1. Items as ECS Entities

Items are full ECS entities, consistent with the existing model where players, bots, meeting tables, and doors are all entities. An item transitions between three spatial states:

- **On the ground**: has `Position`, visible to all players via normal AOI replication.
- **In inventory**: loses `Position`, gains `InventorySlot{owner_entity_id}`. Not visible in the world. Replicated only to the owning player.
- **Equipped**: loses `InventorySlot`, gains `Equipped{owner_entity_id, slot}`. Not visible in the world as a separate entity. The extension updates the player's `AvatarAppearance` to reflect the equipped item.

### New Components

| Component | Fields | On | Replicated to |
|---|---|---|---|
| `Item` | `item_type`, `display_name`, `icon`, `stackable`, `quantity` | All items | All in AOI (ground) / owner only (inventory/equipped) |
| `InventorySlot` | `owner_entity_id` | Items in a player's inventory | Owner only |
| `Equipped` | `owner_entity_id`, `slot` (e.g. "main_hand") | Items equipped by a player | Owner only |
| `Equipment` | `slots: [{slot, item_entity_id, item_type}]` | Player avatars | All in AOI |

The `Equipment` component on the player entity lets other clients render equipped items. The item entities themselves are only replicated to the owning player.

Item-specific data (weapon damage, range, consumable effects) uses custom components registered by the inventory extension, following the existing custom component registration pattern.

## 2. Input Handlers

A new trigger category alongside access and event triggers. Input handlers are registered by extensions for specific input types (`click:left`, `click:right`, `key:E`, etc.) and fire when a player triggers that input. Unlike access and event triggers, input handlers are not bound to tiles — they are bound to input types.

### Registration

```
Subject: extension.<extension_id>.register_triggers
Payload: {
  triggers: [{
    trigger_id: "combat-click-left",
    category: "action",
    binding: "input",
    input: "click:left",
    owner_extension_id: "combat-ext"
  }]
}
```

No `max_range`, `require_los`, or tile coordinates at registration time. The kernel does not gate — it provides data and lets extensions self-filter.

### Kernel Dispatch

When an input event arrives, the kernel:

1. Looks up all extensions registered for that input type.
2. If none registered → `ActionResultFrame{ ok: false, reason: "no_handler" }`.
3. Computes contextual data:
   - **Clicks**: `target_tile`, `entities_on_tile`, `range` (tile distance), `has_los` (Bresenham raycast).
   - **Key presses**: `adjacent_entities` (entities on tiles adjacent to the player). No target tile, no range, no LOS.
4. Gathers the player's `Equipment` snapshot.
5. Broadcasts to all registered extensions.

### Broadcast to Extensions

```
Subject: input.<input_type_with_dots>
  (e.g. input.click.left, input.key.e)
Payload: {
  request_id,
  source_entity_id,
  client_id,
  input: "click:left",
  target_tile: { map_id, x, y },         // null for key presses
  player_position: { map_id, x, y, dir },
  entities_on_tile: [...],                // for clicks; null for keys
  adjacent_entities: [...],               // for keys; null for clicks
  has_los: true,                          // for clicks; null for keys
  range: 8,                               // for clicks; null for keys
  equipment: [
    { slot: "main_hand", item_entity_id: "bow-7", item_type: "bow" },
    { slot: "off_hand", item_entity_id: null }
  ],
  reply_to: "input.click.left.reply.<request_id>"
}
```

### Extension Reply (async, all replies applied)

```
Subject: input.<input_type_with_dots>.reply.<request_id>
Payload: {
  request_id,
  extension_id: "combat-ext",
  updates: [
    { entity_id: "arrow-1", component: "Position", data: { x, y, map_id } },
    { entity_id: "user-42", component: "AvatarAppearance", data: { animation: "shoot" } }
  ],
  consume_items: [{ item_entity_id: "arrow-1" }]
}
```

The kernel collects all replies within a timeout (e.g. 500 ms). All replies are applied — no conflict resolution.

## 3. Network Protocol — ActionFrame

### New Frame

```protobuf
message ActionFrame {
  uint32 seq = 1;
  string input_type = 2;   // "click:left", "click:right", "key:E", etc.
  string target_map_id = 3; // for clicks; empty for key presses
  uint32 target_x = 4;     // for clicks; 0 for key presses
  uint32 target_y = 5;     // for clicks; 0 for key presses
  bytes params = 6;        // optional: action-specific data
}
```

### InteractFrame Deprecated

`InteractFrame` is removed. `action` replaces `interact` in the `ClientFrame` envelope:

```
oneof payload {
  auth · input · action · token_refresh · ping
}
```

The client sends `ActionFrame` for all tile clicks and key presses. The `input_type` field identifies the input. For key presses, the target tile fields are empty — the kernel computes adjacent entities from the player's position.

### ActionResultFrame (new ServerFrame payload)

```protobuf
message ActionResultFrame {
  uint32 seq = 1;
  bool ok = 2;
  string reason = 3;   // "no_handler", "timeout", "rejected"
}
```

Updated `ServerFrame` envelope:

```
oneof payload {
  replication · auth_result · action_result · error · pong · control
}
```

### Kernel Routing

```
ActionFrame arrives
  |
  +-- Look up extensions registered for input_type
  |     +-- none -> ActionResultFrame{ ok: false, reason: "no_handler" }
  |
  +-- Compute contextual data:
  |     +-- clicks: target_tile, entities_on_tile, range, has_los (Bresenham)
  |     +-- keys: adjacent_entities (no target tile, no range, no LOS)
  |     +-- always: equipment snapshot
  |
  +-- Broadcast to all registered extensions
  |     +-- each extension self-filters and replies async
  |
  +-- Collect all replies within timeout
        +-- apply all updates + consume_items
        +-- ActionResultFrame{ ok: true }
        +-- no reply in timeout -> ActionResultFrame{ ok: false, reason: "timeout" }
```

### What's Removed

- `InteractFrame` message type
- `interact` from `ClientFrame.payload`
- Entity interaction routing (`entity.<id>.interact`, `ExtensionOwner`-based fallback)
- `entity.<id>.notify.interact` event (replaced by input handlers)
- Separate InteractFrame handling code path in the kernel
- `max_range`, `require_los`, `los_through_walls` from trigger registration (kernel computes and provides, doesn't gate)

## 4. Kernel LOS Raycasting

The kernel gains a spatial capability: line-of-sight raycasting through the tile grid. This is a spatial operation, consistent with the kernel's role as spatial authority.

### Algorithm — Bresenham Line Through Tiles

The kernel casts a ray from the player's tile to the target tile using Bresenham's line algorithm (tile-space). For each tile along the ray:

1. **Wall check**: is the tile a wall in the Tiled map? If yes, LOS blocked.
2. **Block trigger check**: does the tile have a `block` access trigger? If yes, LOS blocked.
3. **Entity check**: is there a non-traversable entity on the tile (`Traversable=false`)? If yes, LOS blocked.

The ray starts from the tile adjacent to the player (the player's own tile doesn't block).

### Edge Cases

- **Diagonal clicks**: Bresenham handles diagonals. A diagonal move between two walls is blocked (no corner cutting), matching movement rules.
- **Same tile click**: range = 0, LOS trivially passes.
- **Adjacent click**: range = 1, LOS trivially passes (one step, no intermediate tiles).
- **`los_through_walls` flag**: removed — the kernel always computes LOS with wall checks. Extensions decide what to do with the `has_los` value.

### Performance

Raycasting is O(ray length) — at most the distance to the clicked tile. The kernel already has all data in memory.

### What the Kernel Does NOT Decide

The kernel doesn't decide what happens when the input reaches the extension. It only provides spatial data (range, LOS, entities). The extension decides whether the action makes sense (equipment, target validity, cooldowns, range, LOS).

## 5. Inventory Extension — Gameplay Logic

A first-party extension (e.g. `inventory-ext`) owns all inventory and equipment gameplay semantics. The kernel handles only the spatial transitions.

### Kernel's Spatial Responsibilities

- An item on the ground has `Position` — in the ECS, replicated via normal AOI.
- When an extension removes `Position` and adds `InventorySlot` to an item, the kernel validates the owner exists and applies the change. The item disappears from the world.
- When an extension removes `InventorySlot` and adds `Equipped`, the kernel applies it.
- The kernel replicates `InventorySlot` and `Equipped` only to the owning player.
- The kernel replicates the `Equipment` component on the player entity to all clients in AOI.

### Extension's Gameplay Responsibilities

- **Pickup**: receives an input event (click or key press) on an item tile, validates rules, sends component updates (ground -> inventory).
- **Equip/unequip**: receives a client action, validates slot compatibility, swaps `InventorySlot` <-> `Equipped`, updates the player's `Equipment` component.
- **Drop**: sends updates to remove `InventorySlot`/`Equipped`, add `Position` (kernel validates the tile).
- **Use/consume**: handles item effects and sends `consume_items` in input handler replies.
- **Equipment in input handlers**: reads the equipment snapshot from the input handler payload to decide the action.

### Custom Components

```
Subject: extension.inventory-ext.register_components
Payload: {
  components: [
    { component_id: 200, name: "WeaponStats", protobuf_schema: "..." },
    { component_id: 201, name: "ConsumableEffect", protobuf_schema: "..." },
    { component_id: 202, name: "Cooldown", protobuf_schema: "..." }
  ]
}
```

These are gameplay-specific. The kernel replicates them (to the owning player only) without understanding their semantics.

### Persistence

Item definitions (templates) and player inventory state are persisted by the extension to PocketBase (durable) or JetStream KV (semi-persistent). The kernel doesn't persist inventory — it only holds the live ECS state.

## 6. Replication Impact

### Three Replication Visibility Tiers

| Entity state | Replicated to | How |
|---|---|---|
| Item on ground (`Position`) | All clients in AOI | Normal AOI replication |
| Item in inventory (`InventorySlot`) | Owning player only | Kernel filters by `owner_entity_id` matching recipient's entity ID |
| Item equipped (`Equipped`) | Owning player only | Same filter as `InventorySlot` |
| Player's `Equipment` component | All clients in AOI | Normal replication |

### Owner-Only Replication

The replication encoder already filters by AOI. For entities with `InventorySlot` or `Equipped`, the encoder adds a second filter: the entity is only included in a client's replication batch if that client's avatar entity ID matches the `owner_entity_id` field.

### Pickup/Drop Transition

**Ground -> inventory** (extension removes `Position`, adds `InventorySlot`):

1. Item leaves all other clients' AOI — no `Position` means not in spatial index. Next tick sends `DestroyEntity` to everyone who had it.
2. Item enters owner's replication — encoder sees `InventorySlot{owner}` and includes it as `SpawnEntity` in the owner's batch.

**Inventory -> ground** (extension removes `InventorySlot`, adds `Position`):

1. Item leaves owner's replication (`DestroyEntity` to owner).
2. Item enters spatial index — `SpawnEntity` to all clients in AOI on next tick.

## 7. Documentation Files to Update

The following documentation files will need updates to reflect this design:

- `07-network-protocol.md` — add `ActionFrame`, `ActionResultFrame`; remove `InteractFrame`; update `ClientFrame`/`ServerFrame` envelopes
- `13-ecs-design.md` — add `Item`, `InventorySlot`, `Equipped`, `Equipment` components
- `14-zones-and-interactions.md` — add input handler category; update interaction routing to input handler broadcast
- `18-extensions.md` — add input handler registration; add inventory extension pattern; replace entity interaction routing with input handler model
- `10-world-simulator.md` — add LOS raycasting to kernel responsibilities; add input handler dispatch
- `11-replication.md` — add owner-only replication for inventory/equipped items
- `05-architecture.md` — mention inventory extension in the extension list
- `20-roadmap.md` — add inventory/equipment/input handlers to the roadmap
- `09-pusher.md` — no changes (Pusher is a pass-through)
