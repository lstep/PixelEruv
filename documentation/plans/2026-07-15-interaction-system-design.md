# Interaction System Design

**Date:** 2026-07-15
**Status:** Design (not yet implemented)
**Branch:** TBD

## 1. Goal

When a player presses the interaction key ("E") near one or more
interactable entities, the system displays a popup with the available
actions on those entities, or executes an action immediately (entity's
choice). Actions can have side effects across multiple systems — sprite
changes, wall zone toggling, external notifications — all declared as
data on the entity, interpreted by extensions that know how to handle
each effect type.

### Use cases

1. **Light (popup mode):** A lamp entity. Press E → popup shows
   "Activate" (if off) or "Deactivate" (if on). Choosing it plays a
   click sound, swaps the sprite to the "on" frame, and shows a 3-tile
   glow overlay.

2. **Light switch (immediate mode):** A wall switch. Press E →
   immediately toggles one or more remote light entities (sprite +
   glow). No popup.

3. **Door (immediate mode, multi-effect):** A door. Press E → swaps
   the door sprite AND toggles a wall zone (to open/close a room
   boundary). Press E again to reverse both.

4. **Notification button (immediate mode, external effect):** A
   button. Press E → sends a Slack/email notification with a payload.
   No state change, no sprite swap.

5. **Multiple entities nearby:** Player stands between a light and a
   door. Press E → door toggles immediately, popup shows the light's
   actions.

6. **Sparks on approach:** When the player walks within an
   interactable entity's trigger radius, a one-shot sparks animation
   plays above the entity to signal it is interactable.


## 2. Design Evolution (Why We Arrived Here)

This section records the reasoning that shaped the final design, so
future readers understand not just *what* was decided but *why*.

### 2.1 Starting point: existing architecture

The codebase already had most of the plumbing:

- The "E" key sends a `key:E` ActionFrame to the server
  (`GameScene.ts` line 810).
- Worldsim's `applyAction()` finds adjacent entities within trigger
  radius and dispatches to extensions registered for the input type
  via NATS request-reply RPC (`worldsim.go` line 1180).
- `ext-props` already toggles a `light_switch` entity type between
  "on"/"off" state and returns Updates + Animations
  (`ext-props/main.go` line 136).
- `EntityState` (component ID 2) and `PlayAnimation` replication
  already exist in the proto (`components.proto` line 25,
  `replication.proto` line 28).
- `applyActionReply()` applies state updates to entities by ID lookup
  in `s.entities` — **without adjacency filtering** (`worldsim.go`
  line 1264). This is critical: it means an extension can update
  far-away entities if it knows their IDs.

**Gaps:** frontend doesn't handle component 2 or PlayAnimation
messages; `ActionResultFrame` doesn't carry available actions for a
popup; no sound infrastructure; no "sparks on approach" feedback; no
light glow overlay.

### 2.2 Decision 1: Two-phase RPC with immediate-mode opt-out

**Question:** How should "press E → popup → choose action" work?

**Options considered:**

- **A. Two-phase RPC:** Press E → worldsim asks extensions for
  *available actions* (no execution) → client shows popup → user
  clicks → client sends `action:execute` → worldsim dispatches →
  extension executes. Most flexible, supports multiple entities/actions
  in popup.

- **B. Client-side query:** Client inspects replicated state locally
  and builds the popup without a server round-trip. Simpler, but
  action labels must be hardcoded on the client per entity type.

- **C. Hybrid:** Client builds popup locally for known types, falls
  back to server query for unknown types.

**Decision:** **Option A, with a twist.** Some entity types (doors,
switches) should execute immediately on E press without a popup. The
extension decides per-entity: it can either return `AvailableActions`
(popup mode) or return `Updates`+`Animations` (immediate mode), or
both. This maps cleanly onto the existing `Handled` flag — the
extension returns whatever it wants, worldsim aggregates and sends it
all back.

The key insight: **immediate vs popup is an entity property, not a
framework property.** The entity declares whether it wants a popup
(`actions` property) or immediate execution (`on_interact_action`
property). The extension reads this from the dispatch and acts
accordingly.

### 2.3 Decision 2: Light glow as overlay sprite

**Question:** How to render the 3-tile light emission?

**Options considered:**

- **Overlay sprite:** A pre-made 7x7-tile PNG with a soft radial
  gradient, toggled visible/invisible on top of the light entity.
- **Phaser Light2D:** Real dynamic lighting via Phaser's lighting
  pipeline. More immersive but requires enabling the pipeline and
  configuring normal maps.
- **Tinted circle:** A Phaser Graphics circle with low alpha. No asset
  needed but looks flat.

**Decision:** **Overlay sprite.** Simplest, no pipeline changes,
matches the user's own suggestion. One PNG asset, show/hide based on
state. Can upgrade to Light2D later without changing the data model.

### 2.4 Decision 3: Sparks triggered client-side

**Question:** How to trigger the "sparks when approaching" animation?

**Options considered:**

- **Client-side polling:** Client checks distance to interactable
  entities each frame in `update()`. When entering range, play a
  one-shot sparks animation. No server changes.
- **Server proximity event:** Worldsim's mobile proximity zones
  already follow each player. Add a new proximity enter/exit
  replication event. More architecturally consistent but requires new
  proto messages.

**Decision:** **Client-side polling.** The client already knows entity
positions (from replication) and which entities are interactable (from
the `interactable` flag on the Appearance component, to be added).
Polling distance in `update()` is cheap and requires zero server
changes. The sparks animation is purely cosmetic — no need for server
authority.

### 2.5 Decision 4: Generic action routing (not per-entity-type)

**Initial approach:** Hardcode entity types in the extension
(`if ent.EntityType == "light" { ... } else if ent.EntityType ==
"light_switch" { ... }`).

**Problem:** Adding a new entity type (door, fountain, notification
button) requires extension code changes. The extension becomes a
growing pile of per-type conditionals.

**Evolution:** Separate routing (data) from handling (code). The
entity declares which action to send and to whom. The extension's code
decides what the action does. Adding a new entity type is a data
exercise (Tiled properties), not a code exercise.

### 2.6 Decision 5: Fully generic action strings (not on/off state model)

**Initial approach:** `applyAction()` / `actionLabel()` functions
assuming a binary "on"/"off" state model.

**Problem:** The user pointed out that actions can be anything —
"send_notification" with a payload "luc" has nothing to do with
on/off state. The framework must not assume any state model.

**Decision:** The framework is a **pure message router**. It routes
action strings + payloads to target entities. The extension's code
interprets the action string and decides side effects — which may or
may not include state changes. State changes and sprite swaps are
*some possible* side effects, not the only ones.

### 2.7 Decision 6: Per-entity effects list (not per-action-type code)

**Initial approach (Option B):** Entity declares `actions:
"toggle,lock"`. The extension's code knows that "toggle" on a door
means "swap sprite + toggle wall". Per-entity behavior lives in the
code.

**Problem:** "toggle" on door-1 might mean just a sprite swap, while
"toggle" on door-2 might mean sprite + wall + notification. The same
action string has different effects per entity. If behavior is in the
code, adding door-3 with new behavior requires code changes.

**Decision:** **The entity declares the full effects list per action.**
The entity carries an `interactions` map: action_id → list of effects.
Each effect has an action verb (for the target system), an optional
payload, and target IDs. The extension code is a **vocabulary** (set
of action verbs it understands). The entity data is a **sentence**
(which verbs to apply, to whom, with what payload). Composing new
behaviors is a data exercise, not a code exercise.

