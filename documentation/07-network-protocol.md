# Network Protocol

This document defines the **wire conventions** that tie the system together:

1. The **client ↔ Pusher WebSocket frame protocol** (control frames, input,
   interactions).
2. The **NATS subject naming convention** used by all backend services and
   extensions.

It is the single source of truth for these conventions. Other documents
(`09-pusher.md`, `10-world-simulator.md`, `11-replication.md`,
`18-extensions.md`) reference subjects and frames defined here rather than
inventing their own.

> The **replication message format** (server → client game state) is defined
> in `11-replication.md` § 7. This document covers the client → server
> direction and the internal NATS bus.

---

## 1. Client ↔ Pusher WebSocket protocol

All frames are **protobuf**, wrapped in a single envelope. The browser's
native WebSocket API cannot send custom HTTP headers, so authentication and
all control signaling happen in-band as frames.

### 1.1 Frame envelope

```protobuf
message ClientFrame {        // client → server
  oneof payload {
    AuthFrame auth = 1;
    InputFrame input = 2;
    ActionFrame action = 3;             // clicks + key presses (replaces InteractFrame)
    TokenRefreshFrame token_refresh = 4;
    PingFrame ping = 5;
  }
}

message ServerFrame {        // server → client
  oneof payload {
    ReplicationBatch replication = 1;   // defined in 11-replication.md
    AuthResultFrame auth_result = 2;
    ErrorFrame error = 3;
    PongFrame pong = 4;
    ControlFrame control = 5;           // LiveKit token, kick, etc.
    ActionResultFrame action_result = 6; // action validation result (range/LOS/no-target)
  }
}
```

### 1.2 Connection handshake

```
1. Client opens WebSocket (via Traefik, sticky session).
2. Client sends AuthFrame { id_token } as the FIRST frame.
3. Pusher validates the token (JWKS, see 08-auth-and-identity.md).
   - On failure: ErrorFrame { code: 4401 } and close.
   - On success: Pusher assigns a client_id, publishes client.connected,
     and sends AuthResultFrame { client_id, ok: true }.
4. World Simulator provisions the entity and sends the initial snapshot
   (ReplicationBatch of SpawnEntity messages) via NATS → Pusher → client.
5. Steady state: client sends InputFrame / ActionFrame; server sends
   ReplicationBatch (and occasionally ActionResultFrame) per tick.
```

If no `AuthFrame` arrives within **5 seconds** of the WebSocket upgrade, the
Pusher closes the connection with code `4401`.

### 1.3 Frame definitions

```protobuf
message AuthFrame {
  string id_token = 1;             // OIDC JWT from Dex
}

message AuthResultFrame {
  bool ok = 1;
  string client_id = 2;            // assigned by the Pusher
  string entity_id = 3;            // assigned by the World Sim (after provisioning)
}

message InputFrame {
  uint32 seq = 1;                  // client input sequence number (for reconciliation)
  uint32 client_tick = 2;          // client's tick estimate
  InputState state = 3;            // current held inputs
}

message InputState {
  bool up = 1;
  bool down = 2;
  bool left = 3;
  bool right = 4;
  bool run = 5;                    // walk/run modifier
  // action keys, etc.
}

message ActionFrame {
  uint32 seq = 1;                  // client action sequence number (for reconciliation)
  string input = 2;                // "click:left", "click:right", "key:E", "action:execute", etc.
  string traceparent = 3;          // W3C traceparent for this action
  string entity_id = 4;            // target entity for "action:execute" (empty for "key:E")
  string action_id = 5;            // which action to run (e.g. "activate", "toggle") — popup mode
}

message AvailableAction {
  string entity_id = 1;            // entity the action targets
  string action_id = 2;            // action verb (e.g. "activate", "toggle")
  string label = 3;                // user-facing label (e.g. "Activate")
  string entity_label = 4;         // entity type label for popup grouping (e.g. "light")
}

message ActionResultFrame {
  uint32 seq = 1;                  // matches the ActionFrame seq
  bool ok = 2;                     // whether the action was accepted
  string reason = 3;               // "no_handler", "timeout", "" on success
  repeated AvailableAction available_actions = 4;  // popup actions (empty for immediate mode)
}

message TokenRefreshFrame {
  string id_token = 1;             // a freshly obtained OIDC JWT
}

message ErrorFrame {
  uint32 code = 1;                 // WebSocket close code or app error code
  string message = 2;
}

message ControlFrame {
  oneof payload {
    LiveKitTokenControl livekit_token = 1;   // room-scoped LiveKit JWT
    KickControl kick = 2;                    // admin revocation
  }
}
```

