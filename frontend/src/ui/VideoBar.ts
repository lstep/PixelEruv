// VideoBar is a fixed-position horizontal bar of participant video tiles,
// positioned below the TopMenu button row at the top of the screen. Tiles
// wrap to additional rows when they overflow the available width.
//
// A draggable handle bar below the tiles resizes the entire bar (all tiles
// scale together). The tile height is persisted in localStorage so the
// user's preferred size survives reloads.
//
// Each frame, tick() polls speaking state from AvClient to toggle a green
// border on the active speaker's tile. Tile order is stable (local player
// first, then others by join order) and is only re-ordered when
// participants join or leave.
//
// The bar is decoupled from Phaser — it takes an AvClient and a getName
// resolver. This makes it easy to re-layout or add ordering controls later.

import type { AvClient, AvParticipant } from "../net/AvClient";
import { VideoTile } from "./VideoTile";

const STORAGE_KEY = "videobar.tileHeight";
const MIN_TILE_HEIGHT = 60;
const DEFAULT_TILE_HEIGHT = 96;
const TILE_ASPECT = 4 / 3; // width = height * 4/3
// TopMenu sits at top:12px and is ~40px tall; place the bar below it.
const BAR_TOP = 60;
const BAR_MARGIN_X = 12;
const HANDLE_HEIGHT = 8;

export interface VideoBarOptions {
  avClient: AvClient;
  getName: (entityId: string) => string;
  getLocalEntityId: () => string | null;
}

export class VideoBar {
  private avClient: AvClient;
  private getName: (entityId: string) => string;
  private getLocalEntityId: () => string | null;

  private container: HTMLDivElement;
  private tilesRow: HTMLDivElement;
  private handle: HTMLDivElement;

  private tiles = new Map<string, VideoTile>();
  // Join order: entityIds in the order they first appeared. Used as the
  // display order (local player first, then join order).
  private joinOrder: string[] = [];

  private tileHeight: number;

  constructor(opts: VideoBarOptions) {
    this.avClient = opts.avClient;
    this.getName = opts.getName;
    this.getLocalEntityId = opts.getLocalEntityId;

    this.tileHeight = this.loadTileHeight();

    // Container — fixed below the TopMenu, full width with side margins.
    this.container = document.createElement("div");
    this.container.style.cssText =
      `position:fixed;top:${BAR_TOP}px;left:${BAR_MARGIN_X}px;right:${BAR_MARGIN_X}px;z-index:18;font-family:sans-serif;pointer-events:none;`;

    // Tiles row — flex with wrap so tiles flow onto additional rows.
    this.tilesRow = document.createElement("div");
    this.tilesRow.style.cssText =
      "display:flex;flex-wrap:wrap;gap:8px;justify-content:center;pointer-events:auto;";
    this.container.appendChild(this.tilesRow);

    // Draggable handle bar below the tiles. Vertical drag resizes all tiles.
    // Small, centered, semi-transparent white so it's unobtrusive.
    this.handle = document.createElement("div");
    this.handle.style.cssText =
      `width:25%;margin:4px auto 0;background:rgba(255,255,255,0.5);height:${HANDLE_HEIGHT}px;border-radius:4px;cursor:ns-resize;pointer-events:auto;touch-action:none;display:none;`;
    this.container.appendChild(this.handle);

    document.body.appendChild(this.container);

    // Wire participant changes.
    this.avClient.setParticipantsHandler((p) => this.syncParticipants(p));

    // Wire the drag handle.
    this.setupHandleDrag();

    // Apply initial tile size to existing tiles (if any).
    this.applyTileSizes();
  }

  // loadTileHeight reads the persisted tile height, clamped to the minimum.
  private loadTileHeight(): number {
    const stored = localStorage.getItem(STORAGE_KEY);
    if (stored) {
      const h = parseInt(stored, 10);
      if (!isNaN(h)) return Math.max(MIN_TILE_HEIGHT, h);
    }
    return DEFAULT_TILE_HEIGHT;
  }

  private saveTileHeight(): void {
    localStorage.setItem(STORAGE_KEY, String(this.tileHeight));
  }

  // setupHandleDrag wires vertical drag on the handle to resize all tiles.
  private setupHandleDrag(): void {
    let dragging = false;
    let startY = 0;
    let startH = 0;

    const onDown = (e: PointerEvent) => {
      dragging = true;
      startY = e.clientY;
      startH = this.tileHeight;
      this.handle.setPointerCapture(e.pointerId);
      e.preventDefault();
    };
    const onMove = (e: PointerEvent) => {
      if (!dragging) return;
      const dy = e.clientY - startY;
      // Drag down = larger, drag up = smaller.
      this.tileHeight = Math.max(MIN_TILE_HEIGHT, startH + dy);
      this.applyTileSizes();
      this.saveTileHeight();
    };
    const onUp = (e: PointerEvent) => {
      if (!dragging) return;
      dragging = false;
      try { this.handle.releasePointerCapture(e.pointerId); } catch { /* noop */ }
    };

    this.handle.addEventListener("pointerdown", onDown);
    this.handle.addEventListener("pointermove", onMove);
    this.handle.addEventListener("pointerup", onUp);
    this.handle.addEventListener("pointercancel", onUp);
  }

