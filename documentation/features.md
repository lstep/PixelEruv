# Pixel Eruv — Feature Catalogue

A catalogue of the features that make Pixel Eruv work, organized for
reuse in presentations (slides) and video walkthroughs. Each feature
includes a short description and a **Storyboard** section with concrete
visual cues — what to show on screen, in what order, to demonstrate the
feature.

> **How to use this file as a storyboard:** each section is a potential
> slide or video segment. The description is the narration script. The
> Storyboard notes are the camera/screen directions. Group sections under
> Part headings to build a narrative arc.

---

## Part 0 — Why Pixel Eruv Is Different

Pixel Eruv sits in the same category as Gather.town, ZEP, and
Workadventu/re — top-down pixel-art virtual offices with proximity
audio. But it is built from a different set of principles. The sections
below are the differentiators that matter when choosing a platform.

### 0.1 Open Source — Expandable and Fast-Evolving

Gather, ZEP, and Workadventu/re are proprietary and hosted. You pay a
per-seat subscription, your data lives on someone else's servers, and
you cannot audit or modify the code. Your feature requests go into a
backlog you can't see, and you wait for a vendor roadmap you can't
influence.

Pixel Eruv is open source. That changes three things:

- **You own the code.** Self-host with Docker Compose — no Kubernetes,
  no platform-engineering team. Your data stays on your infrastructure.
  Audit every line. Modify anything. No vendor lock-in, no per-seat
  cost.
- **It's expandable.** Pixel Eruv is built around an extension system
  that separates the kernel (spatial authority, replication) from all
  gameplay behavior. The kernel moves players and replicates state —
  everything else (walls, doors, NPCs, zone access policies,
  interactive objects, custom game mechanics) lives in extensions.
  Extensions are peer processes on the NATS message bus, not plugins
  loaded into the engine. Anyone can write one, and it can be written
  in any language that has a NATS client: Go, Python, Rust, Node, Java,
  C#, and more. An extension runs in its own container, crashes
  independently without taking down the world, and can be restarted or
  hot-swapped without touching the kernel. The first-party extensions
  (walls, props, audio/video bridge) use the exact same API as any
  third-party extension — there is no privileged layer. Combined with
  the ECS core and the generic replication protocol, this means new
  components and new gameplay systems flow through the system without
  protocol changes or engine forks. The community can build features
  the core team never imagined.
- **It evolves fast.** Proprietary platforms ship on a vendor's
  schedule. An open-source project ships on the community's schedule.
  Contributions, fixes, and features land as fast as people write
  them. Every deployment can run the latest code — or fork it. The
  roadmap is public, the decisions are documented, and the architecture
  is designed to absorb new ideas without rewrites.

**Storyboard:** Show a side-by-side table: Gather (proprietary, hosted,
per-seat, vendor roadmap) vs Pixel Eruv (open source, self-hosted, flat
cost, community-driven). Show `make up` in a terminal — the full stack
starts. Show the GitHub repo with the open issue list and pull requests.
Narrate: "your office, your servers, your code. No subscription, no
vendor lock-in, no waiting for someone else's roadmap."

### 0.2 Server-Authoritative, Not Client-Trusted

Gather and Workadventu/re run game logic in the browser — the client
decides where your avatar is and tells the server. This is fast to build
but insecure: a modified client can teleport, walk through walls, or
spoof position. Pixel Eruv is server-authoritative. The World Simulator
owns the tile grid, the spatial index, collision, and zone access. The
client predicts movement locally for responsiveness, but the server
rejects anything that violates the rules.

**Storyboard:** Show the architecture diagram with the WorldSim as the
authority. Show a browser dev console attempting to send a bogus
position — the server corrects it. Narrate: "the client renders; the
server decides. You can't cheat position, collision, or zone access."

### 0.2b Why We Designed It This Way — Architecture Rationale

The server-authoritative model with client-side prediction is not
something we invented. It is the standard architecture for
fast-paced multiplayer games, documented over two decades of game
networking literature. We followed it because the alternatives
(frontend-authoritative, relay-only) are simpler to build but
fundamentally insecure and harder to extend.

**The authoritative-server principle.** The core idea is simple:
don't trust the client. The server owns the game state, validates
every input against the real rules, and sends the result back.
Clients send inputs, not positions. This prevents teleporting,
speed-hacking, and wall-clipping — a modified client can render
whatever it wants, but the server's state is what other players see.

This principle and the techniques built on top of it are explained
clearly in two sources that shaped our design:

