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

## Common mistakes

| Mistake | Symptom | Fix |
|---|---|---|
| "Zones" created as tile layer | No zones parsed, no zone events | Delete it, create an **object layer** named "Zones" |
| Zone object has no name | Zone is skipped | Set the object's **Name** property (this is the `zone_id`) |
| Ellipse with width ≠ height | Map load error | Use a polygon approximation, or make it a perfect circle |
| Tileset not embedded in JSON | Phaser can't load tiles | Export as JSON (not TMX); tilesets embed automatically |
| Wrong tile size | Sprites misaligned | Map must use 32×32 tiles |
| Forgot to upload tileset PNGs | Phaser shows blank tiles | Upload all tileset images to the PocketBase record |
