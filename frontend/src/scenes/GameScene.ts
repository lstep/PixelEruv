import Phaser from "phaser";
import { fromBinary } from "@bufbuild/protobuf";
import { AppearanceSchema, DisplayNameSchema, EntityStateSchema, LightEmitterSchema } from "../proto/components_pb";
import { WsClient, decodePosition, ReplicationBatchView, ConnectionState, AvailableActionView } from "../net/WsClient";
import { AvClient } from "../net/AvClient";
import { AfkDetector } from "../net/AfkDetector";
import { VideoBar } from "../ui/VideoBar";
import { ScreenShareOverlay } from "../ui/ScreenShareOverlay";
import { DayNightOverlay } from "../ui/DayNightOverlay";
import { VirtualJoystick } from "../ui/VirtualJoystick";
import type { MapAssets } from "../mapLoader";
import { loadMapAssets } from "../mapLoader";
import type { SpriteBaseAsset } from "../spriteLoader";
import type { TopMenu } from "../ui/TopMenu";
import { parsePlayerOptions } from "../ui/TopMenu";
import type { ChatPanel } from "../ui/ChatPanel";
import { getUsername } from "../username";
import { isLoggedIn } from "../auth";

const TILE_SIZE = 32;

// Animation IDs shared between extensions and the frontend.
const ANIM_CLICK = 3;   // click sound effect

// --- Decoration layers / depth bands ---
// See documentation/plans/2026-07-05-decoration-layers-and-interactive-entities-design.md
// Part B for the full design. Any layer (tile or object) with a
// `layer_type=decoration` custom property is recognized, regardless of
// name. Altitude is the layer's position in the Tiled layer list. A
// per-layer `sort_mode` property picks the depth band:
//   - "static" (default): fixed band by layer order, never interleaves
//     with avatars.
//   - "dynamic": shares DEPTH_BAND_DYNAMIC with avatars, sorted by base/feet
//     Y so tall objects can occlude / be occluded by the player.
const DEPTH_BAND_DYNAMIC = 1000;
const DEPTH_BAND_STATIC_ABOVE = 2000;
// "Walls" (collision fallback, not a decoration layer) sits between the
// static-below decorations and the dynamic band, preserving the previous
// ground(0) < walls(1) < avatar(2) ordering at the new, wider scale.
const DEPTH_WALLS_FALLBACK = 500;

// Minimal Tiled JSON shapes we read directly (Phaser's Tilemap doesn't
// expose original layer order across tile + object layers together).
interface TiledPropertyJSON { name: string; type: string; value: unknown }
interface TiledObjectJSON {
  name: string;
  gid?: number;
  x: number;
  y: number;
  width: number;
  height: number;
  ellipse?: boolean;
  polygon?: { x: number; y: number }[];
  properties?: TiledPropertyJSON[];
}
interface TiledLayerJSON {
  name: string;
  type: string; // "tilelayer" | "objectgroup"
  properties?: TiledPropertyJSON[];
  objects?: TiledObjectJSON[];
  visible?: boolean;
}
interface TiledMapJSON {
  tilewidth: number;
  tileheight: number;
  tilesets: { firstgid: number; name: string; tilewidth?: number; tileheight?: number; tilecount?: number }[];
  layers: TiledLayerJSON[];
}

function layerProp(props: TiledPropertyJSON[] | undefined, name: string): unknown {
  return props?.find((p) => p.name === name)?.value;
}

// Wall zone in tile coordinates, parsed from the Tiled "Zones" object layer.
// Used for client-side prediction of zone collision (matching the server's
// swept segment-vs-shape test) so the local avatar doesn't predict through
// wall zones and rubber-band on reconciliation.
type WallZone =
  | { shape: "rect"; x: number; y: number; w: number; h: number }
  | { shape: "circle"; cx: number; cy: number; r: number }
  | { shape: "polygon"; verts: [number, number][] };

// --- Swept collision helpers (ported from worldsim_swept.go / zones.go) ---
// All operate in continuous tile coords already translated to feet space
// by the caller. Must match the server exactly to avoid prediction drift.

function segmentIntersectsRect(x0: number, y0: number, x1: number, y1: number, rx: number, ry: number, rw: number, rh: number): boolean {
  const rx1 = rx + rw;
  const ry1 = ry + rh;
  const dx = x1 - x0;
  const dy = y1 - y0;

  let t0 = 0, t1 = 1;

  if (dx === 0) {
    if (x0 < rx || x0 > rx1) return false;
  } else {
    let ta = (rx - x0) / dx;
    let tb = (rx1 - x0) / dx;
    if (ta > tb) { const tmp = ta; ta = tb; tb = tmp; }
    if (ta > t0) t0 = ta;
    if (tb < t1) t1 = tb;
    if (t0 > t1) return false;
  }

  if (dy === 0) {
    if (y0 < ry || y0 > ry1) return false;
  } else {
    let ta = (ry - y0) / dy;
    let tb = (ry1 - y0) / dy;
    if (ta > tb) { const tmp = ta; ta = tb; tb = tmp; }
    if (ta > t0) t0 = ta;
    if (tb < t1) t1 = tb;
    if (t0 > t1) return false;
  }

  return t0 <= t1;
}

function segmentIntersectsCircle(x0: number, y0: number, x1: number, y1: number, cx: number, cy: number, r: number): boolean {
  const dx = x1 - x0;
  const dy = y1 - y0;
  const lensq = dx * dx + dy * dy;

  let t = 0;
  if (lensq > 0) {
    t = ((cx - x0) * dx + (cy - y0) * dy) / lensq;
  }
  if (t < 0) t = 0; else if (t > 1) t = 1;

  const px = x0 + t * dx;
  const py = y0 + t * dy;
  const ddx = px - cx;
  const ddy = py - cy;
  return ddx * ddx + ddy * ddy <= r * r;
}

function cross2(cx: number, cy: number, dx: number, dy: number, px: number, py: number): number {
  return (dx - cx) * (py - cy) - (dy - cy) * (px - cx);
}

function segmentsIntersect(ax: number, ay: number, bx: number, by: number, cx: number, cy: number, dx: number, dy: number): boolean {
  const d1 = cross2(cx, cy, dx, dy, ax, ay);
  const d2 = cross2(cx, cy, dx, dy, bx, by);
  const d3 = cross2(ax, ay, bx, by, cx, cy);
  const d4 = cross2(ax, ay, bx, by, dx, dy);
  if ((d1 > 0) !== (d2 > 0) && (d3 > 0) !== (d4 > 0)) return true;
  return false;
}

function pointInPolygon(px: number, py: number, poly: [number, number][]): boolean {
  let inside = false;
  const n = poly.length;
  let j = n - 1;
  for (let i = 0; i < n; i++) {
    const xi = poly[i][0], yi = poly[i][1];
    const xj = poly[j][0], yj = poly[j][1];
    if ((yi > py) !== (yj > py) &&
        (px < (xj - xi) * (py - yi) / (yj - yi) + xi)) {
      inside = !inside;
    }
    j = i;
  }
  return inside;
}

function segmentIntersectsPolygon(x0: number, y0: number, x1: number, y1: number, poly: [number, number][]): boolean {
  if (pointInPolygon(x0, y0, poly) || pointInPolygon(x1, y1, poly)) return true;
  const n = poly.length;
  for (let i = 0; i < n; i++) {
    const ax = poly[i][0], ay = poly[i][1];
    const bx = poly[(i + 1) % n][0], by = poly[(i + 1) % n][1];
    if (segmentsIntersect(x0, y0, x1, y1, ax, ay, bx, by)) return true;
  }
  return false;
}

function pointSegmentDistSq(px: number, py: number, ax: number, ay: number, bx: number, by: number): number {
  const dx = bx - ax;
  const dy = by - ay;
  const lensq = dx * dx + dy * dy;
  let t = 0;
  if (lensq > 0) t = ((px - ax) * dx + (py - ay) * dy) / lensq;
  if (t < 0) t = 0; else if (t > 1) t = 1;
  const cx = ax + t * dx;
  const cy = ay + t * dy;
  const ddx = px - cx;
  const ddy = py - cy;
  return ddx * ddx + ddy * ddy;
}

function segmentSegmentDistLE(p0x: number, p0y: number, p1x: number, p1y: number, q0x: number, q0y: number, q1x: number, q1y: number, r: number): boolean {
  if (segmentsIntersect(p0x, p0y, p1x, p1y, q0x, q0y, q1x, q1y)) return true;
  const r2 = r * r;
  return pointSegmentDistSq(p0x, p0y, q0x, q0y, q1x, q1y) <= r2 ||
         pointSegmentDistSq(p1x, p1y, q0x, q0y, q1x, q1y) <= r2 ||
         pointSegmentDistSq(q0x, q0y, p0x, p0y, p1x, p1y) <= r2 ||
         pointSegmentDistSq(q1x, q1y, p0x, p0y, p1x, p1y) <= r2;
}

// segmentIntersectsPolygonExpanded tests the segment against a polygon
// expanded by radius r (Minkowski sum of polygon + disc). Ported from
// worldsim_swept.go — approximates with vertex circles + edge distance.
function segmentIntersectsPolygonExpanded(x0: number, y0: number, x1: number, y1: number, poly: [number, number][], r: number): boolean {
  if (segmentIntersectsPolygon(x0, y0, x1, y1, poly)) return true;
  const n = poly.length;
  for (let i = 0; i < n; i++) {
    const ax = poly[i][0], ay = poly[i][1];
    if (segmentIntersectsCircle(x0, y0, x1, y1, ax, ay, r)) return true;
    const bx = poly[(i + 1) % n][0], by = poly[(i + 1) % n][1];
    if (segmentSegmentDistLE(x0, y0, x1, y1, ax, ay, bx, by, r)) return true;
  }
  return false;
}

// Find the tileset that contains a given Tiled gid, and return the
// spritesheet frame index for that tile within that tileset.
function gidToFrame(gid: number, tilesets: TiledMapJSON["tilesets"]): { frame: number; sheet: string } | null {
  for (let i = tilesets.length - 1; i >= 0; i--) {
    const ts = tilesets[i];
    if (gid >= ts.firstgid) {
      return { frame: gid - ts.firstgid, sheet: `${ts.name}__tiles` };
    }
  }
  return null;
}

// Is this layer a decoration layer? Recognized by `layer_type=decoration`.
// Backward-compat: a tile layer literally named "Ground" with no
// `layer_type` property at all is still treated as a (static) decoration
// layer, so maps predating this convention keep rendering.
function isDecorationLayer(layer: TiledLayerJSON): boolean {
  if (layerProp(layer.properties, "layer_type") === "decoration") return true;
  return !layer.properties?.length && layer.type === "tilelayer" && layer.name.toLowerCase() === "ground";
}

// Entity interpolation for remote avatars (Gambetta Part III).
// Remote avatars are rendered "in the past" by interpolating between the
// last two server position updates. This avoids the lag drift and
// speed-dependent smoothness of exponential smoothing.
// https://www.gabrielgambetta.com/entity-interpolation.html
//
// REMOTE_RENDER_DELAY_MS must equal TICK_MS for a 2-position buffer.
// With delay = 50ms and updates every 50ms:
//   At t (new update): alpha = 0 → at posA (previous update)
//   At t+25:           alpha = 0.5 → midpoint
//   At t+50:           alpha = 1 → at posB, then buffer shifts
// This gives smooth interpolation with a constant 50ms delay. If an
// update is delayed, the avatar holds at posB until the next arrives.
const REMOTE_RENDER_DELAY_MS = 50; // == TICK_MS (declared below)

// Movement constants — must match worldsim.go movement system.
const SPEED_TILES_PER_TICK = 0.4;
const TICK_MS = 50; // 20 Hz server tick
// Threshold for detecting main-thread stalls. When the frame delta exceeds
// this, the prediction is skipped entirely for that frame. During a stall
// (e.g. A/V / video-tile setup blocking the event loop), the game loop
// doesn't run. When it resumes, delta is inflated. Predicting with the full
// delta would overshoot the server's authoritative position (the server kept
// ticking at 20Hz during the stall, and the prediction tries to "catch up"
// all at once). Clamping the delta (a previous approach) caused the prediction
// to advance faster than the server during repeated short stalls, producing
// oscillation (predict ahead, snap back, repeat) that felt like the avatar
// was "blocked." Skipping prediction instead lets the next reconciliation
// snap the avatar to the server's position — a single forward jump instead
// of oscillation. 150ms = 3 ticks at 20Hz; normal frames (8-50ms) are
// unaffected.
const STALL_THRESHOLD_MS = 150;
// Mirrors the server's proximityRadius (worldsim.go). Two players within
// this distance (in tiles, feet-to-feet) can hear each other's Nearby chat.
const PROXIMITY_RADIUS_TILES = 2.0;
const PROXIMITY_RADIUS_PX = PROXIMITY_RADIUS_TILES * TILE_SIZE; // 64
// Mirrors the worldsim interaction trigger radius (adjacentEntitiesLocked
// defaultRadius = 1.5 tiles). Used to auto-close the interaction popup when
// the local player walks out of range of every entity it lists.
const INTERACTION_RADIUS_TILES = 1.5;
// Vertical offset from Position.Y to the avatar's feet in tile coords.
// Avatars render with origin (0.5, 0.75) on a 64px frame placed at
// (pos.X*32, pos.Y*32+16), so feet sit at Position.Y + 1.0. Collision is
// evaluated at the feet — must match worldsim.go avatarFeetYOffset.
//
// Note the asymmetry with X: origin.x is 0.5 (centered), so sprite.x is
// set to pos.X*32 directly with no added offset — that already puts the
// sprite's horizontal center exactly at Position.X, matching how the
// backend compares Position.X against zone bounds. Adding a `+16` to X
// (as Y needs, to compensate for its off-center 0.75 origin) would shift
// the visual center half a tile from the true collision point: overlapping
// walls approached from the left, and leaving a half-tile gap approached
// from the right.
const FEET_Y_OFFSET = 1.0;

// Player collision radius in tiles — must match worldsim.go
// playerCollisionRadius. Zone shapes are expanded by this radius before
// the swept segment test so the feet center stops `r` tiles before the
// wall edge. A small radius keeps the visible gap tight while still
// allowing the player to squeeze through 1-tile gaps.
const PLAYER_COLLISION_RADIUS = 0.1;

// Camera zoom bounds and default. The wheel handler adjusts zoom within
// [ZOOM_MIN, ZOOM_MAX]; ZOOM_SENSITIVITY converts DOM wheel deltaY (~100
// per notch) to a zoom delta (~0.1 per notch). Default 2 shows a 10x10
// tile window around the player on the 30x20 map.
const ZOOM_MIN = 1;
const ZOOM_MAX = 4;
const ZOOM_DEFAULT = 2;
const ZOOM_SENSITIVITY = 0.001;

// Name tag vertical anchoring. The pill's tail tip sits above the avatar's
// head. The sprite frame is 64px tall but the character art is ~48px sitting
// at the bottom of the frame, so the frame top is ~16 world px above the
// head. NAME_TAG_HEAD_INSET_PX moves the anchor down into that empty space
// (world-space, does not scale with zoom); NAME_TAG_PAD_PX is the remaining
// screen-space gap divided by zoom. Together they keep the pill a constant
// small distance above the head at every zoom level.
const NAME_TAG_HEAD_INSET_PX = 8;
const NAME_TAG_PAD_PX = 1;

// Character sprite sheets — one per player, cycled. Each sheet is 768x192.
// The limezu characters are ~48px tall (taller than a 32px tile): the head
// occupies the bottom of one 32px row and the body fills the next row. We
// therefore slice the sheet into 32x64 frames (24 cols x 3 rows), so each
// frame contains a complete head+body. The three frame-rows are:
//   frame-row 0: idle  (cols 0-3, one per direction)
//   frame-row 1: walk  (24 cols, 6 per direction: right c0-5, up c6-11,
//                       left c12-17, down c18-23)
//   frame-row 2: run   (same 24-col / 6-per-direction layout as walk)
// The run row is used as the default movement animation. Dir field:
// 0=down, 1=left, 2=right, 3=up.
const FRAME_W = 32;
const FRAME_H = 64;
const COLS_PER_ROW = 24;
const CHAR_SPRITES = ["char_0", "char_1", "char_2", "char_3"];
const WALK_ROW = 2;        // frame-row used for the movement animation (run)

// FNV-1a hash used for the guest fallback: when a player has no sprite_base
// (guests / unset), the sprite index is deterministic from the entity ID so
// all clients agree on the same fallback sprite.
function fnv1aHash(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}
function spriteIndexForEntity(entityId: string): number {
  return fnv1aHash(entityId) % CHAR_SPRITES.length;
}
const FRAMES_PER_DIR = 6;  // columns per direction
const DIR_NAMES = ["down", "left", "right", "up"] as const;
// Frame start indices for each direction (index = frameRow * COLS_PER_ROW + col).
const DIR_FRAME_START = [
  (WALK_ROW * COLS_PER_ROW) + 18, // down
  (WALK_ROW * COLS_PER_ROW) + 12, // left
  (WALK_ROW * COLS_PER_ROW) + 0,  // right
  (WALK_ROW * COLS_PER_ROW) + 6,  // up
];

interface InputState { up: boolean; down: boolean; left: boolean; right: boolean; run: boolean }

// A sent input awaiting server acknowledgement. Used to replay un-acked
// inputs against the authoritative server position during reconciliation.
interface InputEvent {
  seq: number;
  state: InputState;
  time: number; // performance.now() when sent
}

