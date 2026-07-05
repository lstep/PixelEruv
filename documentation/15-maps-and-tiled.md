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

## MVP: uploading a map via PocketBase

The `maps` collection is created automatically by the migration in
`pb_migrations/`. Each record has three fields:

| Field | Type | Notes |
|---|---|---|
| `name` | text | Human-readable; the frontend loads by this name (`VITE_MAP_NAME`, default `test-map`) |
| `tiled_json` | file (single) | The Tiled **JSON export** (not `.tmx`) |
| `tilesets` | file (multiple) | Tileset PNG images referenced by the JSON |

### Steps

1. **Start PocketBase** — `docker compose -f docker/docker-compose.yml up pocketbase` (or `make up` for all services). PocketBase serves on `http://localhost:8090`.

2. **Create an admin account** — open `http://localhost:8090/_/` in a browser and create the first admin user.

3. **Author or export your map** — in Tiled, use File → Export As… and choose `*.json` (Phaser loads JSON, not `.tmx`). The tileset must be **embedded inline** in the JSON (no external `source` field) — Phaser 4 does not support external tileset references.

4. **Create a `maps` record** — in the PocketBase admin UI, go to the `maps` collection and click "New record":
   - `name`: `test-map` (or whatever `VITE_MAP_NAME` is set to)
   - `tiled_json`: upload the exported JSON file
   - `tilesets`: upload the tileset PNG(s) — filenames must match the `image` field in the JSON (e.g. `tileset.png`)

5. **Load the app** — the frontend fetches the record by name from PocketBase, retrieves the JSON and tileset images via PB file URLs, and renders the map. If PocketBase is unreachable, it falls back to static files in `frontend/public/maps/`.

### Env vars

| Var | Default | Notes |
|---|---|---|
| `VITE_POCKETBASE_URL` | `http://localhost:8090` | PocketBase base URL (browser-reachable) |
| `VITE_MAP_NAME` | `test-map` | Name of the map record to load |

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

- **[OPEN] Draw-order / Y-sort algorithm** with multi-layer objects.
- **[OPEN] Tiled custom-property → ECS component convention.**
- **[OPEN] Map streaming** — load whole map vs. stream by AOI for large maps.
