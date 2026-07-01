# Pixel Eruv

![Pixel Eruv](assets/gh-banner.jpg)

## The open-source virtual office platform

Pixel Eruv is an open-source platform for building persistent, pixel-art virtual workspaces where distributed teams can meet, collaborate, and communicate naturally.

Inspired by platforms like Gather, ZEP and WorkAdventure, but designed from the ground up as a modern, extensible platform, Pixel Eruv combines the familiarity of a top-down multiplayer world with enterprise collaboration tools.

Instead of switching between chat applications, video meetings and shared documents, teams interact inside a shared virtual office where communication is driven by proximity and collaboration happens naturally.

✨ Features (MVP)

* 🗺️ Persistent multiplayer pixel-art worlds (single world per deployment)
* 🎙️ Proximity-based audio and video powered by LiveKit
* 💬 Real-time chat and presence (PocketBase-backed)
* 🏢 Offices and maps within a world
* 🔐 Authentication via Dex (OIDC; local-password connector first, enterprise connectors later)
* 🎨 Tiled map support
* 🔌 Extension system — add NPCs, custom behaviors, and objects as separate programs in any language
* ⚡ High-performance Go backend
* 🌐 Self-hostable via Docker Compose (no Kubernetes required)

🧭 Roadmap (post-MVP)

* 🤖 AI assistants and NPC agents (built on the extension system)
* 🏢 Multi-organization support (organizations → worlds → maps)
* 💬 Matrix Synapse chat (federation, rich clients, E2EE)
* 🌱 Plant growth, user inventories, owned workplaces, whiteboard objects

🏗️ Architecture

Pixel Eruv is built around a server-authoritative architecture inspired by modern multiplayer games.

The backend is responsible for world simulation, entity replication, permissions and persistence, while the frontend combines a Phaser 4 renderer for the virtual world.

Audio, video and screen sharing are delegated to LiveKit, allowing the simulation engine to remain focused on the virtual environment.

Core technologies include:

* Go for backend services
* Phaser 4 for world rendering
* LiveKit for media
* Protocol Buffers for networking
* PocketBase for durable data (users, world config, audit logs)
* NATS for internal event distribution and reactive state
* SeaweedFS / RustFS for asset storage

🚀 Philosophy

Pixel Eruv is not just another Gather clone.

It is a platform for building spatial collaboration applications.

The virtual office is only one possible client. The same backend can power desktop applications, mobile clients, AI agents, accessibility-focused interfaces, or entirely different visualizations of the same shared world.

By separating simulation, communication and presentation, Pixel Eruv aims to become the open foundation for the next generation of collaborative software.

🌱 Project Status

Pixel Eruv is currently in active design and early development.

Contributions, ideas, discussions and architectural feedback are welcome as the project evolves.

📚 Documentation

The project documentation lives in [`documentation/`](documentation/). Start
with the [system overview](documentation/00-system-overview.md) for the big
picture, then dive into the detailed specs:

* [00 — System overview](documentation/00-system-overview.md) — high-level architecture, start here
* [01 — Vision and goals](documentation/01-vision-and-goals.md) — project purpose and MVP scope
* [02 — Functional requirements](documentation/02-functional-requirements.md) — what the system must do
* [03 — Non-functional requirements](documentation/03-non-functional-requirements.md) — scale, latency, availability targets
* [04 — Tech stack](documentation/04-tech-stack.md) — technology choices and rationale
* [05 — Architecture](documentation/05-architecture.md) — detailed component wiring and data flows
* [06 — Data model and persistence](documentation/06-data-model-and-persistence.md) — where data lives and why
* [07 — Network protocol](documentation/07-network-protocol.md) — WebSocket frames and NATS subjects
* [08 — Authentication and identity](documentation/08-auth-and-identity.md) — OIDC, tokens, identity mapping
* [09 — Pusher](documentation/09-pusher.md) — WebSocket proxy service
* [10 — World Simulator](documentation/10-world-simulator.md) — authoritative simulation service
* [11 — Replication](documentation/11-replication.md) — how game state reaches clients
* [12 — Netcode](documentation/12-netcode.md) — prediction, reconciliation, interpolation *(skeleton)*
* [13 — ECS design](documentation/13-ecs-design.md) — entity-component-system rationale and decisions
* [14 — Zones and interactions](documentation/14-zones-and-interactions.md) — zones, knock-to-join, AOI *(partial)*
* [15 — Maps and Tiled](documentation/15-maps-and-tiled.md) — map authoring and layering *(skeleton)*
* [16 — Avatars](documentation/16-avatars.md) — avatar composition and bubbles *(skeleton)*
* [17 — Chat](documentation/17-chat.md) — text chat (PocketBase-backed)
* [18 — Extensions](documentation/18-extensions.md) — peer-simulator extension system
* [19 — LiveKit](documentation/19-livekit.md) — audio/video integration
* [20 — Roadmap](documentation/20-roadmap.md) — MVP and post-MVP phases
* [A1 — Why Phaser 4](documentation/A1-why-phaser.md) — engine choice rationale (appendix)
* [A2 — Existing applications](documentation/A2-existing-applications.md) — prior-art survey (appendix)


🤝 Contributing

Pixel Eruv is an open-source community project.

Whether you’re interested in Go, Phaser, networking, distributed systems, game development, UI/UX or documentation, contributions are welcome.

Together, let’s build the open infrastructure for spatial collaboration.
