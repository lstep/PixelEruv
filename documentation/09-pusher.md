# Pusher

This document specifies the **Pusher** — a thin Go WebSocket proxy that sits
between the browser and NATS Core. It handles WebSocket I/O, token validation,
and NATS forwarding. It does not run any game logic.

> The Pusher's counterpart is the **World Simulator** (the spatial authority
> and replication gateway), specified in `10-world-simulator.md`. The two
> services communicate exclusively via NATS Core.

---

## 1. Why two services, not one

Initially the Pusher was conceived as a single service handling both WebSocket
I/O and game simulation. As the design evolved, it became clear that this
created a monolith with two very different scaling profiles and failure
characteristics:

| Concern | Pusher | World Simulator |
|---|---|---|
| Primary job | Network I/O | Computation |
| Scaling axis | Number of WebSocket connections | Number of entities / world shards |
| Failure impact | Clients disconnect, reconnect to another Pusher | World state must survive (JetStream KV) |
| State | Session state (ephemeral, sticky) | ECS + authoritative game state |
| Tick rate | None (event-driven) | Fixed tick (e.g. 20–30 Hz) |
| State-store access | NATS Core only | NATS Core + JetStream KV + PocketBase |

Separating them allows each to be scaled, deployed, and restarted
independently. The Pusher can be scaled horizontally by adding instances
behind Traefik. The World Simulator can be sharded per map or per region.
Neither knows about the other's internal state — they communicate exclusively
via NATS Core.

### The Pusher as a transport seam

The deeper reason for the split is **transport isolation**. The World Simulator
speaks one protocol: NATS Core. It knows nothing about browsers, WebSockets,
or any other client transport. The Pusher is the only component that knows how
to talk to a client over the wire.

This makes the Pusher a **seam**: the place where the browser-facing transport
is bound to the simulation. If, in the future, clients use a different transport
— another browser protocol (e.g. WebTransport), a native client over raw TCP,
or something that is not a browser at all — only the Pusher (or a sibling
gateway) needs to be written or replaced. The World Simulator, the ECS, the
replication encoder, and the NATS contract stay unchanged. The simulation is
never reworked to accommodate a new way of reaching a client.

Concretely, the contract the World Simulator depends on is the set of NATS
subjects in §7, not WebSocket frames. Anything that can publish
`client.<client_id>.input` and consume `client.<client_id>.replication` is a
valid client gateway, with or without a browser on the other end.

### Responsibility matrix

This table is the **canonical reference** for who does what. If a document
attributes a responsibility to the wrong service, this table overrides it.

The World Simulator is the **spatial authority and replication gateway** (the
kernel). It owns the ECS, the tile grid, the trigger registry, the zone
boundaries, the AOI filter, and the replication encoder. Its only gameplay
system is player avatar movement. All other gameplay behavior (NPC movement,
trigger logic, zone behavior, AI, custom mechanics) is delegated to extensions
via NATS. See `10-world-simulator.md` and `18-extensions.md`.

