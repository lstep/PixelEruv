# Collision System

> **Read this before modifying collision logic in `worldsim.go` or `GameScene.ts`.** It documents the two-tier collision system and the role of `playerCollisionRadius`.

## Overview

There are **two separate collision systems**, both evaluated at the avatar's **feet point** (a single point, not a box or circle):

1. **Walls tile-layer grid** (point check against tile grid)
2. **Wall zones** (swept segment-vs-shape, expanded by radius)

Both systems check the feet position: `Position.Y + avatarFeetYOffset` (where `avatarFeetYOffset = 1.0`). The sprite itself (1 tile wide, ~48px tall) is **not** tested against walls — only the feet point is.

## 1. Walls tile-layer grid (point check)

The Tiled "Walls" tile layer is read into a boolean grid `[y][x]`. At each endpoint of the movement segment, the feet's tile coordinate is computed (`floor(feetX + 0.5)`, `floor(feetY + 0.5)`) and looked up in the grid. If either endpoint tile is blocked, the move is blocked.

<ref_snippet file="/Users/lstep/Workspace/GIT/PixelEruv.o/backend/internal/worldsim/worldsim.go" lines="1496-1506" />

This is a **point sample** — no radius, no shape. The `playerCollisionRadius` does **not** apply here.

## 2. Wall zones (swept segment-vs-shape, expanded by radius)

For each `zone_type=wall` zone (rect/circle/polygon), the shape is **expanded by `playerCollisionRadius`** (Minkowski sum with a disc of radius r), then the movement segment (in feet-space) is tested against the expanded shape.

### The movement segment

The collision test doesn't just check the destination point — it checks the **entire line segment** from the old feet position to the new feet position. This is called "swept" or "continuous" collision.

**Why a segment instead of just the destination?**

Movement per tick is 0.4 tiles (`SPEED_TILES_PER_TICK`). If a wall is thinner than 0.4 tiles (e.g., a 0.1-tile-thick decorative barrier), point-sampling at the destination would miss it entirely — the player would tunnel through. Swept segment-vs-shape catches walls thinner than the per-tick movement distance.

The segment is computed as:
- Start: `(oldX, oldY + avatarFeetYOffset)` — feet at the start of the tick
- End: `(newX, newY + avatarFeetYOffset)` — feet at the end of the tick

This segment is then tested against each expanded wall zone shape using geometric intersection functions (segment-rect, segment-circle, segment-polygon).

Check `backend/internal/worldsim/worldsim.go` lines="1459-1495"

The expansion works like this:

- **Rect** `X,Y,W,H` → tested as `X-r, Y-r, W+2r, H+2r`
- **Circle** radius `R` → tested as radius `R+r`
- **Polygon** → each vertex gets a circle of radius r, each edge gets a distance-r check (approximation of the true Minkowski sum)

So the **feet point** is swept as a segment, and it's blocked if that segment comes within `r` tiles of the wall zone's boundary.

## What `playerCollisionRadius = 0.1` actually means

It's **not** "the player is a 0.1-tile circle." The player has no actual shape in the collision math — it's a point (the feet). The radius is purely a **buffer added to wall zones** so the feet point stops `r` tiles before the zone's edge.

Concretely, for a rect wall at `X[5, 6]`:

- `r = 0.3` (old): feet stops at X = 4.7 (0.3 gap)
- `r = 0.1` (new): feet stops at X = 4.9 (0.1 gap)
- `r = 0`: feet stops at X = 5.0 (touching)

The sprite itself (1 tile wide, ~48px tall) is **not** tested against walls at all — only the feet point is. The radius is a fudge factor to make the visual gap feel right and to give some corner-snag tolerance for 1-tile passages.

## Where the constants live

These must stay in sync between backend and frontend:

- `backend/internal/worldsim/worldsim.go:1444` — `const playerCollisionRadius float32 = 0.1`
- `frontend/src/scenes/GameScene.ts:252` — `const PLAYER_COLLISION_RADIUS = 0.1`

## Visual asymmetry

The sprite is ~48px tall but collision is only at the feet (a point at `Position.Y + 1.0`). So the **head/body can visually overlap a wall** that the feet haven't reached yet — that's by design (tall-character top-down look, see [22-limezu-sprites.md](22-limezu-sprites.md)).

If you want the collision to feel like "the body, not just the feet," you'd need to either test multiple points along the sprite's height or give the player an actual rect/circle shape — but that's a bigger change than tuning this radius.