This was the most important decision. It ensures:
- Same action string ("toggle") can have completely different effects
  per entity.
- Adding a new entity with new behavior requires zero code changes
  (just Tiled properties).
- Extensions are generic interpreters, not per-entity-type handlers.


## 3. Final Architecture

### 3.1 Separation of concerns

| Layer | Responsibility | Who controls it |
|---|---|---|
| **Routing** | Which action, which payload, which targets | Entity data (Tiled properties) |
| **Dispatch** | Deliver action + payload + target info to extensions | Worldsim (generic, no semantics) |
| **Handling** | What the action actually *does* | Extension code (fully arbitrary) |
| **Side effects** | State changes, sprite swaps, animations, external calls, zone toggles | Extension decides, returns what it wants |
| **Replication** | Propagate side effects to clients | Worldsim (generic) |

The framework never interprets action strings. It doesn't know what
"toggle" or "send_notification" means. It routes and aggregates.

### 3.2 The interactions data model

Each interactable entity carries an `interactions` property — a JSON
string encoding a map of action_id → list of effects:

```json
{
  "toggle": [
    {"action": "toggle", "target_ids": ["door-2"]},
    {"action": "toggle_wall", "target_ids": ["room-2-walls"]},
    {"action": "send_notification", "payload": "door-2 opened", "target_ids": []}
  ],
  "lock": [
    {"action": "set_state", "payload": "locked", "target_ids": ["door-2"]},
    {"action": "toggle_wall", "target_ids": ["room-2-walls"]}
  ]
}
```

Each effect has:
- `action` (string): The action verb for the target system. Different
  extensions handle different verbs. ext-props handles "toggle",
  "set_state", "activate", "deactivate". ext-walls handles
  "toggle_wall". A hypothetical ext-notifications handles
  "send_notification".
- `target_ids` (string array): Entity IDs or zone IDs to apply the
  action to. Empty array = the action is standalone (no target, e.g.
  sending a notification) or applies to the entity itself (extension
  decides based on context).
- `payload` (string, optional): Free-form data passed to the handler.
  Can be a plain string, a JSON string, anything. The extension's code
  interprets it.

### 3.3 Immediate mode vs popup mode

The entity declares which mode it uses via Tiled properties:

- **Immediate mode:** Entity has `on_interact_action: "toggle"`.
  Pressing E fires `interactions["toggle"]` automatically. No popup.
  Used for doors, switches, notification buttons.

- **Popup mode:** Entity has `actions: "toggle,activate,deactivate"`.
  Pressing E shows a popup with those actions. User picks one →
  `interactions["chosen_action"]` fires. Used for lights, complex
  entities with multiple options.

An entity can have both: `on_interact_action` for a default immediate
action AND `actions` for a popup. In that case, pressing E fires the
immediate action AND shows the popup for additional actions. (This is
an edge case — most entities use one mode or the other.)

### 3.4 Multi-extension dispatch

The existing dispatch model already sends `key:E` to **all** extensions
registered for it. Currently only ext-props registers. If ext-walls
also registers for `key:E`, both receive the same dispatch. Each reads
the effects list and handles the verbs it knows, skipping the rest.

```
Press E on door-2
       |
       v
  Worldsim dispatches to ALL extensions registered for "key:E"
       |
       +---> ext-props receives dispatch
       |    reads interactions["toggle"] effects
       |    sees {action:"toggle", targets:["door-2"]} -> I handle "toggle"
       |    sees {action:"toggle_wall", targets:["room-2-walls"]} -> skip, not mine
       |    sees {action:"send_notification", ...} -> skip, not mine
       |    applies "toggle" to door-2
       |    returns: state="on", appearance=gid_on, animation=click
       |
       +---> ext-walls receives dispatch
            reads interactions["toggle"] effects
            sees {action:"toggle", ...} -> skip, not mine
            sees {action:"toggle_wall", targets:["room-2-walls"]} -> I handle this
            toggles gate trigger on "room-2-walls"
            re-publishes register_triggers
            returns: handled=true (no entity updates)
```

A future ext-notifications extension would register for `key:E` and
handle `"send_notification"`. No framework changes needed.

### 3.5 Target entity lookup

When an adjacent entity (e.g., a switch) declares effects with
`target_ids` pointing to far-away entities (e.g., ceiling lights),
worldsim looks up those target IDs in `s.entities` and includes them
in the dispatch as `target_entities`. This gives the extension access
to the target entities' current state, gid, and other data in a single
dispatch, without the extension needing to query the map.

The extension uses `target_entities` to find the current state of
lights it's about to toggle. It uses `adjacent_entities` to find the
switch the player interacted with (which carries the `interactions`
data and `on_interact_action`).

For zone targets (e.g., `toggle_wall` targeting "room-2-walls"), the
extension looks up the zone ID in its own local zone metadata set
(ext-walls already fetches zone metadata via `worldsim.zones.get`).
The framework doesn't need to resolve zone targets — that's the
extension's responsibility.


## 4. Tiled Property Schema

All properties are custom properties on tile objects placed on the
"Entities" object layer in Tiled.

### 4.1 Common properties (all interactable entities)

| Property | Type | Required | Meaning |
|---|---|---|---|
| `entity_type` | string | yes | Extension self-filters on this |
| `owner_extension` | string | yes | Routes to the right extension(s) |
| `trigger_radius` | float | no | Interaction range in tiles (default 1.5) |

### 4.2 Visual state properties (entities whose sprite changes)

| Property | Type | Required | Meaning |
|---|---|---|---|
| `gid_on` | int | no | Alternate sprite GID for the "on" state. The tile object's own `gid` is the "off" state. Passed to the extension in the dispatch — the extension decides whether to use it. |

Note: `gid_on` is not interpreted by the framework. The extension
reads it from the dispatch and returns an `AppearanceUpdate` with the
appropriate GID. This keeps the framework generic — it doesn't know
which state maps to which sprite.

### 4.3 Immediate mode properties

| Property | Type | Required | Meaning |
|---|---|---|---|
| `on_interact_action` | string | yes (immediate mode) | Which action_id fires when E is pressed. Looks up `interactions[on_interact_action]` for the effects list. |

### 4.4 Popup mode properties

| Property | Type | Required | Meaning |
|---|---|---|---|
| `actions` | string | yes (popup mode) | Comma-separated action_ids shown in the popup. Each looks up `interactions[action_id]` for the effects list. |

### 4.5 The interactions property

| Property | Type | Required | Meaning |
|---|---|---|---|
| `interactions` | string (JSON) | yes | JSON-encoded map of action_id → effects list. See section 3.2 for the structure. |

This is a single Tiled string property containing JSON. Tiled's
property editor handles long strings fine — paste the JSON into the
string value field.


## 5. Worked Examples

### 5.1 Light entity (popup mode, individually controllable)

**Tiled object on "Entities" layer:**
- Name: `light-1`
- Tile: lamp-off tile (this sets the `gid` automatically)
- Custom properties:

| Property | Type | Value |
|---|---|---|
| `entity_type` | string | `light` |
| `owner_extension` | string | `props` |
| `trigger_radius` | float | `1.5` |
| `gid_on` | int | `491` (GID of the lamp-on tile) |
| `actions` | string | `toggle,activate,deactivate` |
| `interactions` | string (JSON) | (see below) |