### 1.4 Input model

- The client sends `InputFrame` with a monotonically increasing `seq` whenever
  its input state changes (or every tick while inputs are held — TBD in
  `12-netcode.md`).
- The Pusher forwards each `InputFrame` to NATS Core (subject below). It does
  **not** interpret input — it is a pass-through.
- The World Simulator applies input authoritatively in its tick loop and
  echoes the processed `seq` back in replication so the client can reconcile
  its prediction (see `12-netcode.md`).

### 1.5 Action model (replaces InteractFrame)

- A client sends `ActionFrame` whenever the player clicks a tile or presses an
  input key. The `input` field identifies the input (`click:left`,
  `click:right`, `key:E`, `action:execute`, etc.). For key presses, the
  `entity_id` and `action_id` fields are empty — the kernel computes adjacent
  entities from the player's position.
- The Pusher forwards it to NATS (pass-through). The World Simulator computes
  contextual data (range, LOS, entities on tile / adjacent entities, target
  entities from `interactions` `target_ids`, equipment snapshot) and broadcasts
  to all extensions that registered for that input type:
  1. **No extension registered?** → `ActionResultFrame{ ok: false, reason: "no_handler" }`.
  2. **Extensions registered** → broadcast to all of them. Each extension
     self-filters and replies asynchronously. The kernel collects all replies
     within a timeout, applies state/appearance/animation updates, and sends a
     single `ActionResultFrame{ ok: true, available_actions: [...] }` to the
     client.
  3. **No reply within timeout** → `ActionResultFrame{ ok: false, reason: "timeout" }`.
- **Two-phase interaction flow** (see
  `documentation/plans/2026-07-15-interaction-system-design.md`):
  1. **Phase 1** — client sends `ActionFrame{ input: "key:E" }`. If the entity
     is in popup mode, the extension replies with `available_actions` (no
     effects executed). The client shows a popup.
  2. **Phase 2** — user picks an action; client sends
     `ActionFrame{ input: "action:execute", entity_id: "...", action_id: "..." }`.
     The extension processes the effects and replies with state/appearance
     updates.
  - **Immediate mode** entities skip phase 2: pressing E fires the action
    directly (no popup). `available_actions` is empty in the reply.
- The kernel does not have a `TriggerSystem` — all trigger and interaction
  behavior is implemented by extensions. The kernel only computes spatial data
  (range, LOS, entities) and broadcasts.

### 1.6 WebSocket close codes

| Code | Meaning |
|---|---|
| `1000` | Normal closure |
| `4401` | Unauthorized (missing/invalid/expired token, or admin kick) |
| `4403` | Service unavailable (NATS down, buffer overflow) |
| `4408` | Auth timeout (no `AuthFrame` within 5 s) |

---

## 2. NATS subject naming convention

All backend services and extensions use a consistent subject hierarchy. The
convention is **`<domain>.<scope>.<action>`**, lowercase, dot-separated.

### 2.1 Client session subjects

| Subject | Publisher | Subscriber | Payload |
|---|---|---|---|
| `client.<client_id>.input` | Pusher | World Sim (owning shard) | `InputFrame` / `ActionFrame` |
| `client.<client_id>.replication` | World Sim | Pusher (owning the client) | `ReplicationBatch` |
| `client.<client_id>.control` | LiveKit Bridge | Pusher | `ControlFrame` (LiveKit token) |
| `client.connected` | Pusher | World Sim | `{client_id, sub, pusher_instance}` |
| `client.disconnected` | Pusher | World Sim | `{client_id, reason}` |

### 2.2 World / shard subjects

