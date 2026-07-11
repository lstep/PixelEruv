# Multi-Map Support + PocketBase Embedding — Design

**Date:** 2026-07-11
**Status:** Reviewed — implementation starting. Each phase is a separate branch, tested sequentially.

## Overview

Two coupled changes:
1. **Embed PocketBase in worldsim** — PB becomes a Go library inside worldsim, not a standalone container.
2. **Multi-map support** — one worldsim instance loads and serves multiple maps from PocketBase.

These are coupled because PB embedding changes how worldsim accesses data, and multi-map changes what data worldsim loads. Doing both together avoids converting stores twice.

---

## Part 1: PocketBase Embedding

### Current state

- PB runs as a standalone prebuilt binary (v0.39.5) in a Docker container.
- Worldsim and extensions talk to PB over HTTP API (`POCKETBASE_URL=http://pocketbase:8090`).
- Three stores (MapStore, UserStore, SpriteStore) authenticate as superuser, cache tokens, make HTTP calls.
- Migrations are JS files in `pb_migrations/`, run by PB's JSVM plugin.
- PB admin GUI served at `/_/` on port 8090.

### Target state

- PB is a Go library (`github.com/pocketbase/pocketbase`) imported by worldsim.
- Worldsim initializes PB with `app.Bootstrap()` (DB + migrations, no HTTP server needed for data access).
- Worldsim also runs PB's HTTP server (`app.Start()` in a goroutine) to serve the admin GUI and file downloads (frontend fetches tilesets from PB's `/api/files/` endpoint).
- Stores use PB Go SDK (`app.Dao()`) instead of HTTP API calls.
- Migrations are Go migrations in `backend/migrations/`, compiled into the binary.
- No separate PB container. PB data directory is a volume mounted on the worldsim container.

### Why `app.Start()` and not just `app.Bootstrap()`

The frontend fetches map assets (Tiled JSON, tileset PNGs) from PB's `/api/files/` endpoint. The admin needs the PB admin GUI to edit options. So worldsim must serve PB's HTTP routes. Two approaches:

**Chosen: Run PB's HTTP server alongside worldsim's NATS subscriptions.**

PB's `app.Start()` is blocking and starts its own TCP listener. Worldsim needs its own lifecycle (tick loop, NATS subscriptions). The approach:

```go
func main() {
    app := pocketbase.NewWithConfig(pocketbase.Config{
        DefaultDataDir: os.Getenv("PB_DATA_DIR"),
    })

    // Register Go migrations
    migrations.Register(app)

    // Register realtime hooks for options hot-reload
    registerPBHooks(app, worldOptionsCallback, extensionOptionsCallback)

    // Bootstrap: run migrations, init DB (no HTTP yet)
    if err := app.Bootstrap(); err != nil {
        log.Fatal(err)
    }

    // Create worldsim with PB app for data access
    sim, err := worldsim.New(app, defaultMap, natsURL, tickHz, logger)
    if err != nil {
        log.Fatal(err)
    }

    // Start PB HTTP server in a goroutine (admin GUI + file serving)
    // PB listens on its own port (default 8090, configurable)
    go func() {
        if err := app.Start(); err != nil {
            log.Fatal(err)
        }
    }()

    // Start worldsim tick loop (blocking)
    sim.Run()
}
```

PB's HTTP server handles `/api/` (REST API + file downloads) and `/_/` (admin GUI). Worldsim doesn't need to proxy these — PB serves them directly on its port. Nginx routes `/api/` to PB's port (same as today, just pointing at worldsim's port instead of a separate container).

### Migration conversion: JS → Go

The 4 existing JS migrations become Go migrations. PB v0.39.x uses `migrations.Register()` with Go functions:

```go
// backend/migrations/1751700000_create_maps.go
package migrations

import (
    "github.com/pocketbase/pocketbase/core"
    "github.com/pocketbase/pocketbase/migrations"
)

func init() {
    migrations.Register(func(db dbx.Builder) error {
        // Create maps collection
    }, func(db dbx.Builder) error {
        // Drop maps collection (rollback)
    })
}
```

The JS migration files in `pb_migrations/` are deleted. Go migrations are compiled into the worldsim binary.

