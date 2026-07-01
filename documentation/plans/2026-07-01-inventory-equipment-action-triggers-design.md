# Inventory, Equipment, and Action Triggers

**Date:** 2026-07-01
**Status:** Design — validated

## Overview

Three related features:

1. **Player inventory** — players can hold items.
2. **Equipment** — players can equip items that change available actions.
3. **Action triggers** — a new trigger category fired by clicking a tile, with range and line-of-sight validation by the kernel. Action behavior depends on the player's equipment.

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Items are ECS entities | Items have components, Position, etc. | Unified model; items can have complex state |
| Inventory logic ownership | Split: kernel spatial, extension gameplay | Consistent with "kernel = spatial authority only" |
| Range/LOS validation | Kernel validates both | Spatial concepts; kernel owns the tile grid |
| Trigger model | New "action" trigger category | Player-initiated, semantically distinct from enter/exit |
| Client frame | Always ActionFrame (InteractFrame deprecated) | One frame type for all player-initiated spatial actions |
| Adjacent clicks | Kernel falls back to entity interaction routing | No action trigger on tile → route to entity on tile |

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

## 2. Action Triggers

A new trigger category alongside access and event triggers. Action triggers are registered by extensions on tiles (or tile regions) and fire when a player clicks that tile.

### Registration

```
Subject: extension.<extension_id>.register_triggers
Payload: {
  triggers: [{
    trigger_id: "bow-shot-zone",
    type: "action",
    tile: { map_id, x, y },
    event: "click",
    max_range: 8,
    require_los: true,
    los_through_walls: false,
    adjacent_ok: true,
    default_on_timeout: "drop"
  }]
}
```

### Kernel Validation

When a click arrives, the kernel validates:

1. **Range check**: distance from player's `Position` to clicked tile <= `max_range`. If no `max_range` set, defaults to adjacent-only (distance <= 1).
2. **Line-of-sight check**: if `require_los` is true, the kernel raycasts through the tile grid. A tile blocks LOS if it has a `block` access trigger, a non-traversable entity (`Traversable=false`), or is a wall in the map. If `los_through_walls` is false, walls block.
3. **If validation fails**: kernel sends `ActionResultFrame{ ok: false, reason }` back to the client immediately.
4. **If validation passes**: kernel publishes the action event to the owning extension.

### Dispatch to Extension

```
Subject: trigger.<trigger_id>.action
Payload: {
  trigger_id,
  entity_id,
  client_id,
  clicked_tile: { map_id, x, y },
  equipment: [
    { slot: "main_hand", item_entity_id: "bow-7", item_type: "bow" },
    { slot: "off_hand", item_entity_id: null }
  ],
  reply_to: "trigger.<trigger_id>.action.reply.<request_id>"
}
```

The kernel includes a snapshot of the player's `Equipment` component so the extension knows what the player is holding and can decide the action.

### Extension Reply (async, doesn't block the tick)

```
Subject: trigger.<trigger_id>.action.reply.<request_id>
Payload: {
  updates: [
    { entity_id: "arrow-1", component: "Position", data: { x, y, map_id } },
    { entity_id: "user-42", component: "AvatarAppearance", data: { animation: "shoot" } }
  ],
  consume_items: [{ item_entity_id: "arrow-1" }]
}
```

## 3. Network Protocol — ActionFrame

### New Frame

```protobuf
message ActionFrame {
  uint32 seq = 1;
  string target_map_id = 2;
  uint32 target_x = 3;
  uint32 target_y = 4;
  bytes params = 5;   // optional: interaction_type override, action-specific data
}
```

### InteractFrame Deprecated

`InteractFrame` is removed. `action` replaces `interact` in the `ClientFrame` envelope:

```
oneof payload {
  auth · input · action · token_refresh · ping
}
```

The client sends `ActionFrame` for all tile clicks and keypress interactions. For keypress interactions, the client computes the facing tile from `Position{dir}` and sends `ActionFrame{target_x, target_y}`.

### ActionResultFrame (new ServerFrame payload)

