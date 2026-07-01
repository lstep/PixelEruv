# Chat

> **Status:** decision recorded, detailed spec to follow.

This document specifies text chat. It resolves the previously-open
"Matrix vs. PocketBase" question.

---

## Decision: PocketBase for the MVP

**[DECISION] The MVP uses a PocketBase `messages` collection for chat history.
Matrix Synapse is deferred to the post-MVP roadmap (`20-roadmap.md`).**

Rationale:

- **One less service.** Matrix Synapse requires its own PostgreSQL instance
  and operational overhead; PocketBase is already in the stack for durable
  data.
- **Sufficient for the MVP.** Spatial/zone/DM chat with history and presence
  is well within PocketBase's capabilities.
- **Reversible.** The chat surface is small; migrating to Matrix later (for
  federation, rich clients, E2EE) is a contained change. The data model below
  maps cleanly onto Matrix rooms if/when that happens.

---

## Data model (PocketBase `messages` collection)

See `06-data-model-and-persistence.md` § 3.

| Field | Type | Notes |
|---|---|---|
| `room_id` | string | `zone.<zone_id>`, `map.<map_id>`, or `dm.<entity_a>.<entity_b>` |
| `sender_id` | relation → `users` | |
| `body` | string | Message text |
| `sent_at` | datetime | Server timestamp |

---

## Chat scopes

| Scope | `room_id` form | Behavior |
|---|---|---|
| **Spatial / proximity** | `map.<map_id>` (AOI-filtered) | Messages visible to nearby users |
| **Zone-scoped** | `zone.<zone_id>` | A channel per meeting room / zone |
| **Direct message** | `dm.<a>.<b>` | Private 1:1 |

> **[OPEN]** Whether spatial chat is truly proximity-filtered (AOI) or simply
> per-map. Ties into the AOI work in `14-zones-and-interactions.md`.

## Transport

- Chat messages are sent as an `InteractFrame` (or a dedicated chat frame) over
  the existing WebSocket (see `07-network-protocol.md`), routed through the
  World Simulator, which persists to PocketBase and replicates to recipients.
- Incoming messages may also drive **speech bubbles** on the sender's avatar
  (see `16-avatars.md`, `Bubble` component).

## Open questions

- **[OPEN] Spatial filtering** — AOI-based vs. per-map.
- **[OPEN] Chat wire frame** — reuse `InteractFrame` or add a `ChatFrame` in
  `07-network-protocol.md`.
- **[OPEN] Moderation / retention** — message retention policy, moderation
  tools (post-MVP).
- **[OPEN] Bubble integration** — does every chat message pop a bubble?