| Responsibility | Pusher | World Simulator | Extension |
|---|---|---|---|
| WebSocket I/O (accept, read, write frames) | ✅ | ❌ | ❌ |
| OIDC token validation (JWKS cache) | ✅ | ❌ | ❌ |
| Session management (client_id ↔ WebSocket) | ✅ | ❌ | ❌ |
| Publish client input to NATS Core | ✅ | ❌ | ❌ |
| Publish `client.connected` / `client.disconnected` | ✅ | ❌ | ❌ |
| Subscribe per-client replication batches from NATS | ✅ | ❌ | ❌ |
| Forward replication batches to client over WebSocket | ✅ | ❌ | ❌ |
| Forward control frames (LiveKit token, kick) to client | ✅ | ❌ | ❌ |
| ECS host (entities, components) | ❌ | ✅ | ❌ |
| Spatial index (tile → triggers, tile → entities) | ❌ | ✅ | ❌ |
| Trigger registry (trigger_id → owner, behavior, tiles) | ❌ | ✅ | ❌ |
| Zone boundary registry | ❌ | ✅ | ❌ |
| Replication tick (AOI + encode + publish) | ❌ | ✅ | ❌ |
| AOI manager (area-of-interest filter) | ❌ | ✅ | ❌ |
| Replication encoder (Spawn/Update/Destroy/Anim) | ❌ | ✅ | ❌ |
| Publish per-client replication batches to NATS | ❌ | ✅ | ❌ |
| Publish volatile cross-shard events to NATS | ❌ | ✅ | ❌ |
| Subscribe client input from NATS | ❌ | ✅ | ❌ |
| Player avatar movement (input → position, in-kernel) | ❌ | ✅ | ❌ |
| Gate trigger evaluation (block/allow cached, ask routed) | ❌ | ✅ | ❌ |
| Input trigger dispatch (range/LOS computed, broadcast to extensions) | ❌ | ✅ | ❌ |
| Zone notify trigger dispatch (enter/exit to owning extension) | ❌ | ✅ | ❌ |
| Neutral validation (collision, zone access, bounds, schema) | ❌ | ✅ | ❌ |
| Read / write JetStream KV (player positions; reads zone state) | ❌ | ✅ | ❌ (kernel-owned keys) |
| Read / write PocketBase (users, world config, audit) | ❌ | ✅ | ❌ |
| Identity → entity provisioning (PocketBase lookup) | ❌ | ✅ | ❌ |
| Player position/status persistence | ❌ | ✅ | ❌ |
| Publish `client.provisioned` (for LiveKit Bridge) | ❌ | ✅ | ❌ |
| Token revocation execution (publish `admin.revoke`, despawn) | ❌ | ✅ | ❌ |
| Token revocation policy (who can kick, when) | ❌ | ❌ | ✅ (admin extension) |
| Token revocation event subscription (close WebSocket) | ✅ | ❌ | ❌ |
| Extension lifecycle management (register, heartbeat, freeze/despawn) | ❌ | ✅ | ❌ |
| NPC movement and behavior | ❌ | ❌ | ✅ |
| Trigger logic (what happens when a trigger fires) | ❌ | ❌ | ✅ |
| Zone behavior (exclusivity, knock-to-join, timers) | ❌ | ❌ | ✅ |
| AI / behavior trees / LLM calls | ❌ | ❌ | ✅ |
| Custom game mechanics | ❌ | ❌ | ✅ |
| Register triggers (access + event + action) on tiles/entities | ❌ | ❌ | ✅ |
| Register zones (boundaries + properties) | ❌ | ❌ | ✅ |
| Spawn/despawn extension entities | ❌ | ❌ | ✅ |
| Read / write JetStream KV (extension-private + shared keys) | ❌ | ❌ | ✅ |
| Media proxying (WebRTC) | ❌ | ❌ (client → LiveKit direct) | ❌ |

---

## 2. Responsibilities

The Pusher is a **thin proxy** between the browser's WebSocket and NATS Core.
It does not run the ECS, the spatial index, the trigger registry, the AOI, the
replication encoder, or any gameplay logic. It does not access PocketBase or
JetStream KV.

1. **WebSocket I/O** — accept connections (via Traefik), read/write protobuf
   frames.
2. **Token validation** — validate the OIDC `AUTH` frame using a cached JWKS
   (see `08-auth-and-identity.md`). Extract the `sub` claim.
3. **Session management** — track which `client_id` is on which WebSocket.
   This is the only state the Pusher holds. It is ephemeral and rebuilt on
   reconnect (Traefik sticky sessions route the client back to the same
   Pusher instance).
4. **Input forwarding** — publish client input to NATS Core (subject
   `client.<client_id>.input`).
5. **Replication forwarding** — subscribe to NATS Core (subject
   `client.<client_id>.replication`) and forward received batches to the
   client's WebSocket.
6. **Control frame forwarding** — subscribe to NATS Core (subject
   `client.<client_id>.control`) and forward control frames (LiveKit tokens,
   kick notifications) to the client's WebSocket.
7. **Lifecycle events** — publish `client.connected` and `client.disconnected`
   events to NATS Core so the World Simulator can provision/teardown entities.

---

## 3. Internal modules

```
Pusher process
├── WebSocket gateway     (coder/websocket, goroutine-per-connection)
├── Session manager       (client_id ↔ WebSocket map, in-memory)
├── Token validator       (JWKS cache, refreshed every 10 min)
├── NATS bridge           (publish input, subscribe replication batches)
└── Health aggregator     (subscribes to "healthz" NATS subject, serves HTTP /healthz)
```

