# Tech Stack

This document captures the **technology choices** and the rationale for each.
The wiring between these components is described in `05-architecture.md`.

## Scaling and deployment

- **Docker** is used for packaging and deployment.
- **No Kubernetes.** The project must be deployable with Docker Compose only,
  so a small IT team can self-host without a platform-engineering budget.

### Reverse proxy: Traefik
- Handles automatic SSL termination via Let's Encrypt.
- Must enforce **session affinity (sticky sessions)** for WebSocket requests
  directed to the Pusher service, so that a client that briefly disconnects
  instantly reconnects to the **same Go instance** and retains its session
  state.

### Object storage
- SeaweedFS or RustFS for object storage. It is an ultra-lightweight S3-compatible
  object storage solution that runs inside the Docker Compose setup.
- It replaces a costly AWS S3 bucket for storing and serving the Tiled map
  JSON files and the pixel-art tilesets.

## Backend

- **Go**-based backend, split into two services:
  - **Pusher** — a thin WebSocket proxy that handles client connections, token
    validation, and NATS forwarding. No game logic. See `09-pusher.md`.
  - **World Simulator** — the spatial authority and replication gateway: ECS,
    spatial index, trigger registry, zone boundaries, AOI, replication encoding,
    player avatar movement, and state-store access (provisioning + persistence).
    All other gameplay behavior is delegated to extensions via NATS. See
    `10-world-simulator.md` and `18-extensions.md`.
- The two services communicate exclusively via **NATS Core** pub/sub.
- Uses **protobuf** for serialisation.
- Uses the Go **standard library `net/http`** packages — no web framework
  (Gin, Echo, etc.) is needed.

### Persistence
- Pocketbase (for data written rarely) for configuration, maps, audits
  and user management (see `06-data-model-and-persistence.md` for specific information).

### Client ↔ backend transport: WebSockets
- The backend communicates with the frontend over WebSockets.
- Library: [`coder/websocket`](https://github.com/coder/websocket) — minimal
  and idiomatic WebSocket library for Go.
- Rationale for choosing it over the Gorilla version:
  [coder/websocket vs Gorilla](https://websocket.org/guides/languages/go/).

## Authentication

- Authentication is handled by Dex to be able to integrate with external identity
  providers (e.g. Google, GitHub, etc.).

## NATS (JetStream) for reactive state

NATS JetStream KV is used to store **configurations, room states, user
metadata, and dynamic rules** — the "reactive" and semi-persistent state.

### What goes into JetStream KV
- **Dynamic state of zones**: e.g. key `zones.<zone_id>.properties` with value
  `{"is_exclusive": true, "tint_color": "#222244"}`.
- **Global office variables**: the virtual time of the world (`world.time`),
  the open/closed state of a portal, access rights to a private office.
- **Temporary employee profiles**: current status ("Available", "In a meeting",
  "Do not disturb").

### Why JetStream KV is a good fit
- **Native watchers (`kv.Watch`)**: the World Simulator, extensions, and the
  LiveKit Bridge can simply watch KV keys. As soon as a user clicks a button
  to make a zone exclusive, the owning extension writes to the KV. Instantly,
  without any polling, the World Sim sends a replication update (via NATS,
  forwarded by the Pusher) to the Phaser client, which applies the graphic
  filter. Simultaneously, the LiveKit Bridge reacts and cuts audio
  subscriptions.
- **Compare-And-Swap (CAS)**: during concurrent modifications (e.g. two bots or
  two users trying to modify the same property of an object at the same time),
  NATS guarantees atomic writes without complex locks.
- **History**: JetStream KV preserves the revision history of a key. You can
  know exactly which user modified the properties of a zone and when — a
  sought-after audit feature in enterprise environments.
- **Fault resilience**: if the World Simulator restarts, it
  simply queries the NATS KV store on startup to instantly reconstruct the
  exact state of the office without any data loss.

### What must NOT go into JetStream KV
- **Highly volatile state** — e.g. player movements — must not be stored in
  JetStream. Use **Core NATS** (ephemeral in-memory pub/sub) for that.

## Video and audio: LiveKit

- **LiveKit** is used for video/audio.
- **Redis** serves as the central shared state where LiveKit and the Go servers
  share the state of active rooms and the list of present participants.
  **Redis is used ONLY for LiveKit**, not for other parts of the application.
- **coturn** is the TURN server used by LiveKit's WebRTC stack. It is essential
  for enterprise users located behind restrictive corporate networks that
  block UDP traffic. Configured on **TCP port 443**, it encapsulates WebRTC
  media traffic within a standard TLS stream to bypass firewalls.

## Frontend: Phaser

- **Phaser v4** is used for the frontend.
- Rationale: see `A1-why-phaser.md`.
