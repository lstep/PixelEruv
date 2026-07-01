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
    InteractFrame interact = 3;
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
5. Steady state: client sends InputFrame / InteractFrame; server sends
   ReplicationBatch per tick.
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

message InteractFrame {
  string target_entity_id = 1;     // the entity being interacted with
  string interaction_type = 2;     // "talk", "sit", "open", "use", ...
  bytes params = 3;                // optional interaction-specific payload (e.g. chat text)
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

### 1.5 Interaction model

- A client interacts with an entity by sending `InteractFrame` with the
  `target_entity_id` it learned from a prior `SpawnEntity` message.
- The Pusher forwards it to NATS. The World Simulator validates the
  interaction (proximity) and routes it to the owning extension based on the
  `ExtensionOwner` component or entity-bound notify triggers (see
  `18-extensions.md` § 6). The kernel does not have a `TriggerSystem` — all
  trigger and interaction behavior is implemented by extensions.

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
| `client.<client_id>.input` | Pusher | World Sim (owning shard) | `InputFrame` / `InteractFrame` |
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
| `entity.<entity_id>.interact` | World Sim | Extension | Forwarded client interaction |
| `entity.<entity_id>.interact.reply.<req_id>` | Extension | World Sim | Interaction response |
| `entity.<entity_id>.arrived` | World Sim | Extension | Reached movement target |
| `entity.<entity_id>.despawned` | World Sim | Extension | Entity removed |
| `entity.<entity_id>.notify.<event>` | World Sim | Extension (owning the trigger) | Entity-bound notify trigger dispatch (enter/exit/interact) |
| `trigger.<trigger_id>.query` | World Sim | Extension (owning the trigger) | Access trigger query (for ask) |
| `trigger.<trigger_id>.reply` | Extension | World Sim | Access trigger reply (for ask) |
| `trigger.notify.tile.<map_id>.<x>.<y>` | World Sim | All extensions (broadcast) | Tile-bound notify trigger dispatch |

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
| `extension.<ext_id>.register_triggers` | Extension | World Sim | Register access/event triggers on tiles/entities |
| `extension.<ext_id>.unregister_triggers` | Extension | World Sim | Remove triggers |
| `extension.<ext_id>.register_zone` | Extension | World Sim | Register a zone (boundary + properties) |

### 2.5 Media (LiveKit Bridge) subjects

| Subject | Publisher | Subscriber | Payload |
|---|---|---|---|
| `client.provisioned` | World Sim | LiveKit Bridge | `{client_id, entity_id, zone_id}` |
| `client.<client_id>.control` | LiveKit Bridge | Pusher | `ControlFrame` (LiveKit token, kick) |

### 2.6 Admin subjects

| Subject | Publisher | Subscriber | Payload |
|---|---|---|---|
| `admin.revoke.<entity_id>` | World Sim (execution) | Pusher (all) | Force-disconnect a user (policy decided by an admin extension; execution by the kernel) |

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
