import Phaser from "phaser";
import { fromBinary } from "@bufbuild/protobuf";
import { AppearanceSchema } from "../proto/components_pb";
import { WsClient, decodePosition, ReplicationBatchView, ConnectionState } from "../net/WsClient";
import type { MapAssets } from "../mapLoader";

const TILE_SIZE = 32;

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

// Time constant for remote-avatar exponential smoothing (ms). At a 20 Hz
// server tick (50 ms), 80 ms lets the sprite catch up to a new target within
// roughly two ticks without visibly lagging.
const LERP_TAU_MS = 80;

// Movement constants — must match worldsim.go movement system.
const SPEED_TILES_PER_TICK = 0.4;
const TICK_MS = 50; // 20 Hz server tick
// Vertical offset from Position.Y to the avatar's feet in tile coords.
// Avatars render with origin (0.5, 0.75) on a 64px frame placed at
// (pos*32+16, pos*32+16), so feet sit at Position.Y + 1.0. Collision is
// evaluated at the feet — must match worldsim.go avatarFeetYOffset.
const FEET_Y_OFFSET = 1.0;

// Player collision radius in tiles — must match worldsim.go
// playerCollisionRadius. Zone shapes are expanded by this radius before
// the swept segment test so the feet center stops `r` tiles before the
// wall edge.
const PLAYER_COLLISION_RADIUS = 0.3;

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
// char_5 is excluded: its sheet is malformed (walk cycle only has right/up
// directions; down/left frames are empty), so it renders as an empty sprite.
const CHAR_SPRITES = ["char_0", "char_1", "char_2", "char_3", "char_4"];
const WALK_ROW = 2;        // frame-row used for the movement animation (run)
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
  // Remote avatars: pixel-space lerp target (see LERP_TAU_MS).
  targetX: number;
  targetY: number;
  // Local avatar only: predicted position in tile coordinates.
  predX: number;
  predY: number;
}

// Apply `ticks` worth of movement from (x, y) under `state`, matching the
// server's movement math (worldsim.go: speed, diagonal normalize, collision).
// Uses the collision grid for wall blocking and map bounds.

export class GameScene extends Phaser.Scene {
  private ws: WsClient | null = null;
  private avatars: Map<string, Avatar> = new Map();
  private myEntityId: string | null = null;
  private inputState: InputState = { up: false, down: false, left: false, right: false, run: false };
  private inputDirty = false;
  private colorIndex = 0;
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
  // "Reconnecting…" overlay shown when the WebSocket drops and WsClient is
  // retrying. Fixed to the screen (scrollFactor 0) so it stays visible.
  private reconnectOverlay: Phaser.GameObjects.Text | null = null;

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
    const mapAssets = this.registry.get("mapAssets") as MapAssets | null;
    if (mapAssets) {
      // Load from PocketBase — pass the parsed JSON object directly.
      this.load.tilemapTiledJSON("test-map", mapAssets.tiledJson);
      for (const ts of mapAssets.tilesets) {
        this.load.image(ts.name, ts.url);
        // Also load as a spritesheet so individual tiles can be drawn as
        // standalone sprites for object-layer decorations (see create()).
        // Assumes a single, unspaced tileset grid matching the map's tile
        // size — the same assumption the rest of the map pipeline makes.
        this.load.spritesheet(`${ts.name}__tiles`, ts.url, { frameWidth: TILE_SIZE, frameHeight: TILE_SIZE });
      }
    } else {
      // Fallback: static files served by Vite / nginx.
      this.load.tilemapTiledJSON("test-map", "/maps/test-map.json");
      this.load.json("test-map-raw", "/maps/test-map.json");
      this.load.image("tileset", "/maps/tileset.png");
      this.load.spritesheet("tileset__tiles", "/maps/tileset.png", { frameWidth: TILE_SIZE, frameHeight: TILE_SIZE });
    }

