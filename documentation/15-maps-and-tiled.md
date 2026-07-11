# Maps and Tiled

> **Status:** skeleton. Object placement and draw-order were flagged as
> needing more detail in `02-functional-requirements.md` § 3; this file
> captures the requirements and open questions.

This document will specify how maps are authored in Tiled, stored, loaded, and
how object layering and traversal work.

## Decisions / facts so far

- Maps are authored in **Tiled**; tile size **32×32**.
- **MVP:** map JSON and tileset images are stored as **PocketBase file
  fields** in the `maps` collection (no object storage). Post-MVP, these
  move to SeaweedFS/RustFS and the `tiled_json_url` / `tileset_urls` string
  fields described in `06-data-model-and-persistence.md`.
- Asset sources: limezu Modern Interiors / Modern Office.
- Objects can be **traversable or not**, mutable at runtime (component
  `Traversable`, see `13-ecs-design.md`).
- Zone polygons are authored in Tiled and live in JetStream KV (see
  `14-zones-and-interactions.md`).
- Decoration layers are recognized by the custom layer property
  `layer_type=decoration` (any layer name, tile layer or object layer), not
  by a hardcoded `"Ground"` name. A per-layer `sort_mode` property
  (`static`/`dynamic`) controls Y-sorting against avatars — see
  `documentation/plans/2026-07-05-decoration-layers-and-interactive-entities-design.md`.

## MVP: uploading a map via PocketBase

The `maps` collection is created automatically by the migration in
`backend/migrations/`. Each record has these fields:

| Field | Type | Notes |
|---|---|---|
| `name` | text | Human-readable; the frontend loads by this name (`VITE_MAP_NAME`, default `main`) |
| `tiled_json` | file (single) | The Tiled **JSON export** (not `.tmx`) |
| `tilesets` | file (multiple) | Tileset PNG images referenced by the JSON |

Worldsim loads all maps from PocketBase on startup. Players can transition
between maps via **portal zones** — see [Portal zones](#portal-zones) below.

### First run (automatic)

On worldsim's first startup, if no `maps` record named after the
configured `DEFAULT_MAP` (default `main`) exists, worldsim uploads
`default-map.json` and the tileset PNGs referenced inside it from
`MAP_DIR` as a new `maps` record (with `name` set to the configured
default map). No manual upload step is needed for a fresh deploy — the
world boots with the bundled office map. This mirrors the
`SpriteStore.SeedIfEmpty` pattern used for `sprite_bases`.

> **Note:** PocketBase is embedded in worldsim as a Go library. The seed and
> all map fetches use PB Go SDK DAO calls in-process, not the HTTP API.
> worldsim also serves PB's HTTP API on port 8090 for the admin UI and for
> the frontend (which still fetches map data over HTTP).

| Var | Default | Notes |
|---|---|---|
| `MAP_DIR` | `./maps` (native) / `/maps` (Docker) | Directory containing `default-map.json` + tileset PNGs for first-run seeding |
| `DEFAULT_MAP` | `main` | Name of the default map record; worldsim seeds this on first run and new players spawn here |

The seed is **idempotent**: once a record with the configured name exists,
worldsim never overwrites it. To replace the map, edit the PocketBase record
(see "Replacing a map" below). Seeding runs once on startup, after
`app.Bootstrap()` and `app.RunAllMigrations()` complete (PocketBase is
embedded in worldsim as a Go library, so there is no external service to
wait for).

### Manual upload (replacing or adding maps)

1. **Start worldsim** — `docker compose -f docker/docker-compose.yml up worldsim` (or `make up` for all services). worldsim serves PocketBase's HTTP API and admin UI on `http://localhost:8090`.

2. **Create an admin account** — open `http://localhost:8090/_/` in a browser and create the first admin user.

3. **Author or export your map** — map sources live in the repo root `maps/`
   directory. In Tiled, use File → Export As… and choose `*.json` (Phaser loads
   JSON, not `.tmx`). The tileset must be **embedded inline** in the JSON (no
   external `source` field) — Phaser 4 does not support external tileset
   references.

4. **Create or edit a `maps` record** — in the PocketBase admin UI, go to the `maps` collection and click "New record" (or edit the existing `main`):
   - `name`: `main` (or whatever `VITE_MAP_NAME` is set to)
   - `tiled_json`: upload the exported JSON file
   - `tilesets`: upload the tileset PNG(s) — filenames must match the `image` field in the JSON (e.g. `Room_Builder_Office_32x32.png`, `Modern_Office_32x32.png`)

5. **Load the app** — the frontend fetches the record by name from PocketBase (served by worldsim on port 8090), retrieves the JSON and tileset images via PB file URLs, and renders the map. worldsim must be running; there is no static fallback.

### Env vars

| Var | Default | Notes |
|---|---|---|
| `VITE_POCKETBASE_URL` | `http://localhost:8090` | PocketBase base URL, served by worldsim (browser-reachable) |
| `VITE_MAP_NAME` | `main` | Name of the initial map record to load |

## Portal zones

A zone with `zone_type=portal` triggers a **map transition** when a player
enters it. Portal zones are authored in Tiled (see
`21-map-design-guide.md` for the full how-to) and handled directly by the
kernel — no extension is needed.

| Property | Required | Notes |
|---|---|---|
| `zone_type` | yes | Must be `portal` |
| `target_map` | yes | Name of the destination `maps` record (must exist as a `maps` record) |
| `target_entity` | no | Name of a base entity on the destination map to teleport to (a "beacon"). If omitted, the player spawns at a random `spawn` zone on the target map. |

When a player enters a portal zone, worldsim resolves the spawn position
and sends a `MapTransitionFrame` (`map_id`, `spawn_x`, `spawn_y`) to the
client so the frontend loads the new tilemap. Extensions can also trigger
transitions programmatically via the `worldsim.entity.teleport` NATS
subject (`{"entity_id", "map_id", "target_entity"}`).

## To be specified (the hard parts)

- **Object placement relative to a tile** — objects must anchor to sub-tile
  positions (not only centre), with **front/back** anchors so the renderer can
  compute draw order correctly (FR § 3, flagged "important").
- **Multi-layer tiles** — a single tile can carry multiple stacked objects,
  each with its own characteristics (block, trigger, layer).
- **Draw-order algorithm** — how Y-sorting interacts with multi-layer objects
  and avatars passing in front of / behind furniture.
- **Tiled → runtime mapping** — which Tiled custom properties map to which ECS
  components (e.g. `traversable`, `interactable`, `zone_type`).
- **Collision representation** — per-tile, per-object, or polygon.

## Open questions

- **[RESOLVED]** Draw-order / Y-sort algorithm with multi-layer objects — see
  `documentation/plans/2026-07-05-decoration-layers-and-interactive-entities-design.md`
  and `documentation/depth-layers-diagram.svg`. Summary: any layer with
  `layer_type=decoration` (any name) is recognized; altitude is the layer's
  position in the Tiled layer stack; a per-layer `sort_mode` (`static` or
  `dynamic`) decides whether it Y-sorts against avatars or stays at a fixed
  band. Not yet implemented in the lite MVP code.
- **[OPEN] Tiled custom-property → ECS component convention.** Partially
  resolved for interactive props (`owner_extension`, `interactable`,
  `trigger_radius` → `Interactable`/`TriggerOwner`) — see the design doc
  above, Part C.
- **[OPEN] Map streaming** — load whole map vs. stream by AOI for large maps.