interface Avatar {
  sprite: Phaser.GameObjects.Sprite;
  entityId: string;
  charKey: string;
  dir: number;
  moving: boolean;
  // True for base entities (props) rendered as tile sprites — these don't
  // animate or lerp, they just sit at their replicated position.
  isProp: boolean;
  // Entity state string (EntityState component, comp ID 2). For props,
  // extensions set this to "on"/"off" or other opaque values. Used to
  // show/hide the light glow overlay and filter popup actions.
  state: string;
  // True for entities that support interactions (Appearance.interactable
  // flag). Used by the client-side sparks-on-approach polling.
  interactable: boolean;
  // LightEmitter component values (comp ID 5). intensity 0-100 (0 = no
  // light), color 0xRRGGBB (0 = default warm white 0xffe6b4), radius in
  // tiles (0 = default 3). The glow is rendered when intensity > 0.
  lightIntensity: number;
  lightColor: number;
  lightRadius: number;
  // Light glow overlay (null when not lit). A procedural CanvasTexture with
  // a radial gradient, masked per-frame by walls and wall zones. Shown when
  // lightIntensity > 0.
  lightGlow: Phaser.GameObjects.Image | null;
  // Tracks whether the sparks animation has been shown for the current
  // proximity entry. Reset when the player leaves the trigger radius so
  // it can fire again on re-entry.
  sparksShown: boolean;
  // Remote avatars: last two server positions with arrival timestamps
  // for entity interpolation (Gambetta Part III). posA is the older
  // update, posB is the newer. The sprite interpolates between them over
  // the REMOTE_RENDER_DELAY_MS window.
  remotePosA: { x: number; y: number; t: number } | null;
  remotePosB: { x: number; y: number; t: number } | null;
  // Local avatar only: predicted position in tile coordinates.
  predX: number;
  predY: number;
  // Name tag above the avatar (null for props and local player's own avatar).
  nameTag: Phaser.GameObjects.Container | null;
  // Proximity spotlight (null for props). A soft warm radial-gradient image
  // centered on the avatar's feet, visualizing the 2-tile chat radius.
  // Toggled on/off globally with the Q key.
  glow: Phaser.GameObjects.Image | null;
  // Red outline rect drawn on entities with non-fatal validation warnings.
  // Null when the entity has no warning. Created lazily in updateWarnings().
  warningRect: Phaser.GameObjects.Rectangle | null;
}

// Apply `ticks` worth of movement from (x, y) under `state`, matching the
// server's movement math (worldsim.go: speed, diagonal normalize, collision).
// Uses the collision grid for wall blocking and map bounds.

export class GameScene extends Phaser.Scene {
  private ws: WsClient | null = null;
  private avatars: Map<string, Avatar> = new Map();
  private myEntityId: string | null = null;
  private avClient: AvClient | null = null;
  private afkDetector: AfkDetector | null = null;
  private videoBar: VideoBar | null = null;
  private screenShareOverlay: ScreenShareOverlay | null = null;
  // Display names by entity ID, populated from DisplayName component updates.
  // Used by the VideoBar to label tiles (including the local player's self-view).
  private displayNameByEntity = new Map<string, string>();
  // Guest status by entity ID, from the DisplayName component's is_guest
  // field. Used to render a "GUEST" badge on the name tag.
  private isGuestByEntity = new Map<string, boolean>();
  // Admin badge visibility by entity ID, from the DisplayName component's
  // is_admin field (server-computed as IsAdmin && !HideAdminBadge). Used
  // to render a red "admin" badge on the name tag, to the right of the name.
  private isAdminByEntity = new Map<string, boolean>();
  // Presence status by entity ID, from the DisplayName component's status
  // field (0=Available, 1=Busy, 2=DND). Drives the nametag pill color.
  private statusByEntity = new Map<string, number>();
  // AFK overlay flag by entity ID, from the DisplayName component's afk
  // field. When true, the nametag renders dimmed with an AFK indicator. The
  // manual status (statusByEntity) is preserved underneath.
  private afkByEntity = new Map<string, boolean>();
  // AFK-since timestamp (unix ms, 0 = not AFK) by entity ID, from the
  // DisplayName component's afk_since field. Used by the Players panel to
  // render "AFK 3m" durations. 0 when the player is not AFK.
  private afkSinceByEntity = new Map<string, number>();
  // Admin-only info by entity ID (IP, guest status). Populated from
  // AdminInfoFrame, only received by admin clients. Used to render the IP
  // below the name in the name tag pillbox for admin viewers.
  private adminInfoByEntity = new Map<string, { ip: string; isGuest: boolean; deviceId: string }>();
  private inputState: InputState = { up: false, down: false, left: false, right: false, run: false };
  private inputDirty = false;
  // Un-acked inputs for the local avatar, newest last. Replayed against the
  // server's authoritative position on each reconciliation.
  private pendingInputs: InputEvent[] = [];
  // True while a map transition is in progress (between receiving
  // MapTransitionFrame and the scene restart completing). Spawns arriving
  // during this window are buffered in pendingSpawns and processed after the
  // restart — otherwise the spawn would create an avatar that SHUTDOWN
  // immediately destroys, and the server won't re-send (spawnedTo=true).
  private transitioning = false;
  private pendingSpawns: ReplicationBatchView["spawns"] = [];
  // Collision grid from the Tiled "Walls" layer. [y][x] = true means blocked.
  private collisionGrid: boolean[][] = [];
  // Wall zones from the Tiled "Zones" object layer with zone_type=wall.
  // Used for client-side prediction of zone collision, matching the server's
  // swept segment-vs-rect test.
  private wallZones: WallZone[] = [];
  // [DEBUG] Toggleable zone/wall overlay (press D). Shows collisionGrid
  // cells (red) and wallZones (cyan outline + blue fill) at high depth.
  private debugZoneOverlay: Phaser.GameObjects.Graphics | null = null;
  private mapW = 20;
  private mapH = 20;
  // Tilesets from the Tiled JSON, stored so handleReplication can resolve
  // Appearance gids to tileset sheet + frame for base entity sprites.
  private tilesets: TiledMapJSON["tilesets"] = [];
  // Full-screen "Server not available" overlay shown whenever the WebSocket
  // is not in the "open" state (initial connect, reconnect attempts, or
  // terminal close). Grays out the screen and freezes the scene until the
  // connection is restored. Fixed to the screen (scrollFactor 0).
  private disconnectOverlay: Phaser.GameObjects.Container | null = null;
  // Cosmetic day/night tint overlay — a screen-fixed rectangle whose
  // color/alpha follows the local clock. Toggleable, defaults to on.
  private dayNightOverlay: DayNightOverlay | null = null;
  // Floating on-screen joystick for touch devices. Null on desktop.
  private joystick: VirtualJoystick | null = null;
  // Interaction popup container (null when closed). Shows available
  // actions when pressing E near a popup-mode entity.
  private interactionPopup: Phaser.GameObjects.Container | null = null;
  // Info dropdown opened by clicking a name tag's status dot. Only one open
  // at a time. Shows "Hello world" for regular users, IP + device ID for
  // admins, plus stub "Invite" and (admin-only) "Ban" buttons.
  private openDropdownEntityId: string | null = null;
  private dropdownContainer: Phaser.GameObjects.Container | null = null;
  // Set by the dot/button pointerdown handlers so the scene-level pointerdown
  // listener knows not to close the dropdown on that same click.
  private _dropdownClickedThisFrame = false;
  // Player option: whether to show the local player's own name tag above their
  // avatar. Defaults to true (visible). Set from the player_options JSON sent
  // by the server on auth.
  private showOwnNameTag = true;
  // Debounce timer for persisting camera zoom to player_options. The wheel
  // handler fires per notch; coalescing avoids a SetPlayerOptionsFrame + PB
  // write on every scroll tick.
  private zoomPersistTimer: number | null = null;
  // Player option: whether to show the proximity spotlight around avatars.
  // Off by default; toggled with the Q key. When on, every avatar gets a
  // soft warm pool at its feet (2-tile radius). Players in the local
  // player's proximity chat group get a brighter highlight.
  private showProximityGlow = false;
  // Persistent HTMLAudioElement for ping notifications. Pre-created and
  // unlocked on the first user click so it can play even when the tab is in
  // the background (new Audio() created on-the-fly gets blocked by autoplay
  // policy in hidden tabs). Reused for each ping by resetting currentTime.
  private pingAudio: HTMLAudioElement | null = null;
  // Entity IDs in the local player's current proximity chat group, computed
  // each frame via connected-components on feet-distance ≤ 2 tiles (matching
  // the server's runProximityClustering). Used to highlight group members.
  private proximityGroup: Set<string> = new Set();
  // Map option: whether the LightEmitter lighting system is enabled. Default
  // true. Set from the PB maps.options JSON (lights_enabled key). When false,
  // no glow rendering and no sprite brightening happens at all.
  private lightsEnabled = true;
  // Entity IDs currently emitting light (LightEmitter.intensity > 0).
  // Maintained incrementally on spawn/component-update; the per-frame glow
  // redraw iterates this set, not the full avatar map.
  private activeLights = new Set<string>();
  // Default LightEmitter values when the component or fields are 0/missing.
  private static readonly LIGHT_DEFAULT_COLOR = 0xffe6b4;
  private static readonly LIGHT_DEFAULT_RADIUS = 3;
  // Max light radius we allocate canvas textures for (in tiles). Bounds the
  // canvas size so a stray large radius can't OOM the page.
  private static readonly LIGHT_MAX_RADIUS_TILES = 12;

  constructor() {
    super("GameScene");
  }

  // Unified Y-sort depth for the shared dynamic band (see DEPTH_BAND_DYNAMIC
  // above): band + fractional Y so avatars and dynamic decorations sort by
  // base/feet position, but never cross into the static bands.
  private dynamicDepth(baseYPixels: number): number {
    const mapHeightPixels = this.mapH * TILE_SIZE || 1;
    return DEPTH_BAND_DYNAMIC + baseYPixels / mapHeightPixels;
  }

  // Feet/bottom Y of a sprite in world space. `sprite.y` is the origin point,
  // which for avatars (originY=0.75 on a 64px frame) sits ~16px above the
  // feet. Sorting by `sprite.y` directly makes the avatar flip in front of a
  // decoration only once the top half of the body has passed it; sorting by
  // the feet Y makes the flip happen as soon as the feet cross the base.
  private feetY(sprite: Phaser.GameObjects.Sprite): number {
    return sprite.y + sprite.height * (1 - sprite.originY);
  }

  private isBlocked(tx: number, ty: number): boolean {
    if (tx < 0 || tx >= this.mapW || ty < 0 || ty >= this.mapH) return true;
    return this.collisionGrid[ty]?.[tx] ?? false;
  }

  // Apply `ticks` worth of movement from (x, y) under `state`, matching the
  // server's movement math (worldsim.go: speed, diagonal normalize, collision).
  private applyMovement(x: number, y: number, state: InputState, ticks: number): { x: number; y: number } {
    let dx = 0, dy = 0;
    if (state.up) dy -= 1;
    if (state.down) dy += 1;
    if (state.left) dx -= 1;
    if (state.right) dx += 1;
    if (dx !== 0 && dy !== 0) {
      dx *= 0.7071;
      dy *= 0.7071;
    }
    if (dx === 0 && dy === 0) return { x, y };

    let newX = Math.max(0, Math.min(this.mapW - 1, x + dx * SPEED_TILES_PER_TICK * ticks));
    let newY = Math.max(0, Math.min(this.mapH - 1, y + dy * SPEED_TILES_PER_TICK * ticks));

    // Slide along walls: try X and Y independently. Collision is evaluated at
    // the avatar's feet, which render at Position.Y + FEET_Y_OFFSET (origin
    // 0.5/0.75 on a 64px frame → feet one tile below Position). This must
    // match the server's feet offset (worldsim.go, avatarFeetYOffset) or the
    // local avatar visually clips into walls before reconciliation.
    //
    // Two collision sources, matching the server's isMoveBlocked:
    //   1. Walls tile-layer grid (point check at destination tile)
    //   2. Wall zones from the Zones object layer (swept segment-vs-shape,
    //      expanded by PLAYER_COLLISION_RADIUS)
    //
    // The sprite is 1 tile wide centered on Position.X, so the leading edge
    // is the right edge (+0.5) when moving +X, the left edge (-0.5) when
    // moving -X, and the center when X is static. The feet are a single
    // point, so floor(feet) is direction-independent. The old Math.floor(x
    // +0.5) bias always checked the +edge, which only matched the leading
    // edge for +X/+Y movement; -X/-Y movement checked the trailing edge and
    // tunneled ~1 tile into walls. Must match worldsim.go isMoveBlocked.
    const fy = (y: number) => Math.floor(y + FEET_Y_OFFSET);
    const ledX = (oldX: number, nx: number) =>
      nx > oldX ? Math.floor(nx + 0.5) : nx < oldX ? Math.floor(nx - 0.5) : Math.floor(nx);
    if (this.isBlocked(ledX(x, newX), fy(y)) ||
        this.isZoneBlocked(x, y, newX, y)) newX = x;
    if (this.isBlocked(ledX(newX, newX), fy(newY)) ||
        this.isZoneBlocked(newX, y, newX, newY)) newY = y;
    // Diagonal guard: if both axes moved, check the full diagonal segment.
    // The X-then-Y decomposition can skip a wall that the diagonal crosses
    // but neither axis-aligned segment does. If the diagonal is blocked,
    // revert Y to slide along X. Matches worldsim.go.
    if (newX !== x && newY !== y) {
      if (this.isZoneBlocked(x, y, newX, newY)) newY = y;
    }

    return { x: newX, y: newY };
  }

  // isZoneBlocked checks whether the movement segment from (oldX, oldY) to
  // (newX, newY) intersects any wall zone, evaluated at the avatar's feet
  // (Position.Y + FEET_Y_OFFSET). Each zone shape is expanded by
  // PLAYER_COLLISION_RADIUS (Minkowski sum) so the feet center stops `r`
  // tiles before the wall edge. Matches the server's isMoveBlocked zone
  // collision (worldsim.go) — supports rect, circle, and polygon shapes.
  private isZoneBlocked(oldX: number, oldY: number, newX: number, newY: number): boolean {
    if (this.wallZones.length === 0) return false;
    const ofy = oldY + FEET_Y_OFFSET;
    const nfy = newY + FEET_Y_OFFSET;
    const r = PLAYER_COLLISION_RADIUS;
    for (const z of this.wallZones) {
      switch (z.shape) {
        case "rect":
          if (segmentIntersectsRect(oldX, ofy, newX, nfy, z.x - r, z.y - r, z.w + 2 * r, z.h + 2 * r)) return true;
          break;
        case "circle":
          if (segmentIntersectsCircle(oldX, ofy, newX, nfy, z.cx, z.cy, z.r + r)) return true;
          break;
        case "polygon":
          if (segmentIntersectsPolygonExpanded(oldX, ofy, newX, nfy, z.verts, r)) return true;
          break;
      }
    }
    return false;
  }

  preload(): void {
    const mapAssets = this.registry.get("mapAssets") as MapAssets;
    // Load from PocketBase — pass the parsed JSON object directly.
    this.load.tilemapTiledJSON("map", mapAssets.tiledJson);
    // Read per-tileset tile sizes from the Tiled JSON so non-32x32
    // tilesets (e.g. a 32x64 furniture sheet, a 32x128 tree sheet) are
    // sliced correctly when loaded as spritesheets below. Falls back to
    // the map's tile size for tilesets that don't override it (the
    // common case — most tilesets match the map grid).
    const rawJson = mapAssets.tiledJson as TiledMapJSON;
    const mapTileW = rawJson.tilewidth || TILE_SIZE;
    const mapTileH = rawJson.tileheight || TILE_SIZE;
    for (const ts of mapAssets.tilesets) {
      this.load.image(ts.name, ts.url);
      // Also load as a spritesheet so individual tiles can be drawn as
      // standalone sprites for object-layer decorations (see create()).
      // Uses the tileset's own tilewidth/tileheight if present, else the
      // map's tile size. Assumes a single, unspaced grid per tileset.
      const tsJson = rawJson.tilesets.find((t) => t.name === ts.name);
      const fw = tsJson?.tilewidth ?? mapTileW;
      const fh = tsJson?.tileheight ?? mapTileH;
      this.load.spritesheet(`${ts.name}__tiles`, ts.url, { frameWidth: fw, frameHeight: fh });
    }

    // Load character sprite sheets (768x192). Frames are 32x64 so each frame
    // captures the full ~48px-tall character (see FRAME_H comment above).
    // Static fallback sheets are always loaded (for guests / PB unavailable).
    for (const key of CHAR_SPRITES) {
      this.load.spritesheet(key, `/sprites/${key}.png`, {
        frameWidth: FRAME_W,
        frameHeight: FRAME_H,
      });
    }

    // Load PB-backed sprite sheets, keyed by their PocketBase record ID.
    // These are used when a player's Appearance.sprite_base is set.
    const spriteBases = this.registry.get("spriteBases") as SpriteBaseAsset[] | null;
    if (spriteBases) {
      for (const base of spriteBases) {
        this.load.spritesheet(base.id, base.url, {
          frameWidth: FRAME_W,
          frameHeight: FRAME_H,
        });
      }
    }

    // Interaction system assets.
    this.load.audio("clic", "/assets/sounds/clic.wav");
    // Note: the light glow texture is generated procedurally in create()
    // (lightGlowBase) — no PNG asset is loaded for it.
    this.load.spritesheet("sparks", "/assets/sprites/sparks.png", {
      frameWidth: 32,
      frameHeight: 32,
    });
  }