    // Load character sprite sheets (768x192). Frames are 32x64 so each frame
    // captures the full ~48px-tall character (see FRAME_H comment above).
    for (const key of CHAR_SPRITES) {
      this.load.spritesheet(key, `/sprites/${key}.png`, {
        frameWidth: FRAME_W,
        frameHeight: FRAME_H,
      });
    }
  }

  create(): void {
    // Render the Tiled map
    const map = this.make.tilemap({ key: "test-map" });
    const mapAssets = this.registry.get("mapAssets") as MapAssets | null;
    const rawJson = (mapAssets?.tiledJson ?? this.cache.json.get("test-map-raw")) as TiledMapJSON;
    this.mapW = map.width;
    this.mapH = map.height;
    this.tilesets = rawJson?.tilesets ?? [];

    // Add ALL tilesets so layers using any tileset render correctly. A map
    // can have multiple tilesets with different firstgids; createLayer must
    // be passed the tileset(s) that contain the layer's tile GIDs.
    const allTilesets = (mapAssets ? mapAssets.tilesets : [{ name: "tileset" }]).map(
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
    this.input.on("wheel", (_pointer: Phaser.Input.Pointer, _gameObjects: Phaser.GameObjects.GameObject[], _deltaX: number, deltaY: number) => {
      const z = Phaser.Math.Clamp(
        this.cameras.main.zoom - deltaY * ZOOM_SENSITIVITY,
        ZOOM_MIN,
        ZOOM_MAX,
      );
      this.cameras.main.setZoom(z);
    });

    // Create movement + idle animations for each character sheet. The
    // movement animation uses WALK_ROW (the run cycle, frame-row 2). Idle
    // reuses the same frames at a slower rate for a subtle breathing effect.
    for (const key of CHAR_SPRITES) {
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

    // Connect to Pusher via WebSocket.
    // In Docker (nginx on 8080): ws://host:8080/ws (proxied to pusher:8081)
    // In dev (vite on 5173): ws://host:8081/ws (direct to pusher)
    const wsUrl = window.location.port === "5173"
      ? `ws://${window.location.hostname}:8081/ws`
      : `ws://${window.location.host}/ws`;
    console.log("connecting to", wsUrl);
    this.ws = new WsClient(wsUrl);
    // "Reconnecting…" overlay — created up front, hidden until the WS drops.
    this.reconnectOverlay = this.add
      .text(this.scale.width / 2, 24, "Reconnecting…", {
        fontFamily: "monospace",
        fontSize: "16px",
        color: "#ffffff",
        backgroundColor: "#000000",
        padding: { x: 12, y: 6 },
      })
      .setOrigin(0.5, 0)
      .setScrollFactor(0)
      .setDepth(9999)
      .setVisible(false);
    this.ws.connect({
      onReady: () => {
        // Use the entity ID from the server's AuthResult. The server may
        // assign a different entity ID than "e_"+clientId[2:] when a
        // PocketBase-stored identity exists (persistent entity_id).
        this.myEntityId = this.ws?.getEntityId() ?? null;
        console.log("ready, myEntityId=", this.myEntityId);
      },
      onReplication: (batch: ReplicationBatchView) => this.handleReplication(batch),
      onReconnect: (_clientId: string, entityId: string) => {
        // The pusher mints a fresh session on reconnect, so the entity id
        // changes and worldsim spawns the avatar at the spawn point. Drop the
        // old self sprite (a Destroy will also arrive via replication once
        // worldsim processes client.disconnected, but removing it now avoids
        // a lingering ghost) and clear un-acked inputs that referenced the
        // old entity. Re-mark input dirty so the current input state is
        // re-sent on the new connection and movement resumes seamlessly.
        const old = this.myEntityId ? this.avatars.get(this.myEntityId) : null;
        if (old) {
          old.sprite.destroy();
          this.avatars.delete(this.myEntityId);
        }
        this.myEntityId = entityId || null;
        this.pendingInputs = [];
        this.inputDirty = true;
        console.log("reconnected, myEntityId=", this.myEntityId);
      },
      onStateChange: (state: ConnectionState) => {
        if (!this.reconnectOverlay) return;
        this.reconnectOverlay.setVisible(state === "reconnecting" || state === "closed");
      },
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
      const ticks = delta / TICK_MS;
      const p = this.applyMovement(local.predX, local.predY, this.inputState, ticks);
      local.predX = p.x;
      local.predY = p.y;
      local.sprite.x = local.predX * TILE_SIZE + TILE_SIZE / 2;
      local.sprite.y = local.predY * TILE_SIZE + TILE_SIZE / 2;
      local.sprite.setDepth(this.dynamicDepth(this.feetY(local.sprite)));

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

    // Exponential smoothing toward the latest replicated position for remote
    // avatars. Frame-rate independent: t = 1 - exp(-delta/tau).
    const t = 1 - Math.exp(-delta / LERP_TAU_MS);
    for (const avatar of this.avatars.values()) {
      if (avatar.entityId === this.myEntityId) continue;
      if (avatar.isProp) continue; // props are static — no lerp or animation
      const prevX = avatar.sprite.x, prevY = avatar.sprite.y;
      avatar.sprite.x += (avatar.targetX - avatar.sprite.x) * t;
      avatar.sprite.y += (avatar.targetY - avatar.sprite.y) * t;
      avatar.sprite.setDepth(this.dynamicDepth(this.feetY(avatar.sprite)));
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

  private handleReplication(batch: ReplicationBatchView): void {
    // Spawn new entities
    for (const spawn of batch.spawns) {
      if (this.avatars.has(spawn.entityId)) continue;

      let x = 10 * TILE_SIZE;
      let y = 10 * TILE_SIZE;
      let gid = 0;
      for (const comp of spawn.components) {
        if (comp.componentId === 1) {
          // Position component
          const pos = decodePosition(comp.data);
          x = pos.x * TILE_SIZE;
          y = pos.y * TILE_SIZE;
        } else if (comp.componentId === 3) {
          // Appearance component — Tiled gid for tile-sprite rendering
          const appearance = fromBinary(AppearanceSchema, comp.data);
          gid = appearance.gid;
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
            targetX: x,
            targetY: y,
            predX: 0,
            predY: 0,
          });
          console.log(`spawned prop ${spawn.entityId} at (${x}, ${y}) gid=${gid}`);
          continue;
        }
      }

      const charKey = CHAR_SPRITES[this.colorIndex % CHAR_SPRITES.length];
      this.colorIndex++;
      const sprite = this.add.sprite(x + TILE_SIZE / 2, y + TILE_SIZE / 2, charKey, DIR_FRAME_START[0]);
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
        targetX: x + TILE_SIZE / 2,
        targetY: y + TILE_SIZE / 2,
        predX: x / TILE_SIZE,
        predY: y / TILE_SIZE,
      });
      // Start idle animation immediately.
      sprite.play(`${charKey}_idle_down`, true);
      // Camera follows the local player. roundPixels keeps pixel-art
      // crisp; lerp 1 = hard snap each frame (no sub-pixel smear).
      if (spawn.entityId === this.myEntityId) {
        this.cameras.main.startFollow(sprite, true, 1, 1);
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
        const px = pos.x * TILE_SIZE + TILE_SIZE / 2;
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
          avatar.sprite.x = x * TILE_SIZE + TILE_SIZE / 2;
          avatar.sprite.y = y * TILE_SIZE + TILE_SIZE / 2;
        } else {
          // Remote avatar: store pixel-space lerp target.
          avatar.targetX = px;
          avatar.targetY = py;
        }
      }
    }

    // Destroy entities
    for (const dest of batch.destroys) {
      const avatar = this.avatars.get(dest.entityId);
      if (avatar) {
        avatar.sprite.destroy();
        this.avatars.delete(dest.entityId);
        console.log(`destroyed ${dest.entityId}`);
      }
    }
  }
}