**New collections** (player preference columns) are added as new Go migration files. The `worlds` collection was initially planned but later removed — the default map is now controlled by the `DEFAULT_MAP` env var instead.

### Store refactoring

Each store replaces HTTP calls with Go SDK calls. The stores receive `core.App` instead of `pocketbaseURL`:

```go
// Before
func NewUserStore(pocketbaseURL, adminEmail, adminPassword string) *UserStore

// After
func NewUserStore(app core.App) *UserStore
```

HTTP calls become DAO calls:
- `http.Get("/api/collections/players/records?filter=...")` → `app.Dao().FindRecordsByFilter("players", "oidc_sub = {:sub}", ...)`
- `http.Post("/api/collections/players/records")` → `record := models.NewRecord(collection); record.Set(...); app.Dao().SaveRecord(record)`
- Multipart file uploads → `filesystem.NewFileFromPath(path)` + `record.Set("tiled_json", file)`

No more auth token caching — worldsim has direct DB access.

### Docker changes

- **Remove:** `docker/pocketbase.Dockerfile`, `docker/pocketbase-entrypoint.sh`
- **Remove:** `pocketbase` service from `docker-compose.yml`
- **Add:** `pb_data` volume mount on the `worldsim` service
- **Change:** Nginx routes `/api/` to worldsim's PB port (instead of `pocketbase:8090`)
- **Remove:** `POCKETBASE_URL`, `PB_ADMIN_EMAIL`, `PB_ADMIN_PASSWORD` env vars from worldsim and extensions
- **Add:** `PB_DATA_DIR` env var on worldsim (default `./pb_data`)
- **Add:** `PB_HTTP_ADDR` env var on worldsim (default `:8090`)

### Superuser creation

Currently done by `pocketbase superuser upsert` in the entrypoint script. With embedding, worldsim creates the superuser programmatically after bootstrap:

```go
admin, _ := app.Dao().FindAdminByEmail(adminEmail)
if admin == nil {
    admin = models.NewAdmin(app.Dao().AdminCollection())
    admin.Email = adminEmail
    admin.SetPassword(adminPassword)
    app.Dao().SaveAdmin(admin)
}
```

### Realtime hooks (for options hot-reload)

PB Go event hooks fire in-process when records change. Worldsim registers hooks for the collections it cares about:

```go
app.OnRecordAfterUpdateSuccess("players").BindFunc(func(e *core.RecordEvent) error {
    // Update in-memory player options
    // Publish WorldOptionsFrame to all connected clients
    return e.Next()
})

app.OnRecordAfterUpdateSuccess("av_settings").BindFunc(func(e *core.RecordEvent) error {
    // Publish updated options to ext-av via NATS
    return e.Next()
})
```

No SSE, no NATS relay for PB changes — direct in-process function calls.

### Frontend file access

The frontend fetches map assets from PB's `/api/files/` endpoint. With PB embedded in worldsim, this still works — PB's HTTP server serves these routes. The frontend's `mapLoader.ts` and `PB_URL` constant don't change (they point at the same host:port, just served by worldsim instead of a separate container).

---

## Part 2: Multi-Map Support

### What is a "world"?

A "world" is a virtual space instance — one deployment of a virtual office. The original design grouped maps under a `worlds` collection, but this was **simplified**: the `worlds` collection was removed entirely. The default map is now controlled by the `DEFAULT_MAP` env var (default `main`). Worldsim loads all maps from PocketBase on startup — no world filter. This keeps the data model simpler while still supporting multiple maps and portal transitions between them.

### Current state

- Worldsim boots with a single `MAP_ID` env var.
- `Simulator` has one `mapID string`, one `mapData *MapData`, one `zoneReg *ZoneRegistry`.
- All entities get `Position.MapId = s.mapID` (the field exists in proto but is never read).
- Movement, collision, zone detection all use the single map's data.
- Frontend loads one tilemap at startup via `VITE_MAP_NAME`.
- No map transition mechanism exists.

### Target state

- Worldsim boots with a `DEFAULT_MAP` env var (default `main`).
- `maps` collection: multiple rows, each with a `name`.
- `Simulator` loads all maps from PocketBase on startup.
- Entities are tagged with their current map via `Position.MapId`.
- Movement, collision, zone detection use the entity's current map's data.
- Players can transition between maps via portal zones.
- Frontend loads the player's current map tilemap and switches on map transition.

