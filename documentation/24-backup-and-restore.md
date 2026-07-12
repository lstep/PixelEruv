# Backup and Restore

> **How to back up and restore PocketBase data** (collections schema, records,
> and file fields) for the embedded PB instance inside worldsim.

PocketBase is embedded in the worldsim process as a Go library. Its data lives
in a single directory (`PB_DATA_DIR`, default `./pb_data`, mounted as the
`pb_data` Docker volume in production). There are two ways to back it up; this
document covers both and the trade-offs.

---

## 1. `pb-collections` — portable schema + records export

A standalone Go binary that exports all **application** PocketBase collections
(schema + records + file fields) into a portable JSON file plus a companion
`files/` directory, and imports them back into a (possibly fresh) `PB_DATA_DIR`.

It works **offline**: it bootstraps PocketBase directly on `PB_DATA_DIR` — no
HTTP server, no worldsim running. Same pattern as `seed-sprites`.

### Build

`pb-collections` is built by `make build` alongside the other binaries:

```bash
make build
# → dist/bin/pb-collections
```

### Export

```bash
PB_DATA_DIR=./pb_data ./dist/bin/pb-collections -export ./pb_backup
```

Produces:

```
pb_backup/
├── collections.json              # schema + records for all app collections
└── files/
    └── <collectionName>/
        └── <recordId>/
            └── <filename>        # binary content of file fields
```

`-force` overwrites a non-empty export directory.

### Import

```bash
# Into a fresh data dir (e.g. on a new host):
PB_DATA_DIR=./pb_data_fresh ./dist/bin/pb-collections -import ./pb_backup

# Or wipe-and-replace into an existing one:
PB_DATA_DIR=./pb_data ./dist/bin/pb-collections -import ./pb_backup -force
```

On import:

- Collection schemas are upserted via
  `app.ImportCollectionsByMarshaledJSON(..., false)` — existing unrelated
  collections are left intact.
- Records are inserted with `app.SaveNoValidate` (the export is trusted as a
  valid PB snapshot; field validations are not re-run).
- Re-importing without `-force` is **idempotent**: records that already exist
  (matched by ID) are skipped.
- `-force` deletes all existing records in each imported collection first, then
  re-inserts. Fresh record IDs are minted in this mode (see "Why IDs change
  on `-force`" below).

### What is and isn't exported

| Exported | Skipped |
|---|---|
| `maps`, `players`, `sprite_bases`, `extension_options`, `bans` | `_superusers` (admin password hashes) |
| PB's default `users` auth collection (if present) | `_externalAuths` |
| File fields (`maps.tiled_json`, `maps.tilesets`, `sprite_bases.sheet`) | `_migrations` (internal migration ledger) |

System collections are skipped deliberately — they're internal to PB and
`_superusers` contains admin password hashes you typically don't want to move
between environments. Re-create the initial superuser on the target with
`PB_ADMIN_EMAIL` / `PB_ADMIN_PASSWORD` (the `1752100000_initial_superuser`
migration handles this on a fresh data dir).

### Why restored filenames differ

PocketBase's `normalizeName` always appends a random suffix to uploaded
filenames (e.g. `char_0.png` → `char_0_2665swh3pt.png`). This is PB's built-in
behaviour, not specific to this tool. The restored binary content is
byte-identical to the original; only the filename string differs, and all
internal record references stay consistent.

### Why record IDs change on `-force`

In `-force` mode, existing records are deleted first. PB's delete hook removes
the record's storage directory. If we then re-inserted a record with the same
ID, the file upload would race on the removed directory. To avoid this,
`-force` lets PB mint fresh record IDs on re-insert.

This is safe because nothing in the current schema references record IDs
cross-collection:

- `maps` are keyed by `name` (portal zones use `target_map` = name, not ID).
- `players` are keyed by `user_id` and `entity_id`, not by PB record ID.
- `players.sprite_base` references `sprite_bases` by record ID — **this will
  break on `-force` import** if players have a non-empty `sprite_base`. For
  full-fidelity restores, prefer the volume-snapshot method (§2) or re-run
  `seed-sprites` after import to repopulate `sprite_bases`, then have players
  re-pick a sprite in-game.

In non-force mode (the default), record IDs are preserved and `sprite_base`
references stay valid.

### `created` / `updated` timestamps

PB's autodate hook fires on create, so these reset to import time. They are
not preserved from the export. This is a PB limitation, not specific to this
tool.

---

## 2. Volume snapshot — raw `pb_data` copy

The simplest, highest-fidelity backup: copy the entire `PB_DATA_DIR` while
worldsim is stopped. This preserves everything PB knows about, including
`_superusers`, autodate timestamps, internal migration state, and exact file
paths.

### Local dev

```bash
# Stop worldsim first (SQLite is single-writer).
cp -R ./pb_data ./pb_data.backup
# To restore:
rm -rf ./pb_data && cp -R ./pb_data.backup ./pb_data
```

### Docker (production)

```bash
cd ~/pixeleruv
docker compose down
docker run --rm -v pixeleruv_pb_data:/d -v "$PWD":/b alpine \
  tar czf /b/pb_data.tgz /d
docker compose up -d
```

Restore:

```bash
docker compose down
docker run --rm -v pixeleruv_pb_data:/d -v "$PWD":/b alpine \
  sh -c "rm -rf /d/* && tar xzf /b/pb_data.tgz -C /"
docker compose up -d
```

---

## 3. Which one should I use?

| Need | Use |
|---|---|
| Routine backup before a risky migration or schema change | Volume snapshot (§2) — fastest, full-fidelity |
| Move data between hosts or environments | Volume snapshot (§2) if same PB version; `pb-collections` (§1) for a portable, version-tolerant format |
| Inspect or hand-edit records outside PB | `pb-collections` (§1) — plain JSON |
| Selective restore (one collection, skip auth) | `pb-collections` (§1) — system collections skipped by design |
| Reproduce a bug from production on a dev machine | `pb-collections` (§1) — export prod, import into a fresh local `PB_DATA_DIR` |

**Important**: never run `pb-collections` while worldsim is using the same
`PB_DATA_DIR`. SQLite is single-writer and concurrent access will corrupt the
database. Stop worldsim (or point `pb-collections` at a copy of the data dir)
first.

---

## See also

- [Data Model and Persistence](06-data-model-and-persistence.md) — what lives
  in PocketBase and why.
- [Quick Start § Day-to-day operations](quick-start.md#9-day-to-day-operations)
  — volume snapshot one-liner.
- `backend/cmd/pb-collections/main.go` — the tool's source and usage banner.
