# Data Model and Persistence

This document specifies **where** each piece of data lives, **why**, and
**how** it is accessed. It is a companion to `04-tech-stack.md` (technology
choices) and `05-architecture.md` (wiring). Authentication identity is covered
in `08-auth-and-identity.md`.

## Design principle: one job per store

The stack has three distinct persistence layers. Each is chosen for a specific
access pattern; none of them overlap in responsibility.

| Store | Purpose | Access pattern |
|---|---|---|
| **PocketBase** | Durable relational data (incl. chat history) | Written rarely (most data); chat messages append-heavy |
| **NATS JetStream KV** | Reactive semi-persistent state | Written every session, read on reconnect |

Dex IDP manages its own small store (OAuth sessions, connector state) via its
own SQLite volume. This is entirely internal to Dex and must not be conflated
with application data.

---

## 1. PocketBase (durable store)

### Role

PocketBase is the single source of truth for data that must survive indefinitely
and has a relational shape: user profiles, map configuration, audit logs.

It runs as an **embedded Go library inside the World Simulator** (via
`pocketbase.NewWithConfig()`, `app.Bootstrap()`, `app.RunAllMigrations()`,
and `app.Start()` in a goroutine). The World Simulator accesses PocketBase
through in-process Go SDK DAO calls (`app.FindFirstRecordByData`,
`app.Save`, `app.NewFilesystem`), not over HTTP. PocketBase's admin GUI and
file API are still served on port 8090 by the World Simulator process. Its
data directory (`pb_data`) is mounted as a named Docker volume for durability.

Because PocketBase is embedded, each World Sim instance has its own isolated
PocketBase — there is no shared store. Sharding across multiple World Sim
instances will require a different approach (e.g. a shared external database)
in the future. The Pusher does not access PocketBase. Extensions do not access
PocketBase directly — if an extension needs durable data (e.g. NPC identity),
it coordinates with the kernel or uses its own JetStream KV namespace.

### Collections

#### `players`
Keyed by the Dex `sub` claim (stable OIDC subject identifier). Called
`players` in PocketBase (the collection name in the Go migrations).

| Field | Type | Notes |
|---|---|---|
| `id` | string (PB auto) | PocketBase record ID |
| `oidc_sub` | string (unique) | Dex `sub` — the join key from the token |
| `display_name` | string | Shown above the avatar |
| `entity_id` | string (unique) | The in-world ECS entity ID (string-encoded) assigned on first login |
| `pos_x` | float | Last saved X position (tiles) |
| `pos_y` | float | Last saved Y position (tiles) |
| `map_id` | string | Current map name. Defaults to the `DEFAULT_MAP` env var on first login. Updated on map transitions. |
| `sprite_base` | string | PocketBase record ID in `sprite_bases` selecting the character sheet |
| `created_at` | datetime | Auto |
| `updated_at` | datetime | Auto |

#### `avatar_appearance`
One row per user. Written when the user customises their avatar.

| Field | Type | Notes |
|---|---|---|
| `user_id` | relation → `users` | |
| `body_shape` | string (enum) | e.g. `slim`, `regular`, `broad` |
| `skin_tone` | string (hex) | |
| `hair_style` | string | Asset key |
| `hair_color` | string (hex) | |
| `outfit` | string | Asset key |
| `accessory` | string (nullable) | Asset key |

The wire format for sending appearance to the World Simulator at login will be
defined in `16-avatars.md`.

#### `user_preferences`
UI and input settings. Written on explicit user save, read at login.

| Field | Type | Notes |
|---|---|---|
| `user_id` | relation → `users` | |
| `keybindings` | json | Override map for movement / action keys |
| `ui_scale` | float | Default `1.0` |
| `preferred_language` | string | BCP 47, e.g. `en`, `fr` |
| `push_notifications` | bool | Future use |

#### `maps`
Each Tiled map file registered in the system.

| Field | Type | Notes |
|---|---|---|
| `id` | string (PB auto) | |
| `name` | string | Map name (used by portal `target_map`, `VITE_MAP_NAME`, etc.) |
| `tiled_json` | file | Tiled JSON export |
| `tilesets` | file (multiple) | Tileset PNG images |

> Zone polygon definitions are stored in the Tiled JSON (parsed by worldsim
> into the in-memory zone registry on map load), not as separate records.

#### `audit_log`
Append-only. Written by the World Simulator on any security-relevant event.

| Field | Type | Notes |
|---|---|---|
| `user_id` | relation → `users` (nullable) | Null for system events |
| `event_type` | string | e.g. `zone.exclusive.activated`, `room.join.denied` |
| `payload` | json | Event-specific details |
| `occurred_at` | datetime | Server-side timestamp |

#### `extension_options`
One row per registered extension. Created by worldsim when an extension
registers with an `options_schema`. The admin edits the `options` JSON in
the PB admin GUI; worldsim's in-process hook republishes changes to the
extension via NATS (`extension.<id>.options`).

| Field | Type | Notes |
|---|---|---|
| `id` | string (PB auto) | |
| `extension_id` | string (required) | Extension ID from registration (e.g. `walls`, `av`) |
| `options` | json | JSON object of option name → value (e.g. `{"enabled": true}`) |
| `created_at` | datetime | Auto |
| `updated_at` | datetime | Auto |

---

## 2. NATS JetStream KV (reactive semi-persistent state)

### Role

