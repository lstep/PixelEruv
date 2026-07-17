import Phaser from "phaser";
import { fromBinary } from "@bufbuild/protobuf";
import { AppearanceSchema, DisplayNameSchema, EntityStateSchema } from "../proto/components_pb";
import { WsClient, decodePosition, ReplicationBatchView, ConnectionState, AvailableActionView } from "../net/WsClient";
import { AvClient } from "../net/AvClient";
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
  // Light glow overlay (null for non-light props). A 7x7-tile PNG with a
  // radial gradient, shown when state === "on".
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
}

// Apply `ticks` worth of movement from (x, y) under `state`, matching the
// server's movement math (worldsim.go: speed, diagonal normalize, collision).
// Uses the collision grid for wall blocking and map bounds.

export class GameScene extends Phaser.Scene {
  private ws: WsClient | null = null;
  private avatars: Map<string, Avatar> = new Map();
  private myEntityId: string | null = null;
  private avClient: AvClient | null = null;
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
  // Admin-only info by entity ID (IP, guest status). Populated from
  // AdminInfoFrame, only received by admin clients. Used to render the IP
  // below the name in the name tag pillbox for admin viewers.
  private adminInfoByEntity = new Map<string, { ip: string; isGuest: boolean; deviceId: string }>();
  private inputState: InputState = { up: false, down: false, left: false, right: false, run: false };
  private inputDirty = false;
  // Un-acked inputs for the local avatar, newest last. Replayed against the
  // server's authoritative position on each reconciliation.
  private pendingInputs: InputEvent[] = [];
  // Collision grid from the Tiled "Walls" layer. [y][x] = true means blocked.
  private collisionGrid: boolean[][] = [];
  // Wall zones from the Tiled "Zones" object layer with zone_type=wall.
  // Used for client-side prediction of zone collision, matching the server's
  // swept segment-vs-rect test.
  private wallZones: WallZone[] = [];
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
  // Entity IDs in the local player's current proximity chat group, computed
  // each frame via connected-components on feet-distance ≤ 2 tiles (matching
  // the server's runProximityClustering). Used to highlight group members.
  private proximityGroup: Set<string> = new Set();

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
    const fy = (y: number) => Math.floor(y + FEET_Y_OFFSET + 0.5);
    if (this.isBlocked(Math.floor(newX + 0.5), fy(y)) ||
        this.isZoneBlocked(x, y, newX, y)) newX = x;
    if (this.isBlocked(Math.floor(newX + 0.5), fy(newY)) ||
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
    for (const ts of mapAssets.tilesets) {
      this.load.image(ts.name, ts.url);
      // Also load as a spritesheet so individual tiles can be drawn as
      // standalone sprites for object-layer decorations (see create()).
      // Assumes a single, unspaced tileset grid matching the map's tile
      // size — the same assumption the rest of the map pipeline makes.
      this.load.spritesheet(`${ts.name}__tiles`, ts.url, { frameWidth: TILE_SIZE, frameHeight: TILE_SIZE });
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
    this.load.image("lightGlow", "/assets/sprites/light-glow.png");
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
      const wallsLayerName = rawJson.layers.find((l) => l.type === "tilelayer" && l.name.toLowerCase() === "walls")?.name;
      const walls = wallsLayerName ? map.createLayer(wallsLayerName, validTilesets, 0, 0) : null;
      walls?.setDepth(DEPTH_WALLS_FALLBACK);

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
            // Not yet implemented — render at a flat depth in the shared
            // band so it still interleaves at the layer granularity rather
            // than being silently dropped or crashing.
            console.warn(`decoration layer "${layer.name}": sort_mode=dynamic on a tile layer only gets a flat depth (per-tile Y-sort isn't implemented yet)`);
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
    this.ws = new WsClient(wsUrl);
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
    const chatPanel = this.game.registry.get("chatPanel") as ChatPanel | undefined;
    // "Server not available" overlay — a full-screen gray dim plus a red
    // centered message. Visible from the start (we boot in "connecting"),
    // hidden once the WS reaches "open", and reshown on any drop. While it
    // is visible the scene is paused so local prediction doesn't move the
    // avatar against a dead server.
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
    // Day/night tint overlay — cosmetic, client-side, follows the local
    // clock. Sits below the disconnect overlay (depth 9997 vs 9998).
    this.dayNightOverlay = new DayNightOverlay(this);
    this.scale.on("resize", (gameSize: Phaser.Structs.Size) => {
      this.dayNightOverlay?.resize(gameSize.width, gameSize.height);
    });
    // Freeze the scene until the first successful auth.
    this.scene.pause("GameScene");
    this.ws.connect({
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
        }
        this.avatars.clear();
        this.displayNameByEntity.clear();
        this.isGuestByEntity.clear();
        this.isAdminByEntity.clear();
        this.statusByEntity.clear();
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
      onBanned: (reason, banUntil) => {
        const msg = this.disconnectOverlay?.getAt(1) as Phaser.GameObjects.Text | undefined;
        if (msg) {
          const expiry = banUntil > 0
            ? `until ${new Date(banUntil * 1000).toLocaleString()}`
            : "permanently";
          msg.setText(`You are banned ${expiry}\n${reason}`);
        }
      },
      onActionResult: (result) => {
        if (result.availableActions.length > 0) {
          this.openInteractionPopup(result.availableActions);
        } else {
          this.closeInteractionPopup();
        }
      },
    });
    chatPanel?.setSendHandler((channel, text) => this.ws?.sendChat(channel, text));
    topMenu?.setSetNameHandler((name) => this.ws?.setName(name));
    topMenu?.setSetSpriteBaseHandler((spriteBase) => this.ws?.setSpriteBase(spriteBase));
    topMenu?.setSetPlayerOptionsHandler((options) => this.ws?.setPlayerOptions(options));
    topMenu?.setSetStatusHandler((status) => this.ws?.setStatus(status));

    // Close the info dropdown when clicking outside it. The dot and buttons
    // set _dropdownClickedThisFrame to suppress this on their own clicks.
    this.input.on("pointerdown", () => {
      if (!this._dropdownClickedThisFrame && this.dropdownContainer) {
        this.closeDropdown();
      }
      this._dropdownClickedThisFrame = false;
    });

    // Clean up A/V video bar + LiveKit room on scene shutdown.
    this.events.once(Phaser.Scenes.Events.SHUTDOWN, () => {
      this.videoBar?.destroy();
      this.screenShareOverlay?.destroy();
      this.avClient?.close();
      this.videoBar = null;
      this.screenShareOverlay = null;
      this.avClient = null;
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
        local.nameTag.y = local.sprite.y - 52 / zoom;
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
        avatar.nameTag.y = avatar.sprite.y - 52 / zoom;
        avatar.nameTag.setScale(1 / zoom);
        avatar.nameTag.setDepth(avatar.sprite.depth + 0.01);
      }
      // Reposition the info dropdown to follow the avatar it's attached to.
      if (this.dropdownContainer && this.openDropdownEntityId === avatar.entityId) {
        const zoom = this.cameras.main.zoom;
        this.dropdownContainer.x = avatar.sprite.x;
        this.dropdownContainer.y = avatar.sprite.y - 52 / zoom + 6 / zoom;
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

    // Keep the interaction popup pinned to screen center at a constant
    // screen size while open, so camera pan/zoom doesn't move or resize it.
    if (this.interactionPopup) {
      const zoom = this.cameras.main.zoom;
      this.interactionPopup.x = this.cameras.main.worldView.centerX;
      this.interactionPopup.y = this.cameras.main.worldView.centerY;
      this.interactionPopup.setScale(1 / zoom);
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
    // Apply map options (e.g. day_night_enabled) before loading assets so the
    // overlay is correct when the scene restarts.
    if (mapOptions !== undefined) this.applyMapOptions(mapOptions);
    // Load the new map assets from PocketBase, then restart the scene.
    loadMapAssets(mapId)
      .then((mapAssets) => {
        // Stash the new map assets for the restarted scene.
        this.registry.set("mapAssets", mapAssets);
        this.registry.set("loadedMapName", mapId);
        // Destroy all avatars — the server will re-spawn them on the new map.
        for (const av of this.avatars.values()) {
          av.sprite.destroy();
          av.nameTag?.destroy();
          av.glow?.destroy();
        }
        this.avatars.clear();
        this.displayNameByEntity.clear();
        this.isGuestByEntity.clear();
        this.isAdminByEntity.clear();
        this.statusByEntity.clear();
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
  // toggles. Currently handles day_night_enabled (default true). The map option
  // sets the default — the player's explicit localStorage preference takes
  // precedence (see DayNightOverlay.applyDefault).
  private applyMapOptions(mapOptions: string): void {
    let dayNightEnabled = true; // default
    if (mapOptions) {
      try {
        const opts = JSON.parse(mapOptions) as { day_night_enabled?: boolean };
        if (typeof opts.day_night_enabled === "boolean") {
          dayNightEnabled = opts.day_night_enabled;
        }
      } catch {
        // malformed JSON — use defaults
      }
    }
    this.dayNightOverlay?.applyDefault(dayNightEnabled);
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
    // Spawn new entities
    for (const spawn of batch.spawns) {
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
            lightGlow: null,
            sparksShown: false,
            remotePosA: null,
            remotePosB: null,
            predX: 0,
            predY: 0,
            nameTag: null,
            glow: null,
          });
          // Show glow overlay if the prop starts in the "on" state.
          if (state === "on" && this.textures.exists("lightGlow")) {
            this.showLightGlow(this.avatars.get(spawn.entityId)!);
          }
          console.log(`spawned prop ${spawn.entityId} at (${x}, ${y}) gid=${gid} state=${state}`);
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
        lightGlow: null,
        sparksShown: false,
        remotePosA: null,
        remotePosB: { x, y: y + TILE_SIZE / 2, t: performance.now() },
        predX: x / TILE_SIZE,
        predY: y / TILE_SIZE,
        nameTag: null,
        glow: null,
      });
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
        this.createNameTag(spawn.entityId, displayName, isGuest, isAdmin, status);
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
          const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
          tm?.syncStatusFromServer(status);
        }
      }
      console.log(`spawned ${spawn.entityId} at (${x}, ${y})`);
    }

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
          // Recreate the tag because the pillbox width depends on text width.
          avatar.nameTag?.destroy();
          this.createNameTag(upd.entityId, dn.name, dn.isGuest, dn.isAdmin, dn.status);
          // Keep the local player's AvClient DND flag and TopMenu status
          // selector in sync with the server-stamped status (defense-in-depth
          // client-side enforcement + UI reflection without echoing a
          // SetStatusFrame back to the server).
          if (upd.entityId === this.myEntityId) {
            this.avClient?.setStatus(dn.status);
            const tm = this.game.registry.get("topMenu") as TopMenu | undefined;
            tm?.syncStatusFromServer(dn.status);
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
        // For props, show/hide the light glow overlay based on state.
        const es = fromBinary(EntityStateSchema, upd.data);
        avatar.state = es.state;
        if (avatar.isProp) {
          if (es.state === "on") {
            this.showLightGlow(avatar);
          } else {
            this.hideLightGlow(avatar);
          }
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
        avatar.sprite.destroy();
        this.avatars.delete(dest.entityId);
        this.displayNameByEntity.delete(dest.entityId);
        this.isGuestByEntity.delete(dest.entityId);
        this.isAdminByEntity.delete(dest.entityId);
        this.statusByEntity.delete(dest.entityId);
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
  private createNameTag(entityId: string, name: string, isGuest: boolean, isAdmin: boolean, status: number): void {
    const avatar = this.avatars.get(entityId);
    if (!avatar) return;

    // Close any open dropdown for this entity — the old dot handler is gone.
    if (this.openDropdownEntityId === entityId) {
      this.closeDropdown();
    }

    const container = this.add.container(avatar.sprite.x, avatar.sprite.y - 52);

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

    // --- Status pill (color reflects presence status; clickable, opens
    // info dropdown). 0=Available (green), 1=Busy (yellow), 2=DND (red). ---
    const pillColors = [
      { fill: 0x22c55e, stroke: 0x15803d }, // available
      { fill: 0xeab308, stroke: 0xa16207 }, // busy
      { fill: 0xef4444, stroke: 0x991b1b }, // do not disturb
    ];
    const pc = pillColors[status] ?? pillColors[0];
    const pillRadius = 4;
    const statusPill = this.add.circle(0, 0, pillRadius, pc.fill);
    statusPill.setStrokeStyle(1, pc.stroke);
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
    const contentWidth =
      pillRadius * 2 + gap + text.width + adminBadgeWidth + guestBadgeWidth;
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
    }

    const children: Phaser.GameObjects.GameObject[] = [bg, tail, statusPill, text];
    if (adminBadge) children.push(adminBadge);
    if (guestBadge) children.push(guestBadge);

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

    const container = this.add.container(avatar.sprite.x, avatar.sprite.y - 52);
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

    makeButton("Invite");
    if (isAdmin) makeButton("Ban");

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

  // showLightGlow creates (if needed) and shows the light glow overlay
  // above a prop entity. The overlay is a 7x7-tile PNG with a radial
  // gradient, centered on the prop's base.
  private showLightGlow(avatar: Avatar): void {
    if (!this.textures.exists("lightGlow")) return;
    if (!avatar.lightGlow) {
      avatar.lightGlow = this.add.image(avatar.sprite.x, avatar.sprite.y, "lightGlow");
      avatar.lightGlow.setOrigin(0.5, 0.5);
      avatar.lightGlow.setDepth(avatar.sprite.depth - 0.1);
      avatar.lightGlow.setBlendMode(Phaser.BlendModes.ADD);
    }
    avatar.lightGlow.setVisible(true);
    avatar.lightGlow.setPosition(avatar.sprite.x, avatar.sprite.y);
  }

  // hideLightGlow hides the light glow overlay for a prop entity.
  private hideLightGlow(avatar: Avatar): void {
    avatar.lightGlow?.setVisible(false);
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
  // player to choose from. Positioned at the screen center since
  // multiple entities may be involved.
  private openInteractionPopup(actions: AvailableActionView[]): void {
    this.closeInteractionPopup();

    const cam = this.cameras.main;
    const container = this.add.container(cam.worldView.centerX, cam.worldView.centerY);
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
    // Match the openDropdown pattern: world-space position + counter-scale
    // by 1/zoom so the popup stays at a constant screen size regardless of
    // camera zoom. setScrollFactor(0) is intentionally NOT used — with it,
    // cam.worldView.centerX/Y (world coords) would be misread as screen
    // coords, placing the popup off-center and breaking input hit areas.
    container.setScale(1 / cam.zoom);
    container.setDepth(10000);
    this.interactionPopup = container;
  }

  // closeInteractionPopup destroys the current interaction popup if any.
  private closeInteractionPopup(): void {
    this.interactionPopup?.destroy();
    this.interactionPopup = null;
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
}
