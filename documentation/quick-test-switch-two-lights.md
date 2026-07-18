# Testing a Switch That Activates Two Lights

This is a quick guide to modify the current Tiled map
(`maps/default-map.json`) so that pressing E on a wall switch toggles
two ceiling lights at once.

## What's already in the map

The map has two entities on the "Entities" layer:

| Entity | gid | Mode | What it does now |
|---|---|---|---|
| `light-switch-1` | 380 | Immediate (`on_interact_action=toggle`) | Toggles only itself |
| `light-1` | 508 | Popup (`actions=toggle,activate,deactivate`) | Popup with 3 actions, toggles only itself |

## What you need to change

### 1. Update `light-switch-1` interactions

The switch currently targets only itself. Change its `interactions`
property to target both lights (and itself for the sprite swap):

**Current value:**
```json
{"toggle":[{"action":"toggle","target_ids":["light-switch-1"]}]}
```

**New value:**
```json
{"toggle":[
  {"action":"toggle","target_ids":["light-switch-1"]},
  {"action":"toggle","target_ids":["light-1"]},
  {"action":"toggle","target_ids":["light-2"]}
]}
```

This makes pressing E on the switch fire three toggle effects: the
switch itself (sprite swap) + light-1 + light-2.

### 2. Add a second light entity (`light-2`)

Add a new tile-object on the "Entities" layer:

- **Name:** `light-2`
- **gid:** `508` (same off sprite as light-1)
- **Position:** anywhere on the map, e.g. x=256, y=320 (a few tiles
  right of light-1)
- **Custom properties:**

| Property | Type | Value | Notes |
|---|---|---|---|
| `entity_type` | String | `light` | |
| `owner_extension` | String | `props` | |
| `trigger_radius` | Float | `1.5` | Not needed for the switch scenario but good to have |
| `gid_on` | Int | `491` | On sprite (same as light-1) |
| `actions` | String | `toggle,activate,deactivate` | Optional: lets you also interact with it directly via popup |
| `interactions` | String (JSON) | see below | |

**interactions JSON for light-2:**
```json
{"toggle":[{"action":"toggle","target_ids":["light-2"]}],"activate":[{"action":"set_state","payload":"on","target_ids":["light-2"]}],"deactivate":[{"action":"set_state","payload":"off","target_ids":["light-2"]}]}
```

## Step-by-step in Tiled

1. Open `maps/default-map.tmx` in Tiled.
2. Select the **Entities** object layer.
3. **Edit the switch:** right-click `light-switch-1` → Object Properties.
   Find the `interactions` property and replace its value with:
   ```
   {"toggle":[{"action":"toggle","target_ids":["light-switch-1"]},{"action":"toggle","target_ids":["light-1"]},{"action":"toggle","target_ids":["light-2"]}]}
   ```
4. **Add light-2:** drag a tile (gid 508 — the lamp off sprite) from
   the tileset onto the Entities layer. Place it a few tiles from
   light-1.
5. **Name it:** right-click the new object → Object Properties → set
   Name to `light-2`.
6. **Add properties:** click `+` in the custom properties panel and add
   each property from the table above. For `interactions`, paste the
   JSON string.
7. **Save and export:** File → Export As… → `default-map.json`
   (overwrite the existing file).
8. **Rebuild:** `make web` then `make up` (or restart worldsim +
   frontend).

## How to test in-game

1. Walk your avatar near `light-switch-1` (within 1.5 tiles).
2. Press **E**.
3. You should see:
   - The switch sprite swap (gid 380 → 381).
   - A click sound (`clic.wav`).
   - `light-1` sprite swap (gid 508 → 491) — the lamp turns on.
   - `light-2` sprite swap (gid 508 → 491) — the second lamp turns on.
   - Sparks particles near both lights (client-side, within 2 tiles).
4. Press **E** again. All three swap back to their off sprites.

## How it works

```
Player presses E near light-switch-1
  → client sends ActionFrame{input: "key:E"}
  → worldsim computes adjacent entities (finds light-switch-1)
  → worldsim collects target entities from interactions target_ids
    (finds light-1 and light-2 in the ECS, even though they're far away)
  → dispatches to ext-props with all three entities in the payload
  → ext-props processes the "toggle" effect for each target_id:
      light-switch-1: state off→on, gid 380→381
      light-1:        state off→on, gid 508→491
      light-2:        state off→on, gid 508→491
  → worldsim applies updates, replicates to clients
  → frontend swaps sprites, plays click sound, shows sparks
```

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| Pressing E does nothing | `interactions` JSON is malformed | Validate the JSON string in a linter |
| Only the switch swaps, lights don't | `target_ids` don't match light names | Ensure target_ids match the **Name** field exactly (`light-1`, `light-2`) |
| Lights swap but no sparks | Player is too far from the lights | Sparks only show within 2 tiles of the player. Walk closer to the lights. |
| `light-2` not found | Entity doesn't exist on the Entities layer | Add the object in Tiled, give it the Name `light-2`, export, restart |
| 404 for clic.wav | Game assets not synced | Run `make web` (runs sync-game-assets) or `make sync-assets` |
