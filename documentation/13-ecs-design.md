# ECS Design (decision record)

> **Status:** the detailed ECS design will live in `13-ecs-design.md` (to be
> created). This document captures the rationale and the decisions taken so
> far so they are not lost.

## 1. Why ECS, not OOP

In a classic object-oriented architecture, a player inherits from a `Character`
class, just like an NPC, while an interactive piece of furniture inherits from
a `StaticObject` class. This model creates rigid and complex hierarchies as
soon as we want to add dynamic interactivity — for example, transforming an
office chair into an "NPC" possessed by an AI.

The **Entity-Component-System (ECS)** pattern solves this by strictly
separating data from logic:

- **Entity** — a simple unique identifier (a string). It is just an empty
  container. Players, bots, meeting tables, and doors are all entities.
- **Component** — pure data structures, without algorithmic logic. Components
  are added to and removed from entities freely, so behaviour is composed
  rather than inherited.
- **System** — algorithms executed at each game-loop cycle (_game tick_). They
  query entities possessing a specific set of components.

This makes the codebase modular: new object types, triggers, and AI behaviours
can be added by defining new components and systems, without forking the
engine or modifying existing hierarchies. This is the architectural north star
stated in `01-vision-and-goals.md`.

## 2. Library choice

**[DECISION] Use [Ark](https://github.com/mlange-42/ark) unless a concrete
reason emerges to develop from scratch.**

Ark is an archetype-based ECS for Go. It is mature, actively maintained, and
provides the query/filter API the systems below need. Writing a custom ECS is
not a good use of MVP time.

> **[OPEN]** Revisit this decision during `13-ecs-design.md`. Evaluation
> criteria: archetype vs. sparse-set performance for our access patterns,
> serialization support (for snapshots/reconciliation), and whether the
> library's component model fits a string entity ID (see § 3).

## 3. Entity ID type

**[DECISION] Entity IDs are strings.**

This aligns with PocketBase's `entity_id` field (see
`06-data-model-and-persistence.md`) and avoids a type-conversion boundary
between the durable store and the in-world simulation. If Ark requires a
numeric ID internally, the World Simulator maintains a bidirectional
`string ↔ uint64` map; the string form is the canonical identifier everywhere
data is persisted or transmitted.

## 4. Initial component set

These are the components identified so far from the functional requirements
(`02-functional-requirements.md`) and the auth design (`08-auth-and-identity.md`).
The full set will be finalised in `13-ecs-design.md`.

| Component | Fields | Present on |
|---|---|---|
| `Position` | `x`, `y`, `map_id`, `dir` | Avatars, objects, decorations |
| `Velocity` | `vx`, `vy` | Moving entities (avatars, vehicles) |
| `MovementTarget` | `x`, `y`, `map_id`, `speed` | Entities moving toward a target (extension-driven movement; the kernel interpolates toward the target each tick, see `18-extensions.md` § 5.2) |
| `Interactable` | `interaction_type` (Iframe, Meeting, ExternalLink, …), `trigger_radius` | Interactive objects |
| `Traversable` | `bool` (dynamically mutable) | Objects, tiles |
| `AIBehavior` | `state` (Idle, Patrol, Discussion) | NPCs (post-MVP) |
| `NetworkSession` | `client_id` (links the entity to its Pusher session) | Human-controlled avatars; **absent for NPCs and extension entities** |
| `AvatarAppearance` | body shape, skin tone, hair, outfit, accessory | Avatars |
| `Bubble` | `type` (speech, emoji, status), `content`, `expires_at` | Avatars showing a message/status bubble (see `16-avatars.md`) |
| `Attachment` | `parent_entity_id`, `offset` | An avatar attached to a vehicle/another entity (see `16-avatars.md`) |
| `ZoneMembership` | `zone_id`, `joined_at` | Entities currently inside a zone |
| `ZoneAccess` | `policy` (open, knock, invite_only), `owner_entity_id` | Zone entities with access control (see `14-zones-and-interactions.md`) |
| `ExtensionOwner` | `extension_id` | Entities driven by an extension (server-only, never replicated; see `18-extensions.md`) |
| `TriggerOwner` | `trigger_id`, `extension_id` | Entities claimed by an extension via trigger registration (server-only, never replicated; see `18-extensions.md` §3a) |
| `Item` | `item_type`, `display_name`, `icon`, `stackable`, `quantity` | All items (see inventory/equipment design in `plans/2026-07-01-inventory-equipment-action-triggers-design.md`) |
| `InventorySlot` | `owner_entity_id` | Items in a player's inventory (no `Position`; replicated to owner only) |
| `Equipped` | `owner_entity_id`, `slot` (e.g. "main_hand") | Items equipped by a player (replicated to owner only) |
| `Equipment` | `slots: [{slot, item_entity_id, item_type}]` | Player avatars (replicated to all in AOI, so others can render equipped items) |

## 5. Initial system set

| System | Queries | Responsibility | Runs in |
|---|---|---|---|
| `PlayerMovementSystem` | `Position` + `NetworkSession` + `InputState` | Authoritative player avatar position update per tick. Computes target tile from input, evaluates gate triggers (block/allow cached, ask routed to extension via NATS), updates Position if allowed. This is the only gameplay movement system in the kernel. | World Simulator (kernel) |
| `InputHandlerSystem` | `Position` + `NetworkSession` + `Equipment` | Evaluates `ActionFrame` input: computes range, line-of-sight (Bresenham raycast), entities on clicked tile / adjacent entities, and equipment snapshot. Broadcasts to all extensions registered for that input type. Collects all replies within a timeout and applies them. Spatial data computation only — does not decide what the action does. | World Simulator (kernel) |
| `ReplicationSystem` | `NetworkSession` + (changed components) | Encodes dirty components into generic replication messages (`SpawnEntity`, `UpdateComponent`, `DestroyEntity`, `PlayAnimation`) for interested clients. Runs in the World Simulator. See `11-replication.md`. | World Simulator (kernel) |
| `TriggerSystem` | (deprecated in kernel) | Trigger logic is now implemented by extensions via the trigger registry (see `18-extensions.md` §3a). The kernel evaluates zone gate triggers (block/allow/ask), dispatches zone notify triggers (enter/exit, proximity for mobile circle zones), and broadcasts input events to registered extensions with range/LOS data — but does not implement trigger behavior. | Extension |
| `BehaviorTreeSystem` | (deprecated in kernel) | NPC AI is implemented by extensions. The kernel does not run AI systems. | Extension |
| `ZoneSystem` | (deprecated in kernel) | Zone behavior (exclusivity, knock-to-join, timers) is implemented by extensions via zone triggers. The kernel stores zone shapes and routes zone-entry triggers to the owning extension. | Extension |

## 6. Open questions for `13-ecs-design.md`

- Archetype vs. sparse-set: which fits our access patterns better?
- How does the ECS interact with NATS subject partitioning for area-of-interest
  filtering (see `14-zones-and-interactions.md`)?
- **[DECISION]** The ECS runs inside the **World Simulator** process (the
  kernel), not the Pusher. The Pusher is a thin WebSocket proxy with no ECS
  knowledge. The kernel's only gameplay systems are PlayerMovementSystem,
  InputHandlerSystem (input dispatch + spatial data computation), and ReplicationSystem;
  all other gameplay behavior (inventory, equipment, item effects) is delegated
  to extensions via NATS (see `18-extensions.md`). See `10-world-simulator.md`
  for the World Simulator specification and `09-pusher.md` for the Pusher
  specification.
- How are NPC entities injected: real WebSocket connection or in-process input
  injection? (See `08-auth-and-identity.md` §7.)
