# Sprite Selection (Phase 1: Base Sheet)

Date: 2026-07-07
Branch: `feat/sprite-selection` (to be created)

## Goal

Let a logged-in player choose their character spritesheet from a
PocketBase-managed catalog, persist that choice across reconnects, and see it
applied at spawn. Guests keep the existing hash-based fallback. Phase 1 is
base-sheet selection only; palette recolor and accessory overlays are deferred
to phase 2.

## Background

Today the server assigns `SpriteIndex = hash(entityID) % 5` at provision time
(`worldsim.go:866-870`), sends it in the Appearance component, and the frontend
renders one of five hardcoded `char_0..char_4` sheets. The index is
deterministic per entity ID, but the player cannot choose it, and the catalog
is hardcoded in the frontend.

## Scope

**Phase 1 (this design):**
- PB collection `sprite_bases` holds the catalog (admin-managed).
- `players.sprite_base` stores the chosen sheet ID.
- Pre-join profile screen (Phaser scene) for logged-in users to pick a sheet.
- New `SetSpriteBaseFrame` client→server frame; worldsim persists + replicates
  the change live (hot-swap on all clients).
- Auto-seed `sprite_bases` from a bundled directory on worldsim's first run.
- Standalone `cmd/seed-sprites` CLI for adding sheets later.

**Phase 2 (deferred, separate design):**
- In-game mirror object to re-open the chooser mid-session.
- Palette recolor + accessory overlays (the "recipe").
- Per-player customization of pixels.

## Data model

### New PB collection: `sprite_bases`

Migration `pb_migrations/1751900000_create_sprite_bases.js`, mirroring
`1751700000_create_maps.js`:

```js
migrate((app) => {
  const collection = new Collection({
    name: "sprite_bases",
    type: "base",
    fields: [
      { name: "name", type: "text", required: true, min: 1, max: 100 },
      { name: "sheet", type: "file", required: true, maxSelect: 1,
        maxSize: 1048576, mimeTypes: ["image/png"] },
    ],
    listRule: "",   // public read (frontend catalog fetch, anonymous)
    viewRule: "",
    createRule: null,  // admin-only
    updateRule: null,
    deleteRule: null,
  });
  return app.save(collection);
}, (app) => {
  const c = app.findCollectionByNameOrId("sprite_bases");
  return app.delete(c);
});
```

Each record: `id` (PB auto), `name` (string, e.g. `char_0`), `sheet` (file
field, the 768×192 PNG, same layout as today's `char_N.png` per
`documentation/22-limezu-sprites.md`).

### Existing `players` collection: new field

Migration `pb_migrations/1752000000_add_sprite_base_to_players.js`:

```js
migrate((app) => {
  const c = app.findCollectionByNameOrId("players");
  c.fields.add(new TextField({ name: "sprite_base", required: false }));
  return app.save(c);
}, (app) => {
  const c = app.findCollectionByNameOrId("players");
  c.fields.removeByName("sprite_base");
  return app.save(c);
});
```

`sprite_base` is a plain text field holding a `sprite_bases` record ID (not a
PB relation — matches how `entity_id` is stored today). Empty = unset (use
fallback).

### Proto: `Appearance` component

`proto/components.proto`:

```proto
message Appearance {
  uint32 gid = 1;
  reserved 2;              // was sprite_index
  string sprite_base = 3;  // sprite_bases record ID; empty for props / guests
}
```

`reserved 2` keeps wire compatibility with any in-flight frames from older
binaries during a rolling deploy.

### Proto: new client→server frame

`proto/frames.proto`, mirroring `SetNameFrame`:

```proto
// SetSpriteBaseFrame is a client request to change their character sheet. The
// server validates the sprite_base ID exists in the sprite_bases collection,
// updates Entity.SpriteBase, marks it dirty for replication, and persists to
// PocketBase for logged-in users. Guests are rejected (no PB record).
message SetSpriteBaseFrame {
  string sprite_base = 1;   // sprite_bases record ID; empty = revert to fallback
  string traceparent = 2;
}
```

Add `set_sprite_base = 7;` to the `ClientFrame` oneof.

## Server flow

### `UserRecord` and `UserStore`

`userstore.go`: add `SpriteBase string \`json:"sprite_base"\`` to `UserRecord`.
Add `UpdateSpriteBase(entityID, spriteBase string) error` mirroring
`UpdateDisplayName` (PATCH `players` record).