### Data model

#### `maps` collection (existing)

| Field | Type | Notes |
|---|---|---|
| `id` | string (PB auto) | |
| `name` | text, required | Human-readable map name |
| `tiled_json` | file, required | Tiled JSON |
| `tilesets` | file, required, multiple | Tileset PNGs |

> The `worlds` collection was removed. The default map is controlled by
> the `DEFAULT_MAP` env var (default `main`). World options
> (`allow_anonymous`, `day_night_enabled`) will be handled by env vars
> or a future singleton config collection if needed.

#### `players` collection (existing, add `map_id`)

| Field | Type | Notes |
|---|---|---|
| `map_id` | text | Current map name (or PB record ID). Defaults to the `DEFAULT_MAP` env var on first login. |

### Simulator struct changes

```go
type Simulator struct {
    nc      *nats.Conn
    app        core.App          // PB instance for data access
    defaultMap string            // default map name (from DEFAULT_MAP env var)

    // Per-map data — keyed by map name
    maps    map[string]*MapData       // mapName → MapData
    zones   map[string]*ZoneRegistry  // mapName → ZoneRegistry

    // ... stores, extMgr, tick fields, logger, tracer ...

    mu       sync.Mutex
    entities map[string]*Entity        // all entities (across all maps)
    clients  map[string]*Entity        // player avatars
    entityIDToClient map[string]string
    destroyedEntities []string
    snapshotSeq uint32
}
```

Key changes:
- `mapID string` → `defaultMap string`
- `mapData *MapData` → `maps map[string]*MapData`
- `zoneReg *ZoneRegistry` → `zones map[string]*ZoneRegistry`
- `pocketbaseURL string` → `app core.App`

### Boot sequence

```go
func New(app core.App, defaultMap, natsURL string, tickHz int, logger *slog.Logger) (*Simulator, error) {
    // 1. Connect to NATS
    nc, _ := nats.Connect(natsURL, ...)

    // 2. Load all maps from PB
    mapRecords, err := loadAllMaps(app)
    maps := make(map[string]*MapData)
    zones := make(map[string]*ZoneRegistry)
    for _, mr := range mapRecords {
        md, err := parseMapData(app, mr)  // same Tiled JSON parsing as today
        if err != nil {
            logger.Warn("failed to load map", "map", mr.Name, "err", err)
            continue
        }
        maps[mr.Name] = md
        zones[mr.Name] = NewZoneRegistry(md.Zones, md.Width, md.Height)
    }

    // 3. Auto-seed default map if no maps exist (same pattern as today)
    if len(maps) == 0 {
        seedDefaultMap(app, defaultMap, mapDir)
        // reload
    }

    // 4. Create simulator
    s := &Simulator{
        nc: nc, app: app, defaultMap: defaultMap,
        maps: maps, zones: zones,
        // ...
    }

    // 6. Load base entities for all maps
    for mapName, md := range s.maps {
        s.loadBaseEntities(mapName, md)
    }

    // 7. Subscribe to NATS, start tick loop
    s.subscribe()
    return s, nil
}
```

### Entity changes

The `Entity` struct doesn't change — `Position.MapId` already exists and is already set. The difference is it's now **read** to determine which map's collision grid and zone registry to use.

```go
func (s *Simulator) runMovementSystem() {
    for _, e := range s.entities {
        if e.NetworkSession == nil || e.Position == nil {
            continue
        }

        mapName := e.Position.MapId
        md := s.maps[mapName]
        zr := s.zones[mapName]
        if md == nil {
            continue
        }

        // ... compute dx, dy from input ...

        newX, newY := e.Position.X+dx*speed, e.Position.Y+dy*speed
        newX = clamp(newX, 0, float32(md.Width-1))
        newY = clamp(newY, 0, float32(md.Height-1))

        // Collision check against THIS map's data
        if s.isMoveBlocked(zr, md, e.Position.X, e.Position.Y, newX, e.Position.Y) {
            newX = e.Position.X
        }
        if s.isMoveBlocked(zr, md, newX, e.Position.Y, newX, newY) {
            newY = e.Position.Y
        }

        e.Position.X = newX
        e.Position.Y = newY
        e.dirtyPosition = true
    }
}
```

