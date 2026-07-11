# Non-Functional Requirements

This document captures the **quality targets** the system must meet: scale,
latency, availability, browser support, security posture, and resource
budgets. Functional behavior is in `02-functional-requirements.md`.

> **Status:** initial targets. Numbers marked **[PROPOSED]** are starting
> points to validate during load testing; they are not yet committed. They
> exist so that design decisions (tick rate, AOI algorithm, sharding) have
> concrete numbers to optimize against.

---

## 1. Scale targets

| Metric | MVP target | Notes |
|---|---|---|
| Concurrent users per world | **[PROPOSED]** 100 | A single virtual office for one company |
| Concurrent users per World Sim shard | **[PROPOSED]** 100 | MVP ships a single shard; sharding is post-MVP |
| Entities per shard (incl. objects, NPCs) | **[PROPOSED]** 2,000 | Avatars + interactive objects + decorations + extension entities |
| Concurrent users per Pusher instance | **[PROPOSED]** 250 | Pushers scale horizontally behind Traefik |
| Extensions per world | **[PROPOSED]** 20 | Each a separate NATS-connected process |
| Maps per world | 10–50 | Static Tiled maps |

The MVP must run comfortably on a **single modest host** (e.g. 4 vCPU / 8 GB
RAM) running the full Docker Compose stack for ~100 users. Horizontal scaling
(multiple Pushers, sharded World Sims) is a post-MVP concern but the
architecture must not preclude it.

---

## 2. Latency budgets

The end-to-end "input → see result" budget for the local player is dominated
by client-side prediction (which makes local movement feel instant). The
budgets below are for **server-authoritative** round-trips.

| Path | Target (p95) | Notes |
|---|---|---|
| Local avatar movement (predicted) | **0 ms perceived** | Client-side prediction; reconciled against server (see `12-netcode.md`) |
| Input → server applies → reflected to other clients | **[PROPOSED]** ≤ 150 ms | Client → Pusher → NATS → World Sim → tick → NATS → Pusher → client |
| Server tick interval | **[PROPOSED]** 50 ms (20 Hz) | See § 3 |
| Zone state change → visual filter applied | **[PROPOSED]** ≤ 200 ms | Extension writes KV → World Sim (kv.Watch) → replication → client |
| Interaction → extension reply → client sees result | depends on extension | LLM extensions may take 0.5–2 s; UI must show a pending state |
| Audio/video glass-to-glass | LiveKit defaults | Out of our control; LiveKit + coturn |

> **Geographic assumption:** the MVP assumes all users and the server are in
> the **same region** (a self-hosted company deployment). Multi-region is out
> of scope.

---

## 3. Server tick rate

**[DECISION] The World Simulator runs at a fixed 20 Hz (50 ms tick) for the
MVP.**

Rationale:

- 20 Hz is sufficient for top-down avatar movement smoothed by snapshot
  interpolation on the client (see `12-netcode.md`).
- It leaves CPU headroom for more entities per shard than a 30/60 Hz loop.
- The interval is **configurable** — load testing may raise it to 30 Hz if
  movement feels insufficiently smooth, or lower it if entity counts demand
  more headroom.

The tick rate is the single most important number for sizing the rest of the
system (replication bandwidth, NATS throughput, CPU). It is fixed early so
other specs can reference a concrete value.

---

## 4. Bandwidth budget (per client)

At 20 Hz, with AOI limiting each client to entities it can see:

| Quantity | **[PROPOSED]** target |
|---|---|
| Entities in a typical AOI | ≤ 50 |
| Per-entity `UpdateComponent` size (Position) | ~20–30 bytes (protobuf) |
| Replication batch size (typical tick) | ≤ 2 KB |
| Downstream bandwidth per client (movement-heavy) | ≤ 40 KB/s |
| Upstream bandwidth per client (input) | ≤ 5 KB/s |

The AOI filter (see `11-replication.md` § 3.3 and `14-zones-and-interactions.md`)
is the primary mechanism keeping per-client bandwidth bounded regardless of
total world population.

---

## 5. Availability and recovery