  create(): void {
    // Render the Tiled map
    const mapAssets = this.registry.get("mapAssets") as MapAssets;
    const map = this.make.tilemap({ key: "map" });
    const rawJson = mapAssets.tiledJson as TiledMapJSON;
    this.mapW = map.width;
    this.mapH = map.height;
    this.tilesets = rawJson.tilesets;

    // Add ALL tilesets so layers using any tileset render correctly. A map
    // can have multiple tilesets with different firstgids; createLayer must
    // be passed the tileset(s) that contain the layer's tile GIDs.
    const allTilesets = mapAssets.tilesets.map(
      (ts) => map.addTilesetImage(ts.name, ts.name),
    );
    const validTilesets = allTilesets.filter((t) => t !== null);

    if (validTilesets.length > 0 && rawJson) {
      // "Walls" is a reserved collision-fallback layer, matched by name —
      // not a decoration layer (see 21-map-design-guide.md).
      const wallsLayerDef = rawJson.layers.find((l) => l.type === "tilelayer" && l.name.toLowerCase() === "walls");
      const wallsLayerName = wallsLayerDef?.name;
      const walls = wallsLayerName ? map.createLayer(wallsLayerName, validTilesets, 0, 0) : null;
      walls?.setDepth(DEPTH_WALLS_FALLBACK);
      // Walls is a collision-only layer — never render its tiles, regardless
      // of the Tiled "visible" property. The collision grid is built from the
      // tile data below, which is independent of visibility.
      walls?.setVisible(false);

      if (walls) {
        this.collisionGrid = [];
        for (let y = 0; y < map.height; y++) {
          const row: boolean[] = [];
          for (let x = 0; x < map.width; x++) {
            const tile = walls.getTileAt(x, y);
            row.push(tile !== null && tile.index !== -1);
          }
          this.collisionGrid.push(row);
        }
      }

      // Decoration layers: altitude = position in the Tiled layer list.
      // sort_mode picks the band (see constants above and Part B of the
      // design doc). Bands accumulate independently below/above the shared
      // dynamic band depending on where each layer falls in the list
      // relative to the first dynamic layer.
      let belowBand = 0;
      let aboveBand = 0;
      let seenDynamic = false;
      for (const layer of rawJson.layers) {
        if (!isDecorationLayer(layer)) continue;
        const sortMode = (layerProp(layer.properties, "sort_mode") as string) || "static";

        if (layer.type === "tilelayer") {
          const tiledLayer = map.createLayer(layer.name, validTilesets, 0, 0);
          if (!tiledLayer) continue;
          if (sortMode === "dynamic") {
            seenDynamic = true;
            // Per-tile Y-sort within a tile layer requires per-tile sprites
            // (a Phaser TilemapLayer has one depth for the whole layer).
            // Not planned — use an object layer for Y-sortable scenery.
            // Render at a flat depth in the shared band so it still
            // interleaves at the layer granularity rather than being
            // silently dropped or crashing.
            console.warn(`decoration layer "${layer.name}": sort_mode=dynamic on a tile layer only gets a flat depth (per-tile Y-sort is not planned; use an object layer for Y-sortable scenery)`);
            tiledLayer.setDepth(DEPTH_BAND_DYNAMIC);
          } else {
            tiledLayer.setDepth(seenDynamic ? DEPTH_BAND_STATIC_ABOVE + aboveBand++ : belowBand++);
          }
          continue;
        }

        if (layer.type === "objectgroup") {
          for (const obj of layer.objects ?? []) {
            if (obj.gid === undefined) continue;
            const mapped = gidToFrame(obj.gid, rawJson.tilesets);
            if (!mapped) continue;
            // Tiled tile-objects anchor at bottom-left (obj.x, obj.y is
            // already the base/feet position — see Part B).
            const sprite = this.add.sprite(obj.x, obj.y, mapped.sheet, mapped.frame);
            sprite.setOrigin(0, 1);
            if (sortMode === "dynamic") {
              seenDynamic = true;
              sprite.setDepth(this.dynamicDepth(obj.y));
            } else {
              sprite.setDepth(seenDynamic ? DEPTH_BAND_STATIC_ABOVE + aboveBand++ : belowBand++);
            }
          }
        }
      }
    }

    // Parse wall zones from the "Zones" object layer for client-side
    // collision prediction. Matches the server's zone collision (ext-walls
    // registers block triggers for zone_type=wall). The client predicts
    // against these so the local avatar doesn't predict through wall zones
    // and rubber-band on reconciliation. Supports rect, circle, and polygon
    // shapes — matching the server's isMoveBlocked.
    if (rawJson) {
      const tileW = rawJson.tilewidth || TILE_SIZE;
      const tileH = rawJson.tileheight || TILE_SIZE;
      for (const layer of rawJson.layers) {
        if (layer.name.toLowerCase() !== "zones" || layer.type !== "objectgroup") continue;
        for (const obj of layer.objects ?? []) {
          if (layerProp(obj.properties, "zone_type") !== "wall") continue;
          if (obj.ellipse) {
            // Circle: Tiled ellipse with width == height.
            const r = obj.width / tileW / 2;
            this.wallZones.push({
              shape: "circle",
              cx: obj.x / tileW + r,
              cy: obj.y / tileH + r,
              r,
            });
          } else if (obj.polygon && obj.polygon.length > 0) {
            // Polygon: vertices are relative to (obj.x, obj.y) in Tiled.
            this.wallZones.push({
              shape: "polygon",
              verts: obj.polygon.map((p) => [
                (p.x + obj.x) / tileW,
                (p.y + obj.y) / tileH,
              ] as [number, number]),
            });
          } else {
            // Rect (default).
            this.wallZones.push({
              shape: "rect",
              x: obj.x / tileW,
              y: obj.y / tileH,
              w: obj.width / tileW,
              h: obj.height / tileH,
            });
          }
        }
        break;
      }
    }

    // Camera bounds + initial zoom. startFollow is called once the local
    // avatar spawns (see handleReplication). Zoom is adjustable via the
    // mouse wheel between ZOOM_MIN and ZOOM_MAX.
    this.cameras.main.setBounds(0, 0, this.mapW * TILE_SIZE, this.mapH * TILE_SIZE);
    this.cameras.main.setZoom(ZOOM_DEFAULT);
    this.input.on("wheel", (pointer: Phaser.Input.Pointer, _gameObjects: Phaser.GameObjects.GameObject[], _deltaX: number, deltaY: number) => {
      // Normalize deltaY to pixels. Firefox reports wheel delta in lines
      // (deltaMode=1, ~3/notch) while Chrome/Safari use pixels (deltaMode=0,
      // ~100/notch). Without normalization, zoom is ~33x too slow on Firefox.
      let dy = deltaY;
      const e = pointer.event;
      if (e instanceof WheelEvent) {
        if (e.deltaMode === 1) dy *= 40;            // DOM_DELTA_LINE → px
        else if (e.deltaMode === 2) dy *= this.scale.height; // DOM_DELTA_PAGE → px
      }
      const z = Phaser.Math.Clamp(
        this.cameras.main.zoom - dy * ZOOM_SENSITIVITY,
        ZOOM_MIN,
        ZOOM_MAX,
      );
      this.cameras.main.setZoom(z);
      this.scheduleZoomPersist();
    });

    // Create movement + idle animations for each character sheet. The
    // movement animation uses WALK_ROW (the run cycle, frame-row 2). Idle
    // reuses the same frames at a slower rate for a subtle breathing effect.
    // Register for both static fallback sheets and PB-backed sheets.
    const animKeys: string[] = [...CHAR_SPRITES];
    const spriteBases = this.registry.get("spriteBases") as SpriteBaseAsset[] | null;
    if (spriteBases) {
      for (const base of spriteBases) animKeys.push(base.id);
    }
    for (const key of animKeys) {
      for (let dir = 0; dir < 4; dir++) {
        const start = DIR_FRAME_START[dir];
        const frames = this.anims.generateFrameNumbers(key, { start, end: start + FRAMES_PER_DIR - 1 });
        this.anims.create({
          key: `${key}_walk_${DIR_NAMES[dir]}`,
          frames,
          frameRate: 10,
          repeat: -1,
        });
        this.anims.create({
          key: `${key}_idle_${DIR_NAMES[dir]}`,
          frames: [frames[0], frames[1], frames[0], frames[2]],
          frameRate: 2,
          repeat: -1,
        });
      }
    }

    // --- Sparks animation (interaction proximity feedback) ---
    // 4-frame one-shot: a small spark that shrinks and fades. Played
    // client-side when the player enters an interactable entity's range.
    if (!this.anims.exists("sparks_anim")) {
      this.anims.create({
        key: "sparks_anim",
        frames: this.anims.generateFrameNumbers("sparks", { start: 0, end: 3 }),
        frameRate: 12,
        repeat: 0,
      });
    }

    // --- Proximity spotlight textures ---
    // Two radial-gradient canvas textures used for the toggleable proximity
    // spotlight (Q key). Both are soft warm pools centered on the avatar's
    // feet with a brighter ring near the 2-tile boundary. "proximityGlow" is
    // the default (subtle); "proximityGlowActive" is brighter for players in
    // the local player's chat group. Generated once and reused by all avatars.
    const diameter = Math.ceil(PROXIMITY_RADIUS_PX * 2);
    const makeGlowTexture = (key: string, peakAlpha: number) => {
      if (this.textures.exists(key)) return;
      const tex = this.textures.createCanvas(key, diameter, diameter);
      if (!tex) return;
      const ctx = tex.getContext();
      const cx = diameter / 2;
      const r = cx;
      const grad = ctx.createRadialGradient(cx, cx, 0, cx, cx, r);
      grad.addColorStop(0.00, "rgba(255, 220, 150, 0)");
      grad.addColorStop(0.55, `rgba(255, 210, 130, ${peakAlpha * 0.25})`);
      grad.addColorStop(0.82, `rgba(255, 200, 120, ${peakAlpha * 0.55})`);
      grad.addColorStop(0.92, `rgba(255, 195, 110, ${peakAlpha})`);
      grad.addColorStop(1.00, "rgba(255, 190, 100, 0)");
      ctx.fillStyle = grad;
      ctx.fillRect(0, 0, diameter, diameter);
      tex.refresh();
    };
    makeGlowTexture("proximityGlow", 0.35);
    makeGlowTexture("proximityGlowActive", 0.65);

    // Input — keyboard
    const kb = this.input.keyboard;
    if (!kb) return;
    kb.on("keydown-UP", () => { this.inputState.up = true; this.inputDirty = true; });
    kb.on("keyup-UP", () => { this.inputState.up = false; this.inputDirty = true; });
    kb.on("keydown-DOWN", () => { this.inputState.down = true; this.inputDirty = true; });
    kb.on("keyup-DOWN", () => { this.inputState.down = false; this.inputDirty = true; });
    kb.on("keydown-LEFT", () => { this.inputState.left = true; this.inputDirty = true; });
    kb.on("keyup-LEFT", () => { this.inputState.left = false; this.inputDirty = true; });
    kb.on("keydown-RIGHT", () => { this.inputState.right = true; this.inputDirty = true; });
    kb.on("keyup-RIGHT", () => { this.inputState.right = false; this.inputDirty = true; });

    // Interact key — sends a discrete ActionFrame (not continuous movement
    // state). Phaser fires keydown repeatedly while held; gate with a flag so
    // each press fires exactly one ActionFrame. See 14-zones-and-interactions.md
    // §3a and ext-props (key:E toggles adjacent light switches).
    let eHeld = false;
    kb.on("keydown-E", () => { if (!eHeld) { eHeld = true; this.ws?.sendAction("key:E"); } });
    kb.on("keyup-E", () => { eHeld = false; });

    // [DEBUG] D toggles the zone/wall overlay (collisionGrid + wallZones).
    kb.on("keydown-D", () => { this.toggleDebugZoneOverlay(); });

    // ESC closes the interaction popup (matches the "click outside" close
    // below). The popup's action buttons close it themselves on click, so
    // by the time the global pointerdown handler runs this is already null.
    kb.on("keydown-ESC", () => this.closeInteractionPopup());

    // Proximity spotlight toggle — shows/hides the soft warm pool around every
    // avatar visualizing the 2-tile chat radius. Group members get a brighter
    // highlight (computed each frame in update()).
    kb.on("keydown-Q", () => {
      this.showProximityGlow = !this.showProximityGlow;
      for (const av of this.avatars.values()) {
        av.glow?.setVisible(this.showProximityGlow);
      }
    });

    // When the window loses focus or is hidden, the browser stops delivering
    // DOM events to the page — including keyup. This happens concretely when
    // Safari shows the native camera/mic permission popup (triggered by
    // getUserMedia inside AvClient). Without this, a held arrow key gets
    // "stuck" (its keyup is never delivered) and the avatar keeps walking
    // after the popup appears. Reset all movement state on blur/hidden so the
    // avatar stops until the user presses a key again.
    const clearMovementInput = () => {
      this.inputState.up = false;
      this.inputState.down = false;
      this.inputState.left = false;
      this.inputState.right = false;
      this.inputDirty = true;
      eHeld = false;
      this.joystick?.reset();
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === "hidden") clearMovementInput();
    };
    window.addEventListener("blur", clearMovementInput);
    document.addEventListener("visibilitychange", onVisibilityChange);

    // Touch input — floating virtual joystick. Only created on touch-capable
    // devices so desktop mouse/keyboard is unaffected. Feeds the same
    // inputState booleans as the keyboard handlers above.
    if (navigator.maxTouchPoints > 0) {
      this.joystick = new VirtualJoystick((j) => {
        this.inputState.up = j.up;
        this.inputState.down = j.down;
        this.inputState.left = j.left;
        this.inputState.right = j.right;
        this.inputDirty = true;
      });
    }

