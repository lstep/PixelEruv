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
│  │       ├─ light-switch-1  (entity_type=light_switch)         │  │
│  │       └─ crate-1         (owner_extension=ext-props)        │  │
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
| `zone_type` | string | no | (none) | Hint for the owning extension. Values: `wall`, `meeting`, `water`, `work`, `silent`, or any custom string. The kernel does not interpret this — it passes it to extensions. |
| `is_exclusive` | bool | no | `false` | If true, AOI filter excludes entities inside from non-members' replication. Static zones only. |
| `mobility` | string | no | `"static"` | `"static"` or `"mobile"`. Mobile zones must be circles and follow an entity via `follows_entity_id`. |
| `follows_entity_id` | string | only if mobile | — | Entity ID the zone follows (for mobile zones). |

#### Recognized `zone_type` values

| Value | Behavior | Which extension handles it |
|---|---|---|
| `wall` | Blocks movement (gate trigger: `block`) | ext-walls |
| `meeting` | Meeting room (knock-to-join, exclusive) | future: ext-meeting-rooms |
| `water` | Water area (visual + audio effect) | future: ext-environment |
| `work` | Work area (focus mode, ambient audio) | future: ext-environment |
| `silent` | Silent zone (audio suppression) | future: ext-audio-zones |
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

```
Player walks near entity, presses E
  → client sends ActionFrame{input: "key:E"}
  → pusher forwards to worldsim via NATS
  → worldsim's InputHandlerSystem computes adjacent entities (within trigger_radius)
  → broadcasts to all extensions registered for "key:E"
  → extension checks ownership (owner_extension or entity_type match)
  → for owned entities: replies with state update + animation
  → worldsim applies reply → replicates to clients in AOI
```

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

1. In Tiled: File → Export As… → choose `*.json` format
2. Open PocketBase admin UI at `http://localhost:8090/_/`
3. Go to the `maps` collection → New record
4. Fill in:
   - `name`: the map name (must match `VITE_MAP_NAME`, default `map1`)
   - `tiled_json`: upload the exported JSON file
   - `tilesets`: upload all tileset PNG images
5. Save

The frontend fetches the map by name, loads the JSON and tileset images via
PocketBase file URLs, and renders it with Phaser.

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
                                    ├─→ ext-walls (gate triggers)
                                    └─→ ext-props (interactive entities, key:E)
```

1. **Frontend** fetches the map record from PocketBase, loads the JSON and
   tileset PNGs, and renders:
   - All decoration layers (recognized by `layer_type=decoration`) with
     depth assigned per `sort_mode` (static band or dynamic Y-sort).
   - The "Walls" tile layer at a fixed fallback depth.
   - Avatar sprites, Y-sorted against `dynamic` decorations every frame.
2. **WorldSim** fetches the same JSON, builds:
   - A collision grid from the "Walls" tile layer (fallback)
   - A zone registry from the "Zones" object layer (continuous-space
     point-in-zone checks — no tile rasterization)
   - Base entities from the "Entities" object layer (inert until claimed)
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
5. **Hot-reload**: the worldsim polls PocketBase every 30 seconds for map
   changes (detected by filename change). When the map is updated, it
   reloads the map, rebuilds the zone registry, and publishes a
   `map.updated` NATS event. Extensions (like ext-walls) subscribe to
   this event and re-read the map automatically. No restart needed.

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

### How to: create an interactive entity

Interactive entities (boxes, levers, light switches) are authored as
tile-objects on an **Entities** object layer. They exist in the ECS from
map load and are claimed by extensions (like ext-props) that register
input triggers.

1. **Create an object layer** named `Entities` (Layer → Add Object Layer).

2. **Place a tile-object**: drag a tile from the tileset onto the layer.
   This gives the object a `gid` (sprite appearance) and a free `x, y`
   position.

3. **Name the object**: right-click → Object Properties. Set **Name** to a
   unique identifier, e.g. `light-switch-1`.
   > The name is the `entity_id` — it must be unique within the map.

4. **Add custom properties**:
   - `entity_type` = `light_switch` (String) — hint for the owning
     extension. ext-props recognizes `light_switch` as a generic prop.
   - `trigger_radius` = `1.5` (Float) — how close (in tiles) a player must
     be to interact.
   - `owner_extension` = `ext-props` (String, optional) — explicit
     ownership. If omitted, any extension recognizing the `entity_type`
     can claim it.

5. **Save, export, upload** as usual.

6. **Verify** in the worldsim and ext-props logs:
   ```bash
   docker logs pixeleruv-ext-props-1 2>&1 | grep "toggled prop"
   # After pressing E near the entity in-game:
   # toggled prop entity=light-switch-1 state=on
   ```

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `light-switch-1` (unique) | yes | Becomes the `entity_id` |
| `gid` | (tile from tileset) | yes | Determines sprite appearance |
| `entity_type` | `light_switch` | no | Hint for the owning extension |
| `owner_extension` | `ext-props` | no | Explicit ownership claim |
| `trigger_radius` | `1.5` | no (default 1.5) | Interaction distance in tiles |

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