Stores data that changes frequently during a session but must survive a World
Simulator restart. The World Simulator watches these keys via `kv.Watch`; on
reconnect it reads them to restore session state without round-tripping to
PocketBase.

See `04-tech-stack.md` § NATS for the full rationale (CAS, history, fault
resilience).

### Key schema

#### User session state (per connected user)

| Key | Value (JSON) | Written by | Notes |
|---|---|---|---|
| `users.<entity_id>.position` | `{"map_id":"…","x":42,"y":17,"dir":"south"}` | World Sim | Updated on movement; persisted so last position survives restart |
| `users.<entity_id>.status` | `{"label":"Available","emoji":"🟢"}` | World Sim | User-set status; shown above avatar |
| `users.<entity_id>.zone` | `{"zone_id":"…","joined_at":"…"}` | World Sim | Current zone membership |

> `<entity_id>` is the stable ECS entity ID stored in `users.entity_id` in
> PocketBase. It is the join key between the two stores.

#### Zone state (per zone)

Defined in `14-zones-and-interactions.md`. Zone shapes are stored in the
World Sim's zone registry (in-memory, reconstructed from Tiled + extension
registrations on restart). Zone **properties** (exclusivity, tint, owner) are
written to KV by the owning extension so the LiveKit Bridge can react via
`kv.Watch`:

| Key | Value (JSON) | Written by |
|---|---|---|
| `zones.<zone_id>.properties` | `{"is_exclusive":true,"tint_color":"#222244"}` | Owning extension |
| `zones.<zone_id>.owner` | `{"entity_id":"…"}` | Owning extension |

#### World globals

| Key | Value | Written by |
|---|---|---|
| `world.time` | ISO 8601 string | Extension (e.g. a world-clock extension) |

---

## 3. Message history (`messages` collection in PocketBase)

**[DECISION] MVP chat uses a PocketBase `messages` collection** (see
`17-chat.md` for the decision and the full chat spec). Matrix Synapse is
deferred to the post-MVP roadmap.

| Field | Type | Notes |
|---|---|---|
| `room_id` | string | `zone.<zone_id>`, `map.<map_id>`, or `dm.<entity_a>.<entity_b>` |
| `sender_id` | relation → `users` | |
| `body` | string | Message text |
| `sent_at` | datetime | Server timestamp |

---

## 4. What must NOT be persisted here

| Data | Correct store |
|---|---|
| Avatar movement (per tick) | Core NATS (ephemeral, not persisted) |
| Active LiveKit rooms / participants | Redis (LiveKit-only, see `04-tech-stack.md`) |
| OIDC sessions / refresh tokens | Dex's own SQLite volume |
| Map tilesets and JSON files | SeaweedFS / RustFS |

---

## 5. Login flow (all stores combined)

The login flow spans two services: the **Pusher** (token validation) and the
**World Simulator** (identity provisioning and entity registration). They
communicate via NATS Core.

1. **Pusher** receives the WebSocket upgrade and an `AUTH` frame with an OIDC
   token from Dex.
2. **Pusher** validates the token with Dex (JWKS cache — see
   `08-auth-and-identity.md` §4). Extracts the `sub` claim.
3. **Pusher** publishes a `client.connected` event to NATS Core, containing
   the `sub` and a generated `client_id`.
4. **World Simulator** receives the event and queries **PocketBase** `users`
   by `oidc_sub`:
   - **First login**: creates the `users`, `avatar_appearance`, and
     `user_preferences` rows; assigns a new `entity_id`.
   - **Returning user**: reads existing profile and appearance.
5. **World Simulator** reads **NATS JetStream KV** `users.<entity_id>.position`:
   - If present: spawns the entity at the stored position.
   - If absent (first login or expired): spawns at a random `spawn` zone
     on the default map (configured by the `DEFAULT_MAP` env var).
6. **World Simulator** reads `users.<entity_id>.status` to restore the user's
   status label.
7. **World Simulator** registers the entity in the ECS, computes the initial
   world snapshot (AOI-filtered), and publishes the replication batch to NATS
   Core (subject `client.<client_id>.replication`).
8. **World Simulator** publishes a `client.provisioned` event to NATS Core
   (with `client_id`, `entity_id`, and initial `zone_id`). The LiveKit Bridge
   subscribes to this event to issue a LiveKit room token (see
   `08-auth-and-identity.md` §6).
9. **Pusher** receives the batch and forwards it to the client over WebSocket.

---

## Open questions

- **[DECISION] PocketBase is embedded in the World Simulator as a Go library**,
  not run as a standalone container. The World Simulator accesses it via
  in-process Go SDK DAO calls. The `pb_data` directory is mounted as a named
  Docker volume. Each World Sim instance has its own embedded PocketBase;
  sharding will require a shared external database in the future. The Pusher
  does not access PocketBase.
- **[DECISION] JetStream KV TTL for `users.<entity_id>.position`**: **90 days**.
  Keys for inactive users expire automatically, preventing unbounded accumulation.
  Users returning after 90 days spawn at a random `spawn` zone on the default map.
- **[OPEN] Chat backend**: Matrix Synapse vs. PocketBase `messages` collection.
  To be resolved in `17-chat.md`.
- **[DECISION] Schema migrations**: PocketBase collection schema changes are
  handled by Go migrations in `backend/migrations/` (run via
  `app.RunAllMigrations()` at startup), replacing the earlier JS migrations in
  `pb_migrations/`.
