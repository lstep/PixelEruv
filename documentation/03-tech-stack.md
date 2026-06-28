---
creation date: 2026-06-26 08:37
modification date: 2026-06-27 18:00
---
# Tech Stack

This document captures the **technology choices** and the rationale for each.
The wiring between these components is described in `04-architecture.md`.

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

### Object storage: SeaweedFS or RustFS
- An ultra-lightweight S3-compatible instance runs inside the Docker Compose
  setup.
- It replaces a costly AWS S3 bucket for storing and serving the Tiled map
  JSON files and the pixel-art tilesets.

## Backend

- **Go**-based backend.
- Uses **protobuf** for serialisation.
- Uses the Go **standard library `net/http`** packages — no web framework
  (Gin, Echo, etc.) is needed.

### Client ↔ backend transport: WebSockets
- The backend communicates with the frontend over WebSockets.
- Library: [`coder/websocket`](https://github.com/coder/websocket) — minimal
  and idiomatic WebSocket library for Go.
- Rationale for choosing it over the Gorilla version:
  [coder/websocket vs Gorilla](https://websocket.org/guides/languages/go/).

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
- **Native watchers (`kv.Watch`)**: the Go gateway (Pusher) and the LiveKit
  adapter can simply watch KV keys. As soon as a user clicks a button to make
  a zone exclusive, the Go server writes to the KV. Instantly, without any
  polling, the watcher pushes the information to the Phaser client (which
  applies the graphic filter) and to the LiveKit bridge (audio subscriptions
  are cut off).
- **Compare-And-Swap (CAS)**: during concurrent modifications (e.g. two bots or
  two users trying to modify the same property of an object at the same time),
  NATS guarantees atomic writes without complex locks.
- **History**: JetStream KV preserves the revision history of a key. You can
  know exactly which user modified the properties of a zone and when — a
  sought-after audit feature in enterprise environments.
- **Fault resilience**: if the game-logic service (Go backend) restarts, it
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

> Note: the durable data store for accounts, world metadata, persistent
> inventory, audit logs, etc. is **not yet specified**. See open question in
> `04-architecture.md` and the to-be-created `06-data-model-and-persistence.md`.

## Frontend: Phaser

- **Phaser v4** is used for the frontend.
- Rationale: see `Why Phaser.md`.