  // maxTileHeight returns the largest tile height that keeps the entire bar
  // (tiles + handle) within the screen. Uses binary search because the number
  // of wrap rows changes with tile size, making the relationship non-linear.
  private maxTileHeight(): number {
    const n = this.tiles.size;
    if (n === 0) return DEFAULT_TILE_HEIGHT;
    const availWidth = window.innerWidth - 2 * BAR_MARGIN_X;
    const availHeight = window.innerHeight - BAR_TOP - HANDLE_HEIGHT - 4 - 12;
    let lo = MIN_TILE_HEIGHT;
    let hi = 500;
    while (lo < hi) {
      const mid = Math.ceil((lo + hi + 1) / 2);
      const tileW = mid * TILE_ASPECT;
      const perRow = Math.max(1, Math.floor((availWidth + 8) / (tileW + 8)));
      const rows = Math.ceil(n / perRow);
      const totalH = rows * mid + (rows - 1) * 8;
      if (totalH <= availHeight) lo = mid;
      else hi = mid - 1;
    }
    return lo;
  }

  // applyTileSizes sets every tile to the current tileHeight (width derived
  // from the 4:3 aspect ratio). Clamps to maxTileHeight so the bar never
  // extends past the screen bottom.
  private applyTileSizes(): void {
    const max = this.maxTileHeight();
    if (this.tileHeight > max) this.tileHeight = max;
    const w = Math.round(this.tileHeight * TILE_ASPECT);
    const h = Math.round(this.tileHeight);
    for (const tile of this.tiles.values()) {
      tile.setSize(w, h);
    }
  }

  // syncParticipants creates/removes tiles to match the current participant
  // set. Called when AvClient reports a change (join/leave/track toggle).
  private syncParticipants(participants: Map<string, AvParticipant>): void {
    // Remove tiles for departed participants.
    for (const [entityId, tile] of this.tiles) {
      if (!participants.has(entityId)) {
        tile.destroy();
        this.tiles.delete(entityId);
        this.joinOrder = this.joinOrder.filter((id) => id !== entityId);
      }
    }
    // Add tiles for new participants that have a camera track.
    for (const [entityId, p] of participants) {
      let tile = this.tiles.get(entityId);
      if (!tile && p.cameraTrack) {
        tile = new VideoTile(entityId, p.isLocal);
        tile.setName(this.getName(entityId));
        this.tiles.set(entityId, tile);
        this.joinOrder.push(entityId);
        // Set size BEFORE appending so the video element has dimensions
        // immediately (LiveKit's adaptiveStream pauses tracks with 0-size
        // elements).
        const w = Math.round(this.tileHeight * TILE_ASPECT);
        tile.setSize(w, Math.round(this.tileHeight));
        // Append to DOM BEFORE attaching the track — LiveKit's attach()
        // sets srcObject on the <video> element, and Safari won't start
        // playback if the element isn't in the document.
        this.tilesRow.appendChild(tile.element);
        tile.attachTrack(p.cameraTrack);
      } else if (tile && !p.cameraTrack) {
        // Camera turned off — remove the tile.
        tile.destroy();
        this.tiles.delete(entityId);
        this.joinOrder = this.joinOrder.filter((id) => id !== entityId);
      }
      // Names are refreshed each frame in tick(), no need to set here.
    }
    this.applyTileSizes();
    this.orderTiles();
    this.handle.style.display = this.tiles.size > 0 ? "" : "none";
  }

  // tick is called each frame by the GameScene. It polls speaking state to
  // toggle the green border on the active speaker's tile, and refreshes
  // names so DisplayName changes propagate immediately.
  tick(): void {
    if (this.tiles.size === 0) return;

    for (const entityId of this.tiles.keys()) {
      const tile = this.tiles.get(entityId)!;
      tile.setSpeaking(this.avClient.isSpeaking(entityId));
      tile.setName(this.getName(entityId));
    }
  }

  // orderTiles arranges tiles in stable order: local player first, then
  // others by join order. Only re-appends when the DOM order differs, to
  // avoid unnecessary mutations. Called from syncParticipants on
  // join/leave/track-toggle — not every frame.
  private orderTiles(): void {
    const localId = this.getLocalEntityId();
    const sorted = this.joinOrder.slice().sort((a, b) => {
      if (a === localId) return -1;
      if (b === localId) return 1;
      return 0;
    });
    const current = Array.from(this.tilesRow.children) as HTMLElement[];
    const currentIds = current.map((el) => el.dataset.entityId).filter(Boolean) as string[];
    if (currentIds.length === sorted.length && currentIds.every((id, i) => id === sorted[i])) {
      return; // order unchanged — no DOM work
    }
    for (const entityId of sorted) {
      const tile = this.tiles.get(entityId);
      if (tile) this.tilesRow.appendChild(tile.element);
    }
  }

  destroy(): void {
    for (const tile of this.tiles.values()) {
      tile.destroy();
    }
    this.tiles.clear();
    this.joinOrder = [];
    this.container.remove();
  }
}