### `Entity` struct

`worldsim.go`: rename `SpriteIndex uint32` → `SpriteBase string`. Add
`dirtyAppearance bool` alongside the existing `dirtyName`/`dirtyState`/
`dirtyPosition` dirty flags.

### Provision (`handleConnect`, ~line 466-477)

Replace `SpriteIndex: spriteIndexForEntity(entityID)` with
`SpriteBase: user.SpriteBase`. Delete `spriteIndexForEntity` (~line 866-870)
and `charSpriteCount` (~line 37-41) — no longer used server-side.

### New handler `handleSetSpriteBase`

Mirrors `handleSetName` (~line 809-855):

1. Look up entity by clientID. If not found, return.
2. If `s.spriteStore != nil`: validate `frame.SpriteBase` exists via
   `BaseExists`. If not, reject (log + return, no entity update).
3. Set `entity.SpriteBase = frame.SpriteBase`, `entity.dirtyAppearance = true`.
4. If `s.userStore != nil`: persist via `UpdateSpriteBase(entityID, ...)`.
   Guests have no PB record → no-op.

### NATS subscription

Add `client.*.set_sprite_base` subscription mirroring the `set_name` one
(~line 346-362), unmarshaling `SetSpriteBaseFrame` and dispatching to
`handleSetSpriteBase`.

### Replication

In the dirty-flag replication block (~line 1055-1083), add a `dirtyAppearance`
branch mirroring `dirtyName`:

```go
if e.dirtyAppearance {
    appBytes, _ := proto.Marshal(&pb.Appearance{
        Gid:        e.Gid,
        SpriteBase: e.SpriteBase,
    })
    batch.Updates = append(batch.Updates, &pb.UpdateComponent{
        EntityId:    e.ID,
        ComponentId: compAppearance,
        Data:        appBytes,
        SnapshotSeq: s.snapshotSeq,
    })
}
```

Clear `e.dirtyAppearance = false` in the reset block (~line 950-952).

The existing spawn-time Appearance marshal (~line 1034-1038) uses the new
`SpriteBase` field instead of `SpriteIndex`.

## Frontend

### Catalog fetch: `spriteLoader.ts`

New module mirroring `mapLoader.ts`. Fetches `sprite_bases` records from PB
(anonymous read, same `PB_URL` derivation), returns `{ id, name, url }[]`
where `url` = `${PB_URL}/api/files/<collectionId>/<id>/<filename>`. Called in
`bootstrap()` (`main.ts`) alongside `loadMapAssets()`, stashed in
`game.registry.set("spriteBases", ...)`. On failure, falls back to the static
`char_0..char_4` files (existing behavior).

### Pre-join chooser: `CharacterSelectScene`

New Phaser scene inserted before `GameScene` in the scene list. Shown only
when `isLoggedIn()` and the player's `sprite_base` is unset (checked by
fetching the `players` record, or by a server-provided flag in
`AuthResultFrame` — simplest is a separate PB read in `bootstrap()`).

UI: a row of preview thumbnails (one per `sprite_bases` record), click to
select, a "Confirm" button. On confirm: `wsClient.sendSetSpriteBase(chosenId)`,
then transition to `GameScene`. The live `UpdateComponent` will hot-swap the
player's own avatar; no page reload needed.

Re-openable from `TopMenu` (a "Character" entry) — in phase 1 this re-runs the
select scene; the live-update path means the avatar hot-swaps without a
reload. (The in-game *mirror object* is phase 2; the menu entry is phase 1.)

Guests skip the scene entirely and go straight to `GameScene`.

### Render path: `GameScene`

**`preload()`** (~line 462-494): after the existing static-sheet load, iterate
`spriteBases` and `this.load.spritesheet(base.id, base.url, { frameWidth:
FRAME_W, frameHeight: FRAME_H })` for each — keyed by PB record ID.

**`create()`** animation registration (~line 645): iterate `spriteBases` in
addition to (or instead of) `CHAR_SPRITES` so walk/idle animations are
registered for PB-backed sheets, keyed by PB record ID.

**`handleReplication` spawn** (~line 941-988): replace the `spriteIndex` logic:

```ts
} else if (comp.componentId === 3) {
  const appearance = fromBinary(AppearanceSchema, comp.data);
  gid = appearance.gid;
  spriteBase = appearance.spriteBase;  // new field
}
...
const charKey = spriteBase
  ? spriteBase                              // PB record ID, loaded in preload
  : CHAR_SPRITES[spriteIndexForEntity(spawn.entityId)];  // guest fallback
```