    // Connect to Pusher via WebSocket.
    // In Docker (nginx on 8080): wss://host/ws over HTTPS, ws://host/ws over HTTP
    // In dev (vite on 5173): ws://host:8081/ws (direct to pusher)
    const wsScheme = window.location.protocol === "https:" ? "wss" : "ws";
    const wsUrl = window.location.port === "5173"
      ? `${wsScheme}://${window.location.hostname}:8081/ws`
      : `${wsScheme}://${window.location.host}/ws`;
    console.log("connecting to", wsUrl);
    // Only create the WsClient on the first create() call. On scene.restart()
    // (e.g. map transition), the WsClient persists across the restart —
    // SHUTDOWN doesn't close or null it. Recreating it would open a new
    // WebSocket, mint a new client/entity ID, and the server would provision
    // the new entity on the default map, bouncing the player back.
    const isFirstCreate = !this.ws;
    if (isFirstCreate) {
      this.ws = new WsClient(wsUrl);
    }
    // A/V client + VideoBar for participant video tiles. Mic/camera HUD
    // controls live in the TopMenu (created once in main.ts, stored in the
    // registry). The VideoBar is a fixed DOM bar below the TopMenu.
    this.avClient = new AvClient();
    this.videoBar = new VideoBar({
      avClient: this.avClient,
      getName: (entityId) => this.resolveDisplayName(entityId),
      getLocalEntityId: () => this.myEntityId,
    });
    this.screenShareOverlay = new ScreenShareOverlay({
      avClient: this.avClient,
      getName: (entityId) => this.resolveDisplayName(entityId),
      getLocalEntityId: () => this.myEntityId,
    });
    const topMenu = this.game.registry.get("topMenu") as TopMenu | undefined;
    topMenu?.attachAvControls(this.avClient);
    topMenu?.attachRecordingControl(
      this.ws!,
      () => this.ws?.isAdmin() ?? false,
      () => this.avClient?.currentRoomName() ?? null,
    );
    // Players panel — lists connected players on the current map with status,
    // AFK + duration, and per-row Ping + admin/option-gated Teleport-to.
    topMenu?.attachPlayersPanel({
      getPlayers: () => this.getConnectedPlayers(),
      getMapName: () => this.ws?.getMapId() ?? null,
      isLocalAdmin: () => this.ws?.isAdmin() ?? false,
      isLocalGuest: () => !isLoggedIn(),
      onPing: (entityId) => this.ws?.sendPing(entityId),
      onTeleportTo: (entityId) => this.ws?.sendTeleportTo(entityId),
    });
    // AFK detector — monitors user activity + tab visibility and drives the
    // AFK overlay state (SetAfkFrame) and tab-visibility A/V auto-mute. Has
    // a meeting exemption (no AFK while in a room with other participants).
    // See documentation/plans/2026-07-22-afk-state-design.md.
    this.afkDetector = new AfkDetector(this.ws!, {
      onAfkChange: (afk) => {
        this.avClient?.setAfkMuted(afk);
        topMenu?.setLocalAfk(afk);
      },
      isInMeeting: () => this.avClient?.isInMeeting() ?? false,
    });
    const chatPanel = this.game.registry.get("chatPanel") as ChatPanel | undefined;
    // "Server not available" overlay — a full-screen gray dim plus a red
    // centered message. Visible from the start (we boot in "connecting"),
    // hidden once the WS reaches "open", and reshown on any drop. While it
    // is visible the scene is paused so local prediction doesn't move the
    // avatar against a dead server. Created once (isFirstCreate) and reused
    // across scene restarts (map transitions) — without this guard, each
    // restart would leak orphaned dim+msg objects in the display list.
    if (isFirstCreate) {
      const dim = this.add
        .rectangle(0, 0, this.scale.width, this.scale.height, 0x404040, 0.85)
        .setOrigin(0, 0)
        .setScrollFactor(0)
        .setDepth(9998);
      const msg = this.add
        .text(this.scale.width / 2, this.scale.height / 2, "Server not available", {
          fontFamily: "monospace",
          fontSize: "32px",
          color: "#ff0000",
          stroke: "#000000",
          strokeThickness: 4,
        })
        .setOrigin(0.5)
        .setScrollFactor(0)
        .setDepth(9999);
      this.disconnectOverlay = this.add
        .container(0, 0, [dim, msg])
        .setScrollFactor(0)
        .setDepth(9999);
      // The overlay starts visible (we boot in "connecting"), so mirror that on
      // the DOM: dim and disable floating DOM UI until the WS reaches "open".
      document.body.classList.add("server-unavailable");
    }
    // On restart (map transition), the ws is already connected — hide the
    // overlay immediately since onStateChange won't fire (connect is skipped).
    if (!isFirstCreate && this.ws?.getState() === "open") {
      this.disconnectOverlay?.setVisible(false);
      document.body.classList.remove("server-unavailable");
    }
    // Day/night tint overlay — cosmetic, client-side, follows the local
    // clock. Sits below the disconnect overlay (depth 9997 vs 9998).
    this.dayNightOverlay = new DayNightOverlay(this);
    this.scale.on("resize", (gameSize: Phaser.Structs.Size) => {
      this.dayNightOverlay?.resize(gameSize.width, gameSize.height);
    });
    // Freeze the scene until the first successful auth (first create only).
    // On restart (map transition), the ws is already connected and onReady
    // won't fire again, so pausing would freeze the scene permanently.
    if (isFirstCreate) {
      this.scene.pause("GameScene");
    }
    // Only connect on the first create() — see isFirstCreate above.
    if (isFirstCreate) {
    this.ws!.connect({
      onReady: () => {
        // Use the entity ID from the server's AuthResult. The server may
        // assign a different entity ID than "e_"+clientId[2:] when a
        // PocketBase-stored identity exists (persistent entity_id).
        this.myEntityId = this.ws?.getEntityId() ?? null;
        console.log("ready, myEntityId=", this.myEntityId);
        // Apply map and player options from the auth result.
        this.applyMapOptions(this.ws?.getMapOptions() ?? "");
        this.applyPlayerOptions(this.ws?.getPlayerOptions() ?? "");
        // Sync player options to the TopMenu so the checkbox reflects the
        // current server-side value when the dropdown is opened.
        const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
        tm?.setPlayerOptions(this.ws?.getPlayerOptions() ?? "");
        // If the server says we're on a different map than the one currently
        // loaded (e.g. player logged out on map2 and reconnected), trigger a
        // map transition to load the correct map.
        const serverMapId = this.ws?.getMapId();
        const currentAssets = this.registry.get("mapAssets") as MapAssets | undefined;
        if (serverMapId && currentAssets) {
          // Check if the current map matches by fetching the record name.
          // The mapAssets don't carry the PB record name, so we compare
          // against the default. If the server's map_id differs from what
          // we loaded, transition.
          const loadedMapName = this.registry.get("loadedMapName") as string | undefined;
          if (loadedMapName && loadedMapName !== serverMapId) {
            console.log(`server map_id=${serverMapId} differs from loaded=${loadedMapName}, transitioning`);
            this.handleMapTransition(serverMapId, 10, 10);
          }
        }
        // If the pre-join chooser stashed a pending sprite_base, send it now.
        const pending = this.registry.get("pendingSpriteBase") as string | undefined;
        if (pending) {
          this.ws?.setSpriteBase(pending);
          this.registry.remove("pendingSpriteBase");
        }
        // Pre-load the LiveKit SDK module (~530KB) during idle time so it's
        // already parsed when the player enters an A/V zone. Without this,
        // the first connect() call blocks the main thread to parse/eval the
        // module, adding a significant chunk to the A/V connect freeze.
        this.avClient?.preloadModule();
      },
      onReplication: (batch: ReplicationBatchView) => this.handleReplication(batch),
      onReconnect: (_clientId: string, entityId: string) => {
        // The pusher mints a fresh session on reconnect, so the entity id
        // changes and worldsim spawns the avatar at the spawn point. Clear
        // ALL avatars (not just the old self) because the server has a new
        // clientID and will re-spawn every entity to us — keeping stale
        // entries would cause the client to skip those SpawnEntities and
        // retain sprites from before the reconnect. Clear un-acked inputs
        // that referenced the old entity. Re-mark input dirty so the current
        // input state is re-sent on the new connection and movement resumes
        // seamlessly.
        for (const av of this.avatars.values()) {
          av.sprite.destroy();
          av.nameTag?.destroy();
          av.glow?.destroy();
          av.warningRect?.destroy();
        }
        this.avatars.clear();
        this.displayNameByEntity.clear();
        this.isGuestByEntity.clear();
        this.isAdminByEntity.clear();
        this.statusByEntity.clear();
        this.afkByEntity.clear();
        this.afkSinceByEntity.clear();
        this.adminInfoByEntity.clear();
        this.closeDropdown();
        this.closeInteractionPopup();
        this.myEntityId = entityId || null;
        this.pendingInputs = [];
        this.inputDirty = true;
        // Re-apply map and player options from the fresh AuthResult so a
        // reconnect restores the saved zoom level and map options (e.g.
        // day/night) instead of resetting to defaults. onReady does the same
        // on initial connect; onReconnect must mirror it because the pusher
        // mints a fresh session and worldsim re-sends the AuthResult.
        this.applyMapOptions(this.ws?.getMapOptions() ?? "");
        this.applyPlayerOptions(this.ws?.getPlayerOptions() ?? "");
        const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
        tm?.setPlayerOptions(this.ws?.getPlayerOptions() ?? "");
        console.log("reconnected, myEntityId=", this.myEntityId);
      },
      onStateChange: (state: ConnectionState) => {
        if (!this.disconnectOverlay) return;
        const connected = state === "open";
        this.disconnectOverlay.setVisible(!connected);
        // Mirror the overlay on the DOM so floating buttons (TopMenu, welcome
        // icon, ChatPanel, VideoBar) are dimmed and non-clickable while the
        // server is unavailable.
        document.body.classList.toggle("server-unavailable", !connected);
        if (connected) this.scene.resume("GameScene");
        else this.scene.pause("GameScene");
        // Tear down LiveKit when the game WS drops. Without this, the
        // LiveKit room outlives the game connection: the server despawns
        // the entity and emits zone.exit, but the leave token goes to the
        // old (dead) client ID and never reaches AvClient. On reconnect,
        // the player respawns at the spawn point (outside the A/V zone),
        // so no zone.enter rejoin occurs — leaving a stale video tile
        // forever. Fire-and-forget: close() is idempotent (no-op if no
        // room is connected) and safe to call concurrently.
        if (!connected) {
          void this.avClient?.close();
        }
      },
      onAvToken: (msg) => {
        this.avClient?.handleTokenFrame(msg).catch((err) =>
          console.error("AvClient handleTokenFrame error:", err)
        );
      },
      onChatMessage: (msg) => {
        chatPanel?.addMessage(msg);
      },
      onMapTransition: (msg) => {
        this.handleMapTransition(msg.mapId, msg.spawnX, msg.spawnY, msg.mapOptions);
      },
      onMapOptionsUpdate: (msg) => {
        this.applyMapOptions(msg.mapOptions);
      },
      onAdminInfo: (msg) => {
        for (const e of msg.entities) {
          this.adminInfoByEntity.set(e.entityId, { ip: e.ip, isGuest: e.isGuest, deviceId: e.deviceId });
          // Refresh the dropdown if open so new admin info is shown.
          this.refreshDropdownIfOpen(e.entityId);
        }
      },
      onRecordingState: (msg) => {
        const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
        tm?.setRecordingState(msg.status as "active" | "stopped" | "error", msg.error);
      },
      onRecordingActive: (msg) => {
        const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
        tm?.setRecordingActive(msg.active);
        this.updateRecordingIndicator(msg.active, msg.target);
        // When an admin force-stops the recording from the admin UI, show a
        // transient center-screen toast so participants know why it ended.
        // Manual and auto-empty stops don't get a toast.
        if (!msg.active && msg.reason === "admin_stop") {
          this.showRecordingAdminStopToast();
        }
      },
      onBanned: (reason, banUntil) => {
        const msg = this.disconnectOverlay?.getAt(1) as Phaser.GameObjects.Text | undefined;
        if (msg) {
          const expiry = banUntil > 0
            ? `until ${new Date(banUntil * 1000).toLocaleString()}`
            : "permanently";
          msg.setText(`You are banned ${expiry}\n${reason}`);
        }
      },
      onAlreadyConnected: () => {
        this.showAlreadyConnectedConfirm();
      },
      onKicked: (reason) => {
        const msg = this.disconnectOverlay?.getAt(1) as Phaser.GameObjects.Text | undefined;
        if (msg) {
          msg.setText(`You have been kicked from this world\n${reason}`);
        }
      },
      onPlayerPing: (msg) => {
        // Play a notification sound so the player knows someone wants their
        // attention (e.g. if AFK). DND targets never receive this — worldsim
        // drops the ping server-side.
        //
        // Uses the persistent pingAudio element (pre-unlocked on first click)
        // so playback works even in background tabs. Phaser's Web Audio
        // manager suspends the AudioContext when the tab is hidden; a raw
        // HTMLAudioElement that was primed during a user gesture does not
        // have this limitation. The sound is replayed 3 times for a stronger
        // "ping ping ping" effect. When the tab is hidden, also show a
        // browser Notification as a visual fallback (Chrome freezes deeply
        // backgrounded tabs after ~5 min, killing all JS — the Notification
        // is the only signal that reaches the user in that state).
        const playPing = () => {
          if (this.pingAudio) {
            this.pingAudio.currentTime = 0;
            this.pingAudio.play().catch(() => {});
          }
        };
        playPing();
        setTimeout(playPing, 300);
        setTimeout(playPing, 600);
        if (document.hidden && Notification && Notification.permission === "granted") {
          const name = msg.displayName || "Someone";
          new Notification(`${name} pinged you`, {
            body: "Click to return to the world",
            tag: "player-ping",
          });
        }
      },
      onError: (_code, message) => {
        const msg = this.disconnectOverlay?.getAt(1) as Phaser.GameObjects.Text | undefined;
        if (msg) {
          msg.setText(`Server error\n${message}`);
        }
        this.disconnectOverlay?.setVisible(true);
        document.body.classList.add("server-unavailable");
      },
      onActionResult: (result) => {
        if (result.availableActions.length > 0) {
          this.openInteractionPopup(result.availableActions);
        } else {
          this.closeInteractionPopup();
        }
      },
    });

    // Pre-create the ping notification audio element and unlock it on the
    // first user click. Browsers block audio autoplay in background tabs for
    // elements that weren't "primed" during a user gesture. By creating the
    // element once and calling play() during a click, the element stays
    // unlocked and can be replayed later even when the tab is hidden. Also
    // request Notification permission so we can show a visual fallback when
    // the tab is in the background.
    this.pingAudio = new Audio("/assets/sounds/clic.wav");
    this.pingAudio.preload = "auto";
    this.pingAudio.volume = 1.0;
    document.addEventListener("click", () => {
      if (this.pingAudio) {
        this.pingAudio.play().then(() => {
          this.pingAudio!.pause();
          this.pingAudio!.currentTime = 0;
        }).catch(() => {});
      }
      if (Notification && Notification.permission === "default") {
        Notification.requestPermission().catch(() => {});
      }
    }, { once: true });
    } // end isFirstCreate

    // Process buffered spawns from a map transition. During the transition,
    // handleReplication buffers spawns instead of creating avatars (which
    // SHUTDOWN would destroy). Now that the new map is loaded, process them.
    if (this.transitioning && this.pendingSpawns.length > 0) {
      this.transitioning = false;
      const pending = this.pendingSpawns;
      this.pendingSpawns = [];
      this.processSpawns(pending);
    } else {
      this.transitioning = false;
    }

    chatPanel?.setSendHandler((channel, text) => this.ws?.sendChat(channel, text));
    topMenu?.setSetNameHandler((name) => this.ws?.setName(name));
    topMenu?.setSetSpriteBaseHandler((spriteBase) => this.ws?.setSpriteBase(spriteBase));
    topMenu?.setSetPlayerOptionsHandler((options) => this.ws?.setPlayerOptions(options));
    topMenu?.setSetStatusHandler((status) => this.ws?.setStatus(status));

    // Close the info dropdown when clicking outside it. The dot and buttons
    // set _dropdownClickedThisFrame to suppress this on their own clicks.
    // Also close the interaction popup on any outside click — its action
    // buttons call closeInteractionPopup() on their own pointerdown, so by
    // the time this global handler runs, this.interactionPopup is already
    // null for in-popup clicks and only non-null for outside clicks.
    this.input.on("pointerdown", () => {
      if (!this._dropdownClickedThisFrame && this.dropdownContainer) {
        this.closeDropdown();
      }
      this._dropdownClickedThisFrame = false;
      if (this.interactionPopup) this.closeInteractionPopup();
    });

    // Clean up A/V video bar + LiveKit room on scene shutdown.
    this.events.once(Phaser.Scenes.Events.SHUTDOWN, () => {
      this.afkDetector?.destroy();
      this.afkDetector = null;
      this.videoBar?.destroy();
      this.screenShareOverlay?.destroy();
      this.avClient?.close();
      this.videoBar = null;
      this.screenShareOverlay = null;
      this.avClient = null;
      this.debugZoneOverlay?.destroy();
      this.debugZoneOverlay = null;
      this.closeDropdown();
      this.closeInteractionPopup();
      topMenu?.detachAvControls();
      // Remove generated glow textures so re-creating the scene doesn't
      // warn about duplicate keys.
      if (this.textures.exists("proximityGlow")) this.textures.remove("proximityGlow");
      if (this.textures.exists("proximityGlowActive")) this.textures.remove("proximityGlowActive");
      window.removeEventListener("blur", clearMovementInput);
      document.removeEventListener("visibilitychange", onVisibilityChange);
    });

