# Map Design Guide for PixelEruv

> **Status:** implementation guide. This documents the current Tiled map
> format as supported by the worldsim and frontend.

This guide explains how to author maps in [Tiled](https://www.mapeditor.org/)
for PixelEruv: which layers to create, what properties are recognized, and
how the map is loaded by the game.

## Quick start

1. Open Tiled, create a new map: **Orthogonal**, tile size **32×32**.
2. Create the required layers (see [Layer reference](#layer-reference) below):
   - One or more **decoration layers** (`layer_type=decoration`) for floor
     tiles and scenery.
   - A **Walls** tile layer (collision fallback, optional but recommended).
   - A **Zones** object layer for spatial zones (optional).
   - An **Entities** object layer for interactive props (optional).
3. Draw tiles on decoration layers, wall tiles on the Walls layer, and add
   zone/entity objects.
4. Export as **JSON** (File → Export As… → `*.json`).
5. Upload to PocketBase (see [Uploading](#uploading-to-pocketbase)).

## Layer reference

PixelEruv recognizes layers by **custom properties** (for decoration layers)
and by **name** (case-insensitive, for the reserved `Walls` and `Zones`
layers). The layer type matters — tile layers store raster data, object
layers store shapes.

```
┌──────────────────────────────────────────────────────────────┐
│                    Tiled Map (32×32 tiles)                      │
│                                                                │
│  ┌─── Decoration layers (tile or object, layer_type=decoration) │
│  │   Altitude = layer list order (bottom-to-top)               │  │
│  │   sort_mode: "static" (default) | "dynamic" (Y-sort)        │  │
│  │                                                              │  │
│  │  [0] "Floor"        tile layer   sort_mode=static           │  │
│  │  [1] "Rugs"         tile layer   sort_mode=static           │  │
│  │  [2] "Furniture"    object layer sort_mode=dynamic          │  │
│  │  [3] "Canopy"       tile layer   sort_mode=static (above)   │  │
│  │                                                              │  │
│  ├─── Reserved layers ──────────────────────────────────────────│  │
│  │                                                              │  │
│  │  [4] "Walls"    — wall tiles (collision fallback)           │  │
│  │  [5] "Zones"    — zone shapes (rect/circle/polygon)         │  │
│  │       ├─ meeting-room-1  (zone_type=meeting)               │  │
│  │       ├─ wall-north      (zone_type=wall)                  │  │
│  │       └─ water-pond      (zone_type=water)                 │  │
│  │                                                              │  │
│  ├─── Interactive entities ─────────────────────────────────────│  │
│  │                                                              │  │
│  │  [6] "Entities" — object layer (props with gid + metadata)  │  │
│  │       ├─ switch-1   (entity_type=light_switch,              │  │
│  │       │              on_interact_action=toggle,             │  │
│  │       │              interactions={toggle:[...]})           │  │
│  │       ├─ light-1    (entity_type=light,                     │  │
│  │       │              actions=toggle,activate,deactivate,    │  │
│  │       │              interactions={...})                    │  │
│  │       └─ door-1     (entity_type=door,                      │  │
│  │                     on_interact_action=toggle,              │  │
│  │                     interactions={toggle, toggle_wall})     │  │
│  │                                                              │  │
│  └──────────────────────────────────────────────────────────────┘  │
│                                                                │
└────────────────────────────────────────────────────────────────┘
```

> **Backward compat:** a tile layer literally named `"Ground"` with no
> `layer_type` property is still treated as a static decoration layer, so
> maps predating the `layer_type` convention keep rendering.

### Decoration layers (tile or object layers, recognized by `layer_type=decoration`)

- **Type:** tile layer (`tilelayer`) or object layer (`objectgroup`)
- **Purpose:** visual scenery — floor tiles, rugs, furniture, trees, canopies
- **Recognized by:** frontend (rendering + depth sorting)
- **Required property:** `layer_type` = `decoration` (string)
- **Optional property:** `sort_mode` = `static` (default) or `dynamic` (string)

Any layer — tile or object — carrying the custom property
`layer_type = "decoration"` is treated as a decoration layer, regardless of
its name. You can have any number of decoration layers.

**Altitude** is determined by the layer's position in Tiled's layer list
(top of the list = lowest altitude = drawn first). Reordering layers in
Tiled changes their altitude. No separate numeric property is needed.

#### Tile layers vs. object layers

| Layer type | Use for | Example |
|---|---|---|
| Tile layer | Bulk grid-aligned scenery (floor, rugs, grass, shadows) | A "Floor" tile layer with carpet tiles |
| Object layer (with `gid`) | Individually placed props needing free positioning | A "Furniture" object layer with a tree at (12.5, 7.3) |

Object-layer decorations use Tiled's tile-object feature: drag a tile from
the tileset onto an object layer. The object's `gid` determines its
appearance; its `x, y` position is free (not grid-snapped).

#### `sort_mode`: `static` vs. `dynamic`

| `sort_mode` | Behavior | Use for |
|---|---|---|
| `static` (default) | Fixed depth band from layer order. Never interleaves with avatars. | Floor decals, rugs, shadows, canopy/overhang overlays |
| `dynamic` | Depth computed per-object from its base/feet Y, placed in the same depth band as avatars so it Y-sorts against players. | Furniture, trees, walls-as-props — anything the player can walk in front of or behind |

**Depth formula** (single space so avatars and `dynamic` decorations sort
together):

```
depth = BAND_BASE(layer) + (baseY_pixels / mapHeightPixels)
```

- `static` layers before the first `dynamic` layer in the list → low bands
  (always below avatars).
- The `dynamic` band and avatars share one band (always 1000).
- `static` layers after the first `dynamic` layer → high bands (always
  above avatars — use for canopies, roofs).

> **Note on `dynamic` tile layers:** per-tile Y-sort requires splitting the
> layer into individual sprites, which is not yet implemented. A
> `sort_mode=dynamic` tile layer currently gets a flat depth in the dynamic
> band (with a console warning). Use object layers for dynamic props. See
> `documentation/depth-layers-diagram.svg` for a visual explanation.

#### Anchoring multi-tile-tall objects

Tiled tile-objects anchor at their **bottom-left corner** — `obj.y` from the
Tiled JSON is already the base/feet Y. No adjustment needed.

> **Avatar convention differs.** Player avatars are *not* anchored at their
> feet: `Position.Y` is the sprite origin (upper-body area), and the feet
> render at `Position.Y + 1.0` (origin `0.5/0.75` on a 64px frame). The
> worldsim evaluates collision and zone transitions at the feet
> (`Position.Y + avatarFeetYOffset`, see `worldsim.go`), so wall zones and
> the Walls tile layer block the player where the feet actually are, not
> where `Position` sits. When authoring wall zones meant to block the
> player, draw them where the **feet** should be stopped, not where the
> sprite origin would be.

For correct occlusion, author the object's **collision footprint narrower
than its visual sprite** (e.g. collision only on the trunk's base tile, not
the full 3.5-tile bounding box). This keeps the player out of positions
where a single flat depth value would look visually wrong.

### "Walls" (tile layer, optional — fallback)

- **Type:** tile layer
- **Purpose:** collision grid (any non-zero tile = blocked)
- **Recognized by:** worldsim (collision fallback), frontend (rendering)
- **Properties:** none

The Walls layer is the **fallback** collision system. The worldsim builds a
boolean grid from it at load time: any tile ID > 0 marks that tile as
blocked. If you use extension-driven walls (zones with `zone_type=wall`),
this layer is still checked as a secondary source.

> **Recommendation:** keep the Walls layer even if you use wall zones. It
> serves as a safety net if the walls extension is down or hasn't
> registered yet.

### "Zones" (object layer, optional)

- **Type:** object layer (`objectgroup`)
- **Purpose:** define spatial zones for the extension system
- **Recognized by:** worldsim (zone registry + gate triggers), extensions
- **Critical:** must be an **object layer**, not a tile layer. If you create
  a tile layer named "Zones", it will be ignored.

Each object on this layer is a zone. The object's **name** becomes the
`zone_id` (must be unique within the map).

#### Shape mapping

| Tiled shape | PixelEruv shape | How to create in Tiled |
|---|---|---|
| Rectangle | `rect` | Insert Rectangle tool (R) |
| Ellipse (width == height) | `circle` | Insert Ellipse tool (E) — must be a perfect circle |
| Polygon | `polygon` | Insert Polygon tool (P) — click points, Enter to finish |

> **Rotation is ignored.** All shapes are treated as axis-aligned. If you
> need a rotated rectangle, draw a polygon with the rotated corners.

#### Object properties (custom properties on the object)

| Property | Type | Required | Default | Description |
|---|---|---|---|---|
| `zone_type` | string | no | (none) | Hint for the owning extension. Values: `wall`, `meeting`, `water`, `work`, `silent`, `spawn`, `portal`, or any custom string. The kernel interprets `spawn` and `portal` directly; other values are passed to extensions. |
| `is_exclusive` | bool | no | `false` | If true, AOI filter excludes entities inside from non-members' replication. Static zones only. |
| `mobility` | string | no | `"static"` | `"static"` or `"mobile"`. Mobile zones must be circles and follow an entity via `follows_entity_id`. |
| `follows_entity_id` | string | only if mobile | — | Entity ID the zone follows (for mobile zones). |
| `target_map` | string | only if `zone_type=portal` | — | Name of the destination map. Must exist as a `maps` record. |
| `target_entity` | string | no | — | Name of a base entity on the destination map to teleport to (a "beacon"). If omitted, the player spawns at a random `spawn` zone on the target map. Only used with `zone_type=portal`. |

#### Recognized `zone_type` values

| Value | Behavior | Which extension handles it |
|---|---|---|
| `wall` | Blocks movement (gate trigger: `block`) | ext-walls |
| `meeting` | Meeting room (knock-to-join, exclusive) | future: ext-meeting-rooms |
| `water` | Water area (visual + audio effect) | future: ext-environment |
| `work` | Work area (focus mode, ambient audio) | future: ext-environment |
| `silent` | Silent zone (audio suppression) | future: ext-audio-zones |
| `spawn` | Player spawn point. New players and players transitioning to this map without a target entity are placed at a random `spawn` zone. | kernel (worldsim) |
| `portal` | Map transition. When a player enters a portal zone, worldsim moves them to the target map. Requires `target_map`. Optional `target_entity` for beacon-based teleport. | kernel (worldsim) |
| *(custom)* | Any string — your extension decides | your extension |

#### Example: wall zone

1. Select the "Zones" object layer
2. Draw a rectangle over a wall area
3. Right-click the rectangle → Object Properties
4. Set **Name** to `wall-north`
5. Add custom property: `zone_type` = `wall` (string)

#### Example: meeting room

1. Draw a rectangle over a room
2. Set **Name** to `meeting-room-1`
3. Add custom properties:
   - `zone_type` = `meeting`
   - `is_exclusive` = `true`

### "Entities" (object layer, optional)

- **Type:** object layer (`objectgroup`)
- **Purpose:** interactive props authored in the map (boxes, levers, light
  switches, crates) — "base entities" that exist in the ECS from map load
  and are claimed by extensions at startup
- **Recognized by:** worldsim (entity registry), extensions (via dispatch)
- **Critical:** must be an **object layer**, not a tile layer. Objects must
  have a **Name** (this becomes the `entity_id`) and a **tile `gid`** (drag
  a tile from the tileset onto the object layer to give it a sprite).

Each object on this layer is a base entity. At map load, the worldsim
creates it in the ECS with a `Position` and appearance. It is **inert**
until an extension claims it (registers an input trigger and self-filters
by ownership when dispatched).

#### Object properties (custom properties on the object)

| Property | Type | Required | Default | Description |
|---|---|---|---|---|
| `entity_type` | string | no | (none) | Hint for the owning extension. e.g. `light_switch`, `crate`, `lever`. A generic extension like ext-props claims entities whose `entity_type` it recognizes. |
| `owner_extension` | string | no | (none) | Explicit ownership: the extension whose ID matches this value claims the entity. e.g. `ext-props`, `ext-vault-door`. If omitted, any extension that recognizes the `entity_type` can claim it. |
| `trigger_radius` | float | no | `1.5` | How close (in tiles) a player must be to interact with this entity. Used by the worldsim's `InputHandlerSystem` to compute adjacent entities when an input trigger fires. |
| `gid_on` | int | no | `0` | Alternate sprite GID for the "on" state. The object's own `gid` is the "off" state. The extension reads this from the dispatch and returns an `AppearanceUpdate` with the appropriate GID when the state changes. `0` means no alternate sprite. |
| `on_interact_action` | string | no | (none) | **Immediate mode:** action_id fired automatically when the player presses E near this entity. Looks up `interactions[on_interact_action]` for the effects list. No popup is shown. Used for doors, switches, notification buttons. |
| `actions` | string | no | (none) | **Popup mode:** comma-separated action_ids shown in a popup when the player presses E. Each looks up `interactions[action_id]` for the effects list. Used for lights and complex entities with multiple options. |
| `interactions` | string (JSON) | no | `{}` | JSON-encoded map of action_id to a list of effects. Each effect has an `action` verb, optional `payload`, and `target_ids` array. See [Interactions data model](#interactions-data-model) below. |

#### Ownership model

Multiple extensions can register for the same input (e.g. `key:E`). At
dispatch time, each extension self-filters based on ownership:

1. If `owner_extension` is set, only that extension handles the entity.
2. If `owner_extension` is omitted, an extension that recognizes the
   `entity_type` handles it (generic fallback).

This lets a generic extension (ext-props handles `light_switch`, `crate`)
and a dedicated extension (ext-vault-door handles a specific vault door)
coexist without collision.

#### Example: interactive light switch box

1. Create an object layer named **Entities** (Layer → Add Object Layer).
2. Drag a tile from the tileset onto the layer to place a tile-object.
3. Right-click the object → Object Properties:
   - Set **Name** to `light-switch-1` (this is the `entity_id`)
   - Add custom property: `entity_type` = `light_switch` (String)
   - Add custom property: `trigger_radius` = `1.5` (Float)
4. Save, export, upload.
5. When a player walks within 1.5 tiles and presses **E**, the ext-props
   extension toggles the entity's state and plays an animation.

#### Interaction flow

The interaction system uses a **two-phase RPC** with an
**immediate-mode opt-out**. The entity declares which mode it uses
via Tiled properties:

- **Immediate mode** (`on_interact_action`): pressing E fires the
  action immediately. No popup. Used for doors, switches.
- **Popup mode** (`actions`): pressing E shows a popup with available
  actions. The user picks one, which sends `action:execute`. Used for
  lights and complex entities.

```
Player walks near entity, presses E
  → client sends ActionFrame{input: "key:E"}
  → pusher forwards to worldsim via NATS
  → worldsim computes adjacent entities (within trigger_radius)
  → collects target entities from interactions target_ids (may be far away)
  → broadcasts to all extensions registered for "key:E"
  → extension checks ownership (owner_extension match)
  → for each owned entity:
      if on_interact_action is set (immediate mode):
        → processes interactions[on_interact_action] effects
        → replies with state updates + appearance updates + animations
      if actions is set (popup mode):
        → builds available_actions list (filtered by current state)
        → replies with available_actions (no execution yet)
  → worldsim applies replies → replicates state/sprite/animation to clients
  → worldsim sends ActionResultFrame to client
      if available_actions non-empty: client shows popup
      if available_actions empty: immediate mode, done

User clicks a popup action
  → client sends ActionFrame{input: "action:execute", entity_id, action_id}
  → worldsim dispatches to extensions
  → extension finds target entity, processes interactions[action_id] effects
  → replies with state updates + appearance updates + animations
  → worldsim applies + replicates
```

#### Interactions data model

The `interactions` property is a JSON string encoding a map of
action_id to a list of effects:

```json
{
  "toggle": [
    {"action": "toggle", "target_ids": ["light-1"]}
  ],
  "activate": [
    {"action": "set_state", "payload": "on", "target_ids": ["light-1"]}
  ],
  "deactivate": [
    {"action": "set_state", "payload": "off", "target_ids": ["light-1"]}
  ]
}
```

Each effect has:
- `action` (string): The action verb for the target system. Different
  extensions handle different verbs. ext-props handles `toggle`,
  `set_state`, `activate`, `deactivate`, `turn_on`, `turn_off`.
  ext-walls handles `toggle_wall`.
- `target_ids` (string array): Entity IDs or zone IDs to apply the
  action to. Empty array = standalone action or applies to self.
- `payload` (string, optional): Free-form data passed to the handler
  (e.g. `"on"`, `"off"`, `"locked"`, a notification message).

The framework is a pure message router — it never interprets action
strings. Adding a new entity with new behavior is a Tiled property
exercise, not a code change. See
`documentation/plans/2026-07-15-interaction-system-design.md` for the
full design.

## Tileset requirements

- Tile size: **32×32** pixels
- Format: PNG
- Tilesets must be **embedded inline** in the JSON export (no external
  `source` field). In Tiled, this happens automatically when you export as
  JSON — the tileset image path is stored as a relative filename.
- Multiple tilesets are supported. Each tileset has a `firstgid` (global
  tile ID offset) that Tiled manages automatically.
- When uploading to PocketBase, tileset filenames must match the `image`
  field in the JSON (e.g. `Modern_Office_32x32.png`).

## Map properties

| Property | Required | Default | Notes |
|---|---|---|---|
| Orientation | yes | `orthogonal` | Only orthogonal is supported |
| Tile width | yes | 32 | Must be 32 |
| Tile height | yes | 32 | Must be 32 |
| Render order | no | `right-down` | Standard Tiled default |

## Uploading to PocketBase

> **First run is automatic.** worldsim seeds `default-map.json` + its tileset
> PNGs from `MAP_DIR` (bundled at `/maps` in Docker) into a `maps` record
> named `main` (with `is_default=true`) on first startup, if no `maps` records
> exist. The seed is idempotent — once any record exists, worldsim never
> overwrites it. The steps below are for **replacing** the default map or
> **adding** new ones.

1. In Tiled: File → Export As… → choose `*.json` format
2. Open the PocketBase admin UI at `http://localhost:8090/_/` (served by
   worldsim — PocketBase is embedded in worldsim as a Go library)
3. Go to the `maps` collection → edit the existing map record (or New
   record to add a new map)
4. Fill in:
   - `name`: the map name (e.g. `main`, `map2`)
   - `tiled_json`: upload the exported JSON file
   - `tilesets`: upload all tileset PNG images
5. Save

To add a second map, create a new `maps` record with a different `name`.
Worldsim loads all maps from PocketBase on startup. Players can transition
between maps via portal zones (see [How to: create a portal](#how-to-create-a-portal-map-transition)).

The frontend fetches the map by name, loads the JSON and tileset images via
PocketBase file URLs, and renders it with Phaser. On map transitions, the
frontend fetches the new map's assets dynamically and reloads the scene.

## How the map is loaded

```
Tiled JSON ──┐
             ├─→ PocketBase ──→ Frontend (Phaser render)
             │                 ├─ Decoration layers (layer_type=decoration)
             │                 │  ├─ static:  fixed depth band by layer order
             │                 │  └─ dynamic: Y-sorted with avatars
             │                 └─ Walls tile layer (rendered, depth=fallback)
             │
             └──→ WorldSim ───→ Collision grid (Walls tile layer)
                              ─→ Zone registry (Zones object layer)
                              ─→ Base entities (Entities object layer)
                              ─→ Zone enter/exit events (NATS)
                              ─→ Input trigger dispatch (NATS → extensions)
                                    │
                                    ├─→ ext-demo (logs)
                                    ├─→ ext-walls (gate triggers, toggle_wall)
                                    └─→ ext-props (interactive entities, key:E,
                                          action:execute — toggle, set_state,
                                          activate, deactivate)
```

1. **Frontend** fetches the map record from PocketBase, loads the JSON and
   tileset PNGs, and renders:
   - All decoration layers (recognized by `layer_type=decoration`) with
     depth assigned per `sort_mode` (static band or dynamic Y-sort).
   - The "Walls" tile layer at a fixed fallback depth.
   - Avatar sprites, Y-sorted against `dynamic` decorations every frame.
2. **WorldSim** loads all maps from PocketBase on startup. For each map, it builds:
   - A collision grid from the "Walls" tile layer (fallback)
   - A zone registry from the "Zones" object layer (continuous-space
     point-in-zone checks — no tile rasterization)
   - Base entities from the "Entities" object layer (inert until claimed)
   Each map has its own `MapData` and `ZoneRegistry`. Entities are tagged
   with their current map via `Position.MapId`; movement, collision, zone
   detection, and replication all use the entity's current map's data.
3. **Extensions** register with the worldsim via NATS:
   - ext-walls reads the map, finds `zone_type=wall` zones, registers block
     gate triggers.
   - ext-props registers an input trigger for `key:E` and handles adjacent
     entities it owns (by `owner_extension` or `entity_type`).
4. During gameplay, the worldsim evaluates gate triggers by checking the
   player's position directly against zone shapes in continuous space
   (no tile grid), and publishes `zone.enter` / `zone.exit` events when
   entities cross zone boundaries. When a player presses an input key with
   a registered trigger, the worldsim computes adjacent entities (within
   `trigger_radius`) and dispatches the action to registered extensions.
5. **Hot-reload**: when a `maps` record is updated in PocketBase (e.g.
   re-uploaded via the admin GUI), an in-process PB hook fires and
   worldsim reloads the map immediately, rebuilds the zone registry,
   and publishes a `map.updated` NATS event. Extensions (like ext-walls)
   subscribe to this event and re-read the map automatically. No restart
   needed.

## How-to: common operations

These step-by-step tutorials cover the most common map editing tasks.

### How to: create a wall (extension-driven)

Walls can be defined as zones with `zone_type=wall`. The ext-walls
extension reads these zones and registers block gate triggers with the
worldsim. Players cannot walk into wall zones.

1. **Select the "Zones" object layer** in the Layers panel (create it first
   if it doesn't exist: Layer → Add Object Layer, name it `Zones`).

2. **Draw a rectangle** over the wall area using the Insert Rectangle tool
   (press `R`). The rectangle should cover the tiles you want to block.

3. **Name the zone**: right-click the rectangle → Object Properties. Set
   the **Name** field to a unique identifier, e.g. `wall-north`.
   > The name is the `zone_id` — it must be unique within the map.

4. **Add the `zone_type` property**: in the Object Properties panel, click
   the `+` button to add a custom property:
   - Name: `zone_type`
   - Type: `String`
   - Value: `wall`

5. **(Optional) Remove the corresponding tiles from the "Walls" tile layer**
   if you had drawn them there before. This avoids double-blocking (both
   the tile layer fallback and the extension gate trigger). The tile layer
   is a fallback — if the walls extension is running, the zone takes over.

6. **Save and export** as JSON (File → Export As… → `*.json`).

7. **Upload to PocketBase** (see [Uploading](#uploading-to-pocketbase)).

8. **Wait for hot-reload** (or restart for immediate effect):
   - The worldsim checks for map changes every 30 seconds and automatically
     reloads when the map file changes. It then publishes a `map.updated`
     NATS event that extensions (including ext-walls) subscribe to.
   - For immediate effect without waiting, restart both services:
     ```bash
     docker compose -f docker/docker-compose.yml restart worldsim ext-walls
     ```

> **Note:** Zone collision uses swept (segment-vs-shape) tests in
> continuous space, evaluated at the avatar's **feet**
> (`Position.Y + avatarFeetYOffset`, see "Anchoring multi-tile-tall
> objects" above). The movement segment from the player's old position to
> the new position is tested against each zone shape (slab method for
> rects, point-segment distance for circles, edge intersection for
> polygons), with shapes expanded by a 0.3-tile player collision radius
> (Minkowski sum) so the sprite doesn't visually overlap the wall before
> stopping. A diagonal guard prevents corner-skip through thin walls.
> Walls of any thickness work correctly, including walls thinner than a
> tile or thinner than the per-tick movement distance. (The Walls
> tile-layer fallback is tile-grid-based and cannot represent sub-tile
> walls.)

9. **Verify** in the worldsim logs:
   ```bash
   docker logs pixeleruv-worldsim-1 2>&1 | grep "gate trigger"
   # Should show: gate trigger registered extension=walls zone=wall-north behavior=block
   ```

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `wall-north` (unique) | yes | Becomes the `zone_id` |
| `zone_type` | `wall` | yes | Tells ext-walls to register a block trigger |
| Shape | Rectangle | yes | Use rectangle for walls (simplest) |
| `is_exclusive` | (not set) | no | Walls are not exclusive zones |
| `mobility` | (not set, defaults to `static`) | no | Walls don't move |

### How to: create a meeting room

Meeting rooms are exclusive zones where the first entrant becomes the owner
and subsequent visitors must knock to enter. (Note: the full knock-to-join
logic is not yet implemented — this sets up the zone metadata so a future
extension can handle it.)

1. **Select the "Zones" object layer**.

2. **Draw a rectangle** over the room area (press `R`).

3. **Name the zone**: e.g. `meeting-room-1`.

4. **Add custom properties**:
   - `zone_type` = `meeting` (String)
   - `is_exclusive` = `true` (Boolean)

5. **Save, export, upload** as usual.

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `meeting-room-1` (unique) | yes | Becomes the `zone_id` |
| `zone_type` | `meeting` | yes | Identifies this as a meeting room |
| `is_exclusive` | `true` | yes | AOI filter excludes non-members; audio isolation |
| Shape | Rectangle | yes | Cover the entire room area |

### How to: create a portal (map transition)

Portals teleport a player to a different map when they enter the zone. They
are handled directly by the kernel (worldsim) — no extension needed.

1. **Select the "Zones" object layer**.

2. **Draw a rectangle** over the portal area (e.g. a doorway, a cave
   entrance). Press `R` for the Insert Rectangle tool.

3. **Name the zone**: e.g. `portal-to-map2`.

4. **Add custom properties**:
   - `zone_type` = `portal` (String)
   - `target_map` = `map2` (String) — the name of the destination map.
     This must match a `maps` record that belongs to the same world.

5. **(Optional) Set a beacon target**: if you want the player to appear at a
   specific spot on the destination map (e.g. next to a door), add:
   - `target_entity` = `door-entrance-north` (String) — the name of a base
     entity on the destination map (an object on the "Entities" layer with
     that name). The player will teleport to that entity's position.

   If `target_entity` is omitted, the player spawns at a random `spawn`
   zone on the destination map (same as initial login).

6. **Create the destination map**: upload a second map to PocketBase with
   `name` = `map2`. Add a `spawn` zone
   on the destination map (or a beacon entity matching `target_entity`).

7. **Save, export, upload** as usual.

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `portal-to-map2` (unique) | yes | Becomes the `zone_id` |
| `zone_type` | `portal` | yes | Triggers kernel-handled map transition |
| `target_map` | `map2` | yes | Destination map name (must exist in same world) |
| `target_entity` | `door-entrance-north` | no | Beacon entity name on destination map. If omitted, random spawn point. |
| Shape | Any (rect/circle/polygon) | yes | Typically a rectangle over a doorway |

> **How it works:** when the player walks into the portal zone, worldsim
> detects the `zone.enter` event, sees `zone_type=portal`, and calls
> `transitionEntity`. This changes `Position.MapId` to the target map,
> resolves the spawn position (beacon or random spawn zone), sends a
> `MapTransitionFrame` to the client so the frontend loads the new tilemap,
> and persists the new `map_id` to PocketBase. The player's avatar is
> despawned from the old map's clients and spawned on the new map via the
> normal replication loop.

> **Extensions can also trigger transitions.** An extension can publish to
> the `worldsim.entity.teleport` NATS subject with `{"entity_id", "map_id",
> "target_entity"}` to teleport a player programmatically (e.g. clicking a
> door object, admin teleport command). Same resolution: `target_entity`
> or random spawn point.

### How to: create a spawn zone

Spawn zones determine where new players appear on the map. When a player
logs in for the first time (or transitions to a map without a
`target_entity`), worldsim picks a random `spawn` zone from the map.

1. **Select the "Zones" object layer**.

2. **Draw a rectangle** over the spawn area (e.g. an entrance hall).

3. **Name the zone**: e.g. `spawn-main`.

4. **Add the `zone_type` property**: `zone_type` = `spawn` (String).

5. **Save, export, upload** as usual.

You can have multiple spawn zones on a map — worldsim picks one at random.
If no spawn zone exists, the player spawns at (10, 10) as a fallback.

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `spawn-main` (unique) | yes | Becomes the `zone_id` |
| `zone_type` | `spawn` | yes | Marks this as a spawn point |
| Shape | Any (rect/circle/polygon) | yes | Typically a rectangle over an entrance area |

### How to: create a water area

Water zones are visual/environmental areas. They don't block movement but
can trigger audio effects (water ambient sound) in a future extension.

1. **Select the "Zones" object layer**.

2. **Draw a shape** over the water area. You can use:
   - Rectangle (`R`) for square/rectangular water
   - Ellipse (`E`) for a pond (hold Shift to make it a perfect circle)
   - Polygon (`P`) for irregular shorelines (click points, Enter to finish)

3. **Name the zone**: e.g. `water-pond-1`.

4. **Add the `zone_type` property**: `zone_type` = `water` (String).

5. **Save, export, upload** as usual.

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `water-pond-1` (unique) | yes | Becomes the `zone_id` |
| `zone_type` | `water` | yes | Identifies this as a water area |
| Shape | Any (rect/circle/polygon) | yes | Use whatever fits the shape |
| `is_exclusive` | (not set) | no | Water is not exclusive |

### How to: create a silent zone

Silent zones suppress audio for everyone inside. Useful for libraries,
focus areas, or quiet rooms.

1. **Select the "Zones" object layer**.

2. **Draw a rectangle** over the quiet area.

3. **Name the zone**: e.g. `library-quiet-zone`.

4. **Add the `zone_type` property**: `zone_type` = `silent` (String).

5. **Save, export, upload** as usual.

### How to: create a mobile zone (e.g. NPC vision cone)

Mobile zones follow an entity and are evaluated per-tick via distance
checks. They must be circles.

1. **Select the "Zones" object layer**.

2. **Draw an ellipse** (press `E`) over the entity. The ellipse **must be
   a perfect circle** (width == height). Hold Shift while drawing, or set
   width and height to the same value in the properties.

3. **Name the zone**: e.g. `guard-vision-1`.

4. **Add custom properties**:
   - `zone_type` = `work` (or any relevant type — String)
   - `mobility` = `mobile` (String)
   - `follows_entity_id` = `guard-1` (String) — the entity ID the zone follows

5. **Save, export, upload** as usual.

> **Note:** The entity referenced by `follows_entity_id` must exist in the
> ECS (either a base entity from Tiled or one spawned by an extension).
> Mobile zones are evaluated per-tick via distance checks against the
> following entity's position.

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `guard-vision-1` (unique) | yes | Becomes the `zone_id` |
| `zone_type` | (any) | no | Hint for the owning extension |
| `mobility` | `mobile` | yes | Enables per-tick distance evaluation |
| `follows_entity_id` | `guard-1` | yes | Entity ID the zone follows |
| Shape | Circle (ellipse w==h) | yes | Mobile zones must be circles |

### How to: create a decoration layer

Decoration layers are recognized by the `layer_type=decoration` custom
property. They replace the old hardcoded `"Ground"` layer — you can now
have any number of them, at different altitudes (determined by layer list
order) and with different depth-sorting behavior.

1. **Create a tile layer** (Layer → Add Tile Layer) or an **object layer**
   (Layer → Add Object Layer) for freely-placed props.

2. **Name it** anything descriptive, e.g. `Floor`, `Rugs`, `Furniture`,
   `Canopy`. The name is not interpreted — only the `layer_type` property
   matters.

3. **Add the `layer_type` property**: Map → Layer Properties (or
   right-click the layer → Layer Properties), click `+`:
   - Name: `layer_type`
   - Type: `String`
   - Value: `decoration`

4. **(Optional) Set `sort_mode`**: add another custom property:
   - Name: `sort_mode`
   - Type: `String`
   - Value: `static` (default) or `dynamic`

   Use `dynamic` for layers with tall objects the player can walk behind
   (trees, furniture). Use `static` for floor decals, rugs, and
   always-above overlays (canopies).

5. **Draw tiles** (tile layer) or **place tile-objects** (object layer:
   drag a tile from the tileset onto the layer).

6. **Order the layers** in the layer list: layers higher in the list have
   lower altitude (drawn first / below). Place `static` canopy layers
   after the first `dynamic` layer to get the always-above band.

7. **Save, export, upload** as usual.

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| `layer_type` | `decoration` | yes | Marks this as a decoration layer |
| `sort_mode` | `static` or `dynamic` | no (default `static`) | `dynamic` = Y-sort with avatars |
| Layer list position | (any) | yes | Determines altitude band |
| Layer type | tile layer or object layer | yes | Object layers need `gid` on each object |

> **Backward compat:** a tile layer named `"Ground"` with no `layer_type`
> property is still treated as a static decoration layer, so old maps keep
> rendering without changes.

### How to: create an interactive entity (immediate mode — switch)

Interactive entities are authored as tile-objects on an **Entities**
object layer. They exist in the ECS from map load and are claimed by
extensions (like ext-props) that register input triggers.

**Immediate mode** fires an action automatically when the player
presses E — no popup. Used for doors, switches, notification buttons.

This example creates a wall switch that toggles two remote lights
when pressed.

1. **Create an object layer** named `Entities` (Layer → Add Object Layer).

2. **Place a tile-object**: drag a tile from the tileset onto the layer.
   This gives the object a `gid` (sprite appearance) and a free `x, y`
   position.

3. **Name the object**: right-click → Object Properties. Set **Name** to a
   unique identifier, e.g. `switch-1`.
   > The name is the `entity_id` — it must be unique within the map.

4. **Add custom properties**:
   - `entity_type` = `light_switch` (String)
   - `owner_extension` = `props` (String)
   - `trigger_radius` = `1.5` (Float)
   - `gid_on` = `381` (Int) — GID of the switch's "on" sprite
   - `on_interact_action` = `toggle` (String) — immediate mode: fire
     the `toggle` action on E press
   - `interactions` = (String, JSON) — the effects to fire:

   ```json
   {
     "toggle": [
       {"action": "toggle", "target_ids": ["switch-1"]},
       {"action": "toggle", "target_ids": ["light-1"]},
       {"action": "toggle", "target_ids": ["light-2"]}
     ]
   }
   ```

5. **Create the target light entities** on the same Entities layer
   (see [How to: create a light (popup mode)](#how-to-create-a-light-popup-mode)
   below, or create them as immediate-mode entities too).

6. **Save, export, upload** as usual.

7. **Verify** in the ext-props logs:
   ```bash
   docker logs pixeleruv-ext-props-1 2>&1 | grep "processed interaction"
   # After pressing E near the switch in-game:
   # processed interaction entity=switch-1 input=key:E
   ```

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `switch-1` (unique) | yes | Becomes the `entity_id` |
| `gid` | (tile from tileset) | yes | "Off" sprite appearance |
| `entity_type` | `light_switch` | no | Hint for the owning extension |
| `owner_extension` | `props` | no | Explicit ownership claim |
| `trigger_radius` | `1.5` | no (default 1.5) | Interaction distance in tiles |
| `gid_on` | `381` | no | Alternate GID for "on" state |
| `on_interact_action` | `toggle` | yes (immediate mode) | Action to fire on E press |
| `interactions` | (JSON) | yes | Effects list per action_id |

### How to: create a light (popup mode)

**Popup mode** shows a popup with available actions when the player
presses E. The user picks an action, which sends `action:execute`.
Used for lights and complex entities with multiple options.

1. **Place a tile-object** on the Entities layer and name it e.g. `light-1`.

2. **Add custom properties**:
   - `entity_type` = `light` (String)
   - `owner_extension` = `props` (String)
   - `trigger_radius` = `1.5` (Float)
   - `gid_on` = `491` (Int) — GID of the lamp's "on" sprite
   - `actions` = `toggle,activate,deactivate` (String) — popup mode:
     show these actions in the popup
   - `interactions` = (String, JSON):

   ```json
   {
     "toggle": [
       {"action": "toggle", "target_ids": ["light-1"]}
     ],
     "activate": [
       {"action": "set_state", "payload": "on", "target_ids": ["light-1"]}
     ],
     "deactivate": [
       {"action": "set_state", "payload": "off", "target_ids": ["light-1"]}
     ]
   }
   ```

3. **Save, export, upload** as usual.

4. **In-game**: walk near the light, press E. A popup appears with
   "Toggle", "Activate" (if off), or "Deactivate" (if on). The
   extension filters actions based on current state — "Activate" is
   hidden when the light is already on.

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `light-1` (unique) | yes | Becomes the `entity_id` |
| `gid` | (tile from tileset) | yes | "Off" sprite appearance |
| `entity_type` | `light` | no | Hint for the owning extension |
| `owner_extension` | `props` | no | Explicit ownership claim |
| `trigger_radius` | `1.5` | no (default 1.5) | Interaction distance in tiles |
| `gid_on` | `491` | no | Alternate GID for "on" state |
| `actions` | `toggle,activate,deactivate` | yes (popup mode) | Comma-separated action_ids |
| `interactions` | (JSON) | yes | Effects list per action_id |

### How to: create a door (multi-extension, multi-effect)

A door combines a sprite swap (handled by ext-props) with a wall zone
toggle (handled by ext-walls). The entity declares both effects in its
`interactions` — each extension processes only the verbs it knows.

1. **Place a tile-object** on the Entities layer and name it e.g. `door-1`.

2. **Add custom properties**:
   - `entity_type` = `door` (String)
   - `owner_extension` = `props` (String) — ext-props handles the sprite
   - `trigger_radius` = `1.5` (Float)
   - `gid_on` = `501` (Int) — GID of the open door sprite
   - `on_interact_action` = `toggle` (String) — immediate mode
   - `interactions` = (String, JSON):

   ```json
   {
     "toggle": [
       {"action": "toggle", "target_ids": ["door-1"]},
       {"action": "toggle_wall", "target_ids": ["room-1-walls"]}
     ]
   }
   ```

3. **Create a wall zone** named `room-1-walls` on the Zones layer with
   `zone_type=wall` (see [How to: create a wall](#how-to-create-a-wall-extension-driven)).
   This is the zone that `toggle_wall` targets.

4. **Save, export, upload** as usual.

5. **In-game**: walk near the door, press E. ext-props swaps the door
   sprite (open/closed), ext-walls toggles the wall zone (players can
   walk through the doorway when open). Both effects fire from the
   single `toggle` action — no code changes needed.

> **How multi-extension dispatch works:** worldsim sends the same
> dispatch to all extensions registered for `key:E`. ext-props reads
> the effects list, handles `toggle` (sprite swap), skips
> `toggle_wall`. ext-walls reads the same effects list, skips
> `toggle`, handles `toggle_wall` (zone toggle). Each extension
> processes only the verbs it knows.

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `door-1` (unique) | yes | Becomes the `entity_id` |
| `gid` | (tile from tileset) | yes | "Closed" sprite appearance |
| `entity_type` | `door` | no | Hint for the owning extension |
| `owner_extension` | `props` | no | ext-props handles the sprite |
| `gid_on` | `501` | no | Alternate GID for "open" state |
| `on_interact_action` | `toggle` | yes (immediate mode) | Action to fire on E press |
| `interactions` | (JSON) | yes | Effects: toggle (sprite) + toggle_wall (zone) |

### How to: add a new tileset to the map

1. **In Tiled**: Map → Map Properties → Tilesets → click `+` to add a
   tileset. Browse to your PNG file. Set tile size to 32×32.

2. **Draw tiles** on a decoration layer or the Walls layer using the new
   tileset.

3. **Export** the map as JSON. The tileset is embedded inline automatically.

4. **Upload to PocketBase**: edit the existing `maps` record and add the
   new tileset PNG to the `tilesets` file field. The filename must match
   the `image` field in the JSON.

### How to: verify map integrity

After editing and uploading a map, you can run the integrity checker to
catch common issues (missing layers, duplicate zone IDs, invalid shapes,
etc.):

```bash
# Trigger an integrity check via NATS
nats -s nats://localhost:4222 pub admin.map.integrity ""

# View results
docker logs pixeleruv-worldsim-1 2>&1 | grep "integrity"
```

The checker also runs automatically at worldsim startup and every 5
minutes. Issues are logged at three levels:

| Level | Meaning | Examples |
|---|---|---|
| ERROR | Blocks normal operation | Nil map, invalid dimensions, duplicate zone IDs, zero-size zones, polygon < 3 vertices, mobile zone not a circle |
| WARN | Suspicious but not fatal | No Walls layer, zone outside map bounds, unknown zone_type, exclusive zone without zone_type, spawn on blocked tile |
| INFO | Informational | Check passed with 0 issues |

## Common mistakes

| Mistake | Symptom | Fix |
|---|---|---|
| "Zones" created as tile layer | No zones parsed, no zone events | Delete it, create an **object layer** named "Zones" |
| "Entities" created as tile layer | No entities parsed, no interactions | Delete it, create an **object layer** named "Entities" |
| Zone object has no name | Zone is skipped | Set the object's **Name** property (this is the `zone_id`) |
| Entity object has no name | Entity is skipped | Set the object's **Name** property (this is the `entity_id`) |
| Entity object has no `gid` | No sprite rendered | Drag a tile from the tileset onto the object layer (creates a tile-object) |
| Decoration layer missing `layer_type` | Layer rendered at wrong depth or ignored | Add custom property `layer_type` = `decoration` (a bare "Ground" tile layer still works via backward compat) |
| `sort_mode=dynamic` on a tile layer | Console warning, flat depth (no per-tile Y-sort) | Use an object layer for dynamic props instead |
| Canopy layer renders below player | Player walks over the canopy | Move the canopy layer below the first `dynamic` layer in the layer list (layers after `dynamic` get the always-above band) |
| Ellipse with width ≠ height | Map load error | Use a polygon approximation, or make it a perfect circle |
| Tileset not embedded in JSON | Phaser can't load tiles | Export as JSON (not TMX); tilesets embed automatically |
| Wrong tile size | Sprites misaligned | Map must use 32×32 tiles |
| Forgot to upload tileset PNGs | Phaser shows blank tiles | Upload all tileset images to the PocketBase record |
| Portal `target_map` doesn't exist | Player stays on current map, worldsim logs "portal target map not found" | Create the destination map record in PocketBase |
| Portal `target_entity` not found on destination | Player stays on current map, worldsim logs "target entity not found" | Create a base entity with that name on the destination map's "Entities" layer |
| No `spawn` zone on map | Players spawn at (10, 10) fallback | Add at least one `zone_type=spawn` zone |
| `interactions` JSON malformed | Entity is inert (no effects fire on E press) | Validate the JSON string in Tiled's property editor. Use the examples in [How to: create an interactive entity](#how-to-create-an-interactive-entity-immediate-mode-switch) as a template |
| `on_interact_action` references a key not in `interactions` | Pressing E does nothing (no effects found) | Ensure the action_id in `on_interact_action` exists as a key in the `interactions` JSON |
| `actions` list references a key not in `interactions` | Popup shows the action but clicking it does nothing | Ensure every action_id in `actions` exists as a key in the `interactions` JSON |
| `target_ids` references a non-existent entity | Effect silently skipped (target not found) | Ensure target entity IDs match the **Name** of objects on the Entities layer (or zone IDs on the Zones layer) |
| Both `on_interact_action` and `actions` set | Immediate action fires AND popup shows | Use only one mode per entity. `on_interact_action` = immediate, `actions` = popup |