Animation keys are already parameterized by `charKey` (~line 917-918), so the
only change is `charKey` is now a PB ID instead of a `char_N` string.

**`handleReplication` update** (~line 1026-1077): add `else if
(upd.componentId === 3)` that decodes Appearance, and if `sprite_base` differs
from the avatar's current `charKey`, calls
`avatar.sprite.setTexture(newKey, DIR_FRAME_START[avatar.dir])` and updates
`avatar.charKey`. ~10 lines, mirroring the DisplayName update handler.

### `WsClient`

Add `sendSetSpriteBase(id: string)` mirroring `sendSetName` (whatever that
pattern is).

## Seeding spritesheets into PB

### `spritestore.go`

New file `backend/internal/worldsim/spritestore.go`, mirroring `userstore.go`'s
structure (same `authToken`/`doRequest` pattern, same superuser creds):

```go
type SpriteStore struct {
    pocketbaseURL string
    adminEmail    string
    adminPassword string
    token         string
    mu            sync.Mutex
}

type SpriteBase struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

// ListBases returns all sprite_bases records.
func (s *SpriteStore) ListBases() ([]SpriteBase, error) { ... }

// BaseExists checks if a sprite_bases record ID exists. Used by
// handleSetSpriteBase for validation.
func (s *SpriteStore) BaseExists(id string) (bool, error) { ... }

// SeedIfEmpty uploads every PNG in dir as a sprite_bases record, but only if
// the collection is currently empty. Idempotent: no-op if any records exist.
// Called once at worldsim startup.
func (s *SpriteStore) SeedIfEmpty(dir string) error { ... }

// Seed uploads PNGs from dir. With force=false it's equivalent to SeedIfEmpty.
// With force=true it uploads every PNG, skipping per-file if a record with
// that name already exists. Used by the cmd/seed-sprites CLI.
func (s *SpriteStore) Seed(dir string, force bool) error { ... }
```

### worldsim startup

In the constructor (~line 155-160), alongside `NewUserStore`:

```go
spriteStore := NewSpriteStore(pocketbaseURL, pbAdminEmail, pbAdminPassword)
spritesDir := os.Getenv("SPRITES_DIR")
if spritesDir == "" {
    spritesDir = "./sprites"  // bundled alongside the binary in dist/
}
if err := spriteStore.SeedIfEmpty(spritesDir); err != nil {
    s.logger.WarnContext(ctx, "sprite seed failed", "err", err)
    // non-fatal — frontend falls back to static sheets
}
```

Non-fatal on failure: if PB is down or seeding fails, worldsim still starts.
The frontend's static-sheet fallback covers it.

### `cmd/seed-sprites/main.go`

~40 lines. Parses `-dir` (default `./sprites`) and `-force` (default false),
constructs `SpriteStore` from env vars (`POCKETBASE_URL`, `PB_ADMIN_EMAIL`,
`PB_ADMIN_PASSWORD`), calls `Seed(dir, force)`. Follows the existing
`cmd/worldsim`, `cmd/ext-walls`, `cmd/ext-av` layout.

### Bundling

`make dist-*` already stages `web/sprites/` into `dist/`. The seed reads from
the same directory; `SPRITES_DIR` defaults to `./sprites/` relative to the
worldsim binary. No new packaging step.

## Testing (ginkgo)

### Updated tests (`worldsim_sprite_test.go`)

- `TestSpriteIndex_Deterministic` → **delete**. The deterministic-hash
  property moves to the frontend-only (already covered by
  `spriteIndexForEntity` in `GameScene.ts`).
- `TestReplication_SpawnIncludesSpriteIndex` → rewrite as
  `TestReplication_SpawnIncludesSpriteBase`: set `e.SpriteBase = "sb_abc"`,
  tick, assert the spawned Appearance component has `app.SpriteBase == "sb_abc"`.
- `TestReplication_SpawnAlwaysIncludesAppearanceForPlayers` → keep the intent
  (Appearance always sent for players) but drop the "find an entity ID with
  index 0" logic. Assert the Appearance component is present on a player
  spawn with an empty `SpriteBase` (the guest case).

### `addPlayer` helper (`worldsim_chat_test.go:51-66`)

