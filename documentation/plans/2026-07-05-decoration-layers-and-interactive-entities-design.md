# Decoration Layers, Depth Sorting, and Interactive Entities

**Date:** 2026-07-05
**Status:** Design — validated (brainstormed, not yet implemented)

## Overview

Three related problems, resolved together:

1. **Multiple decoration layers with altitude** — the current lite MVP only
   recognizes a single hardcoded `"Ground"` layer (`21-map-design-guide.md`).
   Mappers need to stack several decoration layers (floor detail, furniture,
   canopy/overhang) at different visual altitudes.
2. **Draw order / Y-sort** — flagged `[OPEN]` in `15-maps-and-tiled.md`. A tall
   object (tree, wall corner, furniture) must be able to hide the player when
   the player is "behind" it, and show in front when the player is "in front"
   of it. A single fixed depth per layer can't do this.
3. **Interactive map-authored entities** — e.g. a fixed box that animates and
   triggers side effects (turning off a light) when interacted with, as
   opposed to a zone enter/exit trigger. The mechanism already exists on
   paper (`13-ecs-design.md`, `14-zones-and-interactions.md` §3a,
   `documentation/plans/2026-07-01-inventory-equipment-action-triggers-design.md`)
   but has never been walked through end-to-end for a concrete map-authored
   prop, and needs an ownership-claiming convention when several extensions
   compete for the same input type.

This design resolves all three without inventing new wire protocol beyond
what's already specced for input triggers (`ActionFrame`).

## Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Decoration layer recognition | Any layer with property `layer_type=decoration`, any name | Removes the `"Ground"` name hack; supports multiple layers |
| Layer altitude | Tiled layer stack order (list order), no separate numeric property | Simplest mental model: reordering layers in Tiled *is* changing altitude |
| Decoration authoring | Tile layers (bulk scenery) **and** object layers (individually placed props) | Different needs: grid-aligned floor detail vs. freely placed, metadata-carrying props |
| Front/back occlusion | Per-layer `sort_mode`: `static` (fixed band) or `dynamic` (Y-sorted with avatars) | Most decorations never need to interleave with the player; only tall/walkable-around objects do |
| Tall-object anchor | Object's base/feet Y (Tiled tile-objects already anchor bottom-left = this is just `obj.y`) | Matches existing avatar anchor convention (`setOrigin(0.5, 0.75)`) |
| Multi-tile-tall sprite occlusion | Whole sprite is one depth value (no per-pixel slicing) | Simpler; correctness depends on collision footprint, not visual slicing |
| Interactive entities | "Base entities" from map object layer, claimed by extensions at startup | Matches existing `10-world-simulator.md` §5a "Base entities" concept |
| Entity ownership | `owner_extension` property + server-only `TriggerOwner` component | Lets a generic extension and dedicated extensions coexist without collisions |
| Interaction wire protocol | `ActionFrame` / input triggers (already specced) | No new frame types; reuses `PlayAnimation` + `UpdateComponent` for effects |

---

## Part A — Decoration layers (authoring)

Any layer (tile layer or object layer) carrying a custom Tiled property
`layer_type = "decoration"` is treated as a decoration layer, regardless of
its name. `Walls` keeps its own reserved role (collision fallback) and is
not a decoration layer.

- **Altitude = layer list order.** Decoration layers draw bottom-to-top in
  the order they appear in Tiled's layer stack — same as today's implicit
  behavior for `Ground`, generalized to N layers. No separate numeric
  altitude property; reordering layers in the Tiled layer list changes
  altitude.
- **Both tile layers and object layers** are valid decoration sources:
  - Tile layers → bulk scenery (floor detail, rugs, grass): grid-aligned,
    cheap.
  - Object layers with a tile `gid` → individually placed props (a specific
    tree, a crate) needing free positioning and per-object custom properties.
- Frontend change (`GameScene.create`): instead of `createLayer(0, …)` /
  `createLayer(1, …)` by hardcoded index, iterate `map.layers`, read each
  layer's `layer_type` property, and for `"decoration"` layers create the
  tile layer (or spawn object-layer sprites) with a depth assigned per Part B
  instead of a hardcoded constant.

## Part B — Depth / Y-sort algorithm

Each decoration layer also carries a `sort_mode` property (default `static`
if absent):

| `sort_mode` | Behavior |
|---|---|
| `static` | Fixed depth = altitude band from layer order (Part A). Never interleaves with avatars. Use for floor decals, rugs, shadows, and canopy/overhang overlays that must always render above everyone. |
| `dynamic` | Depth computed **per-tile / per-object** from its world Y (feet/base) position, placed into the **same depth band as avatars**, so it Y-sorts against players directly. Use for furniture, trees, walls-as-props — anything the player can walk in front of or behind. |

**Unified depth formula** (single space so avatars and `dynamic` decorations
sort together):

```
depth = BAND_BASE(layer) + (baseY_pixels / mapHeightPixels)
```

- `BAND_BASE(layer)`:
  - `static` layers *before* the dynamic content in the layer list get a low
    band (e.g. `0, 1, 2, …` per layer order) — always below avatars.
  - The `dynamic` band and avatars share one band (e.g. `1000`).
  - `static` layers placed *after* the dynamic layers in the list (canopy,
    roof overlays) get a high band (e.g. `2000+`) — always above avatars.