    // Disconnect LiveKit room on page unload/refresh to avoid zombie
    // participants lingering in the room until LiveKit's timeout.
    window.addEventListener("beforeunload", () => {
      this.avClient?.close();
    });
  }

  update(_time: number, delta: number): void {
    // Update record button visibility (admin + in A/V room).
    const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
    tm?.updateRecVisibility();

    // Send input on change and buffer it for reconciliation.
    if (this.inputDirty && this.ws) {
      const seq = this.ws.sendInput(this.inputState);
      if (seq > 0) {
        this.pendingInputs.push({
          seq,
          state: { ...this.inputState },
          time: performance.now(),
        });
      }
      this.inputDirty = false;
    }

    // Predict the local avatar: apply the current input for this frame's
    // worth of ticks, then render. This makes movement feel immediate.
    const local = this.myEntityId ? this.avatars.get(this.myEntityId) : null;
    if (local) {
      // Skip prediction during main-thread stalls (delta exceeds threshold).
      // The avatar freezes during the stall (inherent — the game loop isn't
      // running), and the next reconciliation snaps it to the server's
      // authoritative position. This avoids both the overshoot-and-snap-back
      // from a single long stall and the oscillation from repeated short
      // stalls. See STALL_THRESHOLD_MS above.
      const ticks = delta > STALL_THRESHOLD_MS ? 0 : delta / TICK_MS;
      const p = this.applyMovement(local.predX, local.predY, this.inputState, ticks);
      local.predX = p.x;
      local.predY = p.y;
      local.sprite.x = local.predX * TILE_SIZE;
      local.sprite.y = local.predY * TILE_SIZE + TILE_SIZE / 2;
      local.sprite.setDepth(this.dynamicDepth(this.feetY(local.sprite)));

      // Reposition the local player's name tag (if visible) to follow the
      // sprite and counter-scale, same as remote avatars below.
      if (local.nameTag) {
        const zoom = this.cameras.main.zoom;
        local.nameTag.x = local.sprite.x;
        local.nameTag.y = local.sprite.y -
          local.sprite.displayHeight * local.sprite.originY +
          NAME_TAG_HEAD_INSET_PX -
          NAME_TAG_PAD_PX / zoom;
        local.nameTag.setScale(1 / zoom);
        local.nameTag.setDepth(local.sprite.depth + 0.01);
      }

      // Update walk animation based on input.
      const moving = this.inputState.up || this.inputState.down ||
                     this.inputState.left || this.inputState.right;
      let dir = local.dir;
      if (this.inputState.left) dir = 1;
      else if (this.inputState.right) dir = 2;
      else if (this.inputState.up) dir = 3;
      else if (this.inputState.down) dir = 0;
      this.updateAvatarAnim(local, dir, moving);
    }

    // Entity interpolation for remote avatars (Gambetta Part III).
    // Render remote avatars REMOTE_RENDER_DELAY_MS "in the past" by
    // interpolating between the last two server position updates.
    const renderTime = performance.now() - REMOTE_RENDER_DELAY_MS;
    for (const avatar of this.avatars.values()) {
      if (avatar.entityId === this.myEntityId) continue;
      if (avatar.isProp) continue; // props are static — no interpolation
      const prevX = avatar.sprite.x, prevY = avatar.sprite.y;
      const a = avatar.remotePosA;
      const b = avatar.remotePosB;
      if (b) {
        if (!a) {
          // Only one update — snap to it.
          avatar.sprite.x = b.x;
          avatar.sprite.y = b.y;
        } else {
          const span = b.t - a.t;
          if (span <= 0) {
            avatar.sprite.x = b.x;
            avatar.sprite.y = b.y;
          } else {
            // Interpolate between posA and posB at renderTime. Clamping
            // handles jitter: if renderTime is before posA (alpha < 0),
            // hold at posA; if after posB (alpha > 1), hold at posB until
            // the next update arrives.
            const alpha = Math.max(0, Math.min(1, (renderTime - a.t) / span));
            avatar.sprite.x = a.x + (b.x - a.x) * alpha;
            avatar.sprite.y = a.y + (b.y - a.y) * alpha;
          }
        }
      }
      avatar.sprite.setDepth(this.dynamicDepth(this.feetY(avatar.sprite)));
      // Reposition name tag to follow the sprite and counter-scale so it
      // stays a constant screen size regardless of camera zoom.
      if (avatar.nameTag) {
        const zoom = this.cameras.main.zoom;
        avatar.nameTag.x = avatar.sprite.x;
        avatar.nameTag.y = avatar.sprite.y -
          avatar.sprite.displayHeight * avatar.sprite.originY +
          NAME_TAG_HEAD_INSET_PX -
          NAME_TAG_PAD_PX / zoom;
        avatar.nameTag.setScale(1 / zoom);
        avatar.nameTag.setDepth(avatar.sprite.depth + 0.01);
      }
      // Reposition the info dropdown to follow the avatar it's attached to.
      if (this.dropdownContainer && this.openDropdownEntityId === avatar.entityId) {
        const zoom = this.cameras.main.zoom;
        this.dropdownContainer.x = avatar.sprite.x;
        this.dropdownContainer.y = avatar.sprite.y -
          avatar.sprite.displayHeight * avatar.sprite.originY +
          NAME_TAG_HEAD_INSET_PX -
          NAME_TAG_PAD_PX / zoom + 6 / zoom;
        this.dropdownContainer.setScale(1 / zoom);
        this.dropdownContainer.setDepth(avatar.sprite.depth + 0.02);
      }
      // Animate based on whether the avatar is actually moving on screen.
      const dx = avatar.sprite.x - prevX, dy = avatar.sprite.y - prevY;
      const moving = Math.abs(dx) > 0.1 || Math.abs(dy) > 0.1;
      if (moving) {
        if (Math.abs(dx) > Math.abs(dy)) {
          this.updateAvatarAnim(avatar, dx > 0 ? 2 : 1, true);
        } else {
          this.updateAvatarAnim(avatar, dy > 0 ? 0 : 3, true);
        }
      } else {
        this.updateAvatarAnim(avatar, avatar.dir, false);
      }
    }

    // Keep the interaction popup anchored to the local avatar (to its left,
    // counter-scaled by 1/zoom) and clamped to the viewport, so camera pan,
    // zoom, or border clamping doesn't move it off the avatar or off-screen.
    // Auto-close it once the player is out of interaction range of every
    // entity it lists (mirrors the server's adjacentEntitiesLocked check).
    if (this.interactionPopup) {
      this.positionInteractionPopup(this.interactionPopup);
      const local = this.myEntityId ? this.avatars.get(this.myEntityId) : null;
      if (local) {
        const entityIds = this.interactionPopup.getData("entityIds") as string[];
        const lx = local.sprite.x / TILE_SIZE;
        const ly = local.sprite.y / TILE_SIZE;
        const r = INTERACTION_RADIUS_TILES;
        let anyInRange = false;
        for (const id of entityIds) {
          const av = this.avatars.get(id);
          if (!av) continue;
          const dx = av.sprite.x / TILE_SIZE - lx;
          const dy = av.sprite.y / TILE_SIZE - ly;
          if (dx * dx + dy * dy <= r * r) {
            anyInRange = true;
            break;
          }
        }
        if (!anyInRange) this.closeInteractionPopup();
      }
    }

    // --- Proximity spotlight: compute chat group + update glows ---
    // Matches the server's runProximityClustering: build adjacency on
    // feet-distance ≤ 2 tiles, then BFS from the local player to find the
    // transitive chat group. Group members get the brighter "active" texture.
    if (this.showProximityGlow) {
      const players: { id: string; fx: number; fy: number }[] = [];
      for (const av of this.avatars.values()) {
        if (av.isProp) continue;
        players.push({
          id: av.entityId,
          fx: av.sprite.x / TILE_SIZE,
          fy: this.feetY(av.sprite) / TILE_SIZE,
        });
      }

      this.proximityGroup = new Set();
      if (this.myEntityId && players.length > 1) {
        const localIdx = players.findIndex((p) => p.id === this.myEntityId);
        if (localIdx >= 0) {
          const adj: number[][] = players.map(() => []);
          for (let i = 0; i < players.length; i++) {
            for (let j = i + 1; j < players.length; j++) {
              const d = Math.hypot(players[i].fx - players[j].fx, players[i].fy - players[j].fy);
              if (d <= PROXIMITY_RADIUS_TILES) {
                adj[i].push(j);
                adj[j].push(i);
              }
            }
          }
          const visited = new Set<number>([localIdx]);
          const queue = [localIdx];
          while (queue.length > 0) {
            const cur = queue.shift()!;
            this.proximityGroup.add(players[cur].id);
            for (const n of adj[cur]) {
              if (!visited.has(n)) {
                visited.add(n);
                queue.push(n);
              }
            }
          }
        }
      }

      for (const av of this.avatars.values()) {
        if (!av.glow) continue;
        av.glow.x = av.sprite.x;
        av.glow.y = this.feetY(av.sprite);
        av.glow.setTexture(this.proximityGroup.has(av.entityId) ? "proximityGlowActive" : "proximityGlow");
      }
    }

    // --- Sparks on approach: poll distance to interactable props ---
    if (local) {
      const lx = local.sprite.x / TILE_SIZE;
      const ly = local.sprite.y / TILE_SIZE;
      for (const av of this.avatars.values()) {
        if (!av.isProp || !av.interactable) continue;
        const dx = av.sprite.x / TILE_SIZE - lx;
        const dy = av.sprite.y / TILE_SIZE - ly;
        const dist = Math.hypot(dx, dy);
        const inRange = dist <= 2.0; // 2-tile trigger radius
        if (inRange && !av.sparksShown) {
          this.playSparks(av);
          av.sparksShown = true;
        } else if (!inRange && av.sparksShown) {
          av.sparksShown = false;
        }
      }
    }

    // --- A/V: spatial volume + video bar tick ---
    if (this.avClient && this.videoBar) {
      const local = this.myEntityId ? this.avatars.get(this.myEntityId) : null;
      if (local) {
        const localX = local.sprite.x / TILE_SIZE;
        const localY = local.sprite.y / TILE_SIZE;
        const maxDist = 10; // tiles; volume reaches 0 at this distance
        for (const avatar of this.avatars.values()) {
          if (avatar.entityId === this.myEntityId) continue;
          const ax = avatar.sprite.x / TILE_SIZE;
          const ay = avatar.sprite.y / TILE_SIZE;
          const dist = Math.hypot(ax - localX, ay - localY);
          const vol = Math.max(0, 1 - dist / maxDist);
          this.avClient.setParticipantVolume(avatar.entityId, vol);
        }
      }
      // Drive spectrograms, speaking state, and speaking-first reordering.
      this.videoBar.tick();
    }

    // Per-frame light glow redraw: for each active light, redraw the masked
    // CanvasTexture at the light's current position and brighten nearby
    // sprites. Skipped entirely when lights_enabled is false.
    if (this.lightsEnabled) {
      this.updateLights();
    }

    // Draw red outlines on entities with non-fatal validation warnings.
    this.updateWarningRects();
  }

  // updateWarningRects syncs the red outline rect on every avatar with the
  // current set of warned entity IDs from the WsClient. Rects are created
  // lazily and destroyed when the entity is no longer warned. The rect
  // follows the sprite each frame and is sized to the sprite's display
  // bounds. Counter-scaled by camera zoom so it stays a constant screen
  // thickness.
  private updateWarningRects(): void {
    const warnings = this.ws?.getMapWarnings() ?? [];
    const warnedIds = new Set<string>();
    for (const w of warnings) if (w.entityId) warnedIds.add(w.entityId);

    for (const avatar of this.avatars.values()) {
      const shouldShow = warnedIds.has(avatar.entityId);
      if (shouldShow && !avatar.warningRect) {
        const rect = this.add.rectangle(0, 0, TILE_SIZE, TILE_SIZE)
          .setStrokeStyle(2, 0xff0000)
          .setOrigin(0.5, 0.5)
          .setDepth(10000);
        rect.setStrokeStyle(2 / this.cameras.main.zoom, 0xff0000);
        avatar.warningRect = rect;
      } else if (!shouldShow && avatar.warningRect) {
        avatar.warningRect.destroy();
        avatar.warningRect = null;
      }
      if (avatar.warningRect) {
        avatar.warningRect.x = avatar.sprite.x;
        // Center vertically on the sprite (origin 0,1 means sprite.y is the
        // feet; the visual center is roughly half a tile above).
        avatar.warningRect.y = avatar.sprite.y - TILE_SIZE / 2;
        avatar.warningRect.setScale(1 / this.cameras.main.zoom);
      }
    }
  }

  // Play walk or idle animation for an avatar based on direction and
  // movement state. Only switches animation when the target changes.
  private updateAvatarAnim(avatar: Avatar, dir: number, moving: boolean): void {
    const targetKey = moving
      ? `${avatar.charKey}_walk_${DIR_NAMES[dir]}`
      : `${avatar.charKey}_idle_${DIR_NAMES[dir]}`;
    if (dir === avatar.dir && moving === avatar.moving) return;
    avatar.dir = dir;
    avatar.moving = moving;
    avatar.sprite.play(targetKey, true);
  }

  // handleMapTransition loads the new map's assets from PocketBase, then
  // restarts the scene with the new map. All avatars are cleared during the
  // restart and re-spawned by the server's replication loop.
  private handleMapTransition(mapId: string, spawnX: number, spawnY: number, mapOptions?: string): void {
    console.log(`map transition: ${mapId} (${spawnX}, ${spawnY})`);
    // Mark transitioning so handleReplication buffers spawns instead of
    // creating avatars that SHUTDOWN would immediately destroy.
    this.transitioning = true;
    this.pendingSpawns = [];
    // Apply map options (e.g. day_night_enabled) before loading assets so the
    // overlay is correct when the scene restarts.
    if (mapOptions !== undefined) this.applyMapOptions(mapOptions);
    // Load the new map assets from PocketBase, then restart the scene.
    loadMapAssets(mapId)
      .then((mapAssets) => {
        // Clear old map assets from Phaser caches so the loader accepts the
        // new ones on restart. Phaser's loader skips keys that already exist
        // in the cache — without this, scene.restart() reuses the old map's
        // tilemap data and the new map's tilesets don't match.
        const oldAssets = this.registry.get("mapAssets") as MapAssets | undefined;
        if (oldAssets) {
          if (this.cache.tilemap.has("map")) {
            this.cache.tilemap.remove("map");
          }
          for (const ts of oldAssets.tilesets) {
            if (this.textures.exists(ts.name)) this.textures.remove(ts.name);
            const tilesKey = `${ts.name}__tiles`;
            if (this.textures.exists(tilesKey)) this.textures.remove(tilesKey);
          }
        }
        // Stash the new map assets for the restarted scene.
        this.registry.set("mapAssets", mapAssets);
        this.registry.set("loadedMapName", mapId);
        // Destroy all avatars — the server will re-spawn them on the new map.
        for (const av of this.avatars.values()) {
          av.sprite.destroy();
          av.nameTag?.destroy();
          av.glow?.destroy();
          av.warningRect?.destroy();
        }
        this.avatars.clear();
        this.displayNameByEntity.clear();
        this.isGuestByEntity.clear();
        this.isAdminByEntity.clear();
        this.statusByEntity.clear();
        this.afkByEntity.clear();
        this.afkSinceByEntity.clear();
        this.adminInfoByEntity.clear();
        this.closeDropdown();
        // Restart the scene to reload the map.
        this.scene.restart();
      })
      .catch((err) => {
        console.error("map transition failed: failed to load map assets:", err);
      });
  }

  // applyMapOptions parses the map options JSON and applies map-level feature
  // toggles. Currently handles day_night_enabled (default true) and
  // lights_enabled (default true). The map option sets the default — the
  // player's explicit localStorage preference takes precedence (see
  // DayNightOverlay.applyDefault).
  private applyMapOptions(mapOptions: string): void {
    let dayNightEnabled = true; // default
    let lightsEnabled = true; // default
    if (mapOptions) {
      try {
        const opts = JSON.parse(mapOptions) as { day_night_enabled?: boolean; lights_enabled?: boolean };
        if (typeof opts.day_night_enabled === "boolean") {
          dayNightEnabled = opts.day_night_enabled;
        }
        if (typeof opts.lights_enabled === "boolean") {
          lightsEnabled = opts.lights_enabled;
        }
      } catch {
        // malformed JSON — use defaults
      }
    }
    this.dayNightOverlay?.applyDefault(dayNightEnabled);
    // Apply lights_enabled: when toggled off at runtime, tear down all
    // existing glows; when toggled on, the next update() tick rebuilds them.
    if (this.lightsEnabled !== lightsEnabled) {
      this.lightsEnabled = lightsEnabled;
      if (!lightsEnabled) {
        for (const id of this.activeLights) {
          this.avatars.get(id)?.lightGlow?.setVisible(false);
        }
      }
    }
  }

  // applyPlayerOptions parses the player options JSON and applies player-level
  // preferences. Currently handles show_own_name_tag (default true) and zoom
  // (restores the saved camera zoom on reload, clamped to zoom bounds).
  // Updates the local player's name tag visibility if it exists.
  private applyPlayerOptions(playerOptions: string): void {
    let showOwnNameTag = true; // default
    let savedZoom: number | null = null;
    const opts = parsePlayerOptions(playerOptions);
    if (typeof opts.show_own_name_tag === "boolean") {
      showOwnNameTag = opts.show_own_name_tag;
    }
    if (typeof opts.zoom === "number" && Number.isFinite(opts.zoom)) {
      savedZoom = opts.zoom;
    }
    this.showOwnNameTag = showOwnNameTag;
    // Restore saved zoom, clamped to bounds so a stale/out-of-range value
    // from an older client can't break the camera.
    if (savedZoom !== null) {
      this.cameras.main.setZoom(Phaser.Math.Clamp(savedZoom, ZOOM_MIN, ZOOM_MAX));
    }
    // Update the local player's name tag visibility if it exists.
    if (this.myEntityId) {
      const avatar = this.avatars.get(this.myEntityId);
      if (avatar?.nameTag) {
        avatar.nameTag.setVisible(showOwnNameTag);
      }
    }
  }

  // scheduleZoomPersist debounces a SetPlayerOptionsFrame that merges the
  // current camera zoom into the player_options JSON. 500ms coalesces a
  // stream of wheel notches into a single server write. Guests have no PB
  // record so the server-side persist is a no-op, matching show_own_name_tag.
  private scheduleZoomPersist(): void {
    if (this.zoomPersistTimer !== null) {
      clearTimeout(this.zoomPersistTimer);
    }
    this.zoomPersistTimer = window.setTimeout(() => {
      this.zoomPersistTimer = null;
      // parsePlayerOptions guards against null/non-object JSON (e.g. a PB
      // field whose value is the literal string "null"), which would otherwise
      // throw on the property assignment below and silently drop the zoom write.
      const opts = parsePlayerOptions(this.ws?.getPlayerOptions() ?? "") as { show_own_name_tag?: boolean; zoom?: number };
      opts.zoom = this.cameras.main.zoom;
      const json = JSON.stringify(opts);
      this.ws?.setPlayerOptions(json);
      // Keep the TopMenu's cached playerOptions in sync so its checkbox
      // merge doesn't drop the zoom key we just added.
      const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
      tm?.setPlayerOptions(json);
    }, 500);
  }

  private handleReplication(batch: ReplicationBatchView): void {
    // While a map transition is in progress, buffer spawns — the scene is
    // about to restart and SHUTDOWN would destroy any avatars created now.
    // The server sets spawnedTo=true after sending, so without buffering the
    // spawn would be lost and never re-sent. Buffered spawns are processed
    // in create() after the restart completes.
    if (this.transitioning) {
      for (const spawn of batch.spawns) {
        if (!this.pendingSpawns.some((s) => s.entityId === spawn.entityId)) {
          this.pendingSpawns.push(spawn);
        }
      }
      return;
    }
    this.processSpawns(batch.spawns);
    this.processUpdatesAndDestroys(batch);
  }

  // processSpawns creates avatars for entities in the given spawn list.
  // Extracted from handleReplication so it can be called both during normal
  // replication and after a map transition (from buffered pendingSpawns).
  private processSpawns(spawns: ReplicationBatchView["spawns"]): void {
    for (const spawn of spawns) {
      if (this.avatars.has(spawn.entityId)) continue;

      let x = 10 * TILE_SIZE;
      let y = 10 * TILE_SIZE;
      let gid = 0;
      let spriteBase = "";
      let interactable = false;
      let state = "";
      let displayName = "";
      let isGuest = false;
      let isAdmin = false;
      let status = 0;
      let afk = false;
      let afkSince = 0;
      let lightIntensity = 0;
      let lightColor = 0;
      let lightRadius = 0;
      for (const comp of spawn.components) {
        if (comp.componentId === 1) {
          // Position component
          const pos = decodePosition(comp.data);
          x = pos.x * TILE_SIZE;
          y = pos.y * TILE_SIZE;
        } else if (comp.componentId === 3) {
          // Appearance component — gid for props, sprite_base for player avatars
          const appearance = fromBinary(AppearanceSchema, comp.data);
          gid = appearance.gid;
          spriteBase = appearance.spriteBase;
          interactable = appearance.interactable;
        } else if (comp.componentId === 2) {
          // EntityState component — opaque state string (e.g. "on"/"off")
          const es = fromBinary(EntityStateSchema, comp.data);
          state = es.state;
        } else if (comp.componentId === 4) {
          // DisplayName component — player avatar name tag
          const dn = fromBinary(DisplayNameSchema, comp.data);
          displayName = dn.name;
          isGuest = dn.isGuest;
          isAdmin = dn.isAdmin;
          status = dn.status;
          afk = dn.afk;
          afkSince = Number(dn.afkSince);
        } else if (comp.componentId === 5) {
          // LightEmitter component — intensity/color/radius for the lighting
          // system. intensity > 0 marks the entity as an active light.
          const le = fromBinary(LightEmitterSchema, comp.data);
          lightIntensity = le.intensity;
          lightColor = le.color;
          lightRadius = le.radius;
        }
      }

      // Base entities (props) with a gid render as tile sprites; player
      // avatars render as character sprites with walk/idle animations.
      if (gid !== 0) {
        const mapped = gidToFrame(gid, this.tilesets);
        if (mapped) {
          // Tiled tile-objects anchor at bottom-left (x, y is the base/feet
          // position), matching how decoration object-layer sprites are drawn.
          const sprite = this.add.sprite(x, y, mapped.sheet, mapped.frame);
          sprite.setOrigin(0, 1);
          sprite.setDepth(this.dynamicDepth(y));
          this.avatars.set(spawn.entityId, {
            sprite,
            entityId: spawn.entityId,
            charKey: mapped.sheet,
            dir: 0,
            moving: false,
            isProp: true,
            state,
            interactable,
            lightIntensity,
            lightColor,
            lightRadius,
            lightGlow: null,
            sparksShown: false,
            remotePosA: null,
            remotePosB: null,
            predX: 0,
            predY: 0,
            nameTag: null,
            glow: null,
            warningRect: null,
          });
          // Register as an active light if intensity > 0. The per-frame
          // update() loop will build the masked glow canvas.
          if (lightIntensity > 0) {
            this.activeLights.add(spawn.entityId);
          }
          console.log(`spawned prop ${spawn.entityId} at (${x}, ${y}) gid=${gid} state=${state} light=${lightIntensity}`);
          continue;
        }
      }

      // Use the server-assigned sprite_base (a PB record ID) if set. Fall back
      // to a client-side hash of the entity ID (same FNV-1a as the old server
      // code) for guests / unset — still deterministic, still agrees across
      // clients.
      const charKey = spriteBase
        ? spriteBase
        : CHAR_SPRITES[spriteIndexForEntity(spawn.entityId)];
      const sprite = this.add.sprite(x, y + TILE_SIZE / 2, charKey, DIR_FRAME_START[0]);
      // 64px-tall frame: origin at 0.75 puts the feet at the tile bottom and
      // lets the taller-than-tile head extend up into the tile above.
      sprite.setOrigin(0.5, 0.75);
      // Depth is recomputed every frame in update() from the sprite's feet
      // Y, so avatars Y-sort against dynamic decorations (see Part B).
      sprite.setDepth(this.dynamicDepth(this.feetY(sprite)));
      this.avatars.set(spawn.entityId, {
        sprite,
        entityId: spawn.entityId,
        charKey,
        dir: 0,
        moving: false,
        isProp: false,
        state: "",
        interactable: false,
        lightIntensity,
        lightColor,
        lightRadius,
        lightGlow: null,
        sparksShown: false,
        remotePosA: null,
        remotePosB: { x, y: y + TILE_SIZE / 2, t: performance.now() },
        predX: x / TILE_SIZE,
        predY: y / TILE_SIZE,
        nameTag: null,
        glow: null,
        warningRect: null,
      });
      if (lightIntensity > 0) {
        this.activeLights.add(spawn.entityId);
      }
      // Create the proximity spotlight (soft warm pool at the avatar's feet).
      // Toggled globally with Q; texture swapped to "active" for group members.
      const avatar = this.avatars.get(spawn.entityId)!;
      avatar.glow = this.add.image(x, this.feetY(sprite), "proximityGlow");
      avatar.glow.setOrigin(0.5, 0.5);
      avatar.glow.setDepth(DEPTH_BAND_DYNAMIC - 1);
      avatar.glow.setVisible(this.showProximityGlow);
      // Create name tag if the server sent a DisplayName component. Hidden
      // for the local player's own avatar (you don't need a tag over your
      // own head).
      if (displayName) {
        this.displayNameByEntity.set(spawn.entityId, displayName);
        this.isGuestByEntity.set(spawn.entityId, isGuest);
        this.isAdminByEntity.set(spawn.entityId, isAdmin);
        this.statusByEntity.set(spawn.entityId, status);
        this.afkByEntity.set(spawn.entityId, afk);
        this.afkSinceByEntity.set(spawn.entityId, afkSince);
        this.createNameTag(spawn.entityId, displayName, isGuest, isAdmin, status, afk);
      }
      // Start idle animation immediately.
      sprite.play(`${charKey}_idle_down`, true);
      // Camera follows the local player. roundPixels keeps pixel-art
      // crisp; lerp 1 = hard snap each frame (no sub-pixel smear).
      if (spawn.entityId === this.myEntityId) {
        this.cameras.main.startFollow(sprite, true, 1, 1);
        // Sync local A/V + status UI with the server-confirmed status. On a
        // fresh page reload the server restores the persisted status from
        // PocketBase and sends it in the SpawnEntity DisplayName component;
        // without this the TopMenu would stay on "Available" (its init
        // default) and AvClient wouldn't know about DND until a change.
        if (displayName) {
          this.avClient?.setStatus(status);
          this.avClient?.setAfkMuted(afk);
          const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
          tm?.syncStatusFromServer(status);
          tm?.setLocalAfk(afk);
        }
      }
      console.log(`spawned ${spawn.entityId} at (${x}, ${y})`);
    }
  }

  // processUpdatesAndDestroys handles UpdateComponent, DestroyEntity, and
  // PlayAnimation messages from a replication batch. Called from
  // handleReplication after processSpawns, and skipped during map transitions.
  private processUpdatesAndDestroys(batch: ReplicationBatchView): void {
    // Update components
    for (const upd of batch.updates) {
      const avatar = this.avatars.get(upd.entityId);
      if (!avatar) continue;

      if (upd.componentId === 1) {
        // Position
        const pos = decodePosition(upd.data);
        if (avatar.isProp) {
          // Props use bottom-left anchoring (Tiled tile-object convention).
          avatar.sprite.x = pos.x * TILE_SIZE;
          avatar.sprite.y = pos.y * TILE_SIZE;
          avatar.sprite.setDepth(this.dynamicDepth(avatar.sprite.y));
          continue;
        }
        const px = pos.x * TILE_SIZE;
        const py = pos.y * TILE_SIZE + TILE_SIZE / 2;
        if (upd.entityId === this.myEntityId) {
          // Reconciliation: the server position is authoritative up to
          // lastInputSeq. Discard acked inputs, snap to the server position,
          // then replay the remaining un-acked inputs to re-derive the
          // predicted position.
          this.pendingInputs = this.pendingInputs.filter((e) => e.seq > batch.lastInputSeq);
          let x = pos.x, y = pos.y;
          const now = performance.now();
          for (let i = 0; i < this.pendingInputs.length; i++) {
            const ev = this.pendingInputs[i];
            const next = this.pendingInputs[i + 1];
            const durMs = next ? next.time - ev.time : now - ev.time;
            const r = this.applyMovement(x, y, ev.state, Math.max(0, durMs / TICK_MS));
            x = r.x; y = r.y;
          }
          avatar.predX = x;
          avatar.predY = y;
          avatar.sprite.x = x * TILE_SIZE;
          avatar.sprite.y = y * TILE_SIZE + TILE_SIZE / 2;
        } else {
          // Remote avatar: push position into the interpolation buffer.
          avatar.remotePosA = avatar.remotePosB;
          avatar.remotePosB = { x: px, y: py, t: performance.now() };
        }
      } else if (upd.componentId === 4) {
        // DisplayName component — update or create the name tag.
        const dn = fromBinary(DisplayNameSchema, upd.data);
        if (dn.name) {
          this.displayNameByEntity.set(upd.entityId, dn.name);
          this.isGuestByEntity.set(upd.entityId, dn.isGuest);
          this.isAdminByEntity.set(upd.entityId, dn.isAdmin);
          this.statusByEntity.set(upd.entityId, dn.status);
          this.afkByEntity.set(upd.entityId, dn.afk);
          this.afkSinceByEntity.set(upd.entityId, Number(dn.afkSince));
          // Recreate the tag because the pillbox width depends on text width.
          avatar.nameTag?.destroy();
          this.createNameTag(upd.entityId, dn.name, dn.isGuest, dn.isAdmin, dn.status, dn.afk);
          // Keep the local player's AvClient DND flag and TopMenu status
          // selector in sync with the server-stamped status (defense-in-depth
          // client-side enforcement + UI reflection without echoing a
          // SetStatusFrame back to the server).
          if (upd.entityId === this.myEntityId) {
            this.avClient?.setStatus(dn.status);
            this.avClient?.setAfkMuted(dn.afk);
            const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
            tm?.syncStatusFromServer(dn.status);
            tm?.setLocalAfk(dn.afk);
          }
        }
      } else if (upd.componentId === 3) {
        // Appearance component — hot-swap the character sheet if sprite_base
        // changed (player avatars), or swap the tile sprite frame if gid
        // changed (props, e.g. light turning on/off).
        const app = fromBinary(AppearanceSchema, upd.data);
        if (avatar.isProp) {
          // Props: swap the tile sprite to the new gid frame.
          if (app.gid !== 0) {
            const mapped = gidToFrame(app.gid, this.tilesets);
            if (mapped) {
              avatar.sprite.setTexture(mapped.sheet, mapped.frame);
              avatar.charKey = mapped.sheet;
            }
          }
        } else {
          // Player avatars: hot-swap the character sheet.
          const newKey = app.spriteBase
            ? app.spriteBase
            : CHAR_SPRITES[spriteIndexForEntity(upd.entityId)];
          if (newKey !== avatar.charKey) {
            avatar.sprite.setTexture(newKey, DIR_FRAME_START[avatar.dir]);
            avatar.charKey = newKey;
            const animKey = avatar.moving
              ? `${newKey}_walk_${DIR_NAMES[avatar.dir]}`
              : `${newKey}_idle_${DIR_NAMES[avatar.dir]}`;
            avatar.sprite.play(animKey, true);
          }
        }
      } else if (upd.componentId === 2) {
        // EntityState component — opaque state string (e.g. "on"/"off").
        // No longer drives the light glow (LightEmitter component does).
        const es = fromBinary(EntityStateSchema, upd.data);
        avatar.state = es.state;
      } else if (upd.componentId === 5) {
        // LightEmitter component — intensity/color/radius. Toggle the
        // activeLights membership; the per-frame update() loop draws the
        // masked glow canvas for active lights.
        const le = fromBinary(LightEmitterSchema, upd.data);
        avatar.lightIntensity = le.intensity;
        avatar.lightColor = le.color;
        avatar.lightRadius = le.radius;
        if (le.intensity > 0) {
          this.activeLights.add(upd.entityId);
        } else {
          this.activeLights.delete(upd.entityId);
          this.hideLightGlow(avatar);
        }
      }
    }

    // Destroy entities
    for (const dest of batch.destroys) {
      const avatar = this.avatars.get(dest.entityId);
      if (avatar) {
        if (this.openDropdownEntityId === dest.entityId) {
          this.closeDropdown();
        }
        avatar.nameTag?.destroy();
        avatar.glow?.destroy();
        avatar.lightGlow?.destroy();
        avatar.warningRect?.destroy();
        avatar.sprite.destroy();
        this.activeLights.delete(dest.entityId);
        // Remove the per-light CanvasTexture if one was allocated.
        this.textures.remove(`lightGlow:${dest.entityId}`);
        this.avatars.delete(dest.entityId);
        this.displayNameByEntity.delete(dest.entityId);
        this.isGuestByEntity.delete(dest.entityId);
        this.isAdminByEntity.delete(dest.entityId);
        this.statusByEntity.delete(dest.entityId);
        this.afkByEntity.delete(dest.entityId);
        this.afkSinceByEntity.delete(dest.entityId);
        this.adminInfoByEntity.delete(dest.entityId);
        console.log(`destroyed ${dest.entityId}`);
      }
    }

    // Play animations (PlayAnimation replication messages).
    for (const anim of batch.animations) {
      const avatar = this.avatars.get(anim.entityId);
      if (!avatar) continue;
      if (anim.animationId === ANIM_CLICK) {
        this.sound.play("clic", { volume: 0.5 });
      }
    }
  }

  // createNameTag builds a speech-bubble name tag above the avatar's sprite.
  // A semi-transparent grey pillbox contains a green status pill (clickable —
  // opens an info dropdown), the avatar's name in a scalable web font
  // (Nunito), and optionally a red "admin" badge (for admins who haven't
  // opted out) and/or a grey "GUEST" badge for anonymous users. A small
  // inverted triangle at the bottom points down at the avatar. The container
  // is counter-scaled by 1/zoom each frame (see update) so it stays a
  // constant screen size. Hidden for the local player's own avatar.
  private createNameTag(entityId: string, name: string, isGuest: boolean, isAdmin: boolean, status: number, afk: boolean): void {
    const avatar = this.avatars.get(entityId);
    if (!avatar) return;

    // Close any open dropdown for this entity — the old dot handler is gone.
    if (this.openDropdownEntityId === entityId) {
      this.closeDropdown();
    }

    const container = this.add.container(
      avatar.sprite.x,
      avatar.sprite.y -
        avatar.sprite.displayHeight * avatar.sprite.originY +
        NAME_TAG_HEAD_INSET_PX -
        NAME_TAG_PAD_PX / this.cameras.main.zoom,
    );

    // --- Name text (scalable web font, not pixel art) ---
    const text = this.add.text(0, 0, name, {
      fontFamily: "Nunito, sans-serif",
      fontSize: "13px",
      color: "#ffffff",
      fontStyle: "bold",
    });
    text.setOrigin(0, 0.5); // left-aligned, vertically centered

    // --- ADMIN badge (red, for admins who haven't opted out) ---
    let adminBadge: Phaser.GameObjects.Text | null = null;
    if (isAdmin) {
      adminBadge = this.add.text(0, 0, "admin", {
        fontFamily: "Nunito, sans-serif",
        fontSize: "9px",
        color: "#ffffff",
        fontStyle: "bold",
        backgroundColor: "#dc2626",
        padding: { left: 4, right: 4, top: 1, bottom: 1 },
      });
      adminBadge.setOrigin(0, 0.5);
    }

    // --- GUEST badge (only for anonymous users) ---
    let guestBadge: Phaser.GameObjects.Text | null = null;
    if (isGuest) {
      guestBadge = this.add.text(0, 0, "GUEST", {
        fontFamily: "Nunito, sans-serif",
        fontSize: "9px",
        color: "#f8fafc",
        fontStyle: "bold",
        backgroundColor: "#6b7280",
        padding: { left: 4, right: 4, top: 1, bottom: 1 },
      });
      guestBadge.setOrigin(0, 0.5);
    }

    // --- AFK badge (shown when the AFK overlay is active) ---
    let afkBadge: Phaser.GameObjects.Text | null = null;
    if (afk) {
      afkBadge = this.add.text(0, 0, "AFK", {
        fontFamily: "Nunito, sans-serif",
        fontSize: "9px",
        color: "#f8fafc",
        fontStyle: "bold",
        backgroundColor: "#6b7280",
        padding: { left: 4, right: 4, top: 1, bottom: 1 },
      });
      afkBadge.setOrigin(0, 0.5);
    }

    // --- Status pill (color reflects presence status; clickable, opens
    // info dropdown). 0=Available (green), 1=Busy (yellow), 2=DND (red).
    // When AFK, the pill is dimmed (reduced alpha) to signal the overlay —
    // the manual status color stays visible underneath. ---
    const pillColors = [
      { fill: 0x22c55e, stroke: 0x15803d }, // available
      { fill: 0xeab308, stroke: 0xa16207 }, // busy
      { fill: 0xef4444, stroke: 0x991b1b }, // do not disturb
    ];
    const pc = pillColors[status] ?? pillColors[0];
    const pillRadius = 4;
    const statusPill = this.add.circle(0, 0, pillRadius, pc.fill);
    statusPill.setStrokeStyle(1, pc.stroke);
    if (afk) statusPill.setAlpha(0.4);
    // Enlarge the hit area so the dot is easy to click despite counter-scaling.
    statusPill.setInteractive(
      new Phaser.Geom.Circle(0, 0, 14),
      Phaser.Geom.Circle.Contains,
    );
    statusPill.on("pointerover", () => statusPill.setScale(1.3));
    statusPill.on("pointerout", () => statusPill.setScale(1));
    statusPill.on("pointerdown", () => {
      this._dropdownClickedThisFrame = true;
      this.toggleDropdown(entityId);
    });

    // --- Layout ---
    const padding = 10;
    const gap = 6;
    const pillBoxHeight = 22;
    const tailW = 8;
    const tailH = 5;
    const badgeGap = 6;
    const adminBadgeWidth = adminBadge ? adminBadge.width + badgeGap : 0;
    const guestBadgeWidth = guestBadge ? guestBadge.width + badgeGap : 0;
    const afkBadgeWidth = afkBadge ? afkBadge.width + badgeGap : 0;
    const contentWidth =
      pillRadius * 2 + gap + text.width + adminBadgeWidth + guestBadgeWidth + afkBadgeWidth;
    const pillBoxWidth = contentWidth + padding * 2;

    // (0, 0) in container space = tip of the speech-bubble tail, pointing
    // down at the avatar. Stack grows upward: tail → name.
    const bgCenterY = -tailH - pillBoxHeight / 2;

    // --- Pillbox background (semi-transparent grey, rounded) ---
    const bg = this.add.graphics();
    bg.fillStyle(0x333340, 0.78);
    bg.fillRoundedRect(
      -pillBoxWidth / 2,
      bgCenterY - pillBoxHeight / 2,
      pillBoxWidth,
      pillBoxHeight,
      11,
    );
    bg.setDepth(0);

    // --- Speech-bubble tail (inverted triangle pointing down) ---
    const tail = this.add.graphics();
    tail.fillStyle(0x333340, 0.78);
    tail.fillTriangle(-tailW / 2, -tailH, tailW / 2, -tailH, 0, 0);
    tail.setDepth(0);

    // Position pill, text, and badges inside the name pillbox (left to right):
    // status pill → name → admin badge → guest badge.
    const pillX = -pillBoxWidth / 2 + padding + pillRadius;
    statusPill.setPosition(pillX, bgCenterY);
    const textX = pillX + pillRadius + gap;
    text.setPosition(textX, bgCenterY);
    let badgeX = textX + text.width + badgeGap;
    if (adminBadge) {
      adminBadge.setPosition(badgeX, bgCenterY);
      badgeX += adminBadge.width + badgeGap;
    }
    if (guestBadge) {
      guestBadge.setPosition(badgeX, bgCenterY);
      badgeX += guestBadge.width + badgeGap;
    }
    if (afkBadge) {
      afkBadge.setPosition(badgeX, bgCenterY);
    }

    const children: Phaser.GameObjects.GameObject[] = [bg, tail, statusPill, text];
    if (adminBadge) children.push(adminBadge);
    if (guestBadge) children.push(guestBadge);
    if (afkBadge) children.push(afkBadge);

    container.add(children);
    container.setDepth(avatar.sprite.depth + 0.01);
    // Hide for the local player's own avatar unless showOwnNameTag is enabled.
    container.setVisible(entityId !== this.myEntityId || this.showOwnNameTag);
    // Counter-scale so the tag stays constant screen size regardless of zoom.
    container.setScale(1 / this.cameras.main.zoom);
    avatar.nameTag = container;
  }

  // refreshDropdownIfOpen rebuilds the dropdown for an entity if it is
  // currently open, so admin info that arrives after the dropdown was opened
  // is reflected. The name tag itself no longer depends on admin info.
  private refreshDropdownIfOpen(entityId: string): void {
    if (this.openDropdownEntityId === entityId) {
      this.closeDropdown();
      this.openDropdown(entityId);
    }
  }

  // toggleDropdown opens or closes the info dropdown for an entity. Only one
  // dropdown is open at a time — opening for a new entity closes the previous.
  private toggleDropdown(entityId: string): void {
    if (this.openDropdownEntityId === entityId) {
      this.closeDropdown();
      return;
    }
    this.closeDropdown();
    this.openDropdown(entityId);
  }

  // openDropdown builds and shows the info dropdown for an entity. Content
  // depends on the viewer: regular users see "Hello world", admins see the
  // entity's IP and short device ID. Stub "Invite" and (admin-only) "Ban"
  // buttons are included — they show "Not implemented" on click.
  private openDropdown(entityId: string): void {
    const avatar = this.avatars.get(entityId);
    if (!avatar) return;

    const container = this.add.container(
      avatar.sprite.x,
      avatar.sprite.y -
        avatar.sprite.displayHeight * avatar.sprite.originY +
        NAME_TAG_HEAD_INSET_PX -
        NAME_TAG_PAD_PX / this.cameras.main.zoom +
        6 / this.cameras.main.zoom,
    );
    const children: Phaser.GameObjects.GameObject[] = [];

    const panelW = 130;
    const pad = 8;
    const lineHeight = 14;
    const btnHeight = 18;
    const btnGap = 4;
    const isAdmin = this.ws?.isAdmin() ?? false;

    // --- Info lines ---
    // Regular-user info is always shown; admin-only lines are appended below.
    const infoLines: { text: string; muted: boolean }[] = [
      { text: "Hello world", muted: false },
    ];
    if (isAdmin) {
      const info = this.adminInfoByEntity.get(entityId);
      if (info?.ip) infoLines.push({ text: "IP: " + info.ip, muted: true });
      if (info?.deviceId)
        infoLines.push({ text: "dev: " + info.deviceId.slice(0, 8), muted: true });
    }

    // x origin = center of the panel (matches the name tag pillbox, whose
    // bg is drawn at -pillBoxWidth/2). Content x-coords are offset by -panelW/2.
    const x0 = -panelW / 2;

    let y = pad;
    for (const line of infoLines) {
      const t = this.add.text(x0 + pad, y, line.text, {
        fontFamily: "Nunito, sans-serif",
        fontSize: "11px",
        color: line.muted ? "#cbd5e1" : "#ffffff",
      });
      t.setOrigin(0, 0);
      children.push(t);
      y += lineHeight;
    }

    // --- Separator ---
    y += 3;
    const sep = this.add.graphics();
    sep.fillStyle(0x6b7280, 0.4);
    sep.fillRect(x0 + pad, y, panelW - pad * 2, 1);
    children.push(sep);
    y += 4;

    // --- Buttons (stub — show "Not implemented" on click) ---
    const makeButton = (label: string): void => {
      const btn = this.add.text(x0 + pad, y, label, {
        fontFamily: "Nunito, sans-serif",
        fontSize: "10px",
        color: "#ffffff",
        fontStyle: "bold",
        backgroundColor: "#3e3e4a",
        padding: { left: 6, right: 6, top: 3, bottom: 3 },
      });
      btn.setOrigin(0, 0);
      btn.setInteractive({ useHandCursor: true });
      btn.on("pointerover", () => btn.setStyle({ backgroundColor: "#52525e" }));
      btn.on("pointerout", () => btn.setStyle({ backgroundColor: "#3e3e4a" }));
      btn.on("pointerdown", () => {
        this._dropdownClickedThisFrame = true;
        this.showDropdownStub(container, panelW);
      });
      children.push(btn);
      y += btnHeight + btnGap;
    };

    // makeActionButton is like makeButton but invokes a callback instead of
    // showing the "Not implemented" stub.
    const makeActionButton = (label: string, on_click: () => void): void => {
      const btn = this.add.text(x0 + pad, y, label, {
        fontFamily: "Nunito, sans-serif",
        fontSize: "10px",
        color: "#ffffff",
        fontStyle: "bold",
        backgroundColor: "#3e3e4a",
        padding: { left: 6, right: 6, top: 3, bottom: 3 },
      });
      btn.setOrigin(0, 0);
      btn.setInteractive({ useHandCursor: true });
      btn.on("pointerover", () => btn.setStyle({ backgroundColor: "#52525e" }));
      btn.on("pointerout", () => btn.setStyle({ backgroundColor: "#3e3e4a" }));
      btn.on("pointerdown", () => {
        this._dropdownClickedThisFrame = true;
        on_click();
      });
      children.push(btn);
      y += btnHeight + btnGap;
    };

    makeButton("Invite");
    makeActionButton("Ping", () => {
      this.ws?.sendPing(entityId);
      this.closeDropdown();
    });
    if (isAdmin) {
      makeActionButton("Kick", () => {
        this.ws?.sendKick(entityId, "Kicked by an admin");
        this.closeDropdown();
      });
      makeButton("Ban");
    }

    y -= btnGap; // remove trailing gap
    const panelH = y + pad;

    // --- Background (drawn first so it sits behind content) ---
    const bg = this.add.graphics();
    bg.fillStyle(0x333340, 0.92);
    bg.fillRoundedRect(x0, 0, panelW, panelH, 6);
    bg.setDepth(-1);
    children.unshift(bg);

    container.add(children);
    container.setScale(1 / this.cameras.main.zoom);
    container.setDepth(avatar.sprite.depth + 0.02);
    container.setData("panelH", panelH);
    this.dropdownContainer = container;
    this.openDropdownEntityId = entityId;
  }

  // showDropdownStub shows a temporary "Not implemented yet" text below the
  // dropdown panel, fading out after 1.5 seconds.
  private showDropdownStub(
    container: Phaser.GameObjects.Container,
    panelW: number,
  ): void {
    const panelH = container.getData("panelH") as number;
    const stub = this.add.text(0, panelH + 2, "Not implemented yet", {
      fontFamily: "Nunito, sans-serif",
      fontSize: "9px",
      color: "#fbbf24",
    });
    stub.setOrigin(0.5, 0);
    container.add(stub);
    this.tweens.add({
      targets: stub,
      alpha: { from: 1, to: 0 },
      duration: 1500,
      onComplete: () => stub.destroy(),
    });
  }

  // closeDropdown destroys the current dropdown if any.
  private closeDropdown(): void {
    this.dropdownContainer?.destroy();
    this.dropdownContainer = null;
    this.openDropdownEntityId = null;
  }

  // showAlreadyConnectedConfirm shows a centered confirm dialog when the
  // server reports another session is active for the same logged-in user.
  // "Yes" sends AuthFrame with force=true (displaces the old window);
  // "No" closes the WebSocket so the loading screen stays.
  private showAlreadyConnectedConfirm(): void {
    const cam = this.cameras.main;
    // Hide the "Server not available" overlay while we ask the user — the
    // server IS reachable, it's just waiting for confirmation. onStateChange
    // will re-show it if the user picks "No" (ws.close → state "closed").
    this.disconnectOverlay?.setVisible(false);
    document.body.classList.remove("server-unavailable");
    // Resume the scene so pointer events reach the buttons. The scene was
    // paused on first create awaiting auth; the alreadyConnected branch never
    // transitions to "open" so it stays paused and input is dead. No avatar
    // has been spawned yet, so there's nothing to predict against. If the
    // user picks "No", ws.close → state "closed" → onStateChange re-pauses.
    this.scene.resume("GameScene");
    // Place the container at the screen center in world coordinates and
    // counter-scale by 1/zoom so it renders at constant screen size. Children
    // are positioned relative to the container center (local 0,0). Do NOT use
    // setScrollFactor(0) — per AGENTS.md, it misinterprets world-coord
    // positions as screen coordinates under zoom.
    const overlay = this.add.container(cam.worldView.centerX, cam.worldView.centerY).setDepth(10000);
    const dim = this.add.rectangle(-this.scale.width / 2, -this.scale.height / 2,
      this.scale.width, this.scale.height, 0x000000, 0.7).setOrigin(0, 0);
    const panelW = 360;
    const panelH = 120;
    const bg = this.add.graphics();
    bg.fillStyle(0x333340, 0.95);
    bg.fillRoundedRect(-panelW / 2, -panelH / 2, panelW, panelH, 8);
    const text = this.add.text(0, -panelH / 2 + 16,
      "You are already connected in another window.\nConnect here and disconnect the other?",
      {
        fontFamily: "Nunito, sans-serif",
        fontSize: "14px",
        color: "#ffffff",
        align: "center",
        wordWrap: { width: panelW - 24 },
      }).setOrigin(0.5, 0);
    const btnY = panelH / 2 - 28;
    const yesBtn = this.add.text(-50, btnY, "Yes", {
      fontFamily: "Nunito, sans-serif",
      fontSize: "13px",
      color: "#ffffff",
      fontStyle: "bold",
      backgroundColor: "#4a4a5a",
      padding: { left: 12, right: 12, top: 5, bottom: 5 },
    }).setOrigin(0.5).setInteractive({ useHandCursor: true });
    const noBtn = this.add.text(50, btnY, "No", {
      fontFamily: "Nunito, sans-serif",
      fontSize: "13px",
      color: "#ffffff",
      fontStyle: "bold",
      backgroundColor: "#4a4a5a",
      padding: { left: 12, right: 12, top: 5, bottom: 5 },
    }).setOrigin(0.5).setInteractive({ useHandCursor: true });
    overlay.add([dim, bg, text, yesBtn, noBtn]);
    overlay.setScale(1 / cam.zoom);
    const cleanup = () => overlay.destroy();
    yesBtn.on("pointerover", () => yesBtn.setStyle({ backgroundColor: "#5e5e6e" }));
    yesBtn.on("pointerout", () => yesBtn.setStyle({ backgroundColor: "#4a4a5a" }));
    noBtn.on("pointerover", () => noBtn.setStyle({ backgroundColor: "#5e5e6e" }));
    noBtn.on("pointerout", () => noBtn.setStyle({ backgroundColor: "#4a4a5a" }));
    yesBtn.on("pointerdown", () => {
      cleanup();
      this.ws?.sendAuthForce();
    });
    noBtn.on("pointerdown", () => {
      cleanup();
      this.ws?.close();
    });
  }

  // updateLights is called every frame from update() when lightsEnabled is
  // true. For each active light it redraws the masked glow CanvasTexture at
  // the light's current position.
  private updateLights(): void {
    if (this.activeLights.size === 0) return;
    for (const id of this.activeLights) {
      const avatar = this.avatars.get(id);
      if (!avatar) {
        this.activeLights.delete(id);
        continue;
      }
      this.showLightGlow(avatar);
      this.redrawLightGlowMask(avatar);
    }
  }

  // showLightGlow creates (if needed) and shows the light glow overlay for
  // an entity. The overlay is a procedural CanvasTexture with a radial
  // gradient, masked per-frame by walls and wall zones, centered on the
  // entity's base.
  private showLightGlow(avatar: Avatar): void {
    const radius = this.effectiveLightRadius(avatar);
    const size = Math.ceil(radius * 2 * TILE_SIZE);
    const texKey = `lightGlow:${avatar.entityId}`;
    if (!avatar.lightGlow) {
      // Allocate the per-light CanvasTexture at the light's diameter.
      if (!this.textures.exists(texKey)) {
        this.textures.createCanvas(texKey, size, size);
      }
      avatar.lightGlow = this.add.image(avatar.sprite.x, avatar.sprite.y, texKey);
      avatar.lightGlow.setOrigin(0.5, 0.5);
      avatar.lightGlow.setDepth(avatar.sprite.depth - 0.1);
      avatar.lightGlow.setBlendMode(Phaser.BlendModes.ADD);
      const color = this.effectiveLightColor(avatar);
      avatar.lightGlow.setTint(color);
    }
    avatar.lightGlow.setVisible(true);
    avatar.lightGlow.setPosition(avatar.sprite.x, avatar.sprite.y);
    avatar.lightGlow.setAlpha(avatar.lightIntensity / 100);
  }

  // hideLightGlow hides the light glow overlay for an entity and removes it
  // from the active light set.
  private hideLightGlow(avatar: Avatar): void {
    avatar.lightGlow?.setVisible(false);
  }

  // effectiveLightRadius returns the light's radius in tiles, clamped to
  // LIGHT_MAX_RADIUS_TILES, defaulting to LIGHT_DEFAULT_RADIUS when 0.
  private effectiveLightRadius(avatar: Avatar): number {
    const r = avatar.lightRadius > 0 ? avatar.lightRadius : GameScene.LIGHT_DEFAULT_RADIUS;
    return Math.min(r, GameScene.LIGHT_MAX_RADIUS_TILES);
  }

  // effectiveLightColor returns the light's color as a 0xRRGGBB integer,
  // defaulting to LIGHT_DEFAULT_COLOR (warm white) when 0.
  private effectiveLightColor(avatar: Avatar): number {
    return avatar.lightColor > 0 ? avatar.lightColor : GameScene.LIGHT_DEFAULT_COLOR;
  }

  // [DEBUG] toggleDebugZoneOverlay shows/hides a high-depth overlay of the
  // collisionGrid (red cells) and wallZones (cyan outline + blue fill). Bound
  // to the D key. Redrawn on each toggle so it reflects the current map state.
  private toggleDebugZoneOverlay(): void {
    if (this.debugZoneOverlay) {
      this.debugZoneOverlay.destroy();
      this.debugZoneOverlay = null;
      return;
    }
    const dbg = this.add.graphics();
    dbg.setDepth(100000);
    dbg.fillStyle(0xff0000, 0.35);
    for (let ty = 0; ty < this.collisionGrid.length; ty++) {
      for (let tx = 0; tx < (this.collisionGrid[ty]?.length ?? 0); tx++) {
        if (this.isBlocked(tx, ty)) {
          dbg.fillRect(tx * TILE_SIZE, ty * TILE_SIZE, TILE_SIZE, TILE_SIZE);
        }
      }
    }
    dbg.lineStyle(2, 0x00ffff, 1);
    dbg.fillStyle(0x0000ff, 0.35);
    for (const z of this.wallZones) {
      if (z.shape === "rect") {
        dbg.strokeRect(z.x * TILE_SIZE, z.y * TILE_SIZE, z.w * TILE_SIZE, z.h * TILE_SIZE);
        dbg.fillRect(z.x * TILE_SIZE, z.y * TILE_SIZE, z.w * TILE_SIZE, z.h * TILE_SIZE);
      } else if (z.shape === "circle") {
        dbg.strokeCircle(z.cx * TILE_SIZE, z.cy * TILE_SIZE, z.r * TILE_SIZE);
        dbg.fillCircle(z.cx * TILE_SIZE, z.cy * TILE_SIZE, z.r * TILE_SIZE);
      } else if (z.shape === "polygon") {
        dbg.beginPath();
        z.verts.forEach(([vx, vy], i) => {
          if (i === 0) dbg.moveTo(vx * TILE_SIZE, vy * TILE_SIZE);
          else dbg.lineTo(vx * TILE_SIZE, vy * TILE_SIZE);
        });
        dbg.closePath();
        dbg.fillPath();
        dbg.strokePath();
      }
    }
    this.debugZoneOverlay = dbg;
    console.log("[DEBUG] zone overlay on — wallZones", JSON.stringify(this.wallZones), "activeLights", JSON.stringify([...this.activeLights].map(id => {
      const a = this.avatars.get(id);
      return a ? { id, x: a.sprite.x, y: a.sprite.y, radius: a.lightRadius, intensity: a.lightIntensity } : null;
    })));
  }

  // redrawLightGlowMask redraws the per-light CanvasTexture: a white radial
  // gradient (alpha falloff) with walls and wall zones cut out via
  // destination-out compositing. Called every frame for each active light.
  private redrawLightGlowMask(avatar: Avatar): void {
    const texKey = `lightGlow:${avatar.entityId}`;
    const tex = this.textures.get(texKey) as Phaser.Textures.CanvasTexture | null;
    if (!tex || !(tex instanceof Phaser.Textures.CanvasTexture)) return;
    const ctx = tex.getContext();
    const size = tex.width;
    const cx = size / 2;
    const r = cx;
    // Clear and draw the radial gradient (white with alpha falloff). The
    // color is applied via setTint on the Image, so the texture itself is
    // white — only the alpha channel carries the falloff.
    ctx.clearRect(0, 0, size, size);
    ctx.globalCompositeOperation = "source-over";
    const grad = ctx.createRadialGradient(cx, cx, 0, cx, cx, r);
    grad.addColorStop(0.00, "rgba(255, 255, 255, 0.85)");
    grad.addColorStop(0.45, "rgba(255, 255, 255, 0.45)");
    grad.addColorStop(1.00, "rgba(255, 255, 255, 0)");
    ctx.fillStyle = grad;
    ctx.fillRect(0, 0, size, size);

    // Mask step: cut out walls and wall zones using destination-out so
    // those pixels become transparent (no glow added there).
    ctx.globalCompositeOperation = "destination-out";
    // destination-out subtracts destination alpha by source alpha. The
    // gradient fillStyle from above has near-zero alpha at the canvas edges,
    // so without resetting it the cuts would barely remove anything and light
    // would bleed past walls. Use an opaque fill so cuts fully clear alpha.
    ctx.fillStyle = "rgba(0, 0, 0, 1)";
    // The canvas is centered on the light's sprite position in world space.
    // World-to-canvas offset: canvas pixel (px, py) = world (wx, wy) - (lightX - r, lightY - r).
    const lightX = avatar.sprite.x;
    const lightY = avatar.sprite.y;
    const originX = lightX - r;
    const originY = lightY - r;
    // 1. Cut out blocked collisionGrid cells (full-tile rects in world coords).
    const minTx = Math.floor((originX) / TILE_SIZE);
    const minTy = Math.floor((originY) / TILE_SIZE);
    const maxTx = Math.floor((originX + size) / TILE_SIZE);
    const maxTy = Math.floor((originY + size) / TILE_SIZE);
    for (let ty = minTy; ty <= maxTy; ty++) {
      for (let tx = minTx; tx <= maxTx; tx++) {
        if (this.isBlocked(tx, ty)) {
          const wx = tx * TILE_SIZE;
          const wy = ty * TILE_SIZE;
          ctx.fillRect(wx - originX, wy - originY, TILE_SIZE, TILE_SIZE);
        }
      }
    }
    // 2. Cut out wall zone shapes (raw, no Minkowski expansion). wallZones
    //    are stored in tile coords; convert to pixels to match originX/originY.
    for (const z of this.wallZones) {
      switch (z.shape) {
        case "rect": {
          const zx = z.x * TILE_SIZE;
          const zy = z.y * TILE_SIZE;
          const zw = z.w * TILE_SIZE;
          const zh = z.h * TILE_SIZE;
          if (zx + zw < originX || zx > originX + size || zy + zh < originY || zy > originY + size) break;
          ctx.fillRect(zx - originX, zy - originY, zw, zh);
          break;
        }
        case "circle": {
          const zcx = z.cx * TILE_SIZE;
          const zcy = z.cy * TILE_SIZE;
          const zr = z.r * TILE_SIZE;
          if (zcx + zr < originX || zcx - zr > originX + size || zcy + zr < originY || zcy - zr > originY + size) break;
          ctx.beginPath();
          ctx.arc(zcx - originX, zcy - originY, zr, 0, Math.PI * 2);
          ctx.fill();
          break;
        }
        case "polygon": {
          let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
          for (const [vx, vy] of z.verts) {
            const px = vx * TILE_SIZE;
            const py = vy * TILE_SIZE;
            if (px < minX) minX = px;
            if (py < minY) minY = py;
            if (px > maxX) maxX = px;
            if (py > maxY) maxY = py;
          }
          if (maxX < originX || minX > originX + size || maxY < originY || minY > originY + size) break;
          ctx.beginPath();
          for (let i = 0; i < z.verts.length; i++) {
            const [vx, vy] = z.verts[i];
            const px = vx * TILE_SIZE - originX;
            const py = vy * TILE_SIZE - originY;
            if (i === 0) ctx.moveTo(px, py);
            else ctx.lineTo(px, py);
          }
          ctx.closePath();
          ctx.fill();
          break;
        }
      }
    }
    // 3. Shadow pass: cut out any sub-cell whose center is occluded from
    //    the light by a wall cell or wall zone (raycast). Steps 1 and 2
    //    only remove the occluder itself; this stops the gradient from
    //    bleeding past walls onto the floor behind them. Sampled at
    //    SHADOW_STEP (well below TILE_SIZE) rather than per-tile, since
    //    wall zone edges rarely land on tile boundaries — a per-tile cut
    //    would round the shadow edge out to the nearest tile line, visible
    //    as a dark seam short of the actual wall. Reuses isLightOccluded
    //    (same test as sprite brightening).
    const SHADOW_STEP = 8;
    for (let sy = originY; sy < originY + size; sy += SHADOW_STEP) {
      for (let sx = originX; sx < originX + size; sx += SHADOW_STEP) {
        const ccx = sx + SHADOW_STEP / 2;
        const ccy = sy + SHADOW_STEP / 2;
        if (this.isBlocked(Math.floor(ccx / TILE_SIZE), Math.floor(ccy / TILE_SIZE))) continue; // already cut in step 1
        if (this.isLightOccluded(lightX, lightY, ccx, ccy)) {
          ctx.fillRect(sx - originX, sy - originY, SHADOW_STEP, SHADOW_STEP);
        }
      }
    }
    ctx.globalCompositeOperation = "source-over";
    tex.refresh();
  }

  // isLightOccluded tests whether the segment from (lx,ly) to (tx,ty) in
  // world pixel coords is blocked by a collisionGrid wall cell or a raw
  // wallZone shape. Uses sub-tile DDA for the grid and analytic segment
  // tests for zones (no Minkowski expansion — light has no body).
  private isLightOccluded(lx: number, ly: number, tx: number, ty: number): boolean {
    // Convert to tile coords for grid DDA.
    const ltx = lx / TILE_SIZE;
    const lty = ly / TILE_SIZE;
    const ttx = tx / TILE_SIZE;
    const tty = ty / TILE_SIZE;
    // DDA across the grid cells the segment passes through.
    const dx = ttx - ltx;
    const dy = tty - lty;
    const steps = Math.ceil(Math.max(Math.abs(dx), Math.abs(dy)) * 2) + 1;
    for (let i = 1; i < steps; i++) {
      const f = i / steps;
      const cx = Math.floor(ltx + dx * f);
      const cy = Math.floor(lty + dy * f);
      if (this.isBlocked(cx, cy)) return true;
    }
    // Wall zones: raw segment-vs-shape (no expansion).
    for (const z of this.wallZones) {
      switch (z.shape) {
        case "rect":
          if (segmentIntersectsRect(ltx, lty, ttx, tty, z.x, z.y, z.w, z.h)) return true;
          break;
        case "circle":
          if (segmentIntersectsCircle(ltx, lty, ttx, tty, z.cx, z.cy, z.r)) return true;
          break;
        case "polygon":
          if (segmentIntersectsPolygon(ltx, lty, ttx, tty, z.verts)) return true;
          break;
      }
    }
    return false;
  }

  // playSparks plays a one-shot sparks animation above the entity to
  // signal it is interactable. Client-side only — no server round-trip.
  private playSparks(avatar: Avatar): void {
    if (!this.textures.exists("sparks")) return;
    const spark = this.add.sprite(avatar.sprite.x, avatar.sprite.y - 20, "sparks");
    spark.setOrigin(0.5, 0.5);
    spark.setDepth(avatar.sprite.depth + 0.5);
    spark.play("sparks_anim");
    spark.on("animationcomplete", () => spark.destroy());
  }

  // openInteractionPopup shows a popup with available actions for the
  // player to choose from. Anchored to the left of the local avatar (via
  // positionInteractionPopup) so it follows the player rather than screen
  // center, which matters when the camera clamps at a map border or is
  // zoomed out beyond the map. Multiple entities may be listed, but the
  // popup is triggered by the local player's interaction, so anchoring to
  // the local avatar is the intuitive reference point.
  private openInteractionPopup(actions: AvailableActionView[]): void {
    this.closeInteractionPopup();

    const container = this.add.container(0, 0);
    const children: Phaser.GameObjects.GameObject[] = [];

    const panelW = 160;
    const pad = 8;
    const btnHeight = 22;
    const btnGap = 4;

    // Group actions by entityLabel for display.
    const groups = new Map<string, AvailableActionView[]>();
    for (const a of actions) {
      const key = a.entityLabel || a.entityId;
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key)!.push(a);
    }

    let y = pad;
    for (const [label, acts] of groups) {
      // Group header
      const header = this.add.text(-panelW / 2 + pad, y, label, {
        fontFamily: "Nunito, sans-serif",
        fontSize: "10px",
        color: "#94a3b8",
      });
      header.setOrigin(0, 0);
      children.push(header);
      y += 14;

      // Action buttons
      for (const act of acts) {
        const btn = this.add.text(-panelW / 2 + pad, y, act.label, {
          fontFamily: "Nunito, sans-serif",
          fontSize: "12px",
          color: "#ffffff",
          fontStyle: "bold",
          backgroundColor: "#3e3e4a",
          padding: { left: 8, right: 8, top: 4, bottom: 4 },
        });
        btn.setOrigin(0, 0);
        btn.setInteractive({ useHandCursor: true });
        btn.on("pointerover", () => btn.setStyle({ backgroundColor: "#52525e" }));
        btn.on("pointerout", () => btn.setStyle({ backgroundColor: "#3e3e4a" }));
        btn.on("pointerdown", () => {
          this.ws?.sendAction("action:execute", act.entityId, act.actionId);
          this.closeInteractionPopup();
        });
        children.push(btn);
        y += btnHeight + btnGap;
      }
      y += 4;
    }

    y -= btnGap + 4;
    const panelH = y + pad;

    // Background
    const bg = this.add.graphics();
    bg.fillStyle(0x333340, 0.95);
    bg.fillRoundedRect(-panelW / 2, 0, panelW, panelH, 6);
    bg.setDepth(-1);
    children.unshift(bg);

    container.add(children);
    // Store panel dimensions so positionInteractionPopup can place the
    // popup to the left of the local avatar and clamp it to the viewport
    // (both at creation and every frame in update()). Store the entity IDs
    // the popup lists so update() can auto-close it when the local player
    // walks out of interaction range of all of them.
    container.setData("panelW", panelW);
    container.setData("panelH", panelH);
    container.setData(
      "entityIds",
      actions.map((a) => a.entityId),
    );
    this.positionInteractionPopup(container);
    container.setDepth(10000);
    this.interactionPopup = container;
  }

  // closeInteractionPopup destroys the current interaction popup if any.
  private closeInteractionPopup(): void {
    this.interactionPopup?.destroy();
    this.interactionPopup = null;
  }

  // positionInteractionPopup places the interaction popup to the left of the
  // local avatar, counter-scaled by 1/zoom so it renders at a constant screen
  // size, and clamped to the visible camera viewport so it never goes
  // off-screen (e.g. when the camera clamps at a map border or is zoomed out
  // beyond the map). Falls back to screen center if the local avatar isn't
  // present yet. panelW/panelH are read from the container's data, set in
  // openInteractionPopup. Called once at creation and every frame in update().
  private positionInteractionPopup(container: Phaser.GameObjects.Container): void {
    const cam = this.cameras.main;
    const zoom = cam.zoom;
    const panelW = container.getData("panelW") as number;
    const panelH = container.getData("panelH") as number;
    const halfW = panelW / 2 / zoom; // world-space half width of the panel
    const halfH = panelH / zoom;     // world-space full height of the panel
    const margin = 4 / zoom;         // 4px screen margin in world units

    const local = this.myEntityId ? this.avatars.get(this.myEntityId) : null;
    let x: number, y: number;
    if (local) {
      // Place the panel's right edge GAP screen-px to the left of the avatar,
      // and vertically center the panel on the avatar.
      const GAP = 18 / zoom;
      x = local.sprite.x - GAP - halfW;
      y = local.sprite.y - halfH / 2;
    } else {
      x = cam.worldView.centerX;
      y = cam.worldView.centerY;
    }

    // Clamp so the panel stays within the visible viewport.
    const vw = cam.worldView;
    if (x - halfW < vw.left + margin) x = vw.left + margin + halfW;
    if (x + halfW > vw.right - margin) x = vw.right - margin - halfW;
    if (y < vw.top + margin) y = vw.top + margin;
    if (y + halfH > vw.bottom - margin) y = vw.bottom - margin - halfH;

    container.x = x;
    container.y = y;
    container.setScale(1 / zoom);
  }

  // resolveDisplayName returns the display name for an entity, used by the
  // VideoBar to label tiles. For the local player, getUsername() (localStorage)
  // is checked first since it updates immediately when the user changes their
  // name, before the server echoes back a DisplayName component update.
  private resolveDisplayName(entityId: string): string {
    if (entityId === this.myEntityId) {
      const local = getUsername();
      if (local) return local;
    }
    const name = this.displayNameByEntity.get(entityId);
    if (name) return name;
    if (entityId === this.myEntityId) return "You";
    return entityId;
  }

  // getConnectedPlayers returns the players currently on the local player's
  // map (including self), with their presence status, AFK overlay + since
  // timestamp, and guest/admin badges. Only entities with a DisplayName
  // component are included — props and decorations never have one, so they
  // are naturally filtered out. Used by the TopMenu Players panel.
  getConnectedPlayers(): {
    entityId: string;
    name: string;
    isGuest: boolean;
    isAdmin: boolean;
    status: number;
    afk: boolean;
    afkSinceMs: number;
    isSelf: boolean;
  }[] {
    const out: {
      entityId: string;
      name: string;
      isGuest: boolean;
      isAdmin: boolean;
      status: number;
      afk: boolean;
      afkSinceMs: number;
      isSelf: boolean;
    }[] = [];
    for (const entityId of this.displayNameByEntity.keys()) {
      out.push({
        entityId,
        name: this.resolveDisplayName(entityId),
        isGuest: this.isGuestByEntity.get(entityId) ?? false,
        isAdmin: this.isAdminByEntity.get(entityId) ?? false,
        status: this.statusByEntity.get(entityId) ?? 0,
        afk: this.afkByEntity.get(entityId) ?? false,
        afkSinceMs: this.afkSinceByEntity.get(entityId) ?? 0,
        isSelf: entityId === this.myEntityId,
      });
    }
    // Self first, then alphabetical by name for stable ordering.
    out.sort((a, b) => {
      if (a.isSelf !== b.isSelf) return a.isSelf ? -1 : 1;
      return a.name.localeCompare(b.name);
    });
    return out;
  }

  // --- Recording indicator + consent toast ---
  // DOM elements (not Phaser world-space) so they're unaffected by camera
  // zoom. The indicator is a small "REC ●" pill fixed near the top of the
  // screen; the toast is a one-time auto-fading message shown when a
  // recording starts.
  private recIndicator: HTMLDivElement | null = null;
  private recToast: HTMLDivElement | null = null;

  private updateRecordingIndicator(active: boolean, target: string): void {
    if (active) {
      if (!this.recIndicator) {
        const pill = document.createElement("div");
        pill.style.cssText =
          "position:fixed;top:60px;left:12px;z-index:20;padding:4px 12px;font-size:14px;font-family:sans-serif;font-weight:700;background:#c0392b;color:#fff;border-radius:20px;display:flex;align-items:center;gap:6px;";
        pill.innerHTML = '<span style="animation:blink 1s infinite">●</span> REC';
        const style = document.createElement("style");
        style.textContent = "@keyframes blink{0%,100%{opacity:1}50%{opacity:0.3}}";
        document.head.appendChild(style);
        document.body.appendChild(pill);
        this.recIndicator = pill;
      }
      this.recIndicator.style.display = "flex";
      // One-time toast.
      if (!this.recToast) {
        const toast = document.createElement("div");
        toast.style.cssText =
          "position:fixed;top:50%;left:50%;transform:translate(-50%,-50%);z-index:30;padding:16px 24px;font-size:16px;font-family:sans-serif;background:#2d2d3a;color:#fff;border-radius:12px;box-shadow:0 4px 12px rgba(0,0,0,0.4);transition:opacity 0.5s;opacity:1;";
        toast.textContent = `This meeting is being recorded to ${target}`;
        document.body.appendChild(toast);
        this.recToast = toast;
        setTimeout(() => {
          toast.style.opacity = "0";
          setTimeout(() => {
            toast.remove();
            this.recToast = null;
          }, 500);
        }, 4000);
      }
    } else {
      if (this.recIndicator) {
        this.recIndicator.remove();
        this.recIndicator = null;
      }
    }
  }

  // showRecordingAdminStopToast displays a transient center-screen toast
  // telling participants the recording was force-stopped by an admin.
  // Styled to match the server-updated reload toast in main.ts.
  private showRecordingAdminStopToast(): void {
    const toast = document.createElement("div");
    toast.textContent = "The recording was stopped by an admin.";
    toast.style.cssText =
      "position:fixed;top:50%;left:50%;transform:translate(-50%,-50%);" +
      "padding:10px 16px;font-size:14px;font-family:sans-serif;font-weight:600;" +
      "background:#2d2d3a;color:#fff;border:1px solid rgba(255,255,255,0.15);" +
      "border-radius:20px;cursor:pointer;z-index:10001;" +
      "box-shadow:0 4px 12px rgba(0,0,0,0.4);";
    const dismiss = (): void => {
      toast.remove();
      toast.onclick = null;
      clearTimeout(timer);
    };
    toast.onclick = dismiss;
    document.body.appendChild(toast);
    const timer = setTimeout(dismiss, 8_000);
  }
}
