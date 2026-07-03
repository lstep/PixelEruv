# Vision and Goals

## Project goal

Build a multiuser, open-source web application: a top-down pixel-art MMORPG-style
virtual world (in the spirit of Gather.town, ZEP, Workadventu/re) aimed at
companies that need to bring remote workers together and hold video meetings.

That base experience — a shared spatial office with positional video/audio — is
the essential, must-have scope of the project.

## Architectural north star

The project must be **modular** so that NPCs (bots or AI agents) can be added in
the future, and so that objects can carry generic characteristics and react to /
interact with users. To achieve this, the codebase will be built around an
**Entity-Component-System (ECS)** rather than a classic object-oriented class
hierarchy. See `13-ecs-design.md` and the current
`13-ecs-design.md` for the rationale.

## Why this project

- **Open source**: existing players (Gather, ZEP) are proprietary and hosted;
  companies with strict data-residency or self-hosting requirements have no
  credible open alternative.
- **Modular / extensible**: the ECS core makes it cheap to add new object types,
  triggers, and AI behaviours without forking the engine.
- **Self-hostable, no Kubernetes**: deployable with Docker Compose only, so a
  small IT team can run it for a single company without a platform-engineering
  budget.

## Out of scope (for the MVP)

The following ideas are captured in `02-functional-requirements.md` under
"Extra features" and are **not** part of the MVP; they are listed so the
architecture does not preclude them:

- AI / NPC agents
- Plant growth over time
- User inventories
- Owned workplaces with leave-a-message
- Whiteboard objects
- Map-wide / zone-wide audio-video broadcast via object triggers

A phased roadmap will be defined separately (`20-roadmap.md`).

## Success criteria

A company can docker compose up and have a working spatial office with positional A/V.
100 concurrent users on one modest host at ≤150ms p95 input round-trip, ≤40 KB/s downstream.
The extension system works well enough that walls/doors/zones are extension-driven (not kernel).
It is credibly "the open-source alternative" to Gather/ZEP/Workadventure.