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
`pb_migrations/`. Each record has three fields:

| Field | Type | Notes |
|---|---|---|
| `name` | text | Human-readable; the frontend loads by this name (`VITE_MAP_NAME`, default `map1`) |
| `tiled_json` | file (single) | The Tiled **JSON export** (not `.tmx`) |
| `tilesets` | file (multiple) | Tileset PNG images referenced by the JSON |

### Steps

1. **Start PocketBase** — `docker compose -f docker/docker-compose.yml up pocketbase` (or `make up` for all services). PocketBase serves on `http://localhost:8090`.

2. **Create an admin account** — open `http://localhost:8090/_/` in a browser and create the first admin user.

3. **Author or export your map** — map sources live in the repo root `maps/`
   directory. In Tiled, use File → Export As… and choose `*.json` (Phaser loads
   JSON, not `.tmx`). The tileset must be **embedded inline** in the JSON (no
   external `source` field) — Phaser 4 does not support external tileset
   references.

4. **Create a `maps` record** — in the PocketBase admin UI, go to the `maps` collection and click "New record":
   - `name`: `map1` (or whatever `VITE_MAP_NAME` is set to)
   - `tiled_json`: upload the exported JSON file
   - `tilesets`: upload the tileset PNG(s) — filenames must match the `image` field in the JSON (e.g. `Room_Builder_Office_32x32.png`, `Modern_Office_32x32.png`)

5. **Load the app** — the frontend fetches the record by name from PocketBase, retrieves the JSON and tileset images via PB file URLs, and renders the map. PocketBase must be available; there is no static fallback.

### Env vars

| Var | Default | Notes |
|---|---|---|
| `VITE_POCKETBASE_URL` | `http://localhost:8090` | PocketBase base URL (browser-reachable) |
| `VITE_MAP_NAME` | `map1` | Name of the map record to load |

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
