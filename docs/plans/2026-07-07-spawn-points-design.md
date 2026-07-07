# Spawn Points

Date: 2026-07-07
Branch: `feat/spawn-points`

## Goal

First-time users (no saved position) and guests spawn on a random walkable
tile inside a random spawn zone, instead of always at the map-center
fallback. Returning users keep landing on their saved last position.

## Representation

A spawn point is a `Zone` with `zone_type=spawn`, drawn on the existing
**Zones** object layer in the Tiled map. No new layer, no new struct, no
new protobuf message, no client-side change.

- One spawn zone = random walkable tile within it.
- Multiple spawn zones = pick one uniformly at random, then a random
  walkable tile inside it.
- Spawn zones are also ordinary zones: they enter the `ZoneRegistry` like
  any other, so they may double as `av_enabled`/`is_exclusive` regions if
  the map author wants. They are never replicated to clients; spawning is
  a server-side selection criterion only.

The parser already skips any object layer whose name is not "zones"
(case-insensitive), so `zone_type=spawn` objects must live on the Zones
layer to take effect.

## Data model

- `MapData` gains a `SpawnZones []*Zone` field, populated in `loadMapData`
  by filtering `zones` where `ZoneType == "spawn"`. The full `zones` slice
  still goes to the `ZoneRegistry` unchanged.
- `integrity.go`: add `"spawn": true` to `knownZoneTypes` so spawn zones
  don't trigger the "unknown zone_type" warning meant to catch typos.

## Selection algorithm

New method on `MapData`:

```go
func (m *MapData) FindSpawnPoint(rng *rand.Rand) (float32, float32)
```

1. If `len(m.SpawnZones) == 0` -> return `m.FindSpawn()` (existing
   center-spiral, unchanged).
2. Pick a spawn zone uniformly at random with `rng`.
3. Enumerate integer tiles in the zone's bounding box (clamped to map
   bounds) that satisfy `!m.IsBlocked(tx, ty) && z.Contains(...)`.
   `Zone.Contains` already handles rect/circle/polygon, so all shapes work.
4. If non-empty -> pick one uniformly at random, return `(float32(tx),
   float32(ty))`.
5. If empty -> try remaining spawn zones in random order.
6. If no spawn zone yields a tile -> return `m.FindSpawn()` and log a
   warning.

Enumeration (not rejection sampling) is used because rejection sampling
degrades badly on small or mostly-walled zones and needs a retry cap.
Enumeration is O(bbox area), bounded by zone size, gives a true uniform
distribution, and runs once per session (irrelevant cost).

The tile enumeration is extracted as a shared helper
`walkableTilesInZone(md, z) [][2]int` used by both `FindSpawnPoint` and
the integrity check, so there is one source of truth for "which tiles are
in this zone."

## Call site

In `provisionClient` (`worldsim.go`), the fallback line changes from
`FindSpawn()` to `FindSpawnPoint(s.rng)`. The saved-position restore below
it is unchanged: it overwrites `spawnX, spawnY` only when a valid saved
position exists. So spawn zones apply exclusively to first-time/guest
spawns.

## RNG

`Simulator` gains an `rng *rand.Rand` field, seeded once at construction
from `time.Now().UnixNano()`. The `provisionClient` lock already
serializes spawns, so no extra synchronization is needed. For tests, the
constructor accepts a seed (or a pre-built `*rand.Rand`) so spawn tests
are deterministic.

## Integrity check

`CheckMapIntegrity` warns (LevelWarn, not LevelError) when a spawn zone
contains zero walkable tiles. The runtime fallback already handles this
gracefully; the warning surfaces an almost-certain map-authoring mistake
at startup instead of per-spawn.

## Fallbacks

- No spawn zones on the map -> `FindSpawn()` center-spiral (every existing
  map keeps working with zero edits).
- Spawn zone with no walkable tiles -> startup warning + per-spawn
  fallback to `FindSpawn()`.

## Tests

New file `mapdata_spawn_test.go` (plain `testing.T`, matching the existing
`mapdata_test.go` style):

- `TestFindSpawnPoint_NoSpawnZones_FallsBackToFindSpawn`
- `TestFindSpawnPoint_PicksTileInsideZone` (fixed seed, assert inside zone
  and not blocked)
- `TestFindSpawnPoint_AllZonesBlocked_FallsBack`
- `TestFindSpawnPoint_Distribution` (large zone, fixed seed, every
  walkable tile appears)
- `TestFindSpawnPoint_CircleZone` (assert result satisfies `Contains`)

## Files touched

- `backend/internal/worldsim/mapdata.go` — `SpawnZones` field, filter in
  `loadMapData`, `FindSpawnPoint`, shared `walkableTilesInZone` helper.
- `backend/internal/worldsim/integrity.go` — `"spawn"` in
  `knownZoneTypes`, walkable-tile warning loop.
- `backend/internal/worldsim/worldsim.go` — `rng` field + constructor
  wiring, one call-site change in `provisionClient`.
- `backend/internal/worldsim/mapdata_spawn_test.go` — new test file.

No client, protobuf, or Tiled-layer changes.
