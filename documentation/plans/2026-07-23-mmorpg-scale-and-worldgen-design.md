# MMORPG-Scale World Engine + Procedural World Generation — Design

**Date:** 2026-07-23
**Status:** Draft — research complete, awaiting review before implementation.
**Scope:** Two coupled initiatives: (A) scale the engine to huge maps with hundreds of concurrent players, and (B) procedurally generate those worlds (terrain, biomes, settlements, NPCs, history). They are coupled because infinite/large maps are the natural output of a world generator, and the generator's region/site hierarchy maps directly onto the streaming + AOI architecture.

---

## Table of Contents

1. [Current Architecture Summary](#1-current-architecture-summary)
2. [Research Findings: State of the Art](#2-research-findings-state-of-the-art)
   - 2.1 [Area of Interest (AOI) Management](#21-area-of-interest-aoi-management)
   - 2.2 [Chunked / Streamed Map Loading](#22-chunked--streamed-map-loading)
   - 2.3 [Spatial Partitioning for Server Authority](#23-spatial-partitioning-for-server-authority)
   - 2.4 [Replication & Network Protocols](#24-replication--network-protocols)
   - 2.5 [Collision in Unbounded Worlds](#25-collision-in-unbounded-worlds)
   - 2.6 [Bandwidth & Scaling Math](#26-bandwidth--scaling-math)
   - 2.7 [Frontend Streaming Considerations](#27-frontend-streaming-considerations)
   - 2.8 [Dwarf Fortress World Generation](#28-dwarf-fortress-world-generation)
   - 2.9 [Broader Procedural Generation State of the Art](#29-broader-procedural-generation-state-of-the-art)
3. [Target Architecture](#3-target-architecture)
4. [Blind Spots & Risk Register](#4-blind-spots--risk-register)
5. [Implementation Phases](#5-implementation-phases)

---

## 1. Current Architecture Summary

Derived from direct code reading (file:line citations throughout). This is the baseline the upgrade builds on.

### 1.1 Tick loop

`worldsim.go:2302-2379` — `tick()` runs at 20Hz (default, `TICK_HZ` env, `main.go:26`). Per-tick pipeline under `s.mu` (world mutex):

1. `SnapshotSeq++`
2. `movement.Step` — applies input, collision, clamps to map bounds (`movement.go:41-45`)
3. `zone.Step` — zone enter/exit detection, enqueues portal transitions (`zone_system.go:44-60`)
4. `proximity.Step` — throttled to ~4Hz (`TickCount%5`), clusters nearby players for A/V (`worldsim.go:2326-2332`)
5. `replication.Step` — "replicate everything to everyone on the same map" (`replication.go:27-29`, `worldsim.go:2334-2340`)
6. `s.mu.Unlock()` then `portal.Step` — applies pending portal transitions (re-locks mutex internally, `worldsim.go:2364-2378`)

**Key constraint:** the entire tick holds a single global mutex. All entity iteration is over `map[string]*Entity` with no spatial index.

### 1.2 Replication system

`replication.go:26-74` — `ReplicationSystem.Step` iterates **every entity** for **every connected client** (O(N*M) where N=entities, M=clients). For each (client, entity) pair:

- **Multi-map filter** (`replication.go:93-97`): skip entities on a different `MapId` than the client. This is the *only* spatial filtering today.
- **Spawn tracking** (`replication.go:99`, `worldsim.go:140`): `Entity.spawnedTo map[string]bool` tracks which clients have received a `SpawnEntity`. Late joiners get spawns for existing entities.
- **Dirty flags** (`worldsim.go:128-136`): `dirtyPosition`, `dirtyState`, `dirtyName`, `dirtyAppearance`, `dirtyLightEmitter`, `pendingAnimations`. Cleared globally after all clients replicated (`replication.go:60-68`).
- **Wire format**: protobuf `ReplicationBatch` containing `Spawns []*SpawnEntity`, `Updates []*UpdateComponent`, `Destroys`, `Animations`. Published to NATS subject `client.<id>.replication` (`replication.go:288`, `worldsim.go:1938`).
- **No delta compression, no baseline/ack, no bandwidth budget.** Each dirty position is a full `pb.Position` marshal.

### 1.3 Movement & collision

`movement.go:47-147` — per entity with `NetworkSession`:
- Speed: 0.4 tiles/tick (~8 tiles/sec at 20Hz).
- **Clamp to bounds** (`movement.go:88-90`): `clamp(newX, 0, Width-1)`, `clamp(newY, 0, Height-1)`. Assumes finite map.
- **Collision**: swept segment-vs-shape against blocked zones (`movement.go:186-228`), plus Walls tile-layer fallback at endpoint tiles (`movement.go:229+`). Subtile: `playerCollisionRadius=0.1` tiles (`worldsim.go:2619`), `avatarFeetYOffset=1.0` (`worldsim.go:2593`).
- `IsBlocked` (`mapdata.go:316-320`): out-of-bounds = blocked. This is the implicit world-edge invariant.

### 1.4 Collision grid

`mapdata.go:12-20` — `MapData.Collision [][]bool` is a dense `[Height][Width]` grid. Built from the "Walls" tile layer (`mapdata.go:127-146`). O(1) lookup, cache-friendly, but assumes finite bounds and allocates `Width*Height` bytes.

### 1.5 Zone system

`zones.go:17-35` — `Zone` has shape (rect/circle/polygon), tile-coord position, and metadata (zone_type, is_exclusive, mobility, av_enabled, portal targets). `ZoneRegistry` (`zones.go:77-120`) is a flat `[]*Zone` list — **no spatial index**. `ZonesAtPoint` (`zones.go:107-115`) iterates all zones for every entity every tick (O(Z*E)).

Mobile proximity zones (`worldsim.go:144-148`): each player gets a circle zone (radius `proximityRadius=2.0` tiles, `worldsim.go:2599`) that follows their avatar. Used for A/V proximity clustering.

### 1.6 Map loading & asset serving

- `mapstore.go:146-187` — `LoadMapData` reads Tiled JSON from PocketBase file storage, calls `ParseTiledMapJSON`.
- `mapdata.go:78-107` — `tiledMapJSON` struct parses only `width`, `height`, `tilewidth`, `tileheight`, `layers[].{name,type,data,objects}`. **No `infinite` field, no `chunks` field.** Infinite maps silently produce a 0x0 collision grid.
- `asset_http.go:54-134` — `GET /api/assets/maps/{name}` returns the full Tiled JSON + tileset URLs in one response. Frontend fetches the entire map at once.
- `mapdata.go:306-313` — `MapData` stores `Width`, `Height` from the JSON. For infinite maps these are 0.
- Reload detection: `worldsim.go:2411-2551` compares `tiled_json` filename; on change, reloads the map and swaps `MapData` + `ZoneRegistry` atomically.

### 1.7 Frontend map loading

`mapLoader.ts:52-70` — `loadMapAssets` fetches the full map JSON + tileset list from worldsim's asset API. `GameScene.ts:644` calls `this.load.tilemapTiledJSON("map", ...)` — Phaser 4's built-in parser, which **does** support infinite maps (chunks) natively for rendering. But the backend doesn't, so collision/replication break.

No chunk streaming, no dynamic load/unload. The entire map is loaded into memory at scene start.

### 1.8 NATS subjects

From grep of `worldsim.go` and related:
- `client.connected`, `client.disconnected` — session lifecycle
- `client.*.heartbeat`, `client.*.input`, `client.*.action`, `client.*.chat`, `client.*.set_name`, `client.*.set_sprite_base`, `client.*.set_player_options`, `client.*.set_status`, `client.*.set_afk` — client→server
- `client.<id>.replication` — server→client replication batches
- `client.<id>.admin` — admin-only data (IPs, device IDs)
- `client.<id>.force_close` — admin kick
- `worldsim.ready` — worldsim startup signal
- `worldsim.entity.teleport`, `worldsim.entity_info`, `worldsim.entities.query`, `worldsim.entity.get` — entity queries
- `worldsim.client.kick`, `worldsim.client.ban`, `worldsim.admin.chat`, `worldsim.admin.set_*` — admin actions
- `worldsim.zones.get`, `worldsim.zones` — zone metadata
- `worldsim.stats.get`, `worldsim.recording.*`, `worldsim.world_options.*`, `worldsim.admin_emails.get` — misc
- `extension.*.register`, `extension.*.heartbeat`, `extension.*.register_triggers` — extension lifecycle
- `extension.<id>.action` — extension input dispatch (e.g. `extension.props.action`)
- `chat.broadcast`, `audit.event`, `zone.enter`, `zone.exit`, `proximity.join`, `proximity.leave` — events

Pusher (`backend/cmd/pusher`) is a pure WebSocket↔NATS gateway. It validates PocketBase JWTs and forwards frames. Knows nothing about replication wire format.

### 1.9 Entity component model

`worldsim.go:50-159` — `Entity` struct is a flat aggregate (not a proper ECS): `ID`, `Position *pb.Position`, `NetworkSession`, `DisplayName`, `IsGuest`, `IsAdmin`, `IP`, `DeviceID`, `EntityType`, `OwnerExtension`, `TriggerRadius`, `Gid`, `GidOff`, `GidOn`, `SpriteBase`, `State`, `LightIntensity/Color/Radius`, dirty flags, `spawnedTo`, `currentZones`, `mobileZone`, `currentProximityGroup`, `stationaryTicks`, `PlayerOptions`.

Components are replicated as protobuf messages: `pb.Position`, `pb.Appearance`, `pb.EntityState`, `pb.DisplayName`, `pb.LightEmitter`. Component IDs: `compPosition`, `compAppearance`, `compEntityState`, `compDisplayName`, `compLightEmitter`.

### 1.10 Current scaling limits

- **Replication is O(N*M)** with no spatial filtering beyond same-map. At 100 players on one map, that's 10,000 (client,entity) pairs per tick. At 500 players, 250,000. This is the primary bottleneck.
- **No bandwidth budget**: every dirty position is a full protobuf marshal (~20-30 bytes for `pb.Position`). 100 moving players at 20Hz = ~50KB/s per receiving client = ~400kbps. 500 players = ~2Mbps per client. Feasible but wasteful; 1000 players = ~4Mbps per client, approaching problematic.
- **Zone checks are O(Z*E)** per tick with no spatial index.
- **Single global mutex** — no parallelism in the tick.
- **"Lite MVP: replicate everything to everyone"** (`replication.go:27`, `worldsim.go:2335`) — explicitly acknowledged as a temporary design.

---

## 2. Research Findings: State of the Art

### 2.1 Area of Interest (AOI) Management

**The nine-grid (cell subscription) algorithm** is the workhorse of classic MMOs (Lineage, WoW-style zone servers). The world is divided into a uniform grid of cells. A player subscribes to their current cell + the 8 neighboring cells (the "nine-grid" / "九宫格"). When an entity moves, it's placed in the cell containing its position. Replication only goes to clients subscribed to that cell. When a player crosses a cell boundary, they unsubscribe from cells that fell out of range and subscribe to new ones.

Sources:
- [MMO AOI Algorithm (DEV Community)](https://dev.to/aceld/11-mmo-online-game-aoi-algorithm-l7d) — grid-based AOI with Go implementation
- [Zinx framework AOI nine-grid](https://aceld.gitbooks.io/zinx/content/) — production Go MMO framework
- [AOI algorithm performance analysis](https://wiki.disenone.site/en/game-%E6%B8%B8%E6%88%8FAOI%E7%AE%97%E6%B3%95%E8%A7%A3%E6%9E%90%E5%92%8C%E6%80%A7%E8%83%BD%E5%AE%9E%E6%B5%8B/) — compares nine-grid vs. cross-linked-list AOI
- [Comparing Interest Management Algorithms (NUS)](https://www.comp.nus.edu.sg/~cs4344/0607s1/netgames06/s01Conf96_a32.pdf) — academic comparison of 8 AOI algorithms; "square tile" (nine-grid) is the baseline

**Hysteresis / anti-thrash:** When a player walks along a cell boundary, they'd rapidly enter/exit cells, causing spawn/despawn storms. The standard fix is an **exit radius larger than the enter radius** (PixelEruv already does this for proximity zones: `proximityExitRadius` > `proximityRadius`, `worldsim.go:2601-2603`). Applied to AOI cells: subscribe to cells within radius R, only unsubscribe when beyond radius R+hysteresis.

**The edge problem:** Entities near cell borders are visible to multiple cells. This is handled naturally by the nine-grid: a player subscribed to 9 cells sees all entities in those 9 cells, including those near borders. No special handling needed — the cell is the unit of subscription, not the entity.

**Cross-linked-list AOI** is an alternative: each entity maintains linked lists of watchers (entities whose AOI contains it) and watched (entities in its AOI). On move, update the lists incrementally. Better for very dense scenes but more complex. The nine-grid is simpler and sufficient for our scale.

**Tradeoffs:** Grid cell size should be ~2x the AOI radius. Too small → too many cell transitions. Too large → too many entities per cell (defeats the purpose). For PixelEruv with `proximityRadius=2.0` tiles, a cell size of ~8-16 tiles is reasonable (player sees 2-3 cells in each direction).

**Mapping to PixelEruv:** Replace the same-map filter (`replication.go:93-97`) with a cell-subscription check. Each entity is indexed in an `AOIGrid` keyed by `(cellX, cellY)`. Each client has a `subscribedCells` set. Replication iterates only entities in subscribed cells. This converts O(N*M) to O(N_local * M) where N_local is entities in the client's AOI.

### 2.2 Chunked / Streamed Map Loading

**Tiled infinite maps** ([Tiled JSON Map Format docs](https://doc.mapeditor.org/en/stable/reference/json-map-format/), [TMX format docs](https://doc.mapeditor.org/en/stable/reference/tmx-map-format/)):
- `infinite: true` at the map level.
- `width` and `height` are **0** for infinite maps.
- Tile layers use `chunks` (array of chunk objects) instead of `data` (flat array).
- Each chunk: `{x, y, width, height, data}` — `x`,`y` are the chunk's position in tiles (can be negative), `width`/`height` default to 16x16.
- Chunks are sparse: only painted chunks exist. Unpainted areas have no chunk.
- [Tiled GitHub issue #1733](https://github.com/bjorn/tiled/issues/1733): "Currently they are always 16x16 in size, but this may change in the future... Tiled will handle chunks of any size."

**Minecraft chunk protocol** ([wiki.vg Protocol FAQ](https://wiki.vg/Protocol_FAQ), [Chunk Format](https://wiki.vg/Chunk_Format)):
- Server sends `Chunk Data and Update Light` packets for chunks in a **circular area centered on the player**.
- Client sends `Chunk Center` to tell the server its render distance center.
- Server tracks a per-client "chunk subscription radius" and sends/unloads chunks as the player moves.
- Chunks use **paletted block storage** — a per-chunk palette maps block IDs to compact indices, so common blocks cost ~1-2 bits each. This is critical for bandwidth.
- Heightmaps are sent to avoid client-side recalculation.
- **Key pattern:** server-push-on-move, not request-nearby-chunks. The server proactively sends chunks entering the radius and unloads chunks leaving it.

**Roblox instance streaming** ([Roblox Creator Hub docs](https://create.roblox.com/docs/en-us/workspace/streaming)):
- `StreamingEnabled` (default for new places): server initially sends only content closest to the client.
- `StreamingMinRadius` / `StreamingTargetRadius`: chunks within min radius never stream out; content between min and target streams in/out based on player position.
- `StreamOutBehavior`: `Opportunistic` (stream out when not visible) vs. `Default` (stream out by distance).
- **Replication foci**: multiple points (not just the player) can request streaming — e.g. a camera look direction.
- **Atomic models**: groups of instances that must appear together (a building + its props) stream in as a unit.
- Benefits: faster join times, memory efficiency, better performance (server syncs less).

**Tradeoffs for PixelEruv:**
- **Request-based** (client asks for chunks): simpler, but adds a round-trip and the client must know what to ask for.
- **Server-push-on-move** (server tracks player position and pushes nearby chunks): lower latency, matches Minecraft/Roblox, but server must maintain per-client chunk subscription state.
- **Recommendation:** server-push-on-move, matching the existing replication pattern (server already pushes replication batches).

### 2.3 Spatial Partitioning for Server Authority

**Uniform grid / spatial hash** is the consensus choice for moving entities in 2D games:
- [Spatial hash vs quadtree (GameDev StackExchange)](https://gamedev.stackexchange.com/questions/69776/when-is-a-quadtree-preferable-over-spatial-hashing): "spatial hashing is way more efficient... O(1) insert/remove vs O(log n)" for dynamic scenes.
- [Comparative analysis of spatial partitioning (WSCG 2013)](http://wscg.zcu.cz/wscg2013/program/short/D71-full.pdf): "regular grid is very effective... fast for collision detection and easy to implement... data structure need not be updated any time during the whole simulation."
- [Sparse spatial hash grid (DigiStar)](https://queelius.github.io/sparse_spatial_hash/): 14.7M entities/sec build, 60,000x less memory than dense grids, Morton hashing for cache locality.

**Quadtree** is better when:
- Entity sizes vary wildly (a quadtree adapts; a uniform grid needs multiple grids per size tier).
- The world is very sparse with large empty regions (a quadtree can skip empty subtrees; a hash grid still has hash overhead per query).
- Entities are mostly stationary (quadtree rebuild cost is amortized).

**For PixelEruv:** entities are uniformly sized (avatars are ~1 tile, props are ~1 tile), mostly moving, and the world is 2D. A **uniform spatial hash grid** is the right choice. It doubles as both the AOI grid (section 2.1) and the collision broad-phase.

**Sharding / zone-based world servers** (Eve Online, WoW):
- [Eve Online single-shard architecture](https://www.gamedeveloper.com/design/infinite-space-an-argument-for-single-sharded-architecture-in-mmos): Eve runs one universe across ~90-100 "SOL blades" (solar system servers), with proxy nodes fronting client connections. Each solar system is a separate process. Monikers (RPC handles) allow cross-system calls.
- [Eve cluster architecture](https://www.eveonline.com/news/view/the-eve-cluster): proxies → SOL servers → database. Load balancers mask real server IPs.
- WoW: continents are sharded by zone; crossing a zone boundary transfers the player between server processes (the "seamless zone" problem — it's not truly seamless, there's a brief load).

**For PixelEruv (hundreds, not thousands, of players):** single-process worldsim with AOI filtering is sufficient. Sharding is a future scaling axis (Phase 6) but not needed for the target of "hundreds of players." The AOI grid gives us most of the benefit of sharding (only nearby entities are processed/replicated) without the complexity of cross-process handoff.

### 2.4 Replication & Network Protocols

**Snapshot interpolation** ([Gaffer On Games — Snapshot Interpolation](https://gafferongames.com/post/snapshot_interpolation/)):
- Server captures snapshots of relevant state at a fixed rate (e.g. 20Hz).
- Client buffers snapshots and interpolates between them, trading a small latency buffer for smoothness.
- Interpolation buffer should hold ~100ms of snapshots to handle jitter and 1-2 lost packets.
- **Hermite interpolation** (using velocity hints) eliminates 1st-order discontinuity artifacts at low send rates.
- UDP, not TCP — lost snapshots are skipped, not retransmitted.

**Delta compression** ([Gaffer On Games — Snapshot Compression](https://gafferongames.com/post/snapshot_compression/)):
- Instead of sending full state each tick, send "snapshot N encoded relative to baseline snapshot B."
- Per-entity: 1 bit "not changed" (same as baseline) → skip entirely. This is the "big order of magnitude win."
- Requires ack: client tells server "most recent snapshot received = N." Server updates baseline to the latest acked.
- Handles packet loss: if baseline is lost, fall back to an older acked baseline.
- **Quantization**: bound and quantize position (e.g. 18 bits for x/y in [-256,255] meters at 2mm precision), quaternion "smallest three" (29 bits vs 128). For 2D: position is 2 floats → ~36 bits quantized vs 64 bits raw.
- **At-rest bit**: stationary entities cost 1 bit (just "not changed"). This is huge for MMOs where most entities are idle.

**Overwatch networking** ([GDC Vault — Overwatch Gameplay Architecture and Netcode](https://www.gdcvault.com/play/1024001/-Overwatch-Gameplay-Architecture-and), [Networking Scripted Weapons and Abilities](https://www.gdcvault.com/play/1024041/Networking-Scripted-Weapons-and-Abilities), [Developer Update: Netcode](https://www.youtube.com/watch?v=vTH2ZPgYujQ), [High Bandwidth Update](https://www.youtube.com/watch?v=EqtNUFxgm38)):
- ECS architecture with deterministic simulation + client-side prediction + server reconciliation.
- 20.8Hz base update rate (later 62.5Hz "high bandwidth" mode).
- **Interpolation delay** ("IND"): client renders entities ~1 update back in time, smoothly interpolating. Lower IND = more responsive but more "shot around corners."
- **Relevance filtering**: prioritize what's near the player. Entities outside view are lower priority or not sent.
- **Adaptive bandwidth**: server dynamically adjusts send rate based on client connection quality.

**Relevance / priority filtering** (Overwatch, Unreal):
- Sort entities by relevance (distance, facing, gameplay importance).
- When over bandwidth budget, drop lowest-priority updates (send them next tick).
- Critical events (spawn, destroy, gameplay-affecting state) are always sent; cosmetic updates are droppable.

**For PixelEruv:**
- Current: full protobuf marshal per dirty component, no delta, no budget. Works for <100 players.
- Phase 2: add AOI filtering (section 2.1) — biggest win, converts O(N*M) to O(N_local*M).
- Phase 3: add delta compression (baseline + ack + "not changed" bit) — reduces per-entity cost from ~20-30 bytes to ~1 bit for unchanged.
- Phase 4: add bandwidth budgeting + priority — cap per-client bytes/sec, drop low-priority updates.
- Client-side interpolation already partially exists (frontend renders received positions), but could be formalized with an interpolation buffer.

### 2.5 Collision in Unbounded Worlds

Three approaches, in order of complexity:

1. **Bounding-box dense grid** (simplest): compute the bounding box of all painted chunks, allocate a dense `[H][W]` grid, stamp each chunk's data into it. `IsBlocked` and `clamp` work unchanged. Cost: `Width*Height` bytes. Fine for maps up to ~1000x1000 (1MB). Wasteful for very sparse worlds.

2. **Per-chunk collision storage**: store collision as a `map[chunkKey][]bool` where each chunk is 16x16. `IsBlocked(tx,ty)` looks up the chunk containing `(tx,ty)` and indexes within it. Missing chunk = walkable (or blocked, design choice). O(1) with hash overhead. Memory: only painted chunks. Supports truly unbounded worlds.

3. **Spatial hash of blocked tiles**: `map[(x,y)]bool` of only the blocked tiles. `IsBlocked` is a hash lookup. Most memory-efficient for sparse walls, but iterating "all blocked tiles near X" requires a spatial query.

**For PixelEruv:** Start with approach 1 (bounding-box dense grid) in Phase 1 — it's the minimal change, keeps all downstream code working, and supports maps up to ~1000x1000. Migrate to approach 2 (per-chunk) in Phase 5 if worlds grow beyond that. The swept collision in `movement.go:186-228` queries `IsBlocked` at specific tiles, so it works with any of the three approaches.

### 2.6 Bandwidth & Scaling Math

**Per-entity replication cost (current, uncompressed protobuf):**
- `pb.Position`: entity_id (string, ~10 bytes) + component_id (1 byte) + x (4 bytes float) + y (4 bytes) + dir (1 byte) + snapshot_seq (4 bytes) + protobuf overhead (~5 bytes) ≈ **~25-30 bytes per dirty position update**.
- `pb.Appearance`: ~20-30 bytes.
- `pb.DisplayName`: ~30-50 bytes (name string).

**At 20Hz, 100 moving players, no AOI (current):**
- Each client receives 100 position updates/tick × 20 ticks/sec × 28 bytes ≈ **56KB/s ≈ 448kbps per client**.
- Server sends: 100 clients × 56KB/s = **5.6MB/s total upstream**.
- This is feasible but already noticeable. At 500 players: 2.24Mbps per client, 112MB/s total. At 1000: 4.48Mbps per client (problematic for some connections), 448MB/s total (problematic for server).

**With AOI (assume ~20 entities in AOI radius):**
- Each client receives ~20 updates/tick × 20 × 28 ≈ **11KB/s ≈ 89kbps per client**.
- Server: 100 clients × 11KB/s = 1.1MB/s. 500 clients = 5.5MB/s. 1000 clients = 11MB/s.
- **AOI alone gives a ~5x reduction** at 100 players, growing with player count.

**With AOI + delta compression (at-rest bit):**
- Assume 80% of entities are stationary any given tick. 20 entities × 20% moving × 28 bytes + 20 × 80% × 1 bit ≈ **~1.4KB/s ≈ 11kbps per client**.
- Server: 100 clients × 1.4KB/s = 140KB/s. 1000 clients = 1.4MB/s.
- **AOI + delta gives a ~40x reduction** vs. current at 100 players.

**Conclusion:** AOI filtering (Phase 2) is the single highest-impact change. Delta compression (Phase 3) is the second. Together they bring 1000-player scale from "4.5Mbps per client, 448MB/s server" to "~11kbps per client, 1.4MB/s server" — well within budget.

**Budget target:** 100kbps per client (typical mobile/bad-connection budget). With AOI + delta, we have ~10x headroom at 1000 players.

### 2.7 Frontend Streaming Considerations

**Phaser 4 capabilities:**
- `load.tilemapTiledJSON` natively parses infinite maps (chunks) for rendering — already works.
- No built-in dynamic chunk load/unload. Phaser loads the entire tilemap data at once.
- **Dynamic chunk loading requires custom work**: the frontend would need to receive chunk data incrementally (via NATS/WebSocket), construct Phaser tilemap layers per chunk, and destroy layers for unloaded chunks.
- **Sprite pooling**: when entities despawn (leave AOI), their sprites should be pooled, not destroyed, to avoid GC pressure.
- **Texture management**: tileset textures are shared across chunks, so chunk load/unload doesn't affect texture memory. Only tilemap layer data (the tile index arrays) is per-chunk.

**Camera-followed streaming pattern** (from Minecraft/Roblox):
- Frontend tracks camera center in world coords.
- Requests/subscribes to chunks within `renderRadius` of camera.
- As camera moves, new chunks load (with a priority queue: nearest first).
- Old chunks unload beyond `unloadRadius` (with hysteresis to prevent thrash).
- **Loading indicator**: show a subtle loading state for chunks being fetched.

**For PixelEruv:** Phaser's native infinite map rendering works for the initial implementation (load all chunks at once). True streaming (load/unload chunks dynamically) is Phase 5 — it's needed only when maps grow large enough that loading all chunks at once is too slow/memory-heavy. For a 200x200 tile world (25k tiles), loading all at once is fine. For a 2000x2000 world (4M tiles), streaming becomes necessary.

### 2.8 Dwarf Fortress World Generation

Dwarf Fortress is the gold standard for procedural world generation. The following is synthesized from primary sources.

#### 2.8.1 World generation pipeline

DF generates worlds in distinct phases ([DF Wiki — Advanced World Generation](https://dwarffortresswiki.org/index.php/Advanced_world_generation), [Polygon interview with Tarn Adams](https://www.polygon.com/2014/7/23/5926447/dwarf-fortress-will-crush-your-cpu-because-creating-history-is-hard/)):

1. **Elevation** ("Preparing elevation..."): A fractal generator creates the heightmap, dividing ocean from land. DF generates and **rejects** candidate heightmaps until the elevation distribution is "interesting" (right amount of high/low points). The world is ~16,000 square miles (large world = 257x257 region tiles).

2. **Temperature** ("Setting temperature..."): A temperature fractal is generated. Temperature varies along the Y axis (poles = cold, equator = hot) with fractal noise variation.

3. **Rainfall**: A separate fractal plots annual rainfall per location.

4. **Drainage / rivers** ("Running rivers..."): After biomes are created from temperature + rainfall, rivers are simulated flowing downhill. They carve valleys. When rivers meet the sea, a salinity algorithm defines swamps/deltas/mangroves. **Rain shadows**: warm wet air rising over mountains loses moisture → rain forests on windward side, deserts on leeward side. DF simulates this, causing a visible "shudder" in biome boundaries when it kicks in.

5. **Biomes** ("Verifying terrain..."): Rough biomes emerge as contiguous tiles from temperature × rainfall × drainage (a Whittaker-diagram-like classification). Each biome has a subset of flora/fauna.

6. **Good/Evil alignment**: Each region is assigned energy from good (1) to evil (20) on a scale. This affects place names and creature types.

7. **Vegetation, minerals, wildlife**: Vegetation grows based on biome. Minerals are deposited via a fractal simulating geological strata. Wildlife is imported.

8. **Civilizations** ("Placing civilizations..."): Dwarves, humans, elves, goblins are placed at starting locations based on biome preferences (dwarves in mountains, humans in plains, elves in forests, goblins in evil areas).

9. **Beasts**: Cave civilizations, cave populations, other beasts, megabeasts are placed.

10. **Prehistory / History simulation**: The world's history plays out, **a week at a time, for up to 250+ years**. Towns grow and flourish, expand, conflict with neighbors. Leaders rise and create dynasties which fall. Hundreds of thousands of creatures live and die. Farms run on yearly cycles producing actual food numbers — cities shrink when people starve or migrate. Every object made is tracked.

11. **Finalization**: Civilization materials, art, uniforms, sites are finalized.

**Key insight from Tarn Adams (Polygon interview):** "They have their farms that all go on this yearly cycle and give them actual food numbers. When you see a city shrink down in size, the people have either moved away or they ran out of food. Every object that is made is tracked." A merchant in a city might tell you their leather caps were made in an elvish city half a world away — **and it's true**, because the trade was simulated during worldgen.

#### 2.8.2 The history simulation

([DF Wiki — Legends](https://www.dwarffortresswiki.org/index.php/Legends), [Historical figure](https://dwarffortresswiki.org/index.php/Historical_figure), [World History file](https://dwarffortresswiki.org/index.php/World_History_file))

- **Historical figures (histfigs)**: Due to computation/memory constraints, most population is treated abstractly. A small percentage is tracked explicitly — nobility, leaders, megabeasts, necromancers, villains, anyone who kills a civ member, anyone the player encounters. Rules guarantee consistency: people you encounter don't vanish when you leave a site.
- **Events**: Wars, births, deaths, migrations, beast attacks, site founding/abandonment, artifact creation, duels, abductions, marriages. Each event is recorded and viewable in Legends mode.
- **Emergent narrative**: The story of Sazir Stockadebolt (from the Polygon article) — a dwarf abducted as an infant, raised by goblins, married, husband murdered, went mad, joined goblin army, tamed a cave dragon, became general, was murdered at 106. This wasn't scripted; it emerged from the simulation of personality + circumstance + history.
- **Ages**: The world progresses through "Age of Myth" → "Age of Legends" → "Age of Heroes" as megabeasts are killed and civilizations rise/fall.
- **Legends mode**: Players can browse the entire generated history — maps, civ histories, figure biographies, event chains.

#### 2.8.3 Technical structure

([DF Wiki — World generation](https://dwarffortresswiki.org/index.php/World_generation), [DFHack Maps API](https://docs.dfhack.org/en/53.07-r1/docs/api/Maps.html), [40d:Region](https://dwarffortresswiki.org/index.php/40d:Regions))

**Three-level hierarchy:**
- **World tiles**: 16×16 groups of region tiles (e.g. 257×257 world tiles for a large world). Visible on the world map.
- **Region tiles** (aka "mid-level tiles" / MLTs): 3×3 groups of block columns, or 48×48 local tiles. Visible when resizing a fortress embark.
- **Local tiles**: The smallest scale. 1 local tile = 1 cursor position in fortress/adventure mode.

**A large world**: 257×257 world tiles = 65536 region tiles (wait, that's 16²×257²... actually 257×257 world tiles, each containing 16×16 region tiles, each containing 48×48 local tiles = 768×768 local tiles per world tile). Total: 257² × 768² ≈ 38.6 billion local tiles (before Z-layers). ([40d:Region](https://dwarffortresswiki.org/index.php/40d:Regions))

**On-demand detail generation**: Site maps (towns, caves, tombs) are generated **procedurally when you visit them**, not during worldgen. The worldgen produces the site's history and abstract structure; the actual tile-level map is generated on first visit and cached. This is how DF handles the sheer scale — it doesn't generate 38 billion tiles upfront.

**Storage**: World data is stored in region files (world.dat etc.). The world is persistent — history continues during gameplay, just slower.

**PRNG determinism**: World generation uses a PRNG seeded by a user-enterable seed. Same seed → same world (mostly; some events vary). ([DF Wiki — Advanced World Generation](https://dwarffortresswiki.org/index.php/Advanced_world_generation))

#### 2.8.4 Key algorithms

- **Fractal terrain**: Stacked fractal layers (elevation, temperature, rainfall, drainage) blended with algorithms. Tarn Adams has a PhD in mathematics (Stanford, 2005, "Flat Chains in Banach Spaces" — [Wikipedia](https://en.wikipedia.org/wiki/Tarn_Adams)) though his thesis was in geometric measure theory, not directly terrain generation. The fractal approach is his own engineering.
- **Rain shadows**: Custom algorithm simulating orographic precipitation — warm wet air rises over mountains, loses moisture, creates deserts on the leeward side.
- **Drainage / river generation**: Simulates water flowing downhill from high elevation to the sea, carving valleys. A drainage-basin algorithm.
- **Voronoi for civilization territory**: Civilizations' influence zones are modeled (the mesh-size parameter in advanced worldgen controls the grid of intersection points for terrain characteristics, which is a Voronoi-like decomposition).
- **Pathfinding for roads/trade routes**: Sites are connected by roads; trade routes are pathfound between sites.
- **Entity system for civilizations**: Sites, populations, governments, religions are tracked as "entities" with relationships.

#### 2.8.5 Personality & needs system

([DF Wiki — Thoughts and Preferences](https://dwarffortresswiki.org/index.php/Thoughts_and_Preferences), [Personality facet](https://dwarffortresswiki.org/index.php/Personality_trait), [Personality value](https://dwarffortresswiki.org/index.php/Personality_value))

- **Personality facets**: 0-100 scale, ~50 facets (anxiety, anger, friendship, greed, altruism, etc.). Inspired by the Big Five model. Race-specific defaults (dwarves slightly more greedy, goblins low altruism). Facets determine emotional reactions and social skill training.
- **Values**: -50 to +50, beliefs (tradition, cooperation, sacrifice, romance, etc.). Cultural values (dwarves value craftsdwarfship, goblins disdain fairness). Individual values can conflict with facets (values romance but can't form romantic bonds → generates internal tension text).
- **Needs**: Affect focus (productivity/combat effectiveness) rather than happiness. Dwarves become unfocused if needs aren't met.
- **Memories**: Events change facets over time. A traumatic event can permanently shift a personality facet.

### 2.9 Broader Procedural Generation State of the Art

#### 2.9.1 Terrain generation

**Noise functions** ([OpenSimplex noise — Wikipedia](https://en.m.wikipedia.org/wiki/OpenSimplex_noise), [Simplex noise — Wikipedia](https://en.wikipedia.org/wiki/Simplex_noise), [Perlin noise — Wikipedia](https://en.wikipedia.org/wiki/Perlin_noise)):
- **Perlin noise** (Ken Perlin, 1982): gradient noise, O(2^n) per evaluation. Patent-free. Has directional artifacts (grid-aligned).
- **Simplex noise** (Ken Perlin, 2001): O(n²) per evaluation, isotropic (no grid artifacts). **Patented** for 3D+ applications (US patent 6867776, expired January 8, 2022 — now free to use, but historically drove people to alternatives).
- **OpenSimplex noise**: patent-free alternative to Simplex, similar quality, slightly larger kernel (smoother but slower). Variants: OpenSimplex2F (identical to SuperSimplex), OpenSimplex2S (smoother).
- **Fractal Brownian motion (fBm)**: stack multiple noise octaves with consistent lacunarity (frequency multiplier) and gain (amplitude decay). This is what makes noise look like terrain rather than static.
- **Ridged noise**: `1 - abs(noise)` — creates sharp ridges, good for mountain ranges.
- **Worley/Voronoi noise**: cellular patterns based on distance to nearest point. Good for cell-like structures (cracks, regions).

**Recommendation for PixelEruv:** OpenSimplex2 (patent-free, no artifacts) with fBm octaves for elevation. Ridged OpenSimplex for mountain ranges. Worley for biome region boundaries.

**Hydraulic erosion** ([Particle-Based Hydraulic Erosion (IEEE)](https://ieeexplore.ieee.org/document/11482049), [holzman.dev implementation](http://holzman.dev/articles/2023/12/01/hydraulic_erosion.html)):
- Simulate water droplets moving across a heightmap, picking up and depositing sediment.
- Each droplet: spawned at random position, moves downhill (gradient of heightmap), carries sediment. When velocity drops (flat area or uphill), deposits sediment.
- Parameters: droplet count, droplet lifetime, erosion rate, deposition rate, evaporation rate.
- Produces realistic valleys, gullies, mountain ridges. ~50,000 droplets on a 512x512 heightmap takes ~1 second.
- **For PixelEruv:** Optional post-processing step on the elevation noise. Adds realism but isn't essential for a first pass. Phase 3+.

#### 2.9.2 Biome & ecosystem generation

**Whittaker biome classification** ([Whittaker Diagram Guide](https://gveg.wyobiodiversity.org/application/files/7916/4641/2117/Whittaker_Diagram_Guide.pdf), [Nature Scientific Data](https://www.nature.com/articles/s41597-025-04387-0/figures/2)):
- 2D diagram: x-axis = annual precipitation, y-axis = average temperature.
- 9 biomes: tropical rainforest, tropical seasonal forest, temperate rainforest, temperate deciduous forest, temperate grassland, desert, tundra, taiga, scrubland.
- Each (temperature, precipitation) pair maps to a biome.

**DF's approach**: temperature (Y-axis + fractal) × rainfall (fractal) × drainage → biome. This is essentially Whittaker with an added drainage dimension.

**For PixelEruv:** Generate temperature (latitude-based + noise) and precipitation (noise + rain shadow) maps, then classify each tile into a biome via a Whittaker-style lookup table. Each biome maps to a tileset (grass, sand, snow, forest, water, etc.).

#### 2.9.3 Settlement & road generation

**L-system city generation** ([Parish & Müller — Procedural Modeling of Cities (2001)](https://people.eecs.berkeley.edu/~sequin/CS285/PAPERS/Parish_Muller01.pdf)):
- Input: land-water boundaries, population density map.
- L-system generates highways (long, straight, following population density), then streets (shorter, branching, grid-like or radial).
- Extended L-systems with "global goals and local constraints" — roads check for intersections, water bodies, existing roads.
- Land is divided into lots, buildings generated per lot.
- This is the foundation of Esri CityEngine ([Springer link](https://link.springer.com/chapter/10.1007/978-981-15-8983-6_35)).

**Settlement placement**: Settlements emerge near resources — water (rivers, coast), fertile land (biome), defensibility (hills, chokepoints). DF places civs based on biome preferences.

**For PixelEruv:** Place settlements at high-suitability locations (near water, in habitable biomes, with some spacing). Generate road networks connecting nearby settlements using A* pathfinding avoiding water/mountains. Within each settlement, use a simplified L-system or template-based building placement.

#### 2.9.4 NPC & population generation

**Name generation** ([Markov Name Generator](https://github.com/Tw1ddle/MarkovNameGenerator), [VNameGenerator](https://www.github.com/Valkryst/VNameGenerator)):
- **Markov chains**: learn transition probabilities from training names. Order 2-3 is usually best. Katz back-off model falls back to lower orders when high-order has no data.
- **Consonant-vowel alternation**: simpler, produces pronounceable but less varied names.
- **Context-free grammar**: rules like `<name> ::= <syllable> <syllable> <suffix>`. Most controllable.
- **DF's approach**: Four hardcoded languages (human, dwarven, elven, goblin) with word lists. Names are composed from word lists with grammar rules. Good/evil words bias naming. DF hopes to procedurally generate languages in the future.

**Personality models**: DF uses ~50 facets (0-100) + values (-50 to +50) + needs, inspired by Big Five. RimWorld uses a simpler trait system. For an MMO, a lighter model suffices — a handful of traits that affect NPC behavior (aggression, sociability, diligence).

**Demographics**: Generate populations with age distribution, professions weighted by settlement type (farming town → mostly farmers; trading hub → merchants, craftsmen). Family trees via marriage + birth simulation during history gen.

**For PixelEruv:** Markov chain name generator trained on fantasy name corpora, with per-culture training sets (dwarven, human, elven names feel different). Simple personality model (5-10 traits). NPC schedules can be minimal for an MMO (NPCs stand at posts, wander, or respond to interaction) — full DF-style daily routines are out of scope.

#### 2.9.5 Story & quest generation

**Emergent narrative (DF's philosophy)**: Don't script stories. Simulate systems (personality, needs, conflict, economy) and let stories emerge from their interaction. The player discovers stories by exploring the world and talking to NPCs. DF's Legends mode is a read-only browser of the emergent history.

**Quest generation from world state**:
- Skyrim's Radiant Quest system: templates ("go to X, fetch/kill Y") filled with world-state-appropriate targets.
- DF's rumors system: historical events become rumors that NPCs share. "A cyclops killed an elf here" → the player can go fight the cyclops.
- The quest isn't generated — it's **discovered**. The world state already contains the cyclops and the dead elf; the NPC just tells you about it.

**LLM-assisted narrative (2022+)**: Recent work uses LLMs to generate quest text, NPC dialogue, or story beats from world state. **Critical assessment:** LLMs are good at generating flavorful text (dialogue, descriptions) but bad at maintaining world-state consistency at scale. They hallucinate facts that contradict the simulation. Best use: LLM as a text *renderer* (world state → prose), not as a *generator* (world state from prose). This is an active research area; treat as experimental, not production-ready.

**For PixelEruv:** Phase 4+. Start with DF-style emergent quests: the worldgen produces history (wars, beast attacks, lost artifacts), and NPCs share rumors derived from that history. Quests are templates filled with world-state entities. LLM text rendering (not generation) as an optional enhancement.

#### 2.9.6 Dungeon / interior generation

**BSP (Binary Space Partitioning)** ([Umeå thesis comparing BSP vs CA](http://urn.kb.se/resolve?urn=urn%3Anbn%3Ase%3Aumu%3Adiva-243697), [Jaconir blog](https://jaconir.online/blogs/procedural-level-generation-guide)):
- Recursively split a rectangle into halves until regions are room-sized. Place a room in each leaf. Connect sibling rooms with corridors.
- **Reliable, predictable, well-structured.** Every room is reachable. Best for dungeons with distinct rooms and loot placement.

**Cellular automata caves**:
- Fill map randomly (~45% floor). Apply rule repeatedly (4-6 iterations): cell becomes wall if ≥4 wall neighbors, else floor. Random noise resolves into organic cave shapes.
- **More expressive, more varied**, but can produce isolated rooms (needs flood-fill post-processing). Best for natural caves.

**DF's approach**: Tombs, catacombs, lairs are generated when visited, using structure-appropriate algorithms. Tombs are structured (BSP-like); caves are organic (CA-like).

**For PixelEruv:** BSP for dungeons/interiors (reliable, produces good tile maps). Cellular automata for caves. Both output Tiled JSON tile layers, so they integrate with the existing map pipeline.

---

## 3. Target Architecture

### 3.1 Overview

```
                    ┌─────────────────────────────────────────┐
                    │              WorldSim (Go)               │
                    │                                         │
  Browser ──WS──>   │  ┌─────────┐  ┌──────────┐  ┌────────┐ │
  (Phaser 4)   │    │  │ AOIGrid │  │ ChunkMgr │  │ WorldGen│ │
               │    │  │ (spatial│  │ (per-map │  │ Engine  │ │
               │    │  │  hash)  │  │  chunks) │  │ (offline)│ │
               │    │  └────┬────┘  └────┬─────┘  └───┬────┘ │
               │    │       │            │             │      │
  Nginx ──> Pusher ──NATS── │ ──────────│─────────────│──────│
                    │       │            │             │      │
                    │  ┌────▼────────────▼─────────────▼───┐ │
                    │  │         Tick Loop (20Hz)          │ │
                    │  │  movement → AOI → zones → repl    │ │
                    │  └──────────────────────────────────┘ │
                    │                                         │
                    │  PocketBase (embedded) ── maps, players │
                    │  NATS ── messaging                      │
                    └─────────────────────────────────────────┘
```

### 3.2 AOI Grid (replaces same-map filter)

A uniform spatial hash grid per map:
```go
type AOIGrid struct {
    cellSize int  // tiles per cell (e.g. 16)
    cells    map[[2]int]*Cell  // sparse, keyed by (cellX, cellY)
}
type Cell struct {
    Entities map[string]*Entity  // entities whose center is in this cell
    Watchers map[string]*Entity  // clients subscribed to this cell
}
```

Each tick:
1. **Update**: for each entity that moved (`dirtyPosition`), remove from old cell, insert into new cell.
2. **Subscribe**: for each client, compute the set of cells in their AOI radius. Diff with previous subscription. Send `SpawnEntity` for entities in new cells, `DestroyEntity` for entities in old cells.
3. **Replicate**: for each client, iterate only entities in subscribed cells. Send dirty updates.

This replaces the O(N*M) loop in `replication.go:51-58` with O(N_local * M).

### 3.3 Chunk Manager (replaces full-map loading)

Maps are stored as chunks, not as a single Tiled JSON:
```go
type ChunkKey struct {
    CX, CY int  // chunk coordinates (can be negative)
}
type Chunk struct {
    Key     ChunkKey
    Layers  map[string][]uint32  // layer name → tile data (16x16)
    Objects []TiledObject        // entities, zones in this chunk
}
type ChunkedMap struct {
    ChunkSize int
    Chunks    map[ChunkKey]*Chunk
    BBox      image.Rectangle  // computed bounding box
}
```

**Loading**: `ParseTiledMapJSON` is extended to handle `infinite: true` by parsing `chunks` instead of `data`. For finite maps, the existing path is unchanged. The parser produces a `ChunkedMap` either way (finite maps = one big "chunk").

**Collision**: Phase 1 uses bounding-box dense grid (stamp all chunks into `[BBox.H][BBox.W]`). Phase 5 migrates to per-chunk collision lookup.

**Asset serving**: `GET /api/assets/maps/{name}` continues to return the full JSON for now (Phaser loads it all). Phase 5 adds `GET /api/assets/maps/{name}/chunks/{cx}/{cy}` for streaming.

### 3.4 WorldGen Engine (new, offline or on-demand)

A separate package (`backend/internal/worldgen`) that generates a complete world and outputs Tiled JSON + metadata:

```
worldgen.Generate(seed, params) → {
    TiledJSON []byte          // the map (infinite, chunked)
    Metadata  WorldMetadata   // biomes, sites, NPCs, history (stored in PB)
}
```

**Pipeline** (mirrors DF's phases, simplified for 2D pixel art):
1. Elevation (OpenSimplex2 fBm + ridged noise)
2. Temperature (latitude + noise)
3. Precipitation (noise + rain shadow)
4. Biomes (Whittaker lookup from temp × precip)
5. Rivers (downhill flow from elevation)
6. Settlements (placed at high-suitability locations)
7. Roads (A* between nearby settlements)
8. NPCs (Markov names, personality model, demographics)
9. History simulation (simplified: N years of events, producing legends/quests)
10. Export to Tiled JSON + PB metadata records

**Output**: The Tiled JSON is a standard infinite map that Phaser renders natively and the Chunk Manager parses. Metadata (biomes, sites, NPCs, history) goes into PocketBase collections (`world_regions`, `world_sites`, `world_npcs`, `world_events`). An `ext-worldgen` extension could own runtime worldgen triggers, but the core engine is a library.

### 3.5 Delta Compression (replication optimization)

Add baseline tracking to the replication system:
```go
type Entity struct {
    // ... existing fields ...
    baseline    map[string]*pb.Position  // clientID → last acked position
    ackSeq      map[string]uint32        // clientID → last acked snapshot seq
}
```

Each replication batch:
- For each entity in AOI: if position == baseline for this client, send 1 bit "unchanged." Else send full position + update baseline.
- Client sends ack of latest received snapshot seq.
- Server updates baseline to latest acked.

This requires a protocol change (new ack message from client, new "unchanged" bitfield in batch). The protobuf `ReplicationBatch` gains an `unchangedMask` field.

### 3.6 Bandwidth Budgeting

Per-client bandwidth budget (e.g. 100kbps):
```go
type ClientBudget struct {
    BudgetBytesPerTick int  // e.g. 100000/8/20 = 625 bytes/tick
    Used               int
}
```

Each tick, sort entities in AOI by priority (distance, gameplay relevance). Send updates until budget exhausted. Lower-priority updates deferred to next tick. Critical events (spawn, destroy, state change) always sent.

---

## 4. Blind Spots & Risk Register

| Risk | Impact | Mitigation |
|---|---|---|
| **AOI cell boundary thrash** | Players walking along cell borders cause rapid spawn/despawn storms | Hysteresis: subscribe radius < unsubscribe radius (already proven in proximity zones) |
| **Single global mutex** | Tick can't parallelize; at high entity counts, tick duration exceeds 50ms | Phase 6: shard the world into independent regions with separate mutexes. Not needed for hundreds of players. |
| **Delta compression baseline loss** | If ack is lost, server encodes against stale baseline → client can't decode | Client acks every tick; server keeps multiple baselines and falls back. Standard technique from Gaffer On Games. |
| **Chunk streaming on frontend** | Phaser has no built-in dynamic chunk load/unload | Phase 5: custom chunk manager in GameScene that creates/destroys tilemap layers per chunk. Non-trivial but bounded. |
| **Worldgen determinism** | Same seed should produce same world (for sharing/repro) | Use a seeded PRNG (math/rand/v2 with seed) throughout the pipeline. DF does this. |
| **Worldgen performance** | Full DF-style history simulation (250 years, week at a time) is CPU-intensive | Simplify: simulate at year granularity, not week. Limit histfigs to ~1000. Target <30s generation for a medium world. |
| **LLM narrative hallucination** | LLM-generated quest text contradicts world state | Use LLM only as text renderer (world state → prose), never as state generator. Validate all facts against PB records. |
| **Migration of existing maps** | Finite maps must still work after all changes | Every change branches on `infinite` flag. Finite maps hit the existing code path. Test with `maps/default-map.json` every phase. |
| **Replication protocol change** | Delta compression changes the wire format; breaks existing clients | Version the protocol. Phase 3 ships both old and new formats; frontend supports both until migration complete. |
| **PocketBase storage for worldgen metadata** | New collections (world_regions, world_sites, etc.) need migrations | Add Go migrations in `backend/migrations/` following the existing pattern. |
| **NPC entity scaling** | Thousands of NPC entities in the ECS could bloat replication | NPCs are NOT networked entities by default. They're metadata in PB. Only NPCs near a player are spawned as entities (on-demand, like DF's site generation). Despawned when no players are near. |
| **Zone system O(Z*E)** | With large maps, zone count grows; zone checks become expensive | Phase 2: index zones in the AOI grid (only check zones in the entity's cell). |

---

## 5. Implementation Phases

Each phase is independently shippable and testable. Phases build on each other but don't require later phases to be useful.

### Phase 1: Infinite Map Support (backend parser + bounding-box collision)

**Goal:** Load and play on Tiled infinite maps with working collision, spawning, and zones. Finite maps unchanged.

**Changes:**
1. `mapdata.go`: Add `Infinite bool` and `Chunks []chunk` to `tiledMapJSON` layer struct. When `infinite==true`, parse `chunks` instead of `data`. Compute bounding box from chunk extents. Stamp chunks into a dense collision grid. Set `MapData.Width/Height` to bounding box dimensions.
2. `integrity.go`: Allow `Width==0 && Height==0` when `Infinite==true` (skip the dimension check, validate bounding box instead).
3. `mapdata.go` `RandomSpawnPoint`: Use bounding-box center when `Infinite==true`.
4. Tests: add `TestParseTiledMapJSON_Infinite` with a small infinite map fixture.

**Verify:** `go test ./internal/worldsim/ -v` passes. Load an infinite test map via the asset API, confirm collision works (walk into a wall, get blocked), confirm spawn point works.

**No frontend changes needed** — Phaser already renders infinite maps.

### Phase 2: AOI Grid (spatial filtering for replication + zones)

**Goal:** Replace "replicate everything on the same map" with cell-based AOI filtering. This is the single highest-impact performance change.

**Changes:**
1. New file `aoi.go`: `AOIGrid` struct with `cellSize`, `cells map[[2]int]*Cell`. Methods: `Insert(entity)`, `Remove(entity)`, `Update(entity, oldPos, newPos)`, `CellsInRadius(pos, radius) []Cell`, `EntitiesInCells(cells) []*Entity`.
2. `worldsim.go`: Add `aoiGrids map[string]*AOIGrid` (one per map). Update on entity move (in `movement.Step` or a new post-movement step).
3. `replication.go`: Replace the same-map filter (`replication.go:93-97`) with AOI subscription. Each client has `subscribedCells map[[2]int]bool`. On tick, compute client's AOI cells, diff with subscription, send SpawnEntity/DestroyEntity for enter/exit, send updates only for entities in subscribed cells.
4. `zone_system.go`: Index zones by cell. `ZonesAtPoint` only checks zones in the entity's cell + neighbors.
5. Hysteresis: `aoiSubscribeRadius` (e.g. 3 cells) < `aoiUnsubscribeRadius` (e.g. 4 cells).
6. Tests: `TestAOI_BasicFiltering` (entity outside AOI not replicated), `TestAOI_BoundaryCrossing` (spawn on enter, destroy on exit), `TestAOI_Hysteresis` (no thrash at boundary).

**Verify:** Integration test with 50 entities, confirm a client only receives replication for entities within AOI radius. Confirm no spawn/despawn storms when walking along a cell boundary.

### Phase 3: Delta Compression (reduce per-entity bandwidth)

**Goal:** Send "unchanged" bits instead of full position for stationary entities. Reduces bandwidth ~5-10x.

**Changes:**
1. `replication.go`: Add baseline tracking per (entity, client). `Entity.baseline map[string]*pb.Position`, `Entity.ackSeq map[string]uint32`.
2. Proto: Add `AckSnapshot` message (client→server) and `unchangedEntityIds []string` field to `ReplicationBatch`.
3. `replication.go`: In `replicateToClient`, for each entity in AOI: if position equals baseline (within epsilon), add to `unchangedEntityIds` (1 bit each in a bitfield). Else send full position + update baseline.
4. Frontend: Send `AckSnapshot` after processing each batch. Render unchanged entities at their last known position (interpolation handles this).
5. Tests: `TestReplication_DeltaCompression` (stationary entity = 1 bit, moving entity = full update), `TestReplication_BaselineFallback` (lost ack → server uses older baseline).

**Verify:** Bandwidth measurement test: 100 entities, 80% stationary, confirm batch size < 20% of uncompressed.

### Phase 4: WorldGen Engine — Terrain & Biomes

**Goal:** Procedurally generate a playable infinite map with terrain, biomes, rivers, and basic settlements. Output as Tiled JSON loadable by the existing pipeline.

**Changes:**
1. New package `backend/internal/worldgen/`:
   - `noise.go`: OpenSimplex2 implementation (or vendored library — check Go ecosystem first).
   - `terrain.go`: Elevation (fBm), temperature (latitude + noise), precipitation (noise + rain shadow), biome classification (Whittaker lookup).
   - `rivers.go`: Downhill flow simulation from elevation.
   - `settlements.go`: Place settlements at suitable locations (near water, habitable biome). Generate building layouts (BSP or templates).
   - `roads.go`: A* pathfinding between nearby settlements, avoiding water/mountains.
   - `export.go`: Convert generated world to Tiled JSON (infinite map with chunks). Each biome → tileset mapping.
2. New PocketBase migrations: `world_regions`, `world_sites` collections.
3. New command `backend/cmd/worldgen/`: CLI that takes a seed + params, generates a world, outputs Tiled JSON + populates PB.
4. Tests: `TestWorldGen_Determinism` (same seed = same output), `TestWorldGen_BiomeDistribution` (no desert at pole, no tundra at equator).

**Verify:** Generate a 200x200 world, load it in the game (via PB), walk around, confirm biomes look right, rivers flow, settlements are reachable.

### Phase 5: Chunk Streaming + Per-Chunk Collision

**Goal:** Stream map chunks to the frontend dynamically (load/unload based on camera position). Support truly huge maps (10000x10000+) without loading everything at once.

**Changes:**
1. `asset_http.go`: Add `GET /api/assets/maps/{name}/chunks/{cx}/{cy}` — returns a single chunk's tile data + objects.
2. `mapdata.go`: Migrate collision from bounding-box dense grid to per-chunk storage (`map[ChunkKey]*ChunkCollision`). `IsBlocked` looks up the chunk, then indexes within it.
3. Frontend `mapLoader.ts`: Add `ChunkStreamManager` — tracks camera position, requests chunks within render radius, disposes chunks beyond unload radius.
4. Frontend `GameScene.ts`: Create/destroy Phaser tilemap layers per chunk. Pool sprites for entities in unloaded chunks.
5. NATS: Server pushes chunk-load/unload events to client as it moves (server-push-on-move pattern, matching Minecraft).
6. Tests: `TestChunkStreaming_LoadUnload` (chunks load when entering radius, unload when leaving), `TestPerChunkCollision` (blocked tile in chunk A, walkable in chunk B).

**Verify:** Generate a 2000x2000 world, connect with a client, confirm only nearby chunks load. Walk far enough to trigger chunk loading/unloading. Confirm memory stays bounded.

### Phase 6: NPCs, History Simulation & Emergent Quests

**Goal:** Populate the world with NPCs that have personalities, names, and histories. Generate a simplified history during worldgen. NPCs share rumors derived from history. Quests emerge from world state.

**Changes:**
1. `worldgen/npcs.go`: Markov chain name generator (per-culture training sets). Personality model (5-10 traits, 0-100). Demographics (age, profession weighted by settlement type).
2. `worldgen/history.go`: Simplified history simulation — N years at year granularity (not week). Events: wars, births, deaths, migrations, site founding/abandonment, beast attacks. Track histfigs (limit ~1000). Produce event log.
3. New PB collections: `world_npcs`, `world_events`, `world_artifacts`.
4. `worldgen/quests.go`: Quest templates ("slay beast X at site Y", "deliver artifact Z to site W") filled from world state. NPCs share rumors referencing events.
5. NPC entities: spawned on-demand when a player enters a settlement's chunk (like DF's site generation). Despawned when no players are near. NPC personality affects dialogue.
6. `ext-worldgen` extension (optional): triggers worldgen from admin action, manages world rotation.
7. Tests: `TestHistorySim_ProducesEvents`, `TestQuestGen_FillsTemplate`, `TestNPC_OnDemandSpawn`.

**Verify:** Generate a world with 200 years of history. Visit a town, talk to an NPC, receive a rumor referencing a real historical event. Accept a quest, complete it, confirm the world state reflects completion.

### Phase 7: Sharding & Horizontal Scaling (future)

**Goal:** Run multiple worldsim processes, each owning a region of the world. Cross-region handoff for players crossing boundaries.

**This is explicitly deferred.** At hundreds of players, a single worldsim with AOI filtering (Phase 2) + delta compression (Phase 3) is sufficient. Sharding is needed only at thousands of concurrent players. The AOI grid architecture (Phase 2) makes future sharding easier because the spatial partitioning is already in place — a shard boundary is just a set of cells owned by different processes.

**When needed:**
- Split the world into regions (groups of AOI cells).
- Each region runs in a separate worldsim process.
- Players crossing a region boundary are handed off (state transfer via NATS).
- Cross-region visibility: entities near a boundary are visible to clients on both sides (the edge problem, section 2.1).
- This mirrors Eve Online's SOL server architecture (section 2.3).

---

## Appendix: Source Index

### MMORPG Architecture
- [MMO AOI Algorithm (DEV Community)](https://dev.to/aceld/11-mmo-online-game-aoi-algorithm-l7d)
- [Zinx framework AOI nine-grid](https://aceld.gitbooks.io/zinx/content/)
- [AOI performance analysis (nine-grid vs cross-linked-list)](https://wiki.disenone.site/en/game-%E6%B8%B8%E6%88%8FAOI%E7%AE%97%E6%B3%95%E8%A7%A3%E6%9E%90%E5%92%8C%E6%80%A7%E8%83%BD%E5%AE%9E%E6%B5%8B/)
- [Comparing Interest Management Algorithms (NUS)](https://www.comp.nus.edu.sg/~cs4344/0607s1/netgames06/s01Conf96_a32.pdf)
- [Gaffer On Games — Snapshot Interpolation](https://gafferongames.com/post/snapshot_interpolation/)
- [Gaffer On Games — Snapshot Compression](https://gafferongames.com/post/snapshot_compression/)
- [GDC Vault — Overwatch Gameplay Architecture and Netcode](https://www.gdcvault.com/play/1024001/-Overwatch-Gameplay-Architecture-and)
- [GDC Vault — Networking Scripted Weapons and Abilities in Overwatch](https://www.gdcvault.com/play/1024041/Networking-Scripted-Weapons-and-Abilities)
- [Overwatch Developer Update: Netcode](https://www.youtube.com/watch?v=vTH2ZPgYujQ)
- [Overwatch Developer Update: High Bandwidth](https://www.youtube.com/watch?v=EqtNUFxgm38)
- [Eve Online — Single-Shard Architecture](https://www.gamedeveloper.com/design/infinite-space-an-argument-for-single-sharded-architecture-in-mmos)
- [Eve Online — The Eve Cluster](https://www.eveonline.com/news/view/the-eve-cluster)
- [Eve Online — Gridlock, Monikers, and CPU-per-user](https://www.eveonline.com/news/view/gridlock-monikers-and-cpu-per-user-1)
- [Eve Online — Tranquility Tech IV](https://www.eveonline.com/news/view/tranquility-tech-iv)
- [Tiled JSON Map Format](https://doc.mapeditor.org/en/stable/reference/json-map-format/)
- [Tiled TMX Map Format](https://doc.mapeditor.org/en/stable/reference/tmx-map-format/)
- [Tiled GitHub #1733 — Chunked tile layer data](https://github.com/bjorn/tiled/issues/1733)
- [Minecraft Protocol FAQ (wiki.vg)](https://wiki.vg/Protocol_FAQ)
- [Minecraft Chunk Format (wiki.vg)](https://wiki.vg/Chunk_Format)
- [Roblox Instance Streaming](https://create.roblox.com/docs/en-us/workspace/streaming)
- [Roblox Streaming Techniques](https://create.roblox.com/docs/en-us/workspace/streaming/techniques.md)
- [Spatial hash vs quadtree (GameDev SE)](https://gamedev.stackexchange.com/questions/69776/when-is-a-quadtree-preferable-over-spatial-hashing)
- [Comparative analysis of spatial partitioning (WSCG 2013)](http://wscg.zcu.cz/wscg2013/program/short/D71-full.pdf)
- [Sparse Spatial Hash Grid (DigiStar)](https://queelius.github.io/sparse_spatial_hash/)

### Procedural Generation & Dwarf Fortress
- [DF Wiki — Advanced World Generation](https://dwarffortresswiki.org/index.php/Advanced_world_generation)
- [DF Wiki — World Generation](https://dwarffortresswiki.org/index.php/World_generation)
- [DF Wiki — Legends](https://dwarffortresswiki.org/index.php/Legends)
- [DF Wiki — Historical Figure](https://dwarffortresswiki.org/index.php/Historical_figure)
- [DF Wiki — World History File](https://dwarffortresswiki.org/index.php/World_History_file)
- [DF Wiki — Thoughts and Preferences](https://dwarffortresswiki.org/index.php/Thoughts_and_Preferences)
- [DF Wiki — Personality Facet](https://dwarffortresswiki.org/index.php/Personality_trait)
- [DF Wiki — Personality Value](https://dwarffortresswiki.org/index.php/Personality_value)
- [DF Wiki — Site](https://dwarffortresswiki.org/index.php/Site)
- [DF Wiki — 40d:Region](https://dwarffortresswiki.org/index.php/40d:Regions)
- [DFHack Maps API](https://docs.dfhack.org/en/53.07-r1/docs/api/Maps.html)
- [Polygon — Dwarf Fortress will crush your CPU (Tarn Adams interview)](https://www.polygon.com/2014/7/23/5926447/dwarf-fortress-will-crush-your-cpu-because-creating-history-is-hard/)
- [Wikipedia — Tarn Adams](https://en.wikipedia.org/wiki/Tarn_Adams)
- [OpenSimplex Noise — Wikipedia](https://en.m.wikipedia.org/wiki/OpenSimplex_noise)
- [Simplex Noise — Wikipedia](https://en.wikipedia.org/wiki/Simplex_noise)
- [Perlin Noise — Wikipedia](https://en.wikipedia.org/wiki/Perlin_noise)
- [Whittaker Biome Diagram Guide](https://gveg.wyobiodiversity.org/application/files/7916/4641/2117/Whittaker_Diagram_Guide.pdf)
- [Nature Scientific Data — Whittaker Biome Diagram](https://www.nature.com/articles/s41597-025-04387-0/figures/2)
- [Particle-Based Hydraulic Erosion (IEEE)](https://ieeexplore.ieee.org/document/11482049)
- [Hydraulic Erosion Implementation (holzman.dev)](http://holzman.dev/articles/2023/12/01/hydraulic_erosion.html)
- [Parish & Müller — Procedural Modeling of Cities (2001)](https://people.eecs.berkeley.edu/~sequin/CS285/PAPERS/Parish_Muller01.pdf)
- [CityEngine — Rule-Based Modeling (Springer)](https://link.springer.com/chapter/10.1007/978-981-15-8983-6_35)
- [BSP vs Cellular Automata Dungeon Generation (Umeå thesis)](http://urn.kb.se/resolve?urn=urn%3Anbn%3Ase%3Aumu%3Adiva-243697)
- [Procedural Level Generation Guide (Jaconir)](https://jaconir.online/blogs/procedural-level-generation-guide)
- [Markov Name Generator (GitHub)](https://github.com/Tw1ddle/MarkovNameGenerator)
- [VNameGenerator (GitHub)](https://www.github.com/Valkryst/VNameGenerator)
