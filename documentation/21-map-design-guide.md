# Map Design Guide for PixelEruv

> **Status:** implementation guide. This documents the current Tiled map
> format as supported by the worldsim and frontend.

This guide explains how to author maps in [Tiled](https://www.mapeditor.org/)
for PixelEruv: which layers to create, what properties are recognized, and
how the map is loaded by the game.

## Quick start

1. Open Tiled, create a new map: **Orthogonal**, tile size **32×32**.
2. Create the required layers (see [Layer reference](#layer-reference) below).
3. Draw ground tiles, wall tiles, and add zone objects.
4. Export as **JSON** (File → Export As… → `*.json`).
5. Upload to PocketBase (see [Uploading](#uploading-to-pocketbase)).

## Layer reference

PixelEruv recognizes layers by **name** (case-insensitive). The layer type
matters — tile layers store raster data, object layers store shapes.

```
┌─────────────────────────────────────────────────────────┐
│                    Tiled Map (32×32 tiles)                │
│                                                          │
│  ┌─── Tile Layers ────────────────────────────────────┐  │
│  │                                                     │  │
│  │  [0] "Ground"   — floor tiles (visual only)        │  │
│  │  [1] "Walls"    — wall tiles (collision, fallback) │  │
│  │                                                     │  │
│  └─────────────────────────────────────────────────────┘  │
│                                                          │
│  ┌─── Object Layers ──────────────────────────────────┐  │
│  │                                                     │  │
│  │  [2] "Zones"    — zone shapes (rect/circle/polygon) │  │
│  │       ├─ meeting-room-1  (zone_type=meeting)       │  │
│  │       ├─ wall-north      (zone_type=wall)          │  │
│  │       └─ water-pond      (zone_type=water)         │  │
│  │                                                     │  │
│  └─────────────────────────────────────────────────────┘  │
│                                                          │
└─────────────────────────────────────────────────────────┘
```

### "Ground" (tile layer, required)

- **Type:** tile layer
- **Purpose:** visual floor tiles (carpet, grass, pavement, etc.)
- **Recognized by:** frontend (rendering only)
- **Properties:** none

The Ground layer is purely visual. The game does not interpret tile IDs on
this layer for collision or gameplay — it only renders them.

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
   - `name`: the map name (must match `VITE_MAP_NAME`, default `test-map`)
   - `tiled_json`: upload the exported JSON file
   - `tilesets`: upload all tileset PNG images
5. Save

The frontend fetches the map by name, loads the JSON and tileset images via
PocketBase file URLs, and renders it with Phaser.

## How the map is loaded

```
Tiled JSON ──┐
             ├─→ PocketBase ──→ Frontend (Phaser render)
             │                 Frontend (Ground + Walls layers)
             │
             └──→ WorldSim ───→ Collision grid (Walls tile layer)
                              ─→ Zone registry (Zones object layer)
                              ─→ Zone enter/exit events (NATS)
                                    │
                                    ├─→ ext-demo (logs)
                                    └─→ ext-walls (gate triggers)
```

1. **Frontend** fetches the map record from PocketBase, loads the JSON and
   tileset PNGs, and renders all tile layers with Phaser.
2. **WorldSim** fetches the same JSON, builds:
   - A collision grid from the "Walls" tile layer (fallback)
   - A zone registry from the "Zones" object layer (pre-rasterized for O(1)
     point-in-zone lookup)
3. **Extensions** read the map independently (e.g. ext-walls finds
   `zone_type=wall` zones and registers block gate triggers with the
   worldsim via NATS).
4. During gameplay, the worldsim evaluates gate triggers on zone tiles
   during movement, and publishes `zone.enter` / `zone.exit` events when
   entities cross zone boundaries.

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

8. **Restart worldsim and ext-walls** so they re-read the map:
   ```bash
   docker compose -f docker/docker-compose.yml restart worldsim ext-walls
   ```
   > Both services load the map at startup and don't re-read it when
   > PocketBase is updated. You must restart them after uploading a new map.

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
> Mobile zones are not pre-rasterized — they're evaluated per-tick.

**Attributes used:**

| Attribute | Value | Required | Notes |
|---|---|---|---|
| Object Name | `guard-vision-1` (unique) | yes | Becomes the `zone_id` |
| `zone_type` | (any) | no | Hint for the owning extension |
| `mobility` | `mobile` | yes | Enables per-tick distance evaluation |
| `follows_entity_id` | `guard-1` | yes | Entity ID the zone follows |
| Shape | Circle (ellipse w==h) | yes | Mobile zones must be circles |

### How to: add a new tileset to the map

1. **In Tiled**: Map → Map Properties → Tilesets → click `+` to add a
   tileset. Browse to your PNG file. Set tile size to 32×32.

2. **Draw tiles** on the Ground or Walls layer using the new tileset.

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
| Zone object has no name | Zone is skipped | Set the object's **Name** property (this is the `zone_id`) |
| Ellipse with width ≠ height | Map load error | Use a polygon approximation, or make it a perfect circle |
| Tileset not embedded in JSON | Phaser can't load tiles | Export as JSON (not TMX); tilesets embed automatically |
| Wrong tile size | Sprites misaligned | Map must use 32×32 tiles |
| Forgot to upload tileset PNGs | Phaser shows blank tiles | Upload all tileset images to the PocketBase record |
