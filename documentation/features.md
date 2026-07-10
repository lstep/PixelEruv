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
admin dashboard.

**Storyboard:** Show the Tiled editor with the office map open. Switch
to the browser running Pixel Eruv — the same map renders. Point at the
tile layers (ground, walls, decorations) and the object layers (zones,
entities).

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
the sprite.

**Storyboard:** Two characters on screen, each with a name tag above.
Walk one character around — the tag follows. Point out that guests get
a generated name and logged-in users get their PocketBase display name.

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

---

## Part 3 — Architecture

### 3.1 Server-Authoritative Simulation

The World Simulator is the spatial authority. It owns the ECS, the tile
grid, the spatial index, the trigger registry, and the replication
pipeline. The only gameplay system in the kernel is player avatar
movement — everything else is delegated to extensions. The Pusher is a
thin WebSocket proxy that validates OIDC tokens and forwards frames
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

### 4.1 Dex OIDC Authentication

Authentication uses Dex as the OIDC provider with the authorization
code flow + PKCE. The MVP ships with Dex's local-password connector
(simple username/password). Enterprise connectors (LDAP, Active
Directory, Google, GitHub, SAML) are enabled by changing Dex's config
at deploy time — no application code changes. The Pusher validates the
JWT on WebSocket upgrade using JWKS cached from Dex.

**Storyboard:** Show the login screen — redirect to Dex. Enter
credentials. Redirect back to the app with a token. Show the WebSocket
connection carrying the token in the AUTH frame. Narrate: "swap the
Dex config file and you're on LDAP — same app, same code."

### 4.2 Character Selection

Logged-in users who haven't picked a sprite see a character select
scene before joining the world. It displays the PocketBase-backed
sprite catalog as clickable thumbnails, each showing a walk-cycle
preview. On confirm, the chosen sprite ID is sent to the worldsim via
`SetSpriteBaseFrame`. Guests skip this scene and get a default sprite.

**Storyboard:** Log in as a new user. The character select screen
appears with a grid of sprite thumbnails. Click one — it highlights.
Click confirm — transition to the game scene with the chosen character.
Log in as a guest — go straight to the game with a default sprite.

### 4.3 Guest Mode

Unauthenticated users can join as guests with a generated username and
a default sprite. Guests skip the character select scene. This lets
people try the world without creating an account.

**Storyboard:** Open the app without logging in. A guest username
appears in the top menu. The character spawns with a default sprite.
Walk around, talk to people — everything works except persistence
(positions are not restored for guests).

---

## Part 5 — Operations

### 5.1 Self-Hostable via Docker Compose

The entire stack runs with a single `make up` command — no Kubernetes,
no platform engineering. NATS, PocketBase, Dex, LiveKit, the Pusher,
the WorldSim, and all extensions start as Docker Compose services. A
self-contained `dist/` directory can be copied to any host and run
without source code.

**Storyboard:** Show a terminal. Type `make up`. Show the Docker
Compose logs scrolling — each service starting. Open the browser to
`localhost:4080` — the world is live. Narrate: "one command, one host,
no cluster."

### 5.2 Single-Variable Remote Configuration

One environment variable — `PUBLIC_HOST` — drives everything remote
browsers need: the TLS cert SAN, the Dex redirect URI, and the
LiveKit public URL. Set it to the host's LAN IP or hostname and
rebuild.

**Storyboard:** Show `PUBLIC_HOST=192.168.1.10 make up`. Open a browser
on another machine to `https://192.168.1.10:4043`. Accept the self-signed
cert. The world loads, auth works, audio works — all from one variable.

### 5.3 OpenTelemetry Observability

The backend (pusher, worldsim) and frontend are instrumented with
OpenTelemetry traces and logs. Telemetry is off by default. `make debug`
starts NATS, motel (a local OTel collector with a TUI), the pusher, and
the worldsim with `OTEL_ENABLED=true`. Traces span the full
request path: browser → pusher → NATS → worldsim → replication →
pusher → browser.

**Storyboard:** Run `make debug`. Show the motel TUI with a trace tree
for a player movement: WebSocket receive → NATS publish → worldsim
tick → collision check → replication encode → NATS publish → WebSocket
send. Narrate: "every hop is traced. You can see exactly where time is
spent."

### 5.4 Auto-Seeding on First Boot

On first startup, the worldsim auto-seeds the sprite catalog
(`sprite_bases` collection) from the `SPRITES_DIR` directory and the
default map from the `MAP_DIR` directory into PocketBase. Both seeds
are idempotent — once a record exists, it is never overwritten. This
means a fresh deploy boots with a playable world without any manual
setup.

**Storyboard:** Wipe the PocketBase volume. Start the stack. Show the
worldsim logs: "seeding sprite_bases... done," "seeding map1... done."
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
visibility. The Pusher can be horizontally scaled with Traefik sticky
sessions routing reconnecting clients to the same instance.

**Storyboard (future):** Show two worldsim instances on the architecture
diagram, each handling a different map. A player walks to a map boundary
and crosses — the replication source shifts to the other shard
seamlessly.

---

## Appendix — Suggested Presentation Arcs

### Arc A: "What is Pixel Eruv?" (5 minutes)

1. 1.1 Persistent Pixel-Art World (show the world)
2. 2.1 Proximity Audio/Video (walk two players together)
3. 2.10 Text Chat (send a message)
4. 3.1 Server-Authoritative Simulation (show the architecture diagram)
5. 5.1 Self-Hostable (one command, no Kubernetes)

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
5. 4.1 Dex Authentication (swap connectors)

### Arc E: "The Road Ahead" (5 minutes)

1. 6.1 AI / NPC Agents
2. 6.2 Exclusive Zones and Knock-to-Join
3. 6.3 Inventory and Equipment
4. 6.4 Matrix Synapse Chat
5. 6.5 Horizontal Scaling
