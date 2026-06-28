---
creation date: 2026-06-26 08:37
modification date: 2026-06-27 18:00
---
# Functional Requirements

This document lists what the system must **do**. Non-functional targets
(scale, latency budgets, browser support) will be captured separately in
`03-non-functional-requirements.md` (to be created).

## 1. Zones and interactions

### 1.1 Zone definition
- Zones are **polygon-defined** regions on a map.
- Each zone has typed characteristics, e.g.: water, exclusive zone, work zone,
  silent zone, owner, etc.
- Zone isolation and policy enforcement **must be performed by the backend**
  for security reasons; the client only renders the result.

### 1.2 Exclusive zones (dynamic)
- Exclusive zones can be **activated/deactivated dynamically**.
- Example: a zone of type `exclusive` is defined around a room. The room has a
  door. If the door is **open**, the zone is automatically deactivated. If the
  door is **closed**, the zone is activated.

### 1.3 Knock-to-join meeting rooms
- Some zones are "meeting rooms". The first user to enter becomes the owner.
- A second user trying to enter is **prevented** from crossing the zone
  boundary.
- The prevented entry triggers a knock / notification to the owner.
- The owner receives a popup asking whether to allow the other user to join.
- The owner can also **directly invite** another user to join via a popup.

### 1.4 Audio/video isolation
- Isolation must also apply to video/audio (LiveKit SFU): people outside an
  exclusive zone must not be able to hear or see video from inside it.

### 1.5 Interaction routing
- Routing of interactions between triggers and other components (zones, doors,
  audio bridges, etc.) is done via **NATS pub/sub topics**, including wildcard
  topics. See `07-network-protocol.md` (to be created) for the subject naming
  convention.

## 2. Zone of Interest

- To optimise loading and network traffic, **zones of interest** must be
  defined so that each client only receives state for entities/events within
  its area of interest.
- Algorithm (grid / quadtree / distance-based) is to be specified in
  `09-zones-and-interactions.md` (to be created).

## 3. Maps

- Maps are designed in **Tiled**.
- Tile size: **32×32**.
- Asset sources:
  - https://limezu.itch.io/moderninteriors
  - https://limezu.itch.io/modernoffice
- Objects can be **traversable or not** according to characteristics that can
  be changed dynamically (from the backend by an event, or by triggers).
- **Object placement relative to a tile**: objects must be placeable relative
  to a tile, not only in the centre. Front/back positions must be supported so
  that visual draw order can be determined. **This is important and needs more
  detail** — see `10-maps-and-tiled.md` (to be created).
- **Multi-layer** support: a single tile can carry multiple characteristics
  (block, trigger, etc.) via stacked layers.
- **Sit / sleep interactions**: sitting on a chair changes the avatar's status
  and sprite; sleeping in a bed is supported, etc.
- **Avatar movement sprites**: idle sprites with respiration animation; walking
  speed can be changed dynamically by characteristics (e.g. a button trigger).
  An avatar can be **associated with another entity** (car, bike, vehicle).
- **Animated decorations**: fire in a fireplace with sound, clock with alarm /
  cuckoo sound, screens flickering or changing content.
- **Proximity audio**: proximity increases/decreases the sound of objects and
  of other users' speech.

## 4. Avatars

- Each user (and each NPC) is represented by an avatar.
- The avatar is visually composed of **composable elements**: body shape,
  colour, hairs, clothes.
- Wire format for describing an avatar to the server is to be specified in
  `11-avatars.md` (to be created).

## 5. Chat

- A chat is integrated so users can interact.
- Candidate backend: **Matrix Synapse** (undecided — "maybe").
- Open questions to resolve in `14-chat.md` (to be created):
  - Spatial chat vs. global rooms vs. direct messages.
  - How chat integrates with zones (e.g. a zone-scoped channel per meeting
    room).

## 6. Netcode / latency compensation

A complete latency-compensation netcode protocol is required:

- **Client-side prediction + server reconciliation** for the local avatar, so
  input feels instantaneous.
- **Snapshot interpolation (LERP)** for remote avatars, to smooth movement
  despite ephemeral packet loss in NATS Core.
- **Dead reckoning / extrapolation** in case of disconnection or prolonged
  packet loss.

Detailed spec (tick rate, input sequence numbers, snapshot frequency,
reconciliation algorithm, extrapolation timeout thresholds) will live in
`08-netcode.md` (to be created).

## 7. Authentication and identity

- A **generic auth** layer is created first.
- Later, an **OIDC provider** is plugged in (Dex IDP, Keycloak, or simpler).
- Details (token validation on the WS upgrade, identity → avatar/entity
  mapping, NPC/service-account auth, session lifecycle vs. Traefik sticky
  sessions) will be specified in `12-auth-and-identity.md` (to be created).

## 8. Extra features (not MVP)

These are captured so the architecture does not preclude them, but they are
**out of MVP scope**:

- Broadcast video/audio to a whole map or whole zone / list of zones via an
  object trigger.
- Plants that grow with time (and with water given through interaction).
- Users can have inventory objects (carry and use / interact with them).
- Workplaces that can be owned and where one can leave messages to the owner.
- Whiteboard object that displays a whiteboard.