**interactions JSON:**
```json
{
  "toggle": [
    {"action": "toggle", "target_ids": ["light-1"]}
  ],
  "activate": [
    {"action": "set_state", "payload": "on", "target_ids": ["light-1"]}
  ],
  "deactivate": [
    {"action": "set_state", "payload": "off", "target_ids": ["light-1"]}
  ]
}
```

**Behavior:** Player walks near light-1, sees sparks. Presses E →
popup shows "Toggle", "Activate" (if off) or "Deactivate" (if on).
User clicks "Activate" → ext-props sets state to "on", swaps sprite to
gid_on, plays click sound, shows glow overlay.

### 5.2 Light switch entity (immediate mode, controls remote lights)

**Tiled object on "Entities" layer:**
- Name: `switch-1`
- Tile: switch-off tile
- Custom properties:

| Property | Type | Value |
|---|---|---|
| `entity_type` | string | `light_switch` |
| `owner_extension` | string | `props` |
| `trigger_radius` | float | `1.5` |
| `gid_on` | int | `381` (GID of switch-on tile) |
| `on_interact_action` | string | `toggle` |
| `interactions` | string (JSON) | (see below) |

**interactions JSON:**
```json
{
  "toggle": [
    {"action": "toggle", "target_ids": ["switch-1"]},
    {"action": "toggle", "target_ids": ["light-1"]},
    {"action": "toggle", "target_ids": ["light-2"]}
  ]
}
```

**Behavior:** Player walks near switch-1, sees sparks. Presses E →
immediate toggle. ext-props toggles switch-1, light-1, and light-2
(all flip state + sprite). Click sound plays on switch-1. Glow
overlays appear on both lights. No popup.

Note: light-1 and light-2 are far away (ceiling lights). Worldsim
includes them in `target_entities` of the dispatch because switch-1's
effects reference them by ID. The extension reads their current state
from `target_entities` and toggles them.

### 5.3 Door entity (immediate mode, multi-effect, multi-extension)

**Tiled object on "Entities" layer:**
- Name: `door-2`
- Tile: door-closed tile
- Custom properties:

| Property | Type | Value |
|---|---|---|
| `entity_type` | string | `door` |
| `owner_extension` | string | `props` |
| `trigger_radius` | float | `1.5` |
| `gid_on` | int | `501` (GID of door-open tile) |
| `on_interact_action` | string | `toggle` |
| `interactions` | string (JSON) | (see below) |

**interactions JSON:**
```json
{
  "toggle": [
    {"action": "toggle", "target_ids": ["door-2"]},
    {"action": "toggle_wall", "target_ids": ["room-2-walls"]},
    {"action": "send_notification", "payload": "door-2 opened", "target_ids": []}
  ]
}
```

**Behavior:** Player walks near door-2, sees sparks. Presses E →
immediate toggle. Three effects fire:

1. ext-props handles `"toggle"` on `door-2`: swaps sprite to door-open,
   sets state to "on", plays click sound.
2. ext-walls handles `"toggle_wall"` on `room-2-walls`: removes the
   gate trigger on that wall zone, re-publishes `register_triggers`.
   Players can now walk through the doorway.
3. ext-notifications (hypothetical) handles `"send_notification"` with
   payload "door-2 opened": sends a Slack message.

Pressing E again reverses all three: door closes, wall blocks again,
notification sent.

### 5.4 Door entity (simple — just sprite, no wall, no notification)

**Tiled object:**
- Name: `door-1`
- Tile: door-closed tile
- Custom properties:

| Property | Type | Value |
|---|---|---|
| `entity_type` | string | `door` |
| `owner_extension` | string | `props` |
| `trigger_radius` | float | `1.5` |
| `gid_on` | int | `501` |
| `on_interact_action` | string | `toggle` |
| `interactions` | string (JSON) | `{"toggle":[{"action":"toggle","target_ids":["door-1"]}]}` |

**Behavior:** Press E → door sprite swaps, click sound plays. That's
it. Same action string "toggle" as door-2, but completely different
effects — because the data is different.

### 5.5 Notification button (immediate mode, external effect only)

**Tiled object:**
- Name: `notif-button`
- Tile: button tile
- Custom properties:

| Property | Type | Value |
|---|---|---|
| `entity_type` | string | `notification_button` |
| `owner_extension` | string | `notifications` |
| `trigger_radius` | float | `1.5` |
| `on_interact_action` | string | `press` |
| `interactions` | string (JSON) | `{"press":[{"action":"send_notification","payload":"luc","target_ids":[]}]}` |

**Behavior:** Press E → ext-notifications handles "send_notification"
with payload "luc". Sends a Slack message. No state change, no sprite
swap, no animation. The framework sees `handled=true` with no entity
updates — nothing to replicate. The player sees nothing change (or
maybe a small animation if the extension returns one).

### 5.6 Multiple entities nearby

Player stands between `light-1` (popup mode) and `door-2` (immediate
mode). Presses E:

1. Worldsim dispatches to all extensions with both entities in
   `adjacent_entities`.
2. ext-props processes door-2: immediate toggle (sprite + state +
   animation). Processes light-1: returns `AvailableActions`
   (toggle/activate/deactivate).
3. ext-walls processes door-2: toggles wall zone. Skips light-1 (no
   `toggle_wall` effect).
4. Worldsim aggregates: door-2 state/sprite/animation replicated,
   `ActionResultFrame` sent with `available_actions` for light-1.
5. Client: door-2 visibly toggles, popup shows for light-1.

### 5.7 Popup mode with multiple actions (lockable door)

**Tiled object:**
- Name: `secure-door`
- Tile: door-closed tile
- Custom properties:

| Property | Type | Value |
|---|---|---|
| `entity_type` | string | `door` |
| `owner_extension` | string | `props` |
| `trigger_radius` | float | `1.5` |
| `gid_on` | int | `501` |
| `actions` | string | `toggle,lock` |
| `interactions` | string (JSON) | (see below) |

**interactions JSON:**
```json
{
  "toggle": [
    {"action": "toggle", "target_ids": ["secure-door"]},
    {"action": "toggle_wall", "target_ids": ["vault-walls"]}
  ],
  "lock": [
    {"action": "set_state", "payload": "locked", "target_ids": ["secure-door"]},
    {"action": "toggle_wall", "target_ids": ["vault-walls"]}
  ]
}
```

**Behavior:** Press E → popup shows "Toggle" and "Lock". User picks
"Toggle" → door opens, wall opens. User picks "Lock" → door state
becomes "locked" (sprite could change to a locked variant if
configured), wall stays blocking (or activates if not already).


## 6. Sequence Diagrams

### 6.1 Popup mode: Press E near light → choose action