- `baseY_pixels`: for tile layers, the tile's bottom edge `(row + 1) * 32`;
  for objects/avatars, the sprite's own base/feet Y.
- Avatars recompute this every frame (their Y changes as they move).
  `dynamic` decorations from tile layers are static geometry, so their depth
  is computed once at load time.

### Anchoring multi-tile-tall objects

Tiled tile-objects (dragged from a tileset onto an object layer) already
anchor at their **bottom-left corner** — the same convention the avatar
sprites use (`setOrigin(0.5, 0.75)`). So for a tree spanning 3.5 tiles in
height, `obj.y` from the Tiled JSON is already the base/feet Y — no
`+height` adjustment needed.

### Occlusion for large sprites

A multi-tile-tall sprite is drawn as **one image** with a single depth value
— there is no per-pixel/per-row cutting. The illusion of "walking behind" a
tall object works because:

1. The sprite's depth is anchored to its **base**, so a player standing
   below/beside the base sorts in front, and a player standing above the
   base (inside the canopy's footprint) sorts behind.
2. The object's **collision footprint should be authored narrower than its
   visual sprite** (e.g. `Traversable=false` only on the trunk's base tile,
   not the full 3.5-tile bounding box). This keeps the player out of
   positions where a single flat depth value would look visually wrong for
   that sprite's silhouette.

If a specific asset's silhouette makes single-image occlusion look wrong at
any reachable player position (e.g. a very wide low canopy), the fallback is
splitting the sprite into two objects (a Y-sorted trunk + an always-in-front
canopy on a `static` high band) — not needed for the common case.

---

## Part C — Interactive entities

### Authoring: base entities

Interactive props are authored as objects on an object layer, following the
same pattern the `Zones` layer already uses (`21-map-design-guide.md`):

- Object name → `entity_id` (must be unique, e.g. `light-switch-box-1`)
- Tile `gid` → initial appearance/sprite
- Custom properties → initial components, e.g.:
  - `interactable = true` → adds `Interactable{trigger_radius}` component
  - `trigger_radius = 1.5`
  - `owner_extension = ext-props` (see ownership below)

At map load, the kernel creates these as ECS entities with `Position` +
`Interactable` + appearance components — this is the "Base entities" concept
already described in `10-world-simulator.md` §5a. They have **no
`ExtensionOwner`/`TriggerOwner`** yet; they're inert until claimed.

### Ownership: generic and dedicated extensions coexist

Multiple extensions can register for the same input type (e.g. `key:E`) and
self-filter — this is the existing broadcast model
(`14-zones-and-interactions.md` §3a), the same pattern `ext-walls` already
uses (reads the whole map, only acts on `zone_type=wall`, ignores the rest).
This lets both patterns coexist without collision:

- A **generic extension** (e.g. `ext-props`) claims any base entity with a
  recognized `entity_type` it doesn't need bespoke logic for (crates,
  levers, torches).
- A **dedicated extension** (e.g. `ext-vault-door`) claims one specific
  `entity_id` or `entity_type` needing custom multi-step logic.

**Claiming convention** (resolves ambiguity, avoids double-handling):

1. Each prop object gets an explicit `owner_extension` property in Tiled
   (omitted or set to a generic ID for catch-all entities).
2. At startup, each extension reads the map (like `ext-walls` does) and
   sends an entity update adding the server-only `TriggerOwner{trigger_id,
   extension_id}` component (already in `13-ecs-design.md`'s component
   table) to entities where `owner_extension` matches its own ID.
3. At dispatch time, an extension checks `TriggerOwner.extension_id ==
   itself` before acting on an entity in the payload, even if it's also
   registered for the same input type as another extension.

### Interaction flow

```
Player walks near the box, presses E
  → client sends ActionFrame{input: "key:E"}
  → kernel's InputHandlerSystem computes adjacent_entities (box is one of them)
  → broadcasts to all extensions registered for "key:E"
  → ext-props checks TriggerOwner on adjacent_entities for entities it owns
  → for owned entities: flips internal/KV state, publishes a NATS event for
    side effects (e.g. dims a separate light entity), and replies with an
    UpdateComponent / PlayAnimation instruction for the box
  → kernel applies the reply → ReplicationSystem sends PlayAnimation +
    UpdateComponent to clients in AOI → box animates, light entity dims
```

No new replication message types are needed — `PlayAnimation` and
`UpdateComponent` already exist in `replication.proto`. The only new wire
work is `ActionFrame`/`ActionResultFrame` (client → kernel) and
`InputHandlerSystem` (kernel), both already fully specced in
`documentation/plans/2026-07-01-inventory-equipment-action-triggers-design.md`
but not yet implemented in the lite MVP code.

---

## Diagram

See `documentation/depth-layers-diagram.svg` for a side-view "exploded"
diagram of the depth bands (static below, shared dynamic band with avatars,
static above) and how `sort_mode` maps to each.

## Not addressed here (future work)

- Proximity-based UI affordance (highlighting nearby interactables) — client
  concern, layered on top of the same `Interactable.trigger_radius` data.
- Splitting large sprites into Y-sorted + always-in-front parts — only if a
  concrete asset needs it.
- Persistence of prop/light state across restarts — depends on which store
  (JetStream KV vs. PocketBase) the owning extension chooses; not a kernel
  concern.
