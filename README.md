# Pixel Eruv

![Pixel Eruv](assets/gh-banner.jpg)

## The open-source virtual office platform

Pixel Eruv is an open-source platform for building persistent, pixel-art virtual workspaces where distributed teams can meet, collaborate, and communicate naturally.

Inspired by platforms like Gather, ZEP and WorkAdventure, but designed from the ground up as a modern, extensible platform,Pixel Eruv combines the familiarity of a top-down multiplayer world with enterprise collaboration tools.

Instead of switching between chat applications, video meetings and shared documents, teams interact inside a shared virtual office where communication is driven by proximity and collaboration happens naturally.

✨ Features

* 🗺️ Persistent multiplayer pixel-art worlds
* 🎙️ Proximity-based audio and video powered by LiveKit
* 💬 Real-time chat and presence
* 🏢 Multiple organizations, offices and maps
* 🔐 Enterprise authentication (OIDC/OAuth2)
* 🔌 Plugin architecture for integrations and extensions
* 🤖 AI assistants and MCP integration
* 🎨 Tiled map support
* ⚡ High-performance Go backend
* 🌐 Self-hostable and cloud-friendly

🏗️ Architecture

Pixel Eruv is built around a server-authoritative architecture inspired by modern multiplayer games.

The backend is responsible for world simulation, entity replication, permissions and persistence, while the frontend combines a Phaser 4 renderer for the virtual world.

Audio, video and screen sharing are delegated to LiveKit, allowing the simulation engine to remain focused on the virtual environment.

Core technologies include:

* Go for backend services
* Phaser 4 for world rendering
* LiveKit for media
* Protocol Buffers for networking
* Redis for persistence
* NATS for internal event distribution

🚀 Philosophy

Pixel Eruv is not just another Gather clone.

It is a platform for building spatial collaboration applications.

The virtual office is only one possible client. The same backend can power desktop applications, mobile clients, AI agents, accessibility-focused interfaces, or entirely different visualizations of the same shared world.

By separating simulation, communication and presentation, Pixel Eruv aims to become the open foundation for the next generation of collaborative software.

🌱 Project Status

Pixel Eruv is currently in active design and early development.

Contributions, ideas, discussions and architectural feedback are welcome as the project evolves.

📚 Documentation

The project documentation includes:

* Architecture overview


🤝 Contributing

Pixel Eruv is an open-source community project.

Whether you’re interested in Go, React, Phaser, networking, distributed systems, game development, UI/UX or documentation, contributions are welcome.

Together, let’s build the open infrastructure for spatial collaboration.