Change `SpriteIndex: spriteIndexForEntity(entityID)` to `SpriteBase: ""` (or
accept a `spriteBase` param). Update all callers in lockstep.

### New tests

1. `TestSetSpriteBase_UpdatesEntityAndReplicates`: publish a
   `SetSpriteBaseFrame` on `client.<id>.set_sprite_base`, tick, assert (a)
   `entity.SpriteBase` changed, (b) a `UpdateComponent` with `componentId=3`
   and the new `SpriteBase` was sent to other clients. Mirrors the
   replication-subscription pattern in `worldsim_sprite_test.go:49-66`.

2. `TestSetSpriteBase_GuestRejected`: a guest entity sends the frame; assert
   `entity.SpriteBase` unchanged and no `UpdateComponent` emitted. (UserStore
   is nil in `newChatTestSim`, so persistence is naturally a no-op.)

3. `TestSetSpriteBase_InvalidIDRejected`: send a `sprite_base` ID that doesn't
   exist; assert rejected. Since `SpriteStore` is nil in the test sim, the
   handler should skip validation when `spriteStore == nil` (matching how
   `handleSetName` no-ops persistence when `userStore == nil`). Document this;
   add a separate test with a fake store for the validation path.

4. `TestSeedIfEmpty_EmptySeeds`: empty collection → seeds N records.
5. `TestSeedIfEmpty_NonEmptyNoOps`: non-empty collection → no-op.
6. `TestSeed_ForceSkipsExisting`: `force=true` → uploads new PNGs, skips
   existing names.
7. `TestBaseExists_TrueForRealFalseForBogus`: existence check.

## Verification (definition of done)

- [ ] `go build ./...` passes (including `cmd/seed-sprites`).
- [ ] `go test ./backend/internal/worldsim/...` passes (updated + new tests).
- [ ] Proto recompile (`buf generate` or equivalent) succeeds; `reserved 2`
      doesn't break wire compat.
- [ ] Frontend `tsc` passes; `vite build` succeeds.
- [ ] PB migrations run clean on fresh DB; `sprite_bases` collection visible
      in admin UI.
- [ ] Manual: fresh `make up` with empty PB → `sprite_bases` populated from
      bundled `sprites/`, frontend catalog shows all sheets.
- [ ] Manual: log in → see pre-join chooser → pick a sheet → spawn with it →
      reconnect → same sheet.
- [ ] Manual: guest connects → no chooser → spawns with hash-fallback sprite.
- [ ] Manual: open chooser from TopMenu mid-session → pick new sheet → avatar
      hot-swaps live for self and other clients.
- [ ] Manual: drop a new PNG into `dist/sprites/`, run
      `./dist/bin/seed-sprites -dir dist/sprites -force` → new sheet appears
      in catalog without re-seeding existing ones.

## Risks / notes

- **Preload ordering**: PB-backed sheets must finish loading before the first
  replication batch arrives, or the first `setTexture` will fail. The existing
  map-asset preload-before-Phaser pattern (`bootstrap()` in `main.ts`) handles
  this for maps; the same approach works for sprites (fetch catalog in
  `bootstrap`, load textures in `preload`).
- **Animation key registration**: PB-backed sheets need walk/idle animations
  registered in `create()`, keyed by PB record ID. If a sheet is in PB but the
  catalog fetch hasn't completed, animations will be missing — so catalog
  fetch must complete in `bootstrap()` before `GameScene` starts.
- **`sprite_bases` API rules**: public read (frontend fetches anonymously like
  `maps` today), admin-only writes. Documented in the migration.
- **Validation when `spriteStore == nil`**: in tests (and any deployment
  without PB configured), `handleSetSpriteBase` skips existence validation,
  matching `handleSetName`'s no-op-when-`userStore`-nil pattern. Documented.
- **`char_5.png` is malformed** (per `documentation/22-limezu-sprites.md`):
  exclude it from the bundled seed directory, or fix it upstream. The seed
  uploads whatever PNGs are in `SPRITES_DIR`, so the simplest fix is to not
  ship `char_5.png` in `dist/sprites/`.

## Phase 2 (deferred)

- In-game mirror object to re-open the chooser without a menu trip.
- Palette recolor + accessory overlays (the "recipe"): `players.appearance`
  becomes a JSON recipe `{ base, palette, accessories }`; `Appearance` proto
  gains `recipe_json`; frontend composites the sheet client-side from base +
  recipe.
- Per-player pixel customization (full per-player sheet blob).