```
 Browser          Pusher        WorldSim       ext-props        NATS
    |               |              |              |              |
    |--key:E------->|              |              |              |
    |  ActionFrame  |--action----->|              |              |
    |               |              |              |              |
    |               |    applyAction()           |              |
    |               |    adjacentEntitiesLocked()|              |
    |               |    ExtensionsForInput("key:E")            |
    |               |              |              |              |
    |               |    --- actionDispatchMsg -->|              |
    |               |    {input:"key:E",          |              |
    |               |     adjacent_entities:[{    |              |
    |               |       id:"light-1",         |              |
    |               |       type:"light",         |              |
    |               |       owner:"props",        |              |
    |               |       state:"off",          |              |
    |               |       gid:508, gid_on:491,  |              |
    |               |       actions:"toggle,activate,deactivate",|
    |               |       interactions:{...}}], |              |
    |               |     target_entities:[]}     |              |
    |               |              |              |              |
    |               |              |  ext-props:  |              |
    |               |              |  no on_interact_action -> popup mode
    |               |              |  reads actions: "toggle,activate,deactivate"
    |               |              |  state="off" -> show "toggle","activate" (hide "deactivate")
    |               |              |  returns AvailableActions:  |
    |               |              |    [{light-1, "toggle", "Toggle", "light"},
    |               |              |     {light-1, "activate", "Activate", "light"}]
    |               |              |<--reply------|              |
    |               |              |              |              |
    |               |    sendActionResult()      |              |
    |               |    {ok:true, available_actions:[...]}     |
    |<--actionResult-|<--ServerFrame-|              |              |
    |               |              |              |              |
    |  GameScene: available_actions non-empty     |              |
    |  -> openInteractionPopup()  |              |              |
    |  -> shows "light: Toggle" + "light: Activate"              |
    |               |              |              |              |
    |  User clicks "Activate"     |              |              |
    |               |              |              |              |
    |--action:execute->|          |              |              |
    |  ActionFrame{     |--action->|              |              |
    |   input:"action:execute",    |              |              |
    |   entity_id:"light-1",       |              |              |
    |   action_id:"activate"}      |              |              |
    |               |    applyAction()           |              |
    |               |    adjacentEntitiesLocked()|              |
    |               |    ExtensionsForInput("action:execute")   |
    |               |              |              |              |
    |               |    --- actionDispatchMsg -->|              |
    |               |    {input:"action:execute", |              |
    |               |     target_entity_id:"light-1",           |
    |               |     action_id:"activate",   |              |
    |               |     adjacent_entities:[{light-1, ...}],   |
    |               |     target_entities:[]}     |              |
    |               |              |              |              |
    |               |              |  ext-props:  |              |
    |               |              |  reads interactions["activate"]          |
    |               |              |  -> [{action:"set_state",payload:"on",   |
    |               |              |       target_ids:["light-1"]}]           |
    |               |              |  handles "set_state" with payload "on"   |
    |               |              |  -> Updates: [{light-1, "on"}]           |
    |               |              |  -> AppearanceUpdates: [{light-1, 491}]  |
    |               |              |  -> Animations: [{light-1, anim_click}]  |
    |               |              |<--reply------|              |
    |               |              |              |              |
    |               |    applyActionReply()      |              |
    |               |    -> light-1.State="on", dirtyState      |
    |               |    -> light-1.Gid=491, dirtyAppearance    |
    |               |    -> pendingAnimations=[anim_click]      |
    |               |              |              |              |
    |               |    Next replication tick:   |              |
    |<--replication--|<--batch-----|              |              |
    |  {updates:[{light-1,comp2,"on"},            |              |
    |            {light-1,comp3,gid=491}],        |              |
    |   animations:[{light-1,anim_click}]}        |              |
    |               |              |              |              |
    |  GameScene: handleReplication()              |              |
    |  -> compId 2: state="on" -> show glow overlay|             |
    |  -> compId 3: swap sprite to gid 491 frame   |             |
    |  -> animation: play "clic" sound             |             |
```

### 6.2 Immediate mode: Press E near switch → toggles remote lights

```
 Browser          Pusher        WorldSim       ext-props
    |               |              |              |
    |--key:E------->|--action----->|              |
    |               |  adjacentEntitiesLocked()  |
    |               |  adjacent = [{switch-1,    |
    |               |    type:"light_switch",     |
    |               |    on_interact_action:"toggle",            |
    |               |    interactions:{toggle:[   |
    |               |      {action:"toggle",targets:["switch-1"]},|
    |               |      {action:"toggle",targets:["light-1"]}, |
    |               |      {action:"toggle",targets:["light-2"]}]|
    |               |    }}}]                       |
    |               |              |              |
    |               |  target_entities lookup:    |
    |               |  switch-1 targets = [light-1, light-2]     |
    |               |  -> target_entities = [     |
    |               |       {light-1, state:"off", gid:508, gid_on:491},|
    |               |       {light-2, state:"off", gid:508, gid_on:491}]|
    |               |              |              |
    |               |  dispatch--->|              |
    |               |              |  ext-props:  |
    |               |              |  has on_interact_action="toggle" -> immediate
    |               |              |  reads interactions["toggle"] -> 3 effects
    |               |              |  effect 1: {toggle, [switch-1]}  |
    |               |              |    -> toggle switch-1: off->on  |
    |               |              |    -> Updates: [{switch-1,"on"}]|
    |               |              |    -> AppearanceUpdates: [{switch-1, 381}]
    |               |              |    -> Animations: [{switch-1, click}]
    |               |              |  effect 2: {toggle, [light-1]}  |
    |               |              |    -> find light-1 in target_entities
    |               |              |    -> toggle: off->on           |
    |               |              |    -> Updates: [{light-1,"on"}] |
    |               |              |    -> AppearanceUpdates: [{light-1, 491}]
    |               |              |  effect 3: {toggle, [light-2]}  |
    |               |              |    -> Updates: [{light-2,"on"}] |
    |               |              |    -> AppearanceUpdates: [{light-2, 491}]
    |               |              |<--reply------|              |
    |               |              |              |              |
    |               |  applyActionReply():        |
    |               |    switch-1: state="on", gid=381, dirtyAppearance
    |               |    light-1: state="on", gid=491, dirtyAppearance
    |               |    light-2: state="on", gid=491, dirtyAppearance
    |               |    pendingAnimations: [switch-1 -> click]
    |               |              |              |
    |               |  sendActionResult(ok:true, available_actions:[])
    |<--actionResult-|              |              |
    |               |              |              |
    |               |  Next replication tick:     |
    |<--replication--|  updates: [                 |
    |               |    {switch-1, comp2, "on"}, {switch-1, comp3, 381},
    |               |    {light-1,  comp2, "on"}, {light-1,  comp3, 491},
    |               |    {light-2,  comp2, "on"}, {light-2,  comp3, 491}]
    |               |  animations: [{switch-1, click}]
    |               |              |              |
    |  GameScene:   |              |              |
    |  -> switch-1: sprite swaps, click sound     |
    |  -> light-1: sprite swaps, glow overlay     |
    |  -> light-2: sprite swaps, glow overlay     |
    |  -> available_actions empty -> no popup     |
```

### 6.3 Multi-extension: Press E near door → sprite + wall + notification