| Subject | Publisher | Subscriber | Payload |
|---|---|---|---|
| `world.<shard_id>.volatile` | World Sim | Other World Sim shards | Cross-shard entity state |
| `world_sim.restarted` | World Sim | Extensions | `{shard_id, timestamp}` |

### 2.3 Entity subjects (extensions)

| Subject | Publisher | Subscriber | Payload |
|---|---|---|---|
| `entity.<entity_id>.update` | Extension | World Sim | Direct component update |
| `entity.<entity_id>.move` | Extension | World Sim | Movement target |
| `entity.<entity_id>.arrived` | World Sim | Extension | Reached movement target |
| `entity.<entity_id>.despawned` | World Sim | Extension | Entity removed |
| `entity.<entity_id>.notify.<event>` | World Sim | Extension (owning the trigger) | Proximity trigger dispatch (proximity_enter/proximity_exit for mobile circle zones) |
| `trigger.<trigger_id>.query` | World Sim | Extension (owning the trigger) | Gate trigger query (for ask) |
| `trigger.<trigger_id>.reply` | Extension | World Sim | Gate trigger reply (for ask) |
| `input.<input_type>` | World Sim | Extensions (registered for that input type) | Input trigger dispatch (player clicked or pressed a key; includes range, LOS, entities, equipment) |
| `input.<input_type>.reply.<req_id>` | Extension | World Sim | Input trigger response (updates, consume_items) |
| `zone.<zone_id>.notify.<event>` | World Sim | Extension (owning the zone) | Zone notify trigger dispatch (enter/exit) |
| `entity.<entity_id>.notify.proximity_<event>` | World Sim | Extension (owning the trigger) | Proximity trigger dispatch (proximity_enter/proximity_exit) |

### 2.4 Extension lifecycle subjects

| Subject | Publisher | Subscriber | Payload |
|---|---|---|---|
| `extension.register` | Extension | World Sim | Registration request |
| `extension.<ext_id>.registered` | World Sim | Extension | Registration response |
| `extension.<ext_id>.register_components` | Extension | World Sim | Custom component schemas |
| `extension.<ext_id>.deregister` | Extension | World Sim | Graceful shutdown |
| `extension.<ext_id>.heartbeat` | Extension | World Sim | Liveness |
| `extension.<ext_id>.spawn` | Extension | World Sim | Spawn entity |
| `extension.<ext_id>.despawn` | Extension | World Sim | Despawn entity |
| `extension.<ext_id>.batch_update` | Extension | World Sim | Batched updates |
| `extension.<ext_id>.error` | World Sim | Extension | Validation error |
| `extension.<ext_id>.register_triggers` | Extension | World Sim | Register zone triggers (gate/notify) on zones, or input triggers for input types |
| `extension.<ext_id>.unregister_triggers` | Extension | World Sim | Remove triggers |
| `extension.<ext_id>.register_zone` | Extension | World Sim | Register a zone (shape + mobility + properties) |
| `zone.<zone_id>.query_occupancy` | Extension | World Sim (request/reply) | Query current entity IDs inside the zone (used at init time to bootstrap occupancy state) |

### 2.5 Media (LiveKit Bridge) subjects

| Subject | Publisher | Subscriber | Payload |
|---|---|---|---|
| `client.provisioned` | World Sim | LiveKit Bridge | `{client_id, entity_id, zone_id}` |
| `client.<client_id>.control` | LiveKit Bridge | Pusher | `ControlFrame` (LiveKit token, kick) |

### 2.6 Admin subjects

| Subject | Publisher | Subscriber | Payload |
|---|---|---|---|
| `admin.revoke.<entity_id>` | World Sim (execution) | Pusher (all) | Force-disconnect a user (policy decided by an admin extension; execution by the kernel) |

### 2.6a MCP-server-facing subjects (worldsim request-reply)

These subjects are subscribed by the World Sim and called by the MCP server
(`backend/cmd/mcp`) to expose world state, audit history, and admin actions
to LLM clients. All are request-reply with JSON payloads; admin actions
reply with `{"ok":bool,"error":"..."}` so the MCP client can confirm the
action landed. Audit events emitted by admin actions are stamped with
`actor.extension="mcp"` (configurable via `MCP_ACTOR`) so audit consumers
can filter LLM-initiated actions from client-initiated ones. See
`documentation/plans/2026-07-19-mcp-server-design.md` for the full design.