| Requirement | Target |
|---|---|
| Pusher restart | Clients reconnect (sticky session) within seconds; no data loss (Pusher holds no durable state) |
| World Sim restart | Reconstructs state from JetStream KV; brief replication pause, clients stay connected |
| NATS restart | The bus is critical; a short outage pauses simulation. JetStream KV persists across restarts |
| PocketBase restart | Only affects login/provisioning and audit; in-flight sessions unaffected |
| Extension crash | Isolated; its entities freeze or despawn (see `18-extensions.md`), rest of world unaffected |
| Data durability | PocketBase volume + JetStream KV persisted to disk; backups are an ops concern (deployment doc, future) |

The MVP targets **single-host availability** — no HA clustering. Acceptable
downtime for restarts/upgrades is a few seconds to a minute. HA is post-MVP.

---

## 6. Browser and client support

| Requirement | Target |
|---|---|
| Browsers | Latest 2 versions of Chrome, Edge, Firefox, Safari |
| Rendering | WebGL 2 (Phaser 4 GPU layers); graceful message if unsupported |
| WebRTC | Required for audio/video (LiveKit); TURN-over-TCP-443 fallback via coturn |
| WebSocket | Required for game state; no long-poll fallback |
| Mobile | **[OPEN]** Not an MVP target; the architecture (thin client, server-authoritative) does not preclude a future mobile client |
| Minimum viewport | **[PROPOSED]** 1024×768 |

---

## 7. Security posture

| Requirement | MVP | Production (post-MVP) |
|---|---|---|
| All external traffic over TLS | ✅ (Traefik + Let's Encrypt) | ✅ |
| OIDC token validation at the boundary | ✅ (Pusher, see `08-auth-and-identity.md`) | ✅ |
| Internal services not reachable from outside | ✅ (Docker network) | ✅ |
| NATS authentication | ❌ (trusted Docker network) | ✅ (NATS accounts/JWT, per-extension creds) |
| Extension KV write ACLs | ❌ (unrestricted) | ✅ (namespace-scoped, see `18-extensions.md` § 14) |
| Secrets management | Env vars / Docker secrets | Docker secrets / vault |
| Audit logging | ✅ (PocketBase `audit_log`) | ✅ |

The MVP's threat model assumes a **trusted internal network** (single-company
self-host). Hardening (NATS auth, KV ACLs, rate limits) is documented as
future work and the design does not preclude it.

---

## 8. Resource budgets (single-host MVP)

**[PROPOSED]** rough allocation for a 4 vCPU / 8 GB host at ~100 users:

| Service | CPU | RAM |
|---|---|---|
| World Simulator (incl. embedded PocketBase) | 1–2 vCPU | 1–2 GB |
| Pusher | 0.5 vCPU | 0.5 GB |
| NATS (Core + JetStream) | 0.5 vCPU | 1 GB |
| LiveKit SFU | 1 vCPU | 1 GB |
| Redis (LiveKit) | 0.25 vCPU | 0.25 GB |
| coturn, Traefik, Dex, SeaweedFS/RustFS | shared remainder | ~1 GB total |

These are starting points to validate with load testing, not hard limits.

---

## 9. Observability (requirement, spec deferred)

The system is distributed (Pusher × N, World Sim × N (with embedded
PocketBase), Bridge, extensions, NATS, LiveKit). The following are **required** for operability;
the detailed strategy is deferred to a future ops/deployment document:

- **Structured logging** with a correlation ID per client session
  (`client_id`) traceable across Pusher → NATS → World Sim.
- **Metrics**: tick duration, entities per shard, replication batch sizes,
  NATS lag, per-client bandwidth, extension heartbeat health.
- **Health endpoints** for each service (for Docker/Traefik health checks).

> **[OPEN]** Observability stack (Prometheus/Grafana? OpenTelemetry?) is not
> chosen. Tracked for a future `21-operations.md`.

---

## Open questions

- **[OPEN] Tick rate validation** — confirm 20 Hz vs 30 Hz after load testing.
- **[OPEN] Concurrent-user ceiling per shard** — the 100/shard figure is a
  guess; profile to find the real limit and the sharding trigger point.
- **[OPEN] Mobile client** — out of MVP scope; revisit in the roadmap.
- **[OPEN] Observability stack** — choose tooling; document in a future ops doc.
- **[OPEN] Backup/restore strategy** — for PocketBase and JetStream KV;
  document in a future ops doc.