```
 Browser          Pusher        WorldSim       ext-props      ext-walls    ext-notif
    |               |              |              |              |              |
    |--key:E------->|--action----->|              |              |              |
    |               |  adjacent = [{door-2,       |              |              |
    |               |    type:"door",             |              |              |
    |               |    on_interact_action:"toggle",            |              |
    |               |    interactions:{toggle:[   |              |              |
    |               |      {action:"toggle",targets:["door-2"]}, |              |
    |               |      {action:"toggle_wall",targets:["room-2-walls"]},    |
    |               |      {action:"send_notification",payload:"door-2 opened",targets:[]}
    |               |    ]}}]                       |              |              |
    |               |              |              |              |              |
    |               |  ExtensionsForInput("key:E") = ["props","walls","notifications"]
    |               |              |              |              |              |
    |               |  --- dispatch --->|         |              |              |
    |               |  (same payload to all)      |              |              |
    |               |              |              |              |              |
    |               |              |  ext-props:  |              |              |
    |               |              |  reads interactions["toggle"]              |
    |               |              |  effect {toggle,[door-2]} -> I handle "toggle"
    |               |              |  effect {toggle_wall,[room-2-walls]} -> skip
    |               |              |  effect {send_notification,...} -> skip    |
    |               |              |  -> Updates: [{door-2,"on"}]               |
    |               |              |  -> AppearanceUpdates: [{door-2,501}]      |
    |               |              |  -> Animations: [{door-2,click}]           |
    |               |              |<--reply------|              |              |
    |               |              |              |              |              |
    |               |  --- dispatch ------------->|              |              |
    |               |              |              |  ext-walls:  |              |
    |               |              |              |  reads interactions["toggle"]              |
    |               |              |              |  effect {toggle,[door-2]} -> skip          |
    |               |              |              |  effect {toggle_wall,[room-2-walls]} -> I handle
    |               |              |              |  toggles gate trigger on room-2-walls      |
    |               |              |              |  re-publishes register_triggers            |
    |               |              |              |  -> handled=true (no entity updates)       |
    |               |              |<-------------------reply---|              |              |
    |               |              |              |              |              |
    |               |  --- dispatch ----------------------------->|              |
    |               |              |              |              |  ext-notif:  |
    |               |              |              |              |  reads interactions["toggle"]              |
    |               |              |              |              |  effect {toggle,[door-2]} -> skip          |
    |               |              |              |              |  effect {toggle_wall,...} -> skip         |
    |               |              |              |              |  effect {send_notification,payload:"door-2 opened"} -> I handle
    |               |              |              |              |  sends Slack message with payload          |
    |               |              |              |              |  -> handled=true (no entity updates)       |
    |               |              |<-----------------------------reply--------|
    |               |              |              |              |              |
    |               |  applyActionReply(ext-props): door-2 state/sprite/anim   |
    |               |  applyActionReply(ext-walls): nothing (gate trigger already updated)|
    |               |  applyActionReply(ext-notif): nothing                     |
    |               |              |              |              |              |
    |               |  sendActionResult(ok:true, available_actions:[])         |
    |<--actionResult-|              |              |              |              |
    |               |              |              |              |              |
    |               |  Next replication tick:     |              |              |
    |<--replication--|  updates:[{door-2,comp2,"on"},{door-2,comp3,501}]       |
    |               |  animations:[{door-2,click}]|              |              |
    |               |              |              |              |              |
    |  GameScene:   |              |              |              |              |
    |  -> door-2: sprite swaps to open, click sound              |              |
    |  -> wall zone "room-2-walls" no longer blocks (gate trigger removed)      |
    |  -> players can walk through the doorway    |              |              |
```

### 6.4 Sparks on approach (client-side, no server round-trip)

```
 GameScene.update() each frame
    |
    |  for each avatar where avatar.interactable === true:
    |    dist = distance(localPlayer, avatar.sprite)
    |    if dist <= avatar.triggerRadius and not avatar.sparksShown:
    |      play one-shot sparks animation above avatar
    |      avatar.sparksShown = true
    |    if dist > avatar.triggerRadius:
    |      avatar.sparksShown = false  (reset so it can fire again on re-entry)
```


## 7. Extension Handler Pattern

Extensions are **generic interpreters**: they read the effects list
from the entity data and handle the action verbs they know, skipping
the rest. Adding a new action verb means adding one `case` to the
extension's switch — zero framework changes.

### 7.1 ext-props (handles entity state + sprite)

```go
// handleEffect processes a single effect. ext-props handles action
// verbs related to entity state and appearance. It skips verbs it
// doesn't know (toggle_wall, send_notification, etc.).
func handleEffect(fx effect, dispatch *actionDispatchMsg, resp *actionReplyMsg) {
    switch fx.Action {
    case "toggle":
        for _, tid := range fx.TargetIDs {
            target := findTargetInDispatch(dispatch, tid)
            if target == nil { continue }
            isOn := target.State != "on"
            newState := "off"
            if isOn { newState = "on" }
            resp.Updates = append(resp.Updates, struct {
                EntityID string `json:"entity_id"`
                State    string `json:"state"`
            }{tid, newState})
            if target.GidOn != 0 {
                gid := target.GidOff
                if gid == 0 { gid = target.Gid }
                if isOn { gid = target.GidOn }
                resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
                    EntityID string `json:"entity_id"`
                    Gid      uint32 `json:"gid"`
                }{tid, gid})
            }
        }

    case "set_state":
        for _, tid := range fx.TargetIDs {
            resp.Updates = append(resp.Updates, struct {
                EntityID string `json:"entity_id"`
                State    string `json:"state"`
            }{tid, fx.Payload})
            target := findTargetInDispatch(dispatch, tid)
            if target != nil && target.GidOn != 0 {
                gid := target.GidOff
                if gid == 0 { gid = target.Gid }
                if fx.Payload == "on" { gid = target.GidOn }
                resp.AppearanceUpdates = append(resp.AppearanceUpdates, struct {
                    EntityID string `json:"entity_id"`
                    Gid      uint32 `json:"gid"`
                }{tid, gid})
            }
        }

    case "activate", "turn_on":
        // Same as set_state with payload "on"
        // ...

    case "deactivate", "turn_off":
        // Same as set_state with payload "off". Uses GidOff (the
        // original "off" sprite) rather than Gid (the current gid,
        // which may be GidOn if the entity is currently "on").
        for _, tid := range fx.TargetIDs {
            resp.Updates = append(resp.Updates, {tid, "off"})
            target := findTargetInDispatch(dispatch, tid)
            if target != nil {
                gid := target.GidOff
                if gid == 0 { gid = target.Gid }
                resp.AppearanceUpdates = append(resp.AppearanceUpdates, {tid, gid})
            }
        }

    // "toggle_wall", "send_notification", etc. -> not handled, skip
    }
}
```

### 7.2 ext-walls (handles wall zone toggling)

ext-walls registers for `key:E` in addition to its existing gate
trigger registration. It handles `"toggle_wall"` effects by toggling
gate triggers on the target zones:

```go
func handleEffect(fx effect, dispatch *actionDispatchMsg, resp *actionReplyMsg) {
    switch fx.Action {
    case "toggle_wall":
        for _, zoneID := range fx.TargetIDs {
            if activeWallZones[zoneID] {
                delete(activeWallZones, zoneID)  // open the wall
            } else {
                activeWallZones[zoneID] = true   // close the wall
            }
        }
        reRegisterGateTriggers()  // publish updated register_triggers
        resp.Handled = true

    // "toggle", "set_state", "send_notification", etc. -> not handled, skip
    }
}
```

### 7.3 ext-notifications (hypothetical — handles external notifications)

```go
func handleEffect(fx effect, dispatch *actionDispatchMsg, resp *actionReplyMsg) {
    switch fx.Action {
    case "send_notification":
        sendSlack(fx.Payload)  // or email, webhook, etc.
        resp.Handled = true

    // "toggle", "toggle_wall", etc. -> not handled, skip
    }
}
```

### 7.4 The key:E handler (routing logic)

This is the only "framework-aware" code in each extension — it reads
the routing data and calls the handler for each effect:

```go
nc.Subscribe("extension."+extID+".action", func(m *nats.Msg) {
    var dispatch actionDispatchMsg
    json.Unmarshal(m.Data, &dispatch)

    resp := actionReplyMsg{}
    for _, ent := range dispatch.AdjacentEntities {
        if !owns(ent) { continue }

        // Immediate mode: entity declares on_interact_action
        if ent.OnInteractAction != "" {
            effects := ent.Interactions[ent.OnInteractAction]
            for _, fx := range effects {
                handleEffect(fx, &dispatch, &resp)
            }
            // Click sound/animation on the interacted entity itself
            if resp.Handled {
                resp.Animations = append(resp.Animations, struct {
                    EntityID    string `json:"entity_id"`
                    AnimationID uint32 `json:"animation_id"`
                }{ent.EntityID, animClick})
            }
        }

        // Popup mode: entity declares accepted actions
        if ent.Actions != "" {
            for _, actionID := range strings.Split(ent.Actions, ",") {
                actionID = strings.TrimSpace(actionID)
                label, visible := buildPopupAction(actionID, ent.State)
                if visible {
                    resp.AvailableActions = append(resp.AvailableActions, availableAction{
                        EntityID:    ent.EntityID,
                        ActionID:    actionID,
                        Label:       label,
                        EntityLabel: ent.EntityType,
                    })
                }
            }
            if len(resp.AvailableActions) > 0 {
                resp.Handled = true
            }
        }
    }

    if !resp.Handled { return }
    data, _ := json.Marshal(resp)
    m.Respond(data)
})
```

### 7.5 The action:execute handler (popup choice)

When the user clicks a popup action, the client sends
`action:execute`. The extension finds the target entity, looks up
`interactions[action_id]`, and processes each effect:

```go
// Same subscription, but dispatch.Input == "action:execute"
if dispatch.Input == "action:execute" {
    for _, ent := range dispatch.AdjacentEntities {
        if ent.EntityID != dispatch.TargetEntityID || !owns(ent) { continue }
        effects := ent.Interactions[dispatch.ActionID]
        for _, fx := range effects {
            handleEffect(fx, &dispatch, &resp)
        }
        if resp.Handled {
            resp.Animations = append(resp.Animations, {ent.EntityID, animClick})
        }
    }
}
```

### 7.6 Popup label builder

The extension decides which actions to show in the popup and how to
label them, optionally filtering based on current state. This is
extension code, not framework code:

```go
func buildPopupAction(actionID, currentState string) (label string, visible bool) {
    switch actionID {
    case "toggle":
        return "Toggle", true
    case "activate", "turn_on":
        return "Activate", currentState != "on"
    case "deactivate", "turn_off":
        return "Deactivate", currentState == "on"
    case "lock":
        return "Lock", currentState != "locked"
    case "unlock":
        return "Unlock", currentState == "locked"
    default:
        // Fallback: show the action string itself, always visible.
        // This lets entity authors use custom action IDs without
        // extension code changes to the label builder.
        return strings.Title(actionID), true
    }
}
```


## 8. Data Structures

### 8.1 Tiled property parsing (mapdata.go)

```go
// Effect is a single action to apply to a set of targets.
type Effect struct {
    Action    string   `json:"action"`
    Payload   string   `json:"payload,omitempty"`
    TargetIDs []string `json:"target_ids"`
}

// PropEntity is a base entity authored on the "Entities" object layer.
type PropEntity struct {
    ID             string
    X, Y           float32
    EntityType     string
    OwnerExtension string
    TriggerRadius  float32
    Gid            uint32
    GidOn          uint32                // NEW — alternate sprite gid
    OnInteractAction string              // NEW — immediate mode trigger
    Actions          string              // NEW — popup mode action list
    Interactions     map[string][]Effect // NEW — action_id -> effects
}
```

Property parsing additions in `mapdata.go` (alongside the existing
`entity_type`, `owner_extension`, `trigger_radius` cases):

```go
case "gid_on":
    if f, ok := prop.Value.(float64); ok {
        pe.GidOn = uint32(f)
    }
case "on_interact_action":
    if s, ok := prop.Value.(string); ok {
        pe.OnInteractAction = s
    }
case "actions":
    if s, ok := prop.Value.(string); ok {
        pe.Actions = s
    }
case "interactions":
    if s, ok := prop.Value.(string); ok {
        pe.Interactions = make(map[string][]Effect)
        json.Unmarshal([]byte(s), &pe.Interactions)
    }
```

### 8.2 Entity struct (worldsim.go)

```go
type Entity struct {
    // ... existing fields ...
    EntityType     string
    OwnerExtension string
    TriggerRadius  float32
    Gid            uint32
    GidOff         uint32                // NEW — original gid ("off" sprite)
    GidOn          uint32                // NEW — alternate gid ("on" sprite)
    OnInteractAction string              // NEW
    Actions          string              // NEW
    Interactions     map[string][]Effect // NEW
    State          string
    // ... dirty flags, etc. ...
}
```

`GidOff` stores the original gid from map load. The extension reads
`Gid`, `GidOff`, and `GidOn` from the dispatch and returns
`AppearanceUpdates` with the appropriate gid. Worldsim applies them:
`e.Gid = u.Gid; e.dirtyAppearance = true`.

`GidOff` is included in the dispatch (section 8.3) so the extension
can set the sprite back to the "off" frame when deactivating an
entity that is currently "on" (where `Gid` would be `GidOn`, not the
original off sprite).

### 8.3 Dispatch payload (worldsim.go)

```go
type adjacentEntityInfo struct {
    EntityID         string   `json:"entity_id"`
    EntityType       string   `json:"entity_type,omitempty"`
    OwnerExtension   string   `json:"owner_extension,omitempty"`
    State            string   `json:"state,omitempty"`              // NEW
    Gid              uint32   `json:"gid,omitempty"`                // NEW (current gid)
    GidOff           uint32   `json:"gid_off,omitempty"`            // NEW (original "off" gid)
    GidOn            uint32   `json:"gid_on,omitempty"`             // NEW
    OnInteractAction string   `json:"on_interact_action,omitempty"` // NEW
    Actions          string   `json:"actions,omitempty"`            // NEW
    Interactions     map[string][]Effect `json:"interactions,omitempty"` // NEW
}

type actionDispatchMsg struct {
    EntityID         string               `json:"entity_id"`
    Input            string               `json:"input"`
    AdjacentEntities []adjacentEntityInfo `json:"adjacent_entities"`
    TargetEntities   []adjacentEntityInfo `json:"target_entities,omitempty"` // NEW
    TargetEntityID   string               `json:"target_entity_id,omitempty"` // NEW (for action:execute)
    ActionID         string               `json:"action_id,omitempty"`        // NEW (for action:execute)
}
```

`TargetEntities` is populated by worldsim: for each adjacent entity,
collect all `target_ids` from its `Interactions` effects, look them up
in `s.entities`, and include them in `TargetEntities`. This gives
extensions access to far-away entities' current state in a single
dispatch.

### 8.4 Reply payload (worldsim.go)

```go
type availableAction struct {
    EntityID    string `json:"entity_id"`
    ActionID    string `json:"action_id"`
    Label       string `json:"label"`
    EntityLabel string `json:"entity_label,omitempty"`
}

type actionReplyMsg struct {
    Handled           bool `json:"handled"`
    Updates           []struct {
        EntityID string `json:"entity_id"`
        State    string `json:"state"`
    } `json:"updates,omitempty"`
    AppearanceUpdates []struct {
        EntityID string `json:"entity_id"`
        Gid      uint32 `json:"gid"`
    } `json:"appearance_updates,omitempty"` // NEW
    Animations        []struct {
        EntityID    string `json:"entity_id"`
        AnimationID uint32 `json:"animation_id"`
    } `json:"animations,omitempty"`
    AvailableActions  []availableAction `json:"available_actions,omitempty"` // NEW
}
```

