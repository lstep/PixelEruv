# Maps and Tiled

> **Status:** skeleton. Object placement and draw-order were flagged as
> needing more detail in `02-functional-requirements.md` § 3; this file
> captures the requirements and open questions.

This document will specify how maps are authored in Tiled, stored, loaded, and
how object layering and traversal work.

## Decisions / facts so far

- Maps are authored in **Tiled**; tile size **32×32**.
- Map JSON and tileset images are stored in **SeaweedFS/RustFS** and
  referenced from PocketBase `maps` (see `06-data-model-and-persistence.md`).
- Asset sources: limezu Modern Interiors / Modern Office.
- Objects can be **traversable or not**, mutable at runtime (component
  `Traversable`, see `13-ecs-design.md`).
- Zone polygons are authored in Tiled and live in JetStream KV (see
  `14-zones-and-interactions.md`).

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