`isMoveBlocked` changes from using `s.mapData`/`s.zoneReg` to taking `md`/`zr` as parameters.

### Zone detection

Zone enter/exit detection changes to use the entity's current map's zone registry:

```go
for _, e := range s.entities {
    zr := s.zones[e.Position.MapId]
    if zr == nil {
        continue
    }
    newZones := zr.ZonesAtPoint(e.Position.X, e.Position.Y+avatarFeetYOffset)
    // ... same enter/exit logic as today ...
}
```

### Replication

Replication needs to filter by map. A client on map A should only see entities on map A. Currently "replicate everything to everyone" (lite MVP). With multi-map:

```go
func (s *Simulator) replicateToClient(ctx context.Context, clientEntity *Entity) bool {
    clientMap := clientEntity.Position.MapId

    for _, e := range s.entities {
        // Skip entities on other maps
        if e.Position.MapId != clientMap && e != clientEntity {
            continue
        }
        // ... same spawn/update logic as today ...
    }
}
```

This is actually simpler than AOI filtering — just a map name string comparison.

### Map transitions

#### Portal zones

Maps define "portal" zones in Tiled with properties:
- `zone_type: "portal"`
- `target_map: "map2"` (name of the destination map)
- `target_entity: "door-north"` (optional — name of a base entity on the destination map to teleport to)

When a player enters a portal zone, worldsim resolves the spawn position:
- If `target_entity` is set, teleport to that named base entity's position
  (a "beacon") on the destination map. Fails if the entity doesn't exist.
- If `target_entity` is omitted, pick a random `spawn` zone on the
  destination map (same as initial login).

Then worldsim:
1. Changes `Position.MapId` to the target map.
2. Changes `Position.X`/`Position.Y` to the resolved spawn coordinates.
3. Despawns the entity from the old map's clients (sends `DestroyEntity` to clients on the old map).
4. Spawns the entity on the new map (sends `SpawnEntity` to clients on the new map).
5. Updates the player's `map_id` in PB for persistence.
6. Sends a `MapTransitionFrame` to the client so the frontend loads the new tilemap.

The portal zone check happens in the existing zone enter/exit detection loop — when a `zone.enter` fires for a zone with `zone_type == "portal"`, worldsim handles the transition directly (no extension needed).

#### Extension-triggered transitions

Extensions can also trigger map transitions via NATS. This enables use cases like "click a door to go to another floor" or "admin teleports a player."

```
Subject: worldsim.entity.teleport
Payload: { "entity_id": "e_abc", "map_id": "map2", "target_entity": "door-north" }
```

`target_entity` is optional — if omitted, the player spawns at a random
`spawn` zone on the destination map. Worldsim subscribes to
`worldsim.entity.teleport` and handles it the same way as a portal zone.
The transition logic is shared in a single
`transitionEntity(entityID, targetMap, targetEntity)` method called from
both the portal zone handler and the NATS subscription handler.

#### New proto frame: `MapTransitionFrame`

```protobuf
message MapTransitionFrame {
  string map_id = 1;        // new map name
  float spawn_x = 2;        // resolved spawn X on new map
  float spawn_y = 3;        // resolved spawn Y on new map
}
```

Added to `ServerFrame` oneof. Worldsim publishes it on `client.<id>.replication`. The frontend fetches the new map's Tiled JSON and tilesets from PocketBase by map name (same as initial load), so no URLs need to be in the frame.

### Frontend changes

#### Map loading

Currently: `loadMapAssets()` fetches one map at startup via `VITE_MAP_NAME`.

After: `loadMapAssets(mapName)` fetches a specific map's assets. Called at startup with the player's current map (from `AuthResultFrame` or a new field), and on map transition.

#### Map transition handling

```typescript
case "mapTransition": {
    const mt = serverFrame.payload.value;
    // 1. Fade out current map
    // 2. Destroy all current sprites (avatars, props)
    // 3. Load new tilemap + tilesets
    // 4. Create new tilemap layers
    // 5. Fade in new map
    // 6. Server will send SpawnEntity for all entities on the new map
}
```