```protobuf
message ActionResultFrame {
  uint32 seq = 1;
  bool ok = 2;
  string reason = 3;   // "out_of_range", "no_los", "no_target", "no_trigger", "rejected"
}
```

Updated `ServerFrame` envelope:

```
oneof payload {
  replication · auth_result · action_result · error · pong · control
}
```

### Unified Kernel Routing

```
ActionFrame arrives
  |
  +-- Range check (player Position vs. target tile)
  |     +-- fail -> ActionResultFrame{ ok: false, reason: "out_of_range" }
  |
  +-- LOS check (raycast, if require_los on any matching trigger)
  |     +-- fail -> ActionResultFrame{ ok: false, reason: "no_los" }
  |
  +-- Action triggers on clicked tile?
  |     +-- yes -> dispatch to owning extension (with equipment snapshot)
  |                -> extension replies async -> apply updates
  |
  +-- Entity on clicked tile?
  |     +-- yes -> fallback to interaction routing:
  |                +-- ExtensionOwner? -> entity.<id>.interact -> extension
  |                +-- notify trigger? -> entity.<id>.notify.interact -> extension
  |                +-- neither? -> ActionResultFrame{ ok: false, reason: "no_target" }
  |
  +-- No trigger, no entity
        +-- ActionResultFrame{ ok: false, reason: "no_target" }
```

### What Stays the Same

- NATS subjects for entity interactions: `entity.<id>.interact`, `entity.<id>.notify.interact`, `entity.<id>.interact.reply.<request_id>`
- Interaction routing logic (ExtensionOwner -> notify -> drop)
- Async reply pattern with `reply_to`
- `Interactable` component on entities

### What's Removed

- `InteractFrame` message type
- `interact` from `ClientFrame.payload`
- Separate InteractFrame handling code path in the kernel

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
- **`los_through_walls` flag**: if true, the kernel skips wall checks but still checks entity blocking. Default: false.

### Performance

Raycasting is O(ray length) — at most `max_range` tiles. With `max_range` typically <= 20, this is cheap and runs synchronously in the input handling path. The kernel already has all data in memory.

### What the Kernel Does NOT Decide

The kernel doesn't decide what happens when the click reaches the extension. It only answers: "can this player reach this tile spatially?" The extension decides whether the action makes sense (equipment, target validity, cooldowns).

## 5. Inventory Extension — Gameplay Logic

A first-party extension (e.g. `inventory-ext`) owns all inventory and equipment gameplay semantics. The kernel handles only the spatial transitions.

### Kernel's Spatial Responsibilities

- An item on the ground has `Position` — in the ECS, replicated via normal AOI.
- When an extension removes `Position` and adds `InventorySlot` to an item, the kernel validates the owner exists and applies the change. The item disappears from the world.
- When an extension removes `InventorySlot` and adds `Equipped`, the kernel applies it.
- The kernel replicates `InventorySlot` and `Equipped` only to the owning player.
- The kernel replicates the `Equipment` component on the player entity to all clients in AOI.

### Extension's Gameplay Responsibilities

- **Pickup**: receives an action trigger or interaction on an item entity, validates rules, sends component updates (ground -> inventory).
- **Equip/unequip**: receives a client action, validates slot compatibility, swaps `InventorySlot` <-> `Equipped`, updates the player's `Equipment` component.
- **Drop**: sends updates to remove `InventorySlot`/`Equipped`, add `Position` (kernel validates the tile).
- **Use/consume**: handles item effects and sends `consume_items` in action replies.
- **Equipment in action triggers**: reads the equipment snapshot from the action trigger payload to decide the action.

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
- `14-zones-and-interactions.md` — add action trigger category; update interaction routing to show ActionFrame fallback
- `18-extensions.md` — add action trigger registration; add inventory extension pattern; update interaction routing (ActionFrame entry point)
- `10-world-simulator.md` — add LOS raycasting to kernel responsibilities; add action trigger evaluation
- `11-replication.md` — add owner-only replication for inventory/equipped items
- `05-architecture.md` — mention inventory extension in the extension list
- `20-roadmap.md` — add inventory/equipment/action triggers to the roadmap
- `09-pusher.md` — no changes (Pusher is a pass-through)