- **Gabriel Gambetta's "Fast-Paced Multiplayer" series**
  (https://www.gabrielgambetta.com/client-server-game-architecture.html)
  — a four-part series covering authoritative servers, client-side
  prediction with server reconciliation, entity interpolation, and lag
  compensation. Parts I and II directly informed our architecture:
  the worldsim is the authoritative server (Part I), and the frontend
  predicts movement locally then reconciles against the server's
  authoritative position using sequence numbers (Part II). Part III
  (entity interpolation for remote avatars) is tracked in
  [issue #107](https://github.com/lstep/PixelEruv/issues/107). Part
  IV (lag compensation for shooting) is not applicable — Pixel Eruv
  has no projectile system (yet). We are grateful to Gabriel for making
  these concepts accessible to a wide audience.

- **Glenn Fiedler's "Gaffer on Games" series**
  (https://gafferongames.com/) — the foundational reference on
  game networking, covering fixed timesteps, deterministic
  simulation, state synchronization, and reliability over UDP.
  Fiedler's work on why the server must own the simulation at a
  fixed tick rate (not variable, not client-driven) shaped our
  decision to run worldsim at a fixed 20Hz tick and to make the
  client's prediction match the server's movement math exactly
  (same speed, same diagonal normalization, same collision
  algorithm). Without this determinism, reconciliation would
  produce constant snap-backs — the client and server would
  disagree on where the player is even with identical inputs.

**Why not frontend-authoritative like Workadventu.re?** In a
frontend-authoritative model, the browser owns the player's position
and sends it to the server, which broadcasts it to other clients.
This is simpler — no prediction, no reconciliation, no server-side
movement simulation. But it has a fatal flaw: the server has no way
to reject invalid positions. Any client can send "I'm at (0,0)" or
"I moved 50 tiles in one frame" and the server must accept it. For
a casual virtual office this may be acceptable. For a platform that
aims to support interactive gameplay (item pickups, currency, access
control, NPC interactions), it is not. The server must be the
authority, or the rules are unenforceable.

**Why client-side prediction?** A naive authoritative server is
unresponsive — the player presses a key, waits 100-200ms for the
round-trip, then sees the movement. Client-side prediction
(Gambetta Part II) eliminates this: the client applies the input
locally and immediately, then reconciles when the server's
authoritative position arrives. The player sees instant movement;
the server still validates everything. The cost is complexity —
the client must track un-acked inputs and replay them on
reconciliation — but the result is a game that feels local while
being server-authoritative.

**Why a fixed 20Hz tick?** The server processes all clients in
batches at a fixed rate, not per-input. This bounds CPU usage,
makes the simulation deterministic, and gives the client a
predictable reconciliation cadence. Fiedler covers this in depth:
variable-rate server updates make interpolation and prediction
unreliable because the client can't reason about how much time
the server has simulated. A fixed tick (50ms) lets the client
convert its frame delta into fractional ticks and predict at the
same rate the server simulates.

**Storyboard:** Show Gambetta's Part II diagram (client predicts,
server confirms, client reconciles) side by side with Pixel Eruv's
code: the `pendingInputs` buffer, the `lastInputSeq` ack, and the
reconciliation loop in `GameScene.ts`. Then show the worldsim tick
loop at 20Hz. Narrate: "this isn't a custom protocol — it's the
standard multiplayer architecture, applied to a virtual office.
The server owns the world; the client predicts for responsiveness
and reconciles against authority. The same techniques that make
Counter-Strike feel responsive while preventing cheats make Pixel
Eruv responsive while preventing position spoofing."

### 0.3 ECS Core — Modular by Design

Other platforms use object-oriented class hierarchies. Adding a new
object type (a chair you can sit on, a door that locks, an NPC that
talks) means modifying the engine or working around it. Pixel Eruv is
built on an Entity-Component-System: entities are empty containers,
components are pure data, systems query by component set. New object
types are new components — no engine fork, no class hierarchy changes.

**Storyboard:** Show a code snippet adding a new component type (e.g.
`LightState`) to the registry. Show that the replication protocol, the
network layer, and the renderer don't change. Narrate: "add a
component, register it, and it replicates. The engine doesn't need to
know your object exists."

### 0.4 Extensions in Any Language

Gather has a scripting API, but it runs inside Gather's hosted
environment — JavaScript, sandboxed, limited. Pixel Eruv extensions are
peer processes on the NATS bus. Any language with a NATS client works:
Go, Python, Rust, Node, Java, C#. An LLM-driven NPC can call a Python
inference API. A patrol system can use Rust for performance. A custom
zone policy can be a 50-line Node script. Extensions run in their own
containers, crash independently, and hot-reload without touching the
kernel.

**Storyboard:** Show four terminal windows: ext-walls (Go), ext-props
(Go), ext-av (Go), and a hypothetical Python NPC extension. Kill the
Python extension — the world keeps running. Restart it — the NPC
resumes. Narrate: "the kernel doesn't care what language your extension
is written in. It only sees NATS messages."

### 0.5 Self-Service Authentication from Day One

Gather and ZEP use their own hosted auth systems. Pixel Eruv ships with
PocketBase's built-in authentication: email/password registration with
email verification, password reset, and OAuth2 social login (Google,
GitHub, Facebook). Users self-register — no admin needs to create
accounts. PocketBase is embedded in the World Simulator as a Go library,
so there is no separate identity service to deploy or maintain. The
Pusher validates JWTs by calling the PocketBase API, keeping the auth
flow simple and self-contained.

**Storyboard:** Show the registration page — enter email and password.
Show the verification email arriving in MailHog. Click the link, log in,
and enter the world. Then show the OAuth2 config (just environment
variables) and switch to Google login. Narrate: "users register
themselves, verify by email, and log in. Social login is a config
change — no separate identity service."

### 0.6 The Kernel Has No Gameplay Logic

In Gather, walls, doors, and zone behaviors are built into the engine.
If you want a door that opens on a schedule, you work within Gather's
feature set or you can't. In Pixel Eruv, the kernel handles only
spatial authority and replication. Walls, doors, zone access, light
switches, NPCs — all are extensions. Even the first-party walls and
props ship as sibling processes, not compiled into the kernel. This
means the extension API is the same for first-party and third-party
code: there is no "privileged" gameplay layer.

**Storyboard:** Show the kernel source tree — point out the absence of
door, wall, or NPC code. Show ext-walls and ext-props as separate
binaries. Narrate: "the kernel moves players and replicates state.
Everything else — including walls — is an extension. You have the same
API we do."

### 0.7 Component-Based Replication Protocol

Most multiplayer platforms define a message type per action: `PlayerMoved`,
`DoorOpened`, `NPCStateChanged`. Every new feature needs a new message
type, new serialization, new client and server handlers. Pixel Eruv
uses four generic messages — `SpawnEntity`, `UpdateComponent`,
`DestroyEntity`, `PlayAnimation` — that operate on any entity and any
component. New entity types and new components are registered, not
wired into the protocol. The wire format hasn't changed since the first
commit, and it won't need to for new features.

**Storyboard:** Show the four message types. Add a new component
(`PlantGrowth`) to the registry. Show that no protobuf changes, no
protocol changes, no client handler changes are needed — the component
data flows through `UpdateComponent` automatically. Narrate: "the
protocol is generic. New features don't touch the wire."

### 0.8 Observable by Default

Gather and ZEP are black boxes — you get their logs, not your traces.
Pixel Eruv is instrumented with OpenTelemetry from the browser to the
worldsim. `make debug` starts a local collector (motel) with a TUI that
shows full trace trees: WebSocket receive, NATS publish, worldsim tick,
collision check, replication encode, NATS forward, WebSocket send. When
something is slow, you see exactly which hop is slow.

For production, the stack ships with **OpenObserve** — a single-binary
OTel backend with a SQL search UI at `/otel/`. All services (including
the four extensions) export traces and logs to it when `OTEL_ENABLED=true`.

A standalone **audit service** records lifecycle and interaction events
(player connections, bans, chat messages, zone transitions, extension
registrations, map reloads, A/V token minting) to its own SQLite
database and serves a searchable web UI at `/audit/`. Each audit event
carries an optional trace ID that links to the corresponding trace in
OpenObserve — audit tells you *what* happened, OTel tells you *why*.

**Storyboard:** Run `make debug`. Show the motel TUI with a trace tree
for a single player movement. Point at each span and its duration.
Then open `/audit/` in a browser — show the dashboard with event
severity counts, the recent events table, and a player timeline. Click
a `trace_id` link — it opens the trace in OpenObserve. Narrate: "no
guessing. Every hop is traced, every event is recorded. You see the
exact millisecond the worldsim spent on collision, and you can search
the full history of who did what."

### 0.9 Easy Branding and Customization

Gather and ZEP let you upload a logo and pick a color theme. That's
where customization ends — you're decorating someone else's product.
Pixel Eruv is yours, so customization goes as deep as you want:

- **Maps are Tiled files.** Design your office, campus, conference
  hall, or expo in Tiled — upload the JSON and tileset PNGs to
  PocketBase. No code, no rebuild. The worldsim auto-seeds a default
  map on first boot, and replacing it is a record edit.
- **Sprites are PocketBase records.** Upload your own character
  spritesheets to the `sprite_bases` collection and they appear in the
  character select screen immediately — no rebuild, no redeploy.
- **Zones and objects are map data.** Brand a zone with your company
  name, mark it `av_enabled` for a meeting room, or `zone_type=wall`
  for collision — all authored in Tiled, all read at load time.
- **Extensions define behavior.** A company could write an extension
  that displays their internal status board, a receptionist NPC that
  greets visitors with the company name, or a custom zone policy that
  matches their org chart. The extension API is the same one the
  first-party extensions use.
- **Identity is PocketBase.** Users self-register with email/password
  or log in via Google, GitHub, or Facebook (OAuth2). No separate
  identity service to deploy — PocketBase is embedded in the World
  Simulator.

An enterprise or association can have a fully branded virtual space —
custom map, custom sprites, custom interactions, OAuth2 social login —
without writing engine code or forking the repo.

**Storyboard:** Show a default Pixel Eruv office. Then show the same
deployment with a custom Tiled map (a branded lobby with the company
logo as a decoration), custom character sprites (employees in company
colors), and OAuth2 configured for Google login. Narrate: "logo, map,
characters, behavior, login — all yours. No fork, no vendor, no
per-seat branding fee."

---

## Part 0b — Use Cases

Pixel Eruv is a spatial collaboration platform, not just a virtual
office. The same backend — server-authoritative world, proximity A/V,
extension system, ECS core — powers any scenario where people (or AI
agents) need to share a persistent 2D space and communicate by
proximity. Below are concrete use cases, each with a storyboard.

### 0b.1 Casual Team Hangout

A distributed team drops into a shared pixel-art office instead of a
Slack channel. People walk to each other's desks to ask a quick
question. Proximity audio means you only hear the people near you —
no cross-talk, no mute-all meetings. Walk away, the conversation ends
naturally. No scheduling, no links, no "can you see my screen."

**Storyboard:** Show a team of 5 in the office. Two walk to a corner
and talk — their video tiles appear. Three others are at their desks,
working quietly. Someone walks over to the pair — a three-way
conversation starts. Walk away — audio fades and drops. Narrate:
"spontaneous, like a real office. No meeting link, no calendar invite."

### 0b.2 Company All-Hands and Town Halls

A company meeting in a virtual auditorium. The map has a stage zone
with A/V enabled. The speaker stands on stage — everyone in the zone
sees and hears them. A moderator extension could control who can
"take the mic" (gate the stage zone). Questions from the floor: walk
to a designated Q&A zone to be heard. The chat panel carries the
parallel text conversation.

**Storyboard:** Show a custom auditorium map with a stage. 30 avatars
seated in rows. The speaker walks on stage — their video tile is
prominent. A questioner walks to the Q&A mic zone — their audio
unmutes. The chat panel scrolls with side conversations. Narrate:
"one world, one meeting, no Zoom fatigue — and the side conversations
happen naturally, not in a separate chat window."

### 0b.3 Virtual Conference and Expo

A multi-room conference event. Each room is a zone with its own A/V
room — walk in, you join the conversation; walk out, you leave. Booths
are interactive objects (ext-props) — press E to see a product demo or
collect a virtual flyer. A schedule board extension could display the
next talks. Sponsors get custom-branded booths as map decorations.

**Storyboard:** Show a conference map with 5 rooms, each labeled. Walk
into "Room A" — a talk is in progress, video tiles appear. Walk out,
into the expo hall — booths line the walls. Walk to a booth, press E —
a popup with product info appears. Walk to another room for the next
session. Narrate: "a full conference in a pixel-art world. Rooms,
booths, demos — all by walking around."

### 0b.4 Art Gallery and Exhibition

A curated virtual exhibition. The map is a gallery with artworks as
interactive objects — walk up to a painting, press E, and a popup
shows the artist's statement. A guided tour mode (extension) could
have an NPC guide walk a predetermined path, narrating each piece via
proximity audio. Visitors wander at their own pace; artists can be
present for live Q&A in front of their work.

**Storyboard:** Show a gallery map with paintings on the walls. Walk
to a painting, press E — the artwork details appear. An NPC guide
walks a path and stops at each piece — visitors follow. An artist
stands next to their installation, answering questions from visitors
who walk up. Narrate: "a gallery you can walk through, not scroll
through. The art is spatial — you approach it, you stand with it."

### 0b.5 Event Space — Workshops and Breakout Sessions

A workshop event with a main room and breakout rooms. The main room
has a presenter; breakout rooms are smaller zones where subgroups
collaborate. A facilitator extension could manage room assignments
(walk into breakout room 3, the extension checks your name against a
list). Whiteboard objects (roadmap) could let groups sketch together
in-zone.

**Storyboard:** Show a workshop map with a main room and 4 breakout
rooms. The presenter speaks in the main room. A facilitator announces
"breakout time" — participants walk to their assigned rooms. Each
room has its own A/V conversation. Walk back to the main room to
regroup. Narrate: "breakouts by walking, not by clicking links and
waiting for Zoom to load."

### 0b.6 Environment for AI Agents to Inhabit

Pixel Eruv is not just for humans. The extension system means AI
agents can be first-class inhabitants of the world. An NPC extension
written in Python connects to an LLM API, spawns an entity, and drives
its behavior: a receptionist that greets visitors, a tour guide that
walks new employees around, a bartender that makes small talk, a
security guard that patrols. The kernel treats the AI entity like any
other — it has a position, a sprite, a name tag, and it replicates to
all clients. Human players interact with AI agents through the same
proximity and input mechanisms they use with each other.

**Storyboard:** Show an office with a receptionist NPC near the
entrance. A player walks in — the NPC greets them by name (the
extension received the zone.enter event with the player's entity ID).
The player types a question in chat — the NPC responds. Show the
Python extension process in a terminal: LLM API call, response,
NATS publish. Narrate: "the AI lives in the same world as the humans.
Same space, same rules, same interactions — the kernel doesn't know
it's not a person."

### 0b.7 Persistent Virtual HQ for a Remote Company

A company's permanent virtual headquarters. Employees drop in
throughout the day — the world is always running. Desks are
personalized (owned workplaces, roadmap). The day/night overlay shifts
with the real clock — you can tell it's evening because the world is
tinted dusk. Departments have their own zones. A "water cooler" zone
for casual chat. An "on-call" zone for the engineering team. Position
persistence means your avatar is where you left it yesterday.

**Storyboard:** Show the HQ map at 9 AM — morning tint, a few
avatars at their desks. Fast-forward to noon — several avatars
gathered in the kitchen zone for lunch chat. Fast-forward to 6 PM —
dusk tint, most avatars gone, one still at a desk. Narrate: "it's not
a meeting you join and leave. It's a place that's always there. Your
desk is yours. The sun sets when it sets."

### 0b.8 Educational Campus and Classrooms

A virtual campus with classrooms, a library, and common areas. Each
classroom is a zone with A/V — the teacher is heard by everyone in the
room. The library is a silent zone (no A/V). Students can break into
study groups in smaller rooms. An assignment-board extension could
display deadlines. A bell extension could ring at scheduled times.
Guest mode lets prospective students tour without an account.

**Storyboard:** Show a campus map with 4 classrooms, a library, and a
lounge. A teacher speaks in classroom A — students seated, video tiles
visible. Walk to the library — a "silent zone" sign, no A/V. Walk to
the lounge — students chatting casually. A guest walks in and tours
without logging in. Narrate: "a campus that's always open. Silent
zones, classrooms, social spaces — all by design, all by walking."

---

## Part 1 — The World

### 1.1 Persistent Pixel-Art Multiplayer World

A top-down, tile-based virtual office rendered with Phaser 4. Players
move with arrow keys or WASD. Each browser tab is a player. The world
persists between sessions — player positions restore on reconnect from
PocketBase.

**Storyboard:** Open the browser. Show the office map with a character
standing in it. Press arrow keys — the character walks in four
directions with directional walk animations. Open a second tab — a
second character appears. Both move independently.

### 1.2 Tiled Map Support

Maps are authored in [Tiled](https://www.mapeditor.org/) and exported as
JSON. Tile size is 32×32. The worldsim auto-seeds a default office map
on first startup from the `maps/` directory — no manual upload needed
for a fresh deploy. Maps can be replaced or added via the PocketBase
admin dashboard. Worldsim loads all maps from PocketBase on
startup. Players transition between maps via **portal zones**
(`zone_type=portal` with `target_map` and optional `target_entity`) —
walk into a portal and the client loads the destination map.

**Storyboard:** Show the Tiled editor with the office map open. Switch
to the browser running Pixel Eruv — the same map renders. Point at the
tile layers (ground, walls, decorations) and the object layers (zones,
entities).

### 1.2a Easy Map Design Workflow

Designing a custom world for Pixel Eruv requires no code and no engine
knowledge. The entire workflow happens in Tiled — a free, visual map
editor — and the PocketBase admin dashboard. A designer or office
manager can create a new space in an afternoon:

1. **Draw the map in Tiled.** Place tiles from any 32×32 pixel-art
   tileset (the project ships with Limezu's Modern Interiors / Modern
   Office sets). Layer ground, walls, and decorations. The editor is
   drag-and-drop — no scripting, no JSON editing by hand.
2. **Mark zones on the "Zones" object layer.** Draw rectangles,
   ellipses (circles), or polygons where you want rooms, walls, or
   A/V-enabled spaces. Set custom properties like `zone_type=wall` for
   collision or `av_enabled=true` for proximity rooms. The worldsim
   reads these at load time.
3. **Place interactive objects on the "Entities" object layer.** Add
   objects with `entity_type=light_switch` (or any type your
   extensions handle) and `owner_extension=ext-props`. Players press
   E near them to interact.
4. **Tag decoration layers.** Add the custom property
   `layer_type=decoration` to any layer that should Y-sort against
   avatars. Set `sort_mode=dynamic` for tall objects that should
   occlude and be occluded by players.
5. **Export as JSON.** File → Export As → JSON. Upload the JSON and
   the tileset PNGs to the PocketBase `maps` collection via the admin
   dashboard. Reload the browser — the new map is live.

No rebuild, no redeploy, no code change. A non-developer can design,
upload, and play a custom map. The bundled `default-map.json` and the
`maps/` directory serve as a reference — copy it, modify it in Tiled,
and upload.

**Storyboard:** Open Tiled with a blank map. Drag tiles from the
Limezu tileset to draw a floor, walls, and some furniture. Draw a
rectangle on the Zones layer, set `zone_type=wall`. Draw another, set
`av_enabled=true`. Export as JSON. Open the PocketBase admin dashboard,
upload the JSON + tileset PNG. Reload the browser — the custom map is
live, walls block movement, the A/V room works. Narrate: "no code, no
deploy. Draw, export, upload, play."

### 1.3 Decoration Layers and Depth Sorting

Map layers with the custom property `layer_type=decoration` are
recognized as decoration layers. A per-layer `sort_mode` property
(`static` or `dynamic`) controls how they Y-sort against avatars.
Dynamic decorations share the avatar depth band and sort by their base
Y, so tall objects (trees, pillars) can occlude or be occluded by the
player as they walk past.

**Storyboard:** Walk the character behind a tall decoration (tree,
pillar). The decoration covers the character's upper body. Walk in
front of it — the character covers the decoration. The depth order
shifts smoothly as the Y position crosses.

### 1.4 Day/Night Overlay

A purely cosmetic, client-side full-screen rectangle tints the game
world based on the browser's local clock. Color and alpha interpolate
between 8 time-of-day keyframes (deep night, dawn, morning, noon,
afternoon, dusk, evening) and recalculate once per minute. Alpha is
capped at 0.44 so the map stays readable. Keyframes are configurable
via `setKeyframes()` / `getKeyframes()` and persist in localStorage.

**Storyboard:** Show the world at noon — no tint. Scrub the system clock
forward to 18:00 — a warm dusk tint fades in. Continue to 21:00 — a
deep blue evening tint. Continue to midnight — the world is dark blue
but still readable. Show the keyframe table and how custom keyframes
change the look.

### 1.5 Name Tags

Each player's display name renders as a bitmap-text tag floating above
their avatar. Names come from the replicated `DisplayName` component —
the server stamps them, the client never authors them directly. Name
tags follow the avatar each frame and sit at a fixed pixel offset above
the sprite. A green status dot on the left of the name is clickable and
opens a small info dropdown panel.

The dropdown content depends on the viewer. Regular users see a
placeholder line ("Hello world"). Admins see the player's IP address
and a short device ID, delivered via a dedicated NATS channel that
carries an `AdminInfoFrame` with every player's IP, device ID, guest
status, and user ID — the data only reaches admin clients. Both
regular and admin viewers see an "Invite" button; admins additionally
see a "Ban" button. These buttons are stubs — they show "Not
implemented yet" when clicked. Wiring the ban button to a server-side
ban command is a planned future task.

Logged-in players also have their IP address and last-seen timestamp
persisted in the `players` collection.

**Storyboard:** Two characters on screen, each with a name tag above.
Walk one character around — the tag follows. Point out that guests get
a generated name and logged-in users get their PocketBase display name.
Click the green dot on a name tag — a dropdown panel appears with
"Hello world" and an "Invite" button. Switch to an admin account —
click the same dot — the dropdown now shows the player's IP and device
ID, plus "Invite" and "Ban" buttons. Click elsewhere — the dropdown
closes. Switch back to a regular account — no IP or device ID is
visible.

### 1.6 Mobile Support with Virtual Joystick

Pixel Eruv works on touch devices. On phones and tablets, a floating
virtual joystick replaces the keyboard for movement. Touch and drag
anywhere in the left portion of the screen — a joystick base and thumb
appear at the touch point. The thumb vector is thresholded into the
same up/down/left/right booleans the keyboard uses, with a deadzone for
8-directional movement. Release the finger and the avatar stops. The
joystick is touch-gated, so desktop mouse and keyboard are unaffected.
The viewport is locked to prevent pinch-zoom and browser scroll from
interfering with the drag.

**Storyboard:** Open the client on a phone (or Chrome DevTools mobile
emulation with touch enabled). Touch and hold the lower-left area —
the joystick circles appear centered on the finger. Drag up — the
avatar walks up. Drag diagonally — the avatar moves diagonally. Release
— the avatar stops and the circles disappear. Switch to desktop —
keyboard arrows and mouse wheel zoom work as before.

---

## Part 2 — Communication

### 2.1 Proximity-Based Audio and Video

Audio and video are powered by LiveKit. When two players walk within a
2-tile radius of each other, the ext-av extension mints a LiveKit token
and both clients join the same room. Walk apart, and a 1.5-second
debounce timer delays the leave so momentary zone exits don't thrash
the connection. Audio volume scales with distance — the closer another
player is, the louder their voice.

**Storyboard:** Two characters far apart — no video tiles, no audio.
Walk them toward each other. At proximity range, video tiles appear in
the top bar for both players. Walk them apart — tiles persist briefly
(debounce), then disappear. Walk them back — tiles reappear without a
re-prompt.

### 2.2 Spatial Audio with Distance-Based Volume

Each remote participant's audio volume is adjusted per tick based on
their distance from the local player. The GameScene computes a 0–1
volume factor and calls `AvClient.setParticipantVolume()`, which sets
the LiveKit track volume. Close players are loud; distant players fade.

**Storyboard:** Two players within proximity range. Walk one player
farther away (still in range) — their audio fades. Walk closer — it
rises. Narrate: "volume is not binary — it tracks distance in real
time."

### 2.3 Speaking Indicators

Each frame, the VideoBar polls speaking state from the LiveKit SDK. The
active speaker's video tile gets a green border. This works for both
the local player and remote participants.

**Storyboard:** Two players in a proximity room. One speaks — their
tile border turns green. Stop speaking — border reverts. The other
speaks — their border lights up instead.

### 2.4 Video Bar with Resizable Tiles

A fixed-position horizontal bar of participant video tiles sits below
the top menu. Tiles wrap to additional rows when they overflow the
available width. A draggable handle below the tiles resizes the entire
bar — all tiles scale together. The preferred tile height persists in
localStorage across reloads. Tile order is stable: local player first,
then others by join order.

**Storyboard:** Show the video bar with 3–4 participants. Drag the
resize handle down — tiles grow. Drag it up — tiles shrink. Reload the
page — the preferred size is restored. Add enough participants to
overflow one row — tiles wrap to a second row.

### 2.5 Mic and Camera Controls

The top menu has mic and camera toggle buttons. Muting uses
`pauseUpstream` / `resumeUpstream` on the LiveKit track (not the SDK's
mute flag), so the track stays published but stops sending media. The
mute/camera state persists in localStorage across reconnects.

**Storyboard:** Click the mic button — it toggles to "muted." Click
again — it resumes. Click the camera button — video tile goes dark.
Click again — video returns. Reload — the previous states are restored.

### 2.6 Device Selection

The top menu dropdown has mic and camera `<select>` dropdowns populated
from `enumerateDevices()`. Selected device IDs persist across room
reconnects. Switching devices mid-call calls `switchDevice()`, which
replaces the published track with the new device.

**Storyboard:** Open the dropdown. Show the mic dropdown with two
devices listed. Select a different mic — the audio source switches
without reconnecting. Do the same for the camera.

### 2.7 Noise Cancellation

WebRTC client-side noise cancellation (`noiseSuppression` +
`echoCancellation` + `autoGainControl`) is an explicit, persisted option
(defaults on). Toggling it restarts the mic track mid-call so the change
takes effect without reconnecting. When disabled, all three flags are
explicitly set to `false` to override the SDK's `true` defaults.

**Storyboard:** Show the noise cancellation toggle in the menu. Turn it
off — the mic track restarts. Turn it on — it restarts again. Narrate:
"background noise, echo, and gain are all handled in the browser."

### 2.8 Browser Autoplay Unlock

Browsers block audio playback without a user gesture. On first page
click, a silent 1-byte WAV plays to unlock Safari's autoplay policy. If
audio is still blocked after joining a room, a red "Enable Audio" button
appears in the top menu — clicking it calls `room.startAudio()` within
the user gesture.

**Storyboard:** Open a fresh tab. Show the "Enable Audio" button
appearing after proximity join. Click it — remote audio starts playing.
On subsequent interactions, no button is needed.

### 2.9 Cross-Browser Audio Compatibility

The LiveKit SDK enables `audio/red` (Redundant Audio Data) by default.
Safari cannot decode `audio/red` — only `audio/opus`. The AvClient
forces `publishDefaults: { red: false }` so all published audio tracks
use `audio/opus`, making Chrome-published audio audible on Safari.
Remote audio tracks are explicitly attached to hidden `<audio>` elements
on `TrackSubscribed` — LiveKit does not auto-attach them.

**Storyboard:** Show a Safari window and a Chrome window on different
machines. Both players walk into proximity range. Both hear each other.
Narrate: "this required disabling RED and manually attaching audio
elements — two non-obvious fixes."

### 2.9a Cross-Browser Testing

The full feature set — proximity video, spatial audio, speaking
indicators, mic/camera controls, device selection, text chat, movement,
and the day/night overlay — has been tested and works on Chrome,
Safari, and Firefox. Mixed-browser calls (e.g. Chrome + Safari on
different machines) are confirmed working: both sides publish and
receive audio and video, with speaking indicators and distance-based
volume functioning correctly.

**Storyboard:** Show three browser windows side by side: Chrome, Safari,
and Firefox. All three characters walk into proximity range. Video
tiles appear in all three. Each participant speaks — the green speaking
border lights up on all sides. Narrate: "three browsers, three
machines, one proximity room — everything works."

### 2.10 Text Chat

A DOM sidebar fixed to the right edge of the window with two tabs:
Global and Nearby. Messages are sent as `ChatFrame` over the existing
WebSocket, routed through the worldsim, which stamps the display name
and timestamp. The server persists messages to PocketBase. The panel
toggles via the Chat button in the top menu.

**Storyboard:** Click the Chat button — the sidebar slides in. Type a
message in Global — it appears for all players. Switch to Nearby —
show messages only from players in proximity range. Type in Nearby —
only nearby players see it.

### 2.11 Screen Sharing

Players can share their screen with everyone in their current A/V group
(zone or proximity). A "Screen" toggle button in the top menu calls
`setScreenShareEnabled`, which uses `getDisplayMedia` with system audio
capture enabled. The shared screen is published to the same LiveKit room
as audio/video, so no backend changes are needed — the existing ext-av
token grants already allow publishing any track type.

The shared screen appears as a floating, draggable, resizable DOM window
with three modes: windowed (default, with a bottom-right resize handle),
fullscreen, and minimized (small thumbnail). Multiple simultaneous screen
shares are supported — each gets its own cascaded window.

A visibility relay hooks the `MediaStreamTrack`'s `mute`/`unmute` events
(fired by the browser when the source window is minimized or hidden) and
propagates them via LiveKit's `track.mute()`/`track.unmute()`. Without
this, remote viewers would see a frozen black frame with no indication.
With the relay, a "Screen share paused" overlay appears instead.

**Storyboard:** Two browser tabs in the same zone. Click "Screen" in tab
one — the browser's screen picker appears. Select a window — a floating
window appears in tab two showing the shared content. Drag the window by
its title bar — it moves. Drag the bottom-right corner — it resizes.
Click "Fullscreen" — it fills the screen. Click "Minimize" — it shrinks
to a thumbnail. Click the thumbnail — it restores. Minimize the shared
window on the sharer's side — the remote viewer shows "Screen share
paused." Click "Stop" — the floating window disappears in the remote
viewer.

---

## Part 3 — Architecture

### 3.1 Server-Authoritative Simulation

The World Simulator is the spatial authority. It owns the ECS, the tile
grid, the spatial index, the trigger registry, and the replication
pipeline. The only gameplay system in the kernel is player avatar
movement — everything else is delegated to extensions. The Pusher is a
thin WebSocket proxy that validates PocketBase JWTs and forwards frames
between the browser and the worldsim via NATS.

**Storyboard:** Show the architecture diagram (NATS at the center,
Pusher and WorldSim on either side, extensions as peers). Highlight the
one-way data flow: browser → Pusher → NATS → WorldSim → NATS → Pusher
→ browser. Narrate: "the browser never talks to the worldsim directly."

### 3.2 Entity-Component-System Core

All entities — players, NPCs, doors, props — live in the same ECS
(Ark for Go). Components are pure data; systems are algorithms that
query entities by component set. New entity types are added by defining
new components, not by modifying class hierarchies.

**Storyboard:** Show a simple diagram: Entity (empty container) +
Components (Position, Appearance, DisplayName, Bubble). Show how adding
a `Traversable` component to a chair makes it walkable, and removing it
makes it block movement — no code change to the chair "class."

### 3.3 Component-Based Replication

The replication protocol uses four generic message types: `SpawnEntity`,
`UpdateComponent`, `DestroyEntity`, and `PlayAnimation`. New entity
types and new components are added by registering them in the component
registry — the replication code and the wire protocol do not change.
Each client receives only entities within its area of interest (AOI).

**Storyboard:** Show the four message types as boxes. Add a new
component type (e.g. `LightState`) to the registry — show that no
replication code changes. Show two clients: one near a light switch,
one far — only the near client receives the `UpdateComponent` for the
light.

### 3.4 Client-Side Prediction and Reconciliation

The local avatar moves immediately on input (prediction). Each input
carries a sequence number. The server echoes the last processed sequence
in each replication batch. On receipt, the client replays un-acked
inputs against the authoritative position (reconciliation). This makes
movement feel instant despite a server-authoritative ~150ms round-trip.

**Storyboard:** Show the character moving with no visible lag. Add
artificial latency (e.g. 200ms via dev tools). The character still
moves smoothly — then briefly rubber-bands if prediction was wrong
(e.g. walking into a wall the client didn't know about). Narrate:
"input feels instant; the server corrects only when prediction was
wrong."

### 3.5 Remote Avatar Smoothing

Remote avatars use exponential smoothing (tau = 80ms) toward the latest
replicated position. This masks jitter from packet loss in NATS Core
without adding visible lag. The smoothing target updates on every
replication batch.

**Storyboard:** Show two characters on screen. One is controlled
remotely (another tab). Move the remote character — it glides smoothly
to each new position instead of teleporting. Drop some packets (or add
jitter) — the movement stays smooth.

### 3.6 Two-Tier Collision System

Collision uses two systems, both evaluated at the avatar's feet point:
(1) a walls tile-layer grid point check, and (2) wall zones tested as
swept segment-vs-shape, expanded by a collision radius. The swept test
catches walls thinner than the per-tick movement distance (0.4 tiles),
preventing tunneling.

**Storyboard:** Show the character walking up to a wall. It stops. Show
a thin decorative barrier — the character still can't pass through it.
Narrate: "the segment test catches walls that point-sampling would
miss." Show the client-side prediction matching the server exactly — no
rubber-band when walking along walls.

### 3.7 Zone Triggers and Extensions

Zones are first-class kernel objects with shapes (rect, circle,
polygon) and mobility (static or mobile). Extensions register triggers
on zones: gate triggers (block/allow/ask) control movement, notify
triggers fire enter/exit events. The kernel caches block/allow locally
and routes ask gates to the owning extension via NATS. When multiple
gate zones overlap, block-wins: if any returns block, movement is
refused.

**Storyboard:** Show a meeting room zone on the map. Walk the character
toward it — the door is closed, movement is blocked (gate trigger from
ext-walls). Open the door (ext-props handles the key:E input) — the
gate switches to allow. Walk in. Narrate: "the kernel doesn't know what
a door is — the extension decided."

### 3.8 Extension System

Extensions are peer processes on the NATS bus, written in any language
with a NATS client. They own all gameplay behavior: NPC logic, trigger
logic, zone access policy, interactive objects. The kernel validates
physics and access rules; it does not decide what extensions can do.
A crashed extension freezes its entities but does not take down the
worldsim. First-party extensions (walls, doors, props, A/V) ship as
sibling processes in Docker Compose — the same API as third-party
extensions.

**Storyboard:** Show four terminal windows, one per extension process
(ext-walls, ext-props, ext-av, ext-demo). Kill ext-props — light
switches stop responding, but the world keeps running, players keep
moving, audio keeps working. Restart ext-props — switches work again.
Narrate: "extensions are isolated. The kernel never crashes when an
extension does."

### 3.8.1 ext-walls — Wall Collision Extension

Reads the Tiled map from PocketBase, finds zones with `zone_type=wall`,
and registers block gate triggers on them. The kernel caches these as
`block` and refuses movement locally without a NATS round-trip.

**Storyboard:** Show the Tiled map with wall zones highlighted. Show
the ext-walls log: "registered N wall zones." Walk the character into a
wall — it stops instantly (no round-trip).

### 3.8.2 ext-props — Interactive Objects Extension

Registers for the `key:E` input trigger. When a player presses E near
an entity it owns (e.g. a light switch), it toggles the entity's state
and replicates the change with a `PlayAnimation` event. It never reads
the map — the worldsim's input dispatch includes entity metadata, so
the extension self-filters.

**Storyboard:** Walk the character next to a light switch. Press E —
the switch toggles, a lamp turns on/off. Show the ext-props log
receiving the input event and replying. Narrate: "the kernel broadcast
the input; the extension decided what to do."

### 3.8.3 ext-av — Audio/Video Bridge Extension

Bridges zone and proximity events to LiveKit. It mints LiveKit tokens
and publishes them to clients via NATS (forwarded as `AvTokenFrame` by
the pusher). It reads the Tiled map to find zones with the `av_enabled`
property. It handles both zone-scoped rooms (A/V-enabled zones) and
ad-hoc proximity rooms (2-tile radius).

**Storyboard:** Show the ext-av log. Two players walk into an
A/V-enabled zone — the log shows "minting token for room X." Both
clients receive the token and join the LiveKit room. Walk out — the log
shows "leave event" after the debounce.

### 3.8.4 ext-demo — Minimal Extension Template

A minimal extension that registers with the worldsim, sends heartbeats,
and logs zone enter/exit events. Serves as a starting point for new
extensions.

**Storyboard:** Show the ext-demo source code (it's short — under 100
lines). Show the log output: "registered," "heartbeat," "zone.enter:
player1 entered office." Narrate: "this is all you need to write an
extension."

---

## Part 4 — Identity and Access

### 4.1 Self-Service Registration

Users register themselves — no admin needs to create accounts. The
registration form asks for email, password, and password confirmation.
On submit, PocketBase creates the `users` record and sends a
verification email. Until the email is verified, the user can log in
but is marked as unverified. The Pusher validates the JWT on WebSocket
upgrade by calling the PocketBase API.

PocketBase is embedded in the World Simulator as a Go library — there
is no separate identity service to deploy or maintain.

**Storyboard:** Show the welcome page with a "Register" link. Click it,
fill in email and password. Submit — a toast says "check your email."
Narrate: "no admin involvement. Users create their own accounts."

### 4.2 Email Verification

After registration, PocketBase sends a verification email with a
confirmation link. In dev, MailHog captures the email — open
`http://<host>:8025` to view it. Click the link and the account is
verified. The `APP_URL` environment variable controls the base URL in
the email link, so it works behind reverse proxies and remote hosts.

**Storyboard:** After registering, open MailHog in a second tab. Show
the verification email. Click the link — a PocketBase page confirms
verification. Go back to the app and log in. Narrate: "verification
emails work out of the box — MailHog in dev, your SMTP server in
production."

### 4.3 Password Reset

Users who forgot their password can request a reset link from the
login page. PocketBase sends an email with a time-limited token. The
user clicks the link, enters a new password, and logs in. No admin
intervention required.

**Storyboard:** Show the login page. Click "Forgot password?" Enter the
email. Show the reset email in MailHog. Click the link, enter a new
password. Log in with the new password. Narrate: "self-service password
reset — no admin support tickets."

### 4.4 OAuth2 Social Login

Users can log in or register with Google, GitHub, or Facebook instead
of email/password. Each provider is enabled by setting two environment
variables on the worldsim service (client ID and secret). Leave them
empty to disable. When a social login is used, PocketBase creates or
links the `users` record automatically — no separate registration step.

**Storyboard:** Show the login page with "Log in with Google" and
"Log in with GitHub" buttons. Click Google — redirect to Google's
consent screen. Approve — redirect back to the app, logged in. Narrate:
"social login is two env vars per provider. No application code, no
separate identity service."

### 4.5 Token Validation

The Pusher validates PocketBase JWTs on WebSocket upgrade by calling
the PocketBase API. This delegates signature verification and expiry
checks to PocketBase itself, ensuring the token is still valid and not
revoked. The token is delivered as the first WebSocket message (AUTH
frame) — not as a URL query parameter — to keep it out of nginx access
logs.

**Storyboard:** Show the browser dev console — the WebSocket connection
opens, the first frame is an AUTH frame with the JWT. Show the Pusher
logs validating the token. Narrate: "the token never appears in a URL.
The Pusher validates it against PocketBase — no JWKS caching, no key
rotation to manage."

### 4.6 Character Selection

Logged-in users who haven't picked a sprite see a character select
scene before joining the world. It displays the PocketBase-backed
sprite catalog as clickable thumbnails, each showing a walk-cycle
preview. On confirm, the chosen sprite ID is sent to the worldsim via
`SetSpriteBaseFrame`. Guests skip this scene and get a default sprite.

**Storyboard:** Log in as a new user. The character select screen
appears with a grid of sprite thumbnails. Click one — it highlights.
Click confirm — transition to the game scene with the chosen character.
Log in as a guest — go straight to the game with a default sprite.

### 4.7 Guest Mode

Unauthenticated users can join as guests with a generated username and
a default sprite. Guests skip the character select scene. This lets
people try the world without creating an account.

**Storyboard:** Open the app without logging in. A guest username
appears in the top menu. The character spawns with a default sprite.
Walk around, talk to people — everything works except persistence
(positions are not restored for guests).

### 4.8 Ban System

Pixel Eruv supports a three-layer ban system so admins can block
griefers by whichever identifier is most effective for the situation:

- **User ID** (`user_id`): the strongest identifier. Bans a
  logged-in user's PocketBase account. Evading it requires creating a new
  account, which is real friction.
- **IP address**: coarse but immediate. Stops a griefer right now.
  Can have collateral damage on shared IPs (NAT, household), so use
  judiciously.
- **Device ID**: a client-generated UUID stored in the browser's
  `localStorage`, sent in the `AuthFrame` on every connection. Stable
  across sessions for the same browser. Evadable by clearing storage
  or using incognito mode, but catches casual griefers. Useful when
  IP-banning would hit innocent users on the same network.

Bans are stored in a PocketBase `bans` collection and checked by the
world simulator during entity provisioning, before the player enters
the world. Both temporary bans (with a unix timestamp expiry) and
permanent bans are supported. Admins are exempt — a player with the
`is_admin` flag can always connect, regardless of any ban record
matching their identifiers.

When a banned client connects, the pusher sends the ban reason and
expiry to the browser, then closes the WebSocket. The client displays
the ban message in the disconnect overlay and does not attempt to
reconnect.

Ban records are currently issued via the PocketBase admin dashboard
by adding a row to the `bans` collection with a `target_type`
(`user_id`, `ip`, or `device_id`), `target_value`, `reason`, and
optional `banned_until` timestamp. An in-game admin ban command is
planned as a follow-up.

**Storyboard:** Open the PocketBase admin dashboard. Add a ban record
targeting a guest's device ID (visible in the admin name tag pillbox).
Switch to the banned guest's browser — reload the page. The disconnect
overlay appears with the ban reason and "permanently" or an expiry
date. The client does not reconnect. Remove the ban record in the
admin dashboard — reload the guest's browser — the world loads
normally. Narrate: "three ways to ban, checked before the player even
enters the world. Admins are always exempt."

---

## Part 5 — Operations

### 5.1 Self-Hostable via Docker Compose

The entire stack runs with a single `make up` command — no Kubernetes,
no platform engineering. NATS, MailHog, LiveKit, the Pusher, the WorldSim
(with embedded PocketBase), and all extensions start as Docker Compose
services. A self-contained `dist/` directory can be copied to any host
and run without source code.

**Storyboard:** Show a terminal. Type `make up`. Show the Docker
Compose logs scrolling — each service starting. Open the browser to
`localhost:4080` — the world is live. Narrate: "one command, one host,
no cluster."

### 5.2 Single-Variable Remote Configuration

One environment variable — `PUBLIC_HOST` — drives everything remote
browsers need: the TLS cert SAN, the email verification URL (`APP_URL`),
and the LiveKit public URL. Set it to the host's LAN IP or hostname and
rebuild.

**Storyboard:** Show `PUBLIC_HOST=192.168.1.10 make up`. Open a browser
on another machine to `https://192.168.1.10:4043`. Accept the self-signed
cert. The world loads, auth works, audio works — all from one variable.

### 5.3 OpenTelemetry Observability

The backend (pusher, worldsim) and all four extensions are instrumented
with OpenTelemetry traces and logs. Telemetry is off by default. `make
debug` starts NATS, motel (a local OTel collector with a TUI), the
pusher, and the worldsim with `OTEL_ENABLED=true`. Traces span the full
request path: browser → pusher → NATS → worldsim → replication →
pusher → browser.

For production, the Docker Compose stack includes **OpenObserve** — a
single Rust binary that serves as the OTel backend. Set
`OTEL_ENABLED=true` on any service to ship its traces and logs to
OpenObserve at `http://openobserve:5080/api/default`. The UI is
available at `/otel/` through the nginx proxy.

A standalone **audit service** complements OTel by recording lifecycle
and interaction events (connections, bans, chat, zone transitions,
extension registrations, map reloads, A/V tokens) to its own SQLite
database. The audit UI at `/audit/` provides a dashboard, searchable
event table, event detail view with trace deep-links, and per-player
timeline. See
[`documentation/plans/2026-07-12-audit-observability-design.md`](plans/2026-07-12-audit-observability-design.md)
for the full design.

**Storyboard:** Run `make debug`. Show the motel TUI with a trace tree
for a player movement: WebSocket receive → NATS publish → worldsim
tick → collision check → replication encode → NATS publish → WebSocket
send. Then open `/audit/` — show the dashboard, filter events by type,
click through to a player timeline. Narrate: "every hop is traced, every
event is recorded. You can see exactly where time is spent and search
the full history of who did what."

### 5.4 Auto-Seeding on First Boot

On first startup, the worldsim auto-seeds the sprite catalog
(`sprite_bases` collection) from the `SPRITES_DIR` directory and the
default map from the `MAP_DIR` directory into PocketBase. Both seeds are idempotent — once a record
exists, it is never overwritten. This means a fresh deploy boots with a
playable world without any manual setup.

**Storyboard:** Wipe the PocketBase volume. Start the stack. Show the
worldsim logs: "seeding sprite_bases... done," "seeding main... done."
Open the browser — the world is fully populated with sprites and a
map. No manual upload was needed.

### 5.5 Sprite Catalog from PocketBase

Character spritesheets are stored as PocketBase records in the
`sprite_bases` collection. The frontend fetches the catalog at startup
and loads each spritesheet as a Phaser spritesheet. New characters are
added by uploading a spritesheet PNG to PocketBase — no code changes,
no rebuild.

**Storyboard:** Show the PocketBase admin dashboard with the
`sprite_bases` collection. Upload a new spritesheet PNG. Reload the
browser — the new character appears in the character select grid.

### 5.6 Cloudflare Proxy Support

For deployments behind Cloudflare, a second example nginx config
(`example.cloudflare.nginx.conf`) rewrites `$remote_addr` to the real
visitor IP using `set_real_ip_from` and `real_ip_header
CF-Connecting-IP`. Without this, the pusher's IP tracking stores
Cloudflare's edge IP instead of the client's address. A companion
script (`update-cloudflare-ips.sh`) downloads Cloudflare's current IP
ranges and refreshes the `set_real_ip_from` block in the config —
idempotent, so it can run from cron. Admins pick the config that
matches their topology: `example.nginx.conf` for direct-to-internet,
`example.cloudflare.nginx.conf` behind Cloudflare.

**Storyboard:** Show two terminal windows. In the first, deploy with
`example.nginx.conf` — point out that player IPs in the admin pillbox
show the Cloudflare edge IP. In the second, swap to
`example.cloudflare.nginx.conf`, run `update-cloudflare-ips.sh`, reload
nginx — the admin pillbox now shows real visitor IPs. Narrate: "the
right config for your topology. Cloudflare or direct — the admin sees
the real IP either way."

### 5.7 Audit Log and Event History

A standalone audit service records lifecycle and interaction events —
player connections, disconnects, bans, chat messages, name changes,
sprite changes, zone transitions, map reloads, extension
registrations, A/V token minting — to its own SQLite database,
independent of worldsim or PocketBase. A Go templates + HTMX web UI at
`/audit/` provides a dashboard with severity counts and service
health, a searchable event table with filters (type, severity, actor),
an event detail view with a deep-link to the corresponding OpenObserve
trace, and a per-player timeline showing everything one player has
done. A JSON API (`/audit/api/events`, `/audit/api/events/{id}`,
`/audit/api/players/{sub}`, `/audit/api/stats`) exposes the same data
to programmatic consumers — the MCP server uses it for historical
queries. Events are retained for 30 days (configurable) and the storage
layer is behind an interface designed to upgrade to ClickHouse or
TimescaleDB when volume grows.

**Storyboard:** Open `/audit/` in a browser. Show the dashboard —
service health cards, event type counts, recent events. Connect and
disconnect a client — the events appear in real time. Filter by
`event_type=chat.message` — only chat events show. Click a player's
sub — their full timeline loads. Click a `trace_id` — it opens the
trace in OpenObserve at `/otel/`. Narrate: "who did what, when, and
why. The audit log records every event; OpenObserve shows every
trace. Two clicks from a ban to the exact millisecond it was
processed."

### 5.8 Automatic Reload on Server Update

When the server is redeployed (new images, links, auth flow, or protocol
tweaks), clients already in a game session keep running against the old
assets until they manually refresh — leading to broken images, stale
links, and confusing auth failures mid-session. The frontend detects the
update on its own and reloads.

The version badge in the corner of the game page already polls the
pusher's `/healthz` endpoint every 10 seconds for the kernel (worldsim)
version, which is stamped from `git describe` at build time. The poll
now captures that version as a baseline on the first successful read.
If a later poll reports a different version, the page reloads
immediately. A `sessionStorage` flag carries the reason across the
reload, so the reloaded page shows a small top-center toast — "The page
reloaded because the server was updated." — for about 2 seconds. The
toast is clickable to dismiss sooner. Detection latency is at most one
poll interval (10 seconds); no extra requests are added.

**Storyboard:** Open the game in a browser with the stack running. Show
the version badge in the corner. In a terminal, rebuild and restart the
worldsim with a new version tag. Within ~10 seconds the browser reloads
on its own and the toast appears: "The page reloaded because the server
was updated." Click the toast — it vanishes. Narrate: "deploy a new
version and every connected player picks it up within seconds. No
stale assets, no broken links, no support tickets asking people to
refresh."

### 5.9 MCP Server — Admin Tooling for LLM Clients

A Model Context Protocol (MCP) server exposes Pixel Eruv's internals to
LLM-powered clients — Claude Desktop, Devin, Cursor, any tool that
speaks MCP. Connect a client to `https://<host>/mcp` with a bearer
token, and the LLM can inspect the live world, query audit history,
read PocketBase records, edit server-wide runtime config, and take
administrative actions: kick a player, ban by user ID / IP / device ID,
teleport, send chat as a specific entity, rename a player, set presence
status, swap a character sheet, replace player options, edit world
options, or dispatch an action to any extension. The LLM sees the same
audit trail it just wrote to, so it can verify its own actions.

The server is a separate binary (`backend/cmd/mcp`), not loaded into
worldsim. This is deliberate: MCP request handling can be slow, can
hang on PocketBase, can be hammered by an LLM retry loop, and none of
that should touch the 20Hz game loop. The MCP server talks to worldsim
over NATS request-reply, to the audit service over its JSON API, and
to PocketBase over REST. No new shared state is introduced. Restart or
redeploy the MCP surface without dropping a single player.

The surface is three layers:

- **18 tools** (callable, take arguments). Read: `get_world_stats`,
  `get_zones`, `query_entities`, `get_entity`, `query_audit_events`,
  `get_audit_event`, `player_timeline`, `list_pb_records`,
  `get_pb_record`, `get_world_options`. Control: `teleport_entity`,
  `kick_player`, `ban_player`. Admin overrides: `send_chat_as`,
  `set_player_name`, `set_player_status`, `set_player_sprite`,
  `set_player_options`, `set_world_options`, `dispatch_extension_action`.
- **11 resources** (URI-addressable, read-only). Static:
  `pixeleruv://world/stats`, `…/world/zones`, `…/world/players`,
  `…/world/extensions`, `…/audit/stats`. Templated:
  `pixeleruv://world/maps/{name}`, `…/world/entities/{id}`,
  `…/audit/events/{id}`, `…/audit/players/{sub}`,
  `…/pb/{collection}`, `…/pb/{collection}/{id}`.
- **3 prompts** (pre-baked, fetch live data). `summarize_recent_audit`
  groups the last N events by severity and type.
  `investigate_player` pulls a player's audit timeline, current world
  state, and PocketBase record in one shot.
  `world_health_report` bundles worldsim stats, extension alive
  status, and recent warn/error audit events for a quick health
  assessment.

Admin actions emit audit events stamped with `actor.extension="mcp"`
(configurable via `MCP_ACTOR`) so audit consumers can filter
LLM-initiated actions from client-initiated ones. `set_world_options`
sends `actor={extension:<MCP_ACTOR>}` in its request payload so the
worldsim handler can attribute the audit event correctly; the admin
portal sends `actor={extension:"admin", sub:<admin email>}`. The MCP
server exposes full PII (IP, device_id, client_id) — this is
intentional, since moderation needs those fields (ban by IP, correlate
by device_id). Access control is the bearer token (`MCP_AUTH_TOKEN`,
required — the server refuses to start without it). Do NOT expose the
MCP server on the public internet without a strong token and
network-level restrictions (firewall, VPN, or Tailscale).

See `documentation/25-mcp-server.md` for the full reference (surface,
configuration, client connection examples) and
`documentation/plans/2026-07-19-mcp-server-design.md` for the design.

**Storyboard:** Open Claude Desktop (or Devin, or Cursor) configured
with the Pixel Eruv MCP server URL and bearer token. Ask the LLM:
"who's online right now?" — it calls `get_world_stats` and lists the
players, their maps, and their IPs. Ask: "summarize recent audit
activity" — it runs the `summarize_recent_audit` prompt and reports
that there were 3 kicks and 12 chat messages in the last hour. Ask:
"investigate the player with sub `google-oauth2|12345`" — it runs
`investigate_player` and shows their timeline, current online state,
and PB record. Ask: "kick client `c_abc123` for being abusive" — it
calls `kick_player`, confirms the action landed, then queries the
audit log to show you the `player.kicked` event it just emitted.
Narrate: "your LLM has the same tools a human admin has. It reads the
world, queries the audit log, takes an action, and verifies its own
work — all through one authenticated endpoint."

### 5.10 World Options — Runtime Server Config (No Restart)

Server-wide runtime configuration — SMTP, the public app URL, YouTube
RTMP defaults, ffmpeg audio-extraction limits, the world king, error
email recipients, and a global recording gate — lives in a NATS KV
bucket (`world_options`, key `current`) owned by worldsim, not in env
vars. worldsim seeds hardcoded defaults on first boot; the admin edits
them at runtime via **Admin > World Options** in the admin portal.
Saves write to the KV bucket and broadcast `world_options.update` on
NATS; worldsim hot-reloads its SMTP client and `APP_URL`, ext-rec
hot-reloads the YouTube defaults and ffmpeg limits, and the frontend
picks up the recording gate — all without restarting any service.

`PUBLIC_HOST` and `LIVEKIT_PUBLIC_URL` stay as env vars (they drive
the frontend's TLS cert SAN and ext-av's token URLs at startup — not
safely hot-reloadable). They are mirrored into world_options as
read-only display fields so the admin can see the full picture in one
place.

This is the first use of NATS KV in the codebase (previously NATS Core
pub/sub only). The `nats:2.10-alpine` image already runs with `-js`, so
KV works without config changes. KV is semi-persistent — survives NATS
restarts, lost on volume wipe — and worldsim re-seeds defaults if the
bucket is empty.

**Storyboard:** Open `/admin/world-options` in a browser. Show the
grouped form: SMTP, APP_URL, YouTube, ffmpeg limits, world king, error
emails, recording gate, read-only PUBLIC_HOST / LIVEKIT_PUBLIC_URL.
Change the SMTP host from `mailhog` to a real provider. Click Save.
Show the worldsim log: "world_options hot-reloaded". Trigger a
verification email — it arrives through the new SMTP server. Narrate:
"change server config at runtime. No SSH, no `.env` edit, no restart.
The admin portal is the control plane."

### 5.11 World King — Display-Only World Identity

The admin can name a "king of this world" — a display-only identity
that personalizes the instance. The king's name is shown on the welcome
page footer (`/welcome`) via a public `GET /api/world-king` endpoint
(no auth, returns only the name). The king's email is stored but
visible only on the admin World Options page (spam safety); it doubles
as the default recipient for error email notifications when the
recipient mode is set to "king".

The king has no special permissions — it's metadata, not a role. The
name field is free text; the email field is validated. Both are
hot-reloaded on save.

**Storyboard:** Open `/admin/world-options`. Scroll to "World king".
Type "Lord Pixel" and `king@example.com`. Save. Open `/welcome` in
another tab — the footer now reads "King of this world: Lord Pixel".
Narrate: "your world, your ruler. A small touch that makes the
instance yours."

### 5.12 Error Email Notifications

The audit service can email recipients when an audit event with
severity `error` is emitted — for example, when ffmpeg fails to extract
audio from a recording (`recording.audio_extraction_failed`). Four
recipient modes, all configurable via **Admin > World Options**:

- **none** — disable error emails.
- **king** — send to the world king's email.
- **all_admins** — send to every user linked to a `players` row with
  `is_admin=true`. The audit service resolves the list at send time via
  the `worldsim.admin_emails.get` NATS request-reply (worldsim owns
  PocketBase; the audit service has no PB access).
- **custom** — send to a comma-separated list of addresses.

SMTP config (host, port, credentials, from, sender) comes from the same
World Options page — the same config worldsim uses for verification and
password-reset emails. Hot-reloaded on save. Emails are sent in a
goroutine so audit persistence is never blocked on SMTP; failures are
logged and dropped (best-effort, like audit itself).

**Storyboard:** Open `/admin/world-options`. Set "Error email
notifications" → "King only". Confirm `king_email` is set. Save. In a
terminal, trigger an error audit event (e.g. stop a recording mid-
extraction). Within seconds an email arrives at the king's address with
the event type, severity, timestamp, actor, and details. Open MailHog
to show it. Narrate: "the world tells you when something breaks. The
king gets the email; the audit log has the rest."

### 5.13 Meeting Recording (MP4 + YouTube) with Global Gate

Admins can record A/V meetings from the top-menu Record button (only
visible to admins inside an A/V room). Two targets:

- **MP4** — LiveKit Room Composite Egress writes an MP4 to disk, served
  from `/recordings/<file>.mp4`. A follow-up ffmpeg pass extracts the
  audio track as MP3 so recordings can be listened to without video.
  Thumbnails are extracted for the admin UI grid.
- **YouTube** — streams the meeting to a YouTube RTMP endpoint. A
  confirm modal pre-fills the RTMP URL and stream key from World
  Options; the host can override them per-recording without editing the
  global defaults.

Recordings are listed and managed at **Admin > Recordings**
(`/admin/recordings`): browse, search, download, delete, backfill
thumbnails, stop an active recording. Active recording state is
replicated to all participants in the room — a REC indicator appears
in the corner so everyone knows they're being recorded.

A **global recording gate** (`recording_enabled`, default true) lets
the admin disable recording for the whole instance without code
changes. When unchecked: ext-rec refuses `recording.start` and emits a
`recording.start_denied` audit event (reason=`globally_disabled`); the
frontend dims the Record button and shows a "Recording is disabled
globally" tooltip. Hot-reloaded on save.

**Storyboard:** Enter an A/V room as admin. Click the Record button.
Show the dropdown: "Record to MP4" and "Stream to YouTube". Click
"Stream to YouTube" — a confirm modal appears pre-filled with the RTMP
URL and stream key from World Options. Edit the stream key for this
recording only. Click "Start streaming". A REC indicator appears for
all participants. Open the YouTube studio in another tab — the stream
is live. Back in the world, click "Stop REC". Open **Admin >
Recordings** — the recording appears with a thumbnail, MP4 + MP3
download links, and the duration. Narrate: "record meetings for
posterity, stream them to YouTube, manage them from one admin page.
One toggle turns it all off if you need to."

---

## Part 6 — Roadmap (Post-MVP)

These features are not yet implemented but the architecture does not
preclude them. They are included here as future storyboard segments.

### 6.1 AI / NPC Agents

LLM-controlled NPCs built as extensions. An NPC extension runs Python
with LangChain or LlamaIndex, connects to NATS, and drives an entity
in the world. The kernel treats it like any other extension-owned
entity.

**Storyboard (future):** Show an NPC standing in the office. Walk up
to it and type a chat message. The NPC responds with a generated line.
Show the extension process running a Python script with an LLM API
call.

### 6.2 Exclusive Zones and Knock-to-Join

A zone marked `exclusive` hides entities inside it from non-members'
replication batches — outsiders don't see or hear what's inside. A
meeting room zone uses knock-to-join: the first entrant becomes the
owner; subsequent entrants are blocked and send a knock notification;
the owner admits or denies via a popup.

**Storyboard (future):** Show a meeting room with the door closed. A
second player tries to enter — blocked. The owner gets a popup: "Player
2 wants to join." Click admit — the door opens, the player enters, and
outsiders can no longer see or hear inside.

### 6.3 Inventory and Equipment

Items as ECS entities with `Position` (on the ground) or an
`InventorySlot` component (owned by a player). An inventory extension
handles equipment slots, item effects, and use actions. Input triggers
include an equipment snapshot so actions can be equipment-dependent
(e.g. bow → shoot, empty-handed → no action).

**Storyboard (future):** Show a sword on the ground. Walk over it and
press E — it enters the inventory. Open the inventory panel — equip
the sword. Press a key — the character swings. Drop the sword — it
reappears on the ground.

### 6.4 Matrix Synapse Chat

Migration from PocketBase chat to Matrix Synapse for federation, rich
clients, and end-to-end encryption. The chat surface is small enough
that the migration is contained.

**Storyboard (future):** Show the chat panel connecting to a Matrix
homeserver. Send a message from an external Matrix client (Element) —
it appears in the in-game chat. Narrate: "federation means users on
other Matrix servers can join the conversation."

### 6.5 Horizontal Scaling

Multiple worldsim shards (per-map or per-region) with cross-shard
visibility. The Pusher can be horizontally scaled with nginx sticky
sessions routing reconnecting clients to the same instance.

**Storyboard (future):** Show two worldsim instances on the architecture
diagram, each handling a different map. A player walks to a map boundary
and crosses — the replication source shifts to the other shard
without interrupting the player.

---

## Appendix — Suggested Presentation Arcs

### Arc A: "What is Pixel Eruv?" (5 minutes)

1. 0.1 Open Source and Self-Hostable (the pitch)
2. 1.1 Persistent Pixel-Art World (show the world)
3. 1.6 Mobile Support with Virtual Joystick (show it on a phone)
4. 2.1 Proximity Audio/Video (walk two players together)
5. 2.10 Text Chat (send a message)
6. 5.1 Self-Hostable (one command, no Kubernetes)

### Arc A2: "Why It's Better" (5 minutes)

1. 0.1 Open Source and Self-Hostable (vs proprietary hosted)
2. 0.2 Server-Authoritative (vs client-trusted)
3. 0.4 Extensions in Any Language (vs sandboxed scripting)
4. 0.5 Enterprise Identity from Day One (vs custom auth bridges)
5. 0.6 The Kernel Has No Gameplay Logic (vs hardcoded features)
6. 0.8 Observable by Default (vs black box)
7. 0.9 Easy Branding and Customization (vs logo + color theme)

### Arc B: "How It Works Under the Hood" (10 minutes)

1. 3.1 Server-Authoritative Simulation
2. 3.2 ECS Core
3. 3.3 Component-Based Replication
4. 3.4 Client-Side Prediction
5. 3.6 Two-Tier Collision
6. 3.7 Zone Triggers
7. 3.8 Extension System (kill an extension, show isolation)

### Arc C: "The Communication Experience" (5 minutes)

1. 2.1 Proximity Audio/Video
2. 2.2 Spatial Audio Volume
3. 2.3 Speaking Indicators
4. 2.4 Video Bar
5. 2.5 Mic/Camera Controls
6. 2.9 Cross-Browser Compatibility
7. 2.10 Text Chat

### Arc D: "Deploy and Operate" (5 minutes)

1. 5.1 Docker Compose (one command)
2. 5.2 Remote Configuration (one variable)
3. 5.4 Auto-Seeding (zero setup)
4. 5.3 OpenTelemetry (trace a movement)
5. 5.7 Audit Log (search event history, link to traces)
6. 5.8 Automatic Reload on Server Update (deploy, watch clients reload)
7. 4.1-4.5 PocketBase Authentication (register, verify, password reset, social login, token validation)

### Arc E: "The Road Ahead" (5 minutes)

1. 6.1 AI / NPC Agents
2. 6.2 Exclusive Zones and Knock-to-Join
3. 6.3 Inventory and Equipment
4. 6.4 Matrix Synapse Chat
5. 6.5 Horizontal Scaling

### Arc F: "What Can You Do With It?" (5 minutes)

1. 0b.1 Casual Team Hangout (the office use case)
2. 0b.2 Company All-Hands (the meeting use case)
3. 0b.3 Virtual Conference and Expo (the event use case)
4. 0b.4 Art Gallery and Exhibition (the cultural use case)
5. 0b.6 Environment for AI Agents (the AI use case)
6. 0b.7 Persistent Virtual HQ (the long-term use case)