`AppearanceUpdates` is **extension-controlled**. The extension decides
which gid to set based on the action and the entity's `GidOn`/`Gid`
fields. Worldsim just applies: `e.Gid = u.Gid; e.dirtyAppearance =
true`. This keeps worldsim generic — it doesn't know which state maps
to which sprite.

### 8.5 Proto changes

**ActionFrame** — add target entity + action ID for the popup→execute
phase:

```protobuf
message ActionFrame {
  uint32 seq = 1;
  string input = 2;         // "key:E" or "action:execute"
  string traceparent = 3;
  string entity_id = 4;     // target entity for "action:execute" (empty for "key:E")
  string action_id = 5;     // which action to run (e.g. "activate")
}
```

**ActionResultFrame** — carry available actions for the popup:

```protobuf
message AvailableAction {
  string entity_id = 1;
  string action_id = 2;
  string label = 3;         // "Activate", "Deactivate", "Toggle"
  string entity_label = 4;  // "light", "door" (popup grouping/header)
}

message ActionResultFrame {
  uint32 seq = 1;
  bool ok = 2;
  string reason = 3;
  repeated AvailableAction available_actions = 4;  // NEW
}
```

**Appearance** — add interactable flag for client-side sparks:

```protobuf
message Appearance {
  uint32 gid = 1;
  reserved 2;
  string sprite_base = 3;
  bool interactable = 4;  // NEW — true for entities that support interactions
}
```

The `interactable` flag is set by worldsim at spawn time: `true` for
entities with `EntityType != ""` or `OwnerExtension != ""`. The
frontend uses it to know which entities to poll for sparks-on-approach.


## 9. Frontend Changes

### 9.1 WsClient (WsClient.ts)

**Add animations to ReplicationBatchView:**

```typescript
export interface PlayAnimationView {
  entityId: string;
  animationId: number;
}

export interface ReplicationBatchView {
  lastInputSeq: number;
  spawns: SpawnEntityView[];
  updates: UpdateComponentView[];
  destroys: DestroyEntityView[];
  animations: PlayAnimationView[];  // NEW
}
```

Map `batch.animations` in the replication handler (currently dropped).

**Add onActionResult handler:**

```typescript
export interface AvailableActionView {
  entityId: string;
  actionId: string;
  label: string;
  entityLabel: string;
}

export interface ActionResultView {
  seq: number;
  ok: boolean;
  reason: string;
  availableActions: AvailableActionView[];
}
```

Replace the current `console.log` in the `actionResult` case with a
call to `this.handlers.onActionResult?.(result)`.

**Extend sendAction:**

```typescript
sendAction(input: string, entityId?: string, actionId?: string): number
```

When `input === "action:execute"`, include `entityId` and `actionId`
in the ActionFrame.

### 9.2 GameScene (GameScene.ts)

**Handle component 2 (EntityState) in handleReplication():**

For props: show/hide glow overlay based on state. Store state on
Avatar for popup label decisions.

```typescript
} else if (upd.componentId === 2) {
  const st = fromBinary(EntityStateSchema, upd.data);
  avatar.state = st.state;
  if (avatar.isProp) {
    if (st.state === "on") this.showGlowOverlay(avatar);
    else this.hideGlowOverlay(avatar);
  }
}
```

**Handle component 3 (Appearance) for props:**

Currently only avatars hot-swap sprites. Extend to swap prop sprite
frame when gid changes:

```typescript
if (avatar.isProp) {
  const app = fromBinary(AppearanceSchema, upd.data);
  if (app.gid !== 0) {
    const mapped = gidToFrame(app.gid, this.tilesets);
    if (mapped) avatar.sprite.setTexture(mapped.sheet, mapped.frame);
  }
}
```

Also read `app.interactable` at spawn time and store on Avatar.

**Handle batch.animations in handleReplication():**

```typescript
for (const anim of batch.animations) {
  const avatar = this.avatars.get(anim.entityId);
  if (!avatar) continue;
  if (anim.animationId === ANIM_CLICK) {
    this.sound.play("clic", { volume: 0.5 });
  }
}
```

**Interaction popup:**

New method `openInteractionPopup(actions: AvailableActionView[])`
modeled on the existing `openDropdown()` pattern (line 1737). Groups
actions by `entityLabel`, shows buttons with `label` text. On click:
`this.ws?.sendAction("action:execute", action.entityId, action.actionId)`
and close popup.

Position the popup relative to the screen center (not attached to a
single sprite) since multiple entities may be involved.

**Wire onActionResult handler:**

If `availableActions.length > 0`, call `openInteractionPopup()`. If
empty and `ok`, do nothing (immediate action already replicated).

**Glow overlay:**

A `Phaser.GameObjects.Sprite` with a pre-loaded 7x7 tile PNG
(`light-glow.png`), positioned above the light entity, initially
invisible. Show/hide based on state. Use additive blend mode for a
glow effect.

**Sparks on approach:**

In `update()`, poll distance from local player to each
`avatar.interactable` entity. When entering range, play a one-shot
sparks animation above the entity.

The trigger radius is hardcoded at 2.0 tiles on the client because
`trigger_radius` is a Tiled property that is not replicated to the
client in any component. Adding it to the Appearance proto would be
a heavier change; the hardcoded value is pragmatic and can be
upgraded later if per-entity radius becomes important.

**Load assets in preload():**

```typescript
this.load.audio("clic", "/assets/sounds/clic.wav");
this.load.image("light-glow", "/assets/sprites/light-glow.png");
this.load.spritesheet("sparks", "/assets/sprites/sparks.png", {
  frameWidth: 32, frameHeight: 32,
});
```

### 9.3 Animation ID mapping

Animation IDs are shared constants between extensions and the
frontend. The existing pattern uses `animationOn=1`, `animationOff=2`
in ext-props. The frontend needs a matching mapping:

```typescript
const ANIM_CLICK = 3;   // click sound (matches animationClick=3 in ext-props)
```

Note: sparks on approach is client-side only (no server round-trip),
so it doesn't use the PlayAnimation replication path. It's triggered
directly in `update()` and uses a local spritesheet animation, not an
animation ID.


## 10. Implementation Steps

### Phase 1: Proto changes

1. Add `entity_id`, `action_id` to `ActionFrame` in `proto/frames.proto`
2. Add `AvailableAction` message and `available_actions` to
   `ActionResultFrame` in `proto/frames.proto`
3. Add `interactable` bool to `Appearance` in `proto/components.proto`
4. Run `make proto` to regenerate Go + TypeScript

### Phase 2: Backend worldsim

5. Add `GidOn`, `OnInteractAction`, `Actions`, `Interactions` to
   `PropEntity` in `mapdata.go`. Parse the new Tiled properties.
6. Add `GidOff`, `GidOn`, `OnInteractAction`, `Actions`,
   `Interactions` to `Entity` struct in `worldsim.go`. Set them in
   `loadBaseEntities()`.
7. Extend `adjacentEntityInfo` with `State`, `Gid`, `GidOff`, `GidOn`,
   `OnInteractAction`, `Actions`, `Interactions`.
8. Extend `actionDispatchMsg` with `TargetEntities`,
   `TargetEntityID`, `ActionID`.
9. Update `adjacentEntitiesLocked()` to include the new fields.
10. Add `TargetEntities` lookup in `applyAction()`: collect
    `target_ids` from adjacent entities' `Interactions`, look up in
    `s.entities`, include in dispatch.
11. Extend `actionReplyMsg` with `AppearanceUpdates` and
    `AvailableActions`.
12. Update `applyActionReply()` to apply `AppearanceUpdates` (`e.Gid
    = u.Gid; e.dirtyAppearance = true`).