---

## 4. Goroutine model

- **One goroutine per WebSocket connection** — reads frames, publishes input
  to NATS.
- **One goroutine per client for NATS subscription** — listens on
  `client.<client_id>.replication` and writes to the WebSocket.
- **One goroutine for JWKS refresh** — ticks every 10 minutes.
- **One goroutine for graceful shutdown** — listens for SIGTERM, drains
  connections.
- **One goroutine for health publishing** — publishes the pusher's own health
  JSON to the `healthz` NATS subject every 10 seconds.

The session manager is a concurrent-safe map protected by a `sync.RWMutex`.
No other shared state exists.

---

## 5. Packet flow

```
Browser ──WS frame──► Pusher
                       │
                       ├── AUTH frame? ──► Token validator ──► Dex (JWKS)
                       │                                      │
                       │                                      └── ok ──► assign client_id
                       │                                                  │
                       ├── Input frame? ──► NATS publish (client.<id>.input)
                       │
                       └── (NATS subscribe client.<id>.replication)
                                  │
                                  └──► WS frame ──► Browser
```

---

## 6. Failure recovery

- **Pusher crash**: clients lose their WebSocket. Traefik sticky sessions
  route them to the same Pusher instance if it restarts quickly, or to
  another instance if not. On reconnect, the client sends a new `AUTH` frame,
  the Pusher publishes a new `client.connected` event, and the World
  Simulator re-attaches the existing entity (it was never gone — the World
  Sim kept it alive).
- **NATS disconnect**: the Pusher cannot forward input or replication. It
  buffers a small number of frames (configurable) and flushes when NATS
  reconnects. If the buffer overflows, it closes the WebSocket with a `4403
  Service Unavailable` close code; the client reconnects.
- **World Sim crash**: the Pusher is unaffected — it keeps its WebSocket
  connections open. It simply has no replication batches to forward until the
  World Sim recovers. Clients see a brief pause in game-state updates but do
  not disconnect.

---

## 7. Communication contract (Pusher side)

All communication with the World Simulator is via NATS Core. No direct RPC,
no shared memory. See `10-world-simulator.md` §8 for the full contract from
the World Sim's perspective.

### Inbound (subscribed by the Pusher)

| Subject | Publisher | Payload | Frequency |
|---|---|---|---|
| `client.<client_id>.replication` | World Sim | `ReplicationBatch` (protobuf) | Per tick |
| `client.<client_id>.control` | LiveKit Bridge | `ControlFrame` (LiveKit token) | Event-driven |
| `admin.revoke.<entity_id>` | World Sim | `{entity_id, reason}` | On admin kick |
| `healthz` | All services (pusher, worldsim, extensions) | Health JSON (see §9) | Every 10s |

### Outbound (published by the Pusher)

| Subject | Subscriber | Payload | Frequency |
|---|---|---|---|
| `client.<client_id>.input` | World Sim (owning shard) | Protobuf input frame | Per client input |
| `client.connected` | World Sim | `{client_id, sub, pusher_instance}` | On connect |
| `client.disconnected` | World Sim | `{client_id, reason}` | On disconnect |
| `healthz` | Pusher (self) | Health JSON (see §9) | Every 10s |

> The subject naming convention is illustrative. The final convention will be
> defined in `07-network-protocol.md`.

---

## 8. What the Pusher does NOT do

This section is explicit because the Pusher's responsibilities were
previously overloaded (see §1).

- ❌ Does not run the ECS, the spatial index, the trigger registry, or any
  gameplay logic.
- ❌ Does not compute AOI or encode replication messages.
- ❌ Does not enforce zone isolation or access policies (the World Sim
  evaluates triggers; extensions decide zone behavior).
- ❌ Does not read or write JetStream KV.
- ❌ Does not read or write PocketBase.
- ❌ Does not communicate with the LiveKit Bridge directly — the Bridge
  publishes control frames (LiveKit tokens) to `client.<client_id>.control`,
  which the Pusher forwards.