The frontend destroys all avatar sprites on map transition (same as reconnect handling today), loads the new tilemap, and lets the replication stream re-spawn entities.

#### `AuthResultFrame` extension

Add `map_id` to `AuthResultFrame` so the frontend knows which map to load initially:

```protobuf
message AuthResultFrame {
  bool ok = 1;
  string client_id = 2;
  string entity_id = 3;
  string sub = 4;
  string map_id = 5;    // NEW: player's current map
}
```

### Extension changes: zone metadata via NATS

Extensions no longer read PB directly. Worldsim broadcasts zone metadata on `worldsim.ready`:

```json
// Subject: worldsim.ready
{
  "default_map": "main",
  "maps": {
    "main": {
      "zones": [
        {"id": "wall_north", "properties": {"zone_type": "wall"}},
        {"id": "av_room1", "properties": {"av_enabled": true, "zone_type": "av"}},
        {"id": "portal_south", "properties": {"zone_type": "portal", "target_map": "map2", "target_x": 15, "target_y": 10}}
      ]
    },
    "map2": {
      "zones": [...]
    }
  }
}
```

Extensions parse this to find their zones:
- ext-walls: filters for `zone_type == "wall"`, registers gate triggers.
- ext-av: filters for `av_enabled == true`, uses those zone IDs.

On `map.updated` (map reload), worldsim re-broadcasts updated zone metadata. Extensions re-register.

Extensions remove `POCKETBASE_URL` env var and all PB HTTP code (`findWallZones`, `findAVZones` functions are deleted).

### Extension registration with options schema

Extensions declare their options schema in the registration message:

```json
// Subject: extension.av.register
{
  "extension_id": "av",
  "heartbeat_interval_s": 10,
  "options_schema": {
    "collection_name": "av_settings",
    "fields": [
      {"name": "room_prefix", "type": "text", "default": "room"},
      {"name": "max_participants", "type": "int", "default": 8}
    ]
  }
}
```

Worldsim receives this, creates the PB collection if it doesn't exist (using Go SDK), inserts a default row, and sends the current values back to the extension via NATS:

```json
// Subject: extension.av.options
{
  "room_prefix": "room",
  "max_participants": 8
}
```

When the admin edits `av_settings` in the PB GUI, the PB `OnRecordAfterUpdateSuccess` hook fires in worldsim, and worldsim publishes the updated options to `extension.av.options`. The extension picks them up via NATS subscription.

---

## Implementation Plan

Each phase is a separate branch, implemented and tested sequentially. A phase is merged before the next begins.

### Phase 1: PB Embedding — branch `feat/pb-embedding`

Goal: PB runs inside worldsim as a Go library. No functional change from the user's perspective — same single-map behavior, just no separate PB container.

1. **Add PB dependency** — `go get github.com/pocketbase/pocketbase@v0.39.5`
   → verify: `go mod tidy` succeeds

2. **Convert JS migrations to Go** — `backend/migrations/` with 4 Go migration files matching the existing JS schemas
   → verify: worldsim boots, PB creates collections, existing data works

3. **Refactor stores to Go SDK** — MapStore, UserStore, SpriteStore take `core.App` instead of HTTP params
   → verify: `go test ./internal/worldsim/ -v` passes (existing tests)

4. **Initialize PB in worldsim main** — `app.Bootstrap()` + `app.Start()` in goroutine + superuser creation
   → verify: worldsim starts, PB admin GUI accessible at `:8090/_/`

5. **Update Docker** — remove PB container, mount `pb_data` volume on worldsim, update nginx routing
   → verify: `make up` starts stack, frontend loads map assets from worldsim's PB port

6. **Register PB realtime hooks** — `OnRecordAfterUpdateSuccess` for `players`, extension settings collections
   → verify: changing a record in PB admin GUI fires the hook

**Merge gate:** full stack works identically to before, but with no separate PB container. All existing tests pass.

### Phase 2: Multi-Map Support — branch `feat/multi-map`

Goal: worldsim loads multiple maps from PocketBase. Players can transition between maps via portal zones and extension-triggered teleports.

7. **Add `map_id` to `players` collection** — Go migration adding the column
   → verify: column visible in PB admin GUI