13. Update `sendActionResult()` to accept and include
    `availableActions`.
14. Update `applyAction()` to aggregate `AvailableActions` from all
    extension replies and pass them to `sendActionResult()`.
15. Update `replicateToClient()` to set `Interactable: true` in the
    Appearance component for entities with `EntityType != ""` or
    `OwnerExtension != ""`.

### Phase 3: Extension changes

16. Update `ext-props/main.go`:
    - Extend `adjacentEntityInfo` and `actionReplyMsg` structs to
      match worldsim.
    - Add `Effect` struct and `Interactions` map.
    - Implement `handleEffect()` for "toggle", "set_state",
      "activate", "deactivate", "turn_on", "turn_off".
    - Implement `buildPopupAction()` for popup labels.
    - Update `key:E` handler: read `on_interact_action` (immediate)
      and `actions` (popup) from dispatch.
    - Add `action:execute` handler: look up
      `interactions[action_id]`, process effects.
    - Register for `action:execute` input trigger.

17. Update `ext-walls/main.go`:
    - Register for `key:E` input trigger (in addition to existing
      gate triggers).
    - Add `handleEffect()` for `"toggle_wall"`.
    - On `"toggle_wall"`: toggle zone in `activeWallZones`,
      re-publish `register_triggers`.

### Phase 4: Frontend WsClient

18. Add `PlayAnimationView` and `animations` to
    `ReplicationBatchView`. Map `batch.animations` in the replication
    handler.
19. Add `AvailableActionView`, `ActionResultView`, and
    `onActionResult` handler. Replace `console.log` in `actionResult`
    case.
20. Extend `sendAction()` with `entityId` and `actionId` parameters.

### Phase 5: Frontend GameScene

21. Handle component 2 (EntityState) in `handleReplication()`: store
    state on Avatar, show/hide glow overlay for props.
22. Handle component 3 (Appearance) for props: swap sprite frame when
    gid changes. Read `interactable` flag at spawn time.
23. Handle `batch.animations` in `handleReplication()`: play sounds
    and effects based on animation ID.
24. Add `openInteractionPopup()` method (modeled on existing
    `openDropdown()`). Group actions by `entityLabel`, wire button
    clicks to `sendAction("action:execute", entityId, actionId)`.
25. Wire `onActionResult` handler: if `availableActions.length > 0`,
    open popup. If empty, do nothing.
26. Add glow overlay: create sprite with `light-glow.png`, show/hide
    based on state, additive blend mode.
27. Add sparks on approach: poll distance in `update()`, play
    one-shot sparks animation when entering range.
28. Load assets in `preload()`: `clic.wav`, `light-glow.png`,
    `sparks.png`.

### Phase 6: Assets and map

29. Create `frontend/public/assets/sounds/clic.wav` — short click
    sound.
30. Create `frontend/public/assets/sprites/light-glow.png` — 7x7 tile
    (224x224px) PNG with soft radial gradient.
31. Create `frontend/public/assets/sprites/sparks.png` — small
    spritesheet for the sparks animation.
32. Add light, switch, and door entities to `maps/default-map.tmx`
    with the Tiled properties described in section 4.
33. Export map to `maps/default-map.json`. Re-seed PocketBase.


## 11. Verification

### Unit tests (worldsim)

- `applyActionReply` with `AppearanceUpdates` sets `e.Gid` and
  `dirtyAppearance`.
- `adjacentEntitiesLocked` includes `State`, `Gid`, `GidOn`,
  `Interactions` for entities.
- `applyAction` with `on_interact_action` entity: dispatch includes
  `TargetEntities` looked up from `Interactions` target_ids.
- `applyAction` aggregates `AvailableActions` from multiple extension
  replies.
- `mapdata.go` parses `gid_on`, `on_interact_action`, `actions`, and
  `interactions` properties correctly.

### Unit tests (ext-props)

- `key:E` on entity with `on_interact_action="toggle"` and
  `interactions` containing 3 effects: returns Updates for all
  targets.
- `key:E` on entity with `actions="toggle,activate"` and state="off":
  returns AvailableActions with "toggle" and "activate" (not
  "deactivate").
- `action:execute` with `action_id="activate"`: processes
  `interactions["activate"]` effects only.

### Manual tests

- Walk near light entity → sparks animation plays.
- Press E near light → popup shows "Toggle", "Activate" (if off).
- Click "Activate" → click sound, sprite swaps, glow overlay appears.
- Press E near switch → immediate toggle, click sound, switch + all
  target lights change sprite + glow.
- Press E near door → door sprite swaps, wall zone opens (can walk
  through), notification sent (if ext-notifications running).
- Press E near door again → door closes, wall blocks again.
- Two entities nearby (light + door) → door toggles immediately,
  popup shows for light.
- Light switch toggles lights that are far away (ceiling lights).


## 12. Risks and Considerations

1. **Target entity existence:** If `target_ids` references a
   non-existent entity, `applyActionReply` silently ignores it (the
   `s.entities[u.EntityID]` lookup fails). Map author must ensure IDs
   match. Could add a warning log in worldsim when a target_id lookup
   fails.

2. **State persistence:** ext-props tracks state in memory (`states`
   map). If ext-props restarts, state is lost. This is existing
   behavior — not a regression. A future improvement could persist to
   JetStream KV.

3. **Cross-map targets:** `target_ids` only works within the same map
   (worldsim's `s.entities` is per-map). Cross-map interactions would
   require cross-map messaging. Not in scope.

4. **Zone targets vs entity targets:** The framework resolves entity
   targets (looks them up in `s.entities` → `target_entities`). Zone
   targets (e.g., `toggle_wall` targeting "room-2-walls") are NOT
   resolved by the framework — the extension looks them up in its own
   local zone metadata. This is by design: the framework doesn't know
   about zones; that's extension-specific knowledge.

5. **Multiple extensions for `key:E`:** If multiple extensions
   register for `key:E`, worldsim dispatches to all (existing
   behavior). Each self-filters by `owner_extension` and handles only
   the action verbs it knows. The 300ms RPC timeout per extension
   still applies.

6. **Sound autoplay policy:** The existing "Enable Audio" button in
   TopMenu unlocks browser audio. The `clic` sound will only play
   after the user has clicked that button. Consistent with existing
   A/V audio approach.

7. **Popup positioning:** The popup should be positioned relative to
   the screen center (or the closest entity), not attached to a single
   sprite — since multiple entities may be involved. This differs from
   the existing name-tag dropdown which attaches to one avatar.

8. **interactions JSON validation:** The `interactions` property is a
   JSON string in Tiled. If the JSON is malformed, `mapdata.go`
   silently ignores it (the `json.Unmarshal` error is swallowed).
   Could add a warning log. A Tiled plugin could validate the JSON at
   authoring time, but that's out of scope.

9. **Security:** The `action:execute` dispatch includes
   `AdjacentEntities`, so the extension can verify the target entity
   is actually near the player. The extension must check
   `dispatch.TargetEntityID` is in `dispatch.AdjacentEntities` before
   executing. For immediate mode, the effects only fire if the entity
   is adjacent (it's in the dispatch because it's adjacent).

10. **Extension vocabulary growth:** Adding a new action verb (e.g.,
    "play_sound", "spawn_particle") means adding a `case` to the
    relevant extension's `handleEffect` switch. The framework never
    changes. This is the core design property — the framework is a
    stable, generic router; extensions grow their vocabulary
    independently.