- ❌ Does not proxy media (WebRTC goes directly to LiveKit via Traefik).

The Pusher is a **stateless network proxy** with one job: bridge WebSocket
connections to NATS Core, validating tokens at the boundary.

---

## 9. Health endpoint (`/healthz`)

The Pusher exposes an HTTP `GET /healthz` endpoint that returns the aggregated
health of all backend services as JSON. This is the only HTTP endpoint on the
Pusher (everything else is WebSocket).

### How it works

Every backend service (pusher, worldsim, ext-demo, ext-walls, ext-props,
ext-av) publishes a health JSON to the `healthz` NATS subject every 10 seconds.
The Pusher subscribes to this subject and maintains an in-memory map of
service name → latest health entry. When an HTTP request arrives at `/healthz`,
the Pusher returns the map as JSON.

### Health JSON format (published by each service)

```json
{
  "service": "kernel",
  "status": "OK",
  "version": "v1.2.3",
  "uptime": "4h32m",
  "extras": { ... }
}
```

| Field | Description |
|---|---|
| `service` | Service identifier: `"pusher"`, `"kernel"`, or `"ext-<id>"` |
| `status` | `"OK"` or `"stale"` (set by the pusher if no message received in 30s) |
| `version` | Git tag if HEAD is on a tag, otherwise short commit hash. Set via Go ldflags at build time. Defaults to `"dev"`. |
| `uptime` | Human-readable duration since process start |
| `extras` | Service-specific JSON (see below) |

### Extras by service

| Service | Extras |
|---|---|
| `pusher` | `nats_connected` (bool), `active_sessions` (int) |
| `kernel` | `entity_count` (int), `connected_players` (int), `running_extensions` (int) |
| `ext-*` | `{}` (empty for now) |

### HTTP response

```json
{
  "services": [
    { "service": "ext-av", "status": "OK", "version": "v1.2.3", "uptime": "4h32m", "extras": {}, "last_seen": "2026-07-10T12:00:00Z" },
    { "service": "ext-demo", "status": "OK", "version": "v1.2.3", "uptime": "4h32m", "extras": {}, "last_seen": "2026-07-10T12:00:00Z" },
    { "service": "ext-props", "status": "OK", "version": "v1.2.3", "uptime": "4h32m", "extras": {}, "last_seen": "2026-07-10T12:00:00Z" },
    { "service": "ext-walls", "status": "OK", "version": "v1.2.3", "uptime": "4h32m", "extras": {}, "last_seen": "2026-07-10T12:00:00Z" },
    { "service": "kernel", "status": "OK", "version": "v1.2.3", "uptime": "4h32m", "extras": { "entity_count": 42, "connected_players": 7, "running_extensions": 4 }, "last_seen": "2026-07-10T12:00:00Z" },
    { "service": "pusher", "status": "OK", "version": "v1.2.3", "uptime": "4h32m", "extras": { "nats_connected": true, "active_sessions": 7 }, "last_seen": "2026-07-10T12:00:00Z" }
  ]
}
```

The response is always HTTP 200 (it's informational). Services that haven't
published in 30 seconds are marked `"stale"` so consumers can detect dead
processes. The `services` array is sorted alphabetically by service name.

### Version injection

The version is baked into each Go binary at compile time via ldflags:

```
-ldflags="-X github.com/lstep/pixeleruv/backend/internal/version.Version=<version>"
```

The Makefile computes the version as `git describe --tags --exact-match` (tag
if HEAD is exactly on a tag) or `git rev-parse --short HEAD` (short commit
hash), falling back to `"dev"` if git is unavailable. The Dockerfile accepts
a `VERSION` build arg with the same ldflags injection.

### Frontend usage

The frontend polls `GET /healthz` every 10 seconds and displays the kernel's
version in a small, semi-transparent badge fixed to the bottom-left corner of
the screen (10px monospace, `pointer-events: none`). This gives users and
developers an at-a-glance indicator of which version is running without
obstructing the game view.

### Nginx proxy

In production, nginx proxies `/healthz` to the pusher (`proxy_pass
http://pusher:8081/healthz`) in both the HTTP and HTTPS server blocks, so the
frontend can fetch it same-origin. In dev, Vite proxies it to
`http://localhost:8081`.