8. **Refactor Simulator for multi-map** — `maps map[string]*MapData`, `zones map[string]*ZoneRegistry`, boot with `DEFAULT_MAP`, load all maps from PocketBase
   → verify: worldsim loads multiple maps, entities spawn on correct maps

9. **Update movement/collision/zone for per-map** — use `e.Position.MapId` to look up the correct map data
   → verify: unit test — entities on different maps don't collide with each other's walls

10. **Update replication for per-map filtering** — only replicate entities on the same map as the client
    → verify: unit test — client on map A doesn't receive spawns for entities on map B

11. **Implement portal zones** — parse `zone_type: "portal"` from Tiled, handle in zone enter detection, transition entity
    → verify: unit test — entering portal zone changes entity's map and position

12. **Implement extension-triggered transitions** — subscribe to `worldsim.entity.teleport`, shared `transitionEntity()` method
    → verify: unit test — NATS teleport request changes entity's map and position

13. **Add `MapTransitionFrame` to proto** — new ServerFrame variant
    → verify: `make proto` generates code

14. **Frontend map transition** — `loadMapAssets(mapName)`, handle `MapTransitionFrame`, destroy/recreate tilemap
    → verify: manual test — walking into portal loads new map

15. **Add `map_id` to `AuthResultFrame`** — frontend knows initial map
    → verify: frontend loads correct map on connect

**Merge gate:** worldsim loads multiple maps, portal zones and extension teleports work, frontend transitions between maps, replication is per-map.

### Phase 3: Extension NATS Refactor — branch `feat/extension-nats`

Goal: extensions no longer access PB directly. They get zone data from worldsim via NATS and declare their options schema at registration.

18. **Worldsim broadcasts zone metadata on `worldsim.ready`** — include zone IDs + properties for all maps
    → verify: extensions receive zone data via NATS

19. **Refactor ext-walls** — remove PB access, use zone metadata from NATS
    → verify: ext-walls registers wall zones correctly

20. **Refactor ext-av** — remove PB access, use zone metadata from NATS
    → verify: ext-av registers AV zones correctly

21. **Extension options schema in registration** — extensions declare options, worldsim creates PB collections
    → verify: extension registration creates PB collection with defaults

22. **Extension options hot-reload via NATS** — PB hook → worldsim → NATS → extension
    → verify: changing extension options in PB GUI updates extension behavior

**Merge gate:** extensions have zero PB access, zone data comes via NATS, extension options are admin-editable with hot-reload.

### Verification

- [ ] `make proto` generates Go + TS without errors
- [ ] `cd backend && go test ./internal/worldsim/ -v` — all existing + new tests pass
- [ ] `cd backend && go build ./...` — compiles
- [ ] `cd frontend && npm run build` — compiles
- [ ] `make up` — full stack starts without separate PB container
- [ ] PB admin GUI accessible at worldsim's port
- [ ] Frontend loads map, player can move, collision works
- [ ] Multiple maps loaded by worldsim
- [ ] Player can transition between maps via portal zones
- [ ] Extensions can trigger map transitions via NATS teleport
- [ ] Extensions register without PB access, get zone data via NATS
- [ ] Extension options editable in PB admin GUI, hot-reloaded via NATS

### Risks

- **PB Go SDK API changes**: The subagent's code snippets may not exactly match v0.39.x API. Need to verify against pkg.go.dev. The `app.Dao()` API has been stable but field/method names may differ.
- **PB `app.Start()` + worldsim tick loop concurrency**: PB starts its own HTTP server in a goroutine. Need to ensure no shared-state issues between PB's HTTP handlers and worldsim's tick loop. The PB DAO is goroutine-safe (SQLite with WAL mode), but the Simulator's `mu` mutex must still protect ECS state.
- **Migration conversion**: JS → Go migrations must produce identical schemas. Any mismatch breaks existing data. Test with an existing `pb_data` directory.
- **Frontend map transition UX**: Destroying and recreating the tilemap may cause a visible flash. May need a loading screen or fade transition.
- **Portal zone edge cases**: What if the target map doesn't exist? What if the target coordinates are inside a wall? Need validation.
- **Extension zone metadata format**: The JSON schema for zone metadata broadcast needs to be stable. Extensions parse it, so breaking changes require coordinated updates.
