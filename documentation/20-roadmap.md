# Roadmap

> **Status:** skeleton. A living document; phases and ordering will evolve.

This document sketches the phased plan. The MVP scope is defined in
`01-vision-and-goals.md`; everything below "MVP" is explicitly out of MVP
scope but the architecture must not preclude it.

---

## MVP

The must-have base experience: a shared spatial pixel-art office with
positional audio/video.

- Server-authoritative spatial simulation (Pusher + World Simulator, see
  `09-pusher.md`, `10-world-simulator.md`). The World Sim is the spatial
  authority and replication gateway; all gameplay behavior is delegated to
  extensions.
- ECS core (`13-ecs-design.md`), component-based replication
  (`11-replication.md`), netcode (`12-netcode.md`).
- Trigger system (access: block/allow/ask; event: notify) — the mechanism
  by which extensions declare spatial rules on tiles and entities
  (`18-extensions.md` §3a).
- Zones incl. exclusive zones and knock-to-join meeting rooms
  (`14-zones-and-interactions.md`) — implemented as extension-owned triggers
  on zone boundary tiles.
- First-party extension pack (walls, doors, base zone behaviors) as sibling
  processes in Docker Compose (`18-extensions.md`).
- Proximity audio/video via LiveKit (`19-livekit.md`) + Bridge.
- Tiled maps (`15-maps-and-tiled.md`), composable avatars (`16-avatars.md`).
- Chat via PocketBase (`17-chat.md`).
- Auth via Dex, local-password connector first (`08-auth-and-identity.md`).
- Extension system (`18-extensions.md`) — the modularity foundation.
- Self-host via Docker Compose, single host (`03-non-functional-requirements.md`).

> **Note:** the extension system and first-party extension pack are in the MVP
> because they are the architectural foundation (modularity). The kernel has no
> gameplay logic except player avatar movement — even walls and doors are
> extension-driven. NPCs/AI built *on* extensions are post-MVP.

---

## Post-MVP (architecture must not preclude)

From `01-vision-and-goals.md` and `02-functional-requirements.md` § 8:

- **AI / NPC agents** built as extensions (LLM-driven characters).
- **Horizontal scale**: multiple World Sim shards (per-map/per-region),
  cross-shard visibility (`10-world-simulator.md` § 6).
- **Organizations**: an `organizations → worlds → maps` layer (deferred; MVP
  is single-world, no org layer).
- **Matrix Synapse chat** for federation / rich clients / E2EE
  (`17-chat.md`).
- **Map-wide / zone-wide A/V broadcast** via object triggers.
- **Plant growth**, **user inventories**, **owned workplaces with
  leave-a-message**, **whiteboard objects**.
- **Mobile client**; **multi-region**.
- **HA deployment** (beyond single-host).
- **Production hardening**: NATS auth, extension KV ACLs, rate limits
  (`03-non-functional-requirements.md` § 7).
- **Operations**: observability stack, backup/restore (future
  `21-operations.md`).

---

## Open questions

- **[OPEN] Phase boundaries** — group post-MVP items into ordered milestones.
- **[OPEN] First post-MVP target** — likely NPC extensions or horizontal
  scaling, depending on early adopter needs.