| Subject | Payload (request) | Reply |
|---|---|---|
| `worldsim.stats.get` | *(empty)* | `worldsim.statsResponse` (tick rate, uptime, players, entities, extensions, per-map counts) |
| `worldsim.zones.get` | *(empty)* | `worldsim.zoneMetadataMsg` (zone metadata for all maps) |
| `worldsim.entities.query` | `{"map_id?", "entity_type?", "owner_extension?", "zone_id?", "limit?"}` | `[]entitySnapshot` (up to 500, sorted by entity_id) or `{"error":"..."}` |
| `worldsim.entity.get` | `{"entity_id"}` | `entitySnapshot` or `{"error":"entity not found"}` |
| `worldsim.entity.teleport` | `{"entity_id", "map_id", "target_entity?"}` | *(fire-and-forget, no reply)* |
| `worldsim.client.kick` | `{"client_id", "reason?", "actor"}` | `{"ok":true}` or `{"ok":false,"error":"not_connected"}` |
| `worldsim.client.ban` | `{"target_type", "target_value", "reason?", "banned_until", "banned_by?", "actor"}` | `{"ok":true,"kicked":bool}`. `target_type` ∈ `user_id`/`ip`/`device_id`. |
| `worldsim.admin.chat` | `{"entity_id", "channel", "text", "actor"}` | `{"ok":bool,"error":"..."}`. `channel` ∈ `global`/`proximity`. |
| `worldsim.admin.set_name` | `{"entity_id", "name", "actor"}` | `{"ok":bool,"error":"..."}`. Sanitized to ASCII printable, truncated to 20 runes. |
| `worldsim.admin.set_status` | `{"entity_id", "status", "actor"}` | `{"ok":bool,"error":"..."}`. `status` ∈ 0/1/2. |
| `worldsim.admin.set_sprite` | `{"entity_id", "sprite_base", "actor"}` | `{"ok":bool,"error":"..."}`. Validated against `sprite_bases` PB collection unless empty. |
| `worldsim.admin.set_player_options` | `{"entity_id", "options", "actor"}` | `{"ok":bool,"error":"..."}`. Full replace (not partial merge). |

### 2.7 JetStream KV buckets

KV keys are **not** NATS subjects but follow a parallel convention. See
`06-data-model-and-persistence.md` § 2 for the authoritative key schema.

| Key pattern | Owner |
|---|---|
| `users.<entity_id>.position` / `.status` / `.zone` | World Sim (kernel — player state persistence) |
| `zones.<zone_id>.properties` / `.owner` | Owning extension |
| `world.time` | Extension (e.g. world-clock extension) |
| `ext.<ext_id>.*` | Extension (private namespace) |

---

## 3. Versioning

- Protobuf schemas are **forward/backward compatible by convention**: never
  reuse or renumber a field tag; only add new optional fields.
- A `protocol_version` is exchanged in `AuthResultFrame` (**[OPEN]** — add the
  field) so the client can detect a server it cannot speak to and prompt a
  reload.

---

## Open questions

- **[OPEN] Input cadence** — does the client send `InputFrame` only on change,
  or every tick while held? Decide in `12-netcode.md`.
- **[OPEN] Protocol version negotiation** — formalize the `protocol_version`
  handshake.
- **[OPEN] Binary vs. text frames** — confirm all frames are binary protobuf
  (assumed) and that no JSON is sent on the hot path.
- **[OPEN] Subject wildcards & sharding** — how `client.*.input` is partitioned
  across World Sim shards; ties into `14-zones-and-interactions.md`.
- **[RESOLVED] Knock/invite wire protocol** — uses transient popup entities +
  `ActionFrame` (input trigger). See `14-zones-and-interactions.md` §8. No new
  frame types. The client sends an `ActionFrame` with `input_type: "click:left"`
  on the popup entity's tile; the kernel broadcasts to extensions registered for
  `click:left`; the meeting extension self-filters based on `entities_on_tile`.
