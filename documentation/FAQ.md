# FAQ

Common questions about designing maps, configuring the stack, and building
extensions for PixelEruv. For full reference, see the linked docs.

---

## Map design

### How do I design a teleport from one map to another?

Create a **portal zone** on the source map and a **spawn zone** (or a beacon
entity) on the destination map.

**On the source map** (in Tiled):

1. Select the "Zones" object layer. Draw a rectangle over the portal area
   (e.g. a doorway).
2. Name the zone (e.g. `portal-to-map2`).
3. Add custom properties:
   - `zone_type` = `portal`
   - `target_map` = `map2` (must match a `maps` record)

**On the destination map** (`map2`):

- Add at least one `zone_type=spawn` zone. The player will appear at a
  random spawn zone.
- **Or**, if you want the player to appear at a specific spot, place a
  base entity on the "Entities" layer with a name (e.g.
  `door-entrance-north`), then set `target_entity` =
  `door-entrance-north` on the portal zone. The player teleports to that
  entity's position.

Upload both maps to PocketBase. No extension code
needed — the kernel handles transitions directly.

See: [Map Design Guide § Portal](21-map-design-guide.md#how-to-create-a-portal-map-transition)

### How do I make walls that block movement?

Two options, can be combined:

- **Walls tile layer**: draw wall tiles on a tile layer named "Walls". Any
  non-zero tile = blocked. This is the fallback collision grid.
- **Wall zones**: draw rectangles on the "Zones" layer with `zone_type =
  wall`. The ext-walls extension registers block gate triggers for these.
  Supports sub-tile precision (continuous-space swept collision).

Keep the Walls tile layer as a safety net even if you use wall zones.

See: [Map Design Guide § Walls](21-map-design-guide.md#walls-tile-layer-optional-fallback)

### How do I place an interactive object (box, lever, light switch)?

Use the "Entities" object layer. Drag a tile from the tileset onto the
layer to create a tile-object with a `gid` (sprite). Name the object
(this becomes the `entity_id`). Add `entity_type` (e.g. `light_switch`)
and optionally `owner_extension` and `trigger_radius`. An extension like
ext-props claims it and handles interaction when a player presses E
within `trigger_radius`.

See: [Map Design Guide § Entities](21-map-design-guide.md#entities-object-layer-optional)

### Where do new players appear?

At a random `zone_type=spawn` zone on the default map (configured by
the `DEFAULT_MAP` env var, default `main`).
If no spawn zone exists, the fallback is (10, 10). You can have multiple
spawn zones — worldsim picks one at random each time.

### How do I add a second map?

1. Design the map in Tiled, export as JSON.
2. In the PocketBase admin GUI (`http://localhost:8090/_/`), create a new
   record in the `maps` collection:
   - `name`: the map name (e.g. `map2`)
   - `tiled_json`: upload the JSON
   - `tilesets`: upload the PNGs
3. Add portal zones on either map to let players travel between them.

Worldsim loads all maps from PocketBase on startup. No restart
needed if you use the PB admin GUI — the map reload hook picks up changes.

### My map edits don't show up in the game. Why?

You probably edited the file in the repo but didn't upload it to
PocketBase. The game reads maps from PB, not from the filesystem (except
on first-run auto-seed). Re-upload via the admin GUI.

### Why is my "Zones" or "Entities" layer being ignored?

It must be an **object layer** (`objectgroup`), not a tile layer. Delete
the tile layer and create an object layer with the same name.

---

## Configuration

### What environment variables do I need to set?

The critical ones for a basic setup:

| Variable | Default | Purpose |
|---|---|---|
| `DEFAULT_MAP` | `main` | Name of the default map record; new players spawn here |
| `NATS_URL` | `nats://nats:4222` | NATS connection string |
| `PB_DATA_DIR` | `./pb_data` | PocketBase data directory |
| `PUBLIC_HOST` | `localhost` | Hostname for TLS cert and Dex redirect |
| `LIVEKIT_API_SECRET` | (dev placeholder) | Must match `docker/livekit.yaml` |

See: [Quick Start § Env vars](quick-start.md#3d-environment-variables-reference)

### How do I change which map is the default for new players?

Set the `DEFAULT_MAP` env var to the map name you want. New players will
spawn on that map at a random `spawn` zone.

---

## Backup and restore

### How do I back up PocketBase data?

Two ways, depending on what you need:

- **Volume snapshot** (fastest, full fidelity): stop worldsim, copy the whole
  `PB_DATA_DIR`. In Docker:
  ```bash
  docker compose down
  docker run --rm -v pixeleruv_pb_data:/d -v "$PWD":/b alpine tar czf /b/pb_data.tgz /d
  docker compose up -d
  ```
- **`pb-collections` export** (portable, plain JSON): exports all app
  collections — schema, records, and file fields — into a directory you can
  inspect or move between hosts:
  ```bash
  make build   # produces dist/bin/pb-collections
  PB_DATA_DIR=./pb_data ./dist/bin/pb-collections -export ./pb_backup
  ```

The volume snapshot is the right default for routine backups. Use
`pb-collections` when you want a portable format, a selective restore, or to
reproduce a production bug on a dev machine.

See: [Backup and Restore](24-backup-and-restore.md)

### How do I restore a backup?

**Volume snapshot:**
```bash
docker compose down
docker run --rm -v pixeleruv_pb_data:/d -v "$PWD":/b alpine \
  sh -c "rm -rf /d/* && tar xzf /b/pb_data.tgz -C /"
docker compose up -d
```

**`pb-collections` import** (into a fresh or existing data dir):
```bash
PB_DATA_DIR=./pb_data ./dist/bin/pb-collections -import ./pb_backup
# Add -force to wipe existing records before import.
```

Re-importing without `-force` is idempotent — records that already exist (by
ID) are skipped.

See: [Backup and Restore](24-backup-and-restore.md#import)

### Can I run `pb-collections` while worldsim is running?

No. SQLite is single-writer and concurrent access will corrupt the database.
Stop worldsim first, or point `pb-collections` at a copy of the `pb_data`
directory.

---

## Development

### How do I build and test?

```bash
make proto    # generate protobuf (required before first build)
make build    # build all Go binaries into dist/bin/
cd backend && go test ./internal/worldsim/ -v   # unit tests (no Docker)
```

See: [AGENTS.md](../AGENTS.md) for full build/test instructions.

### The build fails with "package pb not found". Why?

You need to run `make proto` first. The `backend/internal/pb/` directory
is gitignored — the `.pb.go` files are generated from `proto/*.proto`.

### How do I add a new extension?

1. Create a new service that connects to NATS.
2. Subscribe to `worldsim.ready` to get zone metadata.
3. Publish to `extension.<your-id>.register` with your extension ID,
   heartbeat interval, and trigger registrations.
4. Subscribe to your trigger subjects (e.g. `extension.<your-id>.trigger`).

See: [Extensions doc](18-extensions.md)

### How do I teleport a player from an extension?

Publish to the `worldsim.entity.teleport` NATS subject:

```json
{
  "entity_id": "e_abc",
  "map_id": "map2",
  "target_entity": "door-north"
}
```

`target_entity` is optional. If omitted, the player spawns at a random
spawn zone on the target map.

---

## Troubleshooting

### Black screen with `crypto.subtle` error

You're accessing over plain HTTP from a remote browser. Auth needs a
secure context. Use the HTTPS endpoint (`https://<host-ip>:4043`) or
browse from `localhost` on the host.

### A/V connects but no media flows

`LIVEKIT_NODE_IP` is wrong, or UDP ports 50000-50020 are blocked by a
firewall. Check ICE candidates in the browser's WebRTC internals.

### `token signature is invalid` after rotating the LiveKit secret

Old browser tokens are signed with the old secret. Restart `livekit` and
`ext-av`, then have browsers refresh the page.

### Dex rejects the redirect URI

`PUBLIC_HOST` doesn't match the URL the browser used. Recreate the `dex`
container after changing `PUBLIC_HOST`:
```bash
docker compose -f docker/docker-compose.yml up -d --force-recreate dex
```

See: [Quick Start § Common pitfalls](quick-start.md#9-common-pitfalls)
