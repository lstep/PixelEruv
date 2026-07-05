import Phaser from "phaser";
import { WsClient, decodePosition, ReplicationBatchView } from "../net/WsClient";
import type { MapAssets } from "../mapLoader";

const TILE_SIZE = 32;

// Time constant for remote-avatar exponential smoothing (ms). At a 20 Hz
// server tick (50 ms), 80 ms lets the sprite catch up to a new target within
// roughly two ticks without visibly lagging.
const LERP_TAU_MS = 80;

// Movement constants — must match worldsim.go movement system.
const SPEED_TILES_PER_TICK = 0.8;
const TICK_MS = 50; // 20 Hz server tick

// Character sprite sheets — one per player, cycled. Each sheet is 768x192
// (24 cols x 6 rows of 32px tiles). Walk cycles are in row 3: down (c0-5),
// left (c6-11), right (c12-17), up (c18-23). Dir field: 0=down, 1=left,
// 2=right, 3=up.
const CHAR_SPRITES = ["char_0", "char_1", "char_2", "char_3", "char_4", "char_5"];
const WALK_ROW = 3;        // row containing walk cycles
const FRAMES_PER_DIR = 6;  // columns per direction
const DIR_NAMES = ["down", "left", "right", "up"] as const;
// Frame start indices in row 3 for each direction. The limezu sheet column
// order is: right (c0-5), up (c6-11), left (c12-17), down (c18-23).
const DIR_FRAME_START = [
  (WALK_ROW * 24) + 18, // down
  (WALK_ROW * 24) + 12, // left
  (WALK_ROW * 24) + 0,  // right
  (WALK_ROW * 24) + 6,  // up
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
  private mapW = 20;
  private mapH = 20;

  constructor() {
    super("GameScene");
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

    // Slide along walls: try X and Y independently. Use +0.5 because the
    // sprite center is at position*TILE_SIZE + TILE_SIZE/2, so the tile the
    // center is in is floor(position + 0.5).
    if (this.isBlocked(Math.floor(newX + 0.5), Math.floor(y + 0.5))) newX = x;
    if (this.isBlocked(Math.floor(newX + 0.5), Math.floor(newY + 0.5))) newY = y;

    return { x: newX, y: newY };
  }

  preload(): void {
    const mapAssets = this.registry.get("mapAssets") as MapAssets | null;
    if (mapAssets) {
      // Load from PocketBase — pass the parsed JSON object directly.
      this.load.tilemapTiledJSON("test-map", mapAssets.tiledJson);
      for (const ts of mapAssets.tilesets) {
        this.load.image(ts.name, ts.url);
      }
    } else {
      // Fallback: static files served by Vite / nginx.
      this.load.tilemapTiledJSON("test-map", "/maps/test-map.json");
      this.load.image("tileset", "/maps/tileset.png");
    }

    // Load character sprite sheets (768x192, 32px frames).
    for (const key of CHAR_SPRITES) {
      this.load.spritesheet(key, `/sprites/${key}.png`, {
        frameWidth: TILE_SIZE,
        frameHeight: TILE_SIZE,
      });
    }
  }

  create(): void {
    // Render the Tiled map
    const map = this.make.tilemap({ key: "test-map" });
    const mapAssets = this.registry.get("mapAssets") as MapAssets | null;
    const tilesetKey = mapAssets ? mapAssets.tilesets[0].name : "tileset";
    const tileset = map.addTilesetImage(tilesetKey, tilesetKey);
    if (tileset) {
      const ground = map.createLayer(0, tileset, 0, 0);
      const walls = map.createLayer(1, tileset, 0, 0);
      if (ground) ground.setDepth(0);
      if (walls) walls.setDepth(1);

      // Build collision grid from the walls layer.
      this.mapW = map.width;
      this.mapH = map.height;
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
    }

    // Camera bounds
    this.cameras.main.setBounds(0, 0, this.mapW * TILE_SIZE, this.mapH * TILE_SIZE);

    // Create walk + idle animations for each character sheet. Walk cycles
    // are in row 3. Idle uses the same frames at a slower rate for a subtle
    // breathing effect.
    for (const key of CHAR_SPRITES) {
      for (let dir = 0; dir < 4; dir++) {
        const start = DIR_FRAME_START[dir];
        const frames = this.anims.generateFrameNumbers(key, { start, end: start + FRAMES_PER_DIR - 1 });
        this.anims.create({
          key: `${key}_walk_${DIR_NAMES[dir]}`,
          frames,
          frameRate: 4,
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

    // Connect to Pusher via WebSocket.
    // In Docker (nginx on 8080): ws://host:8080/ws (proxied to pusher:8081)
    // In dev (vite on 5173): ws://host:8081/ws (direct to pusher)
    const wsUrl = window.location.port === "5173"
      ? `ws://${window.location.hostname}:8081/ws`
      : `ws://${window.location.host}/ws`;
    console.log("connecting to", wsUrl);
    this.ws = new WsClient(wsUrl);
    this.ws.connect(
      () => {
        // Derive our avatar's entity id from the assigned client id
        // (worldsim: entityID = "e_" + clientID[2:]).
        const cid = this.ws?.getClientId();
        if (cid) this.myEntityId = "e_" + cid.slice(2);
        console.log("ready, waiting for replication...");
      },
      (batch: ReplicationBatchView) => this.handleReplication(batch),
    );
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
      const prevX = avatar.sprite.x, prevY = avatar.sprite.y;
      avatar.sprite.x += (avatar.targetX - avatar.sprite.x) * t;
      avatar.sprite.y += (avatar.targetY - avatar.sprite.y) * t;
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
  // movement state.
  private updateAvatarAnim(avatar: Avatar, dir: number, moving: boolean): void {
    avatar.dir = dir;
    avatar.moving = moving;
    const animKey = moving
      ? `${avatar.charKey}_walk_${DIR_NAMES[dir]}`
      : `${avatar.charKey}_idle_${DIR_NAMES[dir]}`;
    avatar.sprite.play(animKey, true);
  }

  private handleReplication(batch: ReplicationBatchView): void {
    // Spawn new entities
    for (const spawn of batch.spawns) {
      if (this.avatars.has(spawn.entityId)) continue;

      let x = 10 * TILE_SIZE;
      let y = 10 * TILE_SIZE;
      for (const comp of spawn.components) {
        if (comp.componentId === 1) {
          // Position component
          const pos = decodePosition(comp.data);
          x = pos.x * TILE_SIZE;
          y = pos.y * TILE_SIZE;
        }
      }

      const charKey = CHAR_SPRITES[this.colorIndex % CHAR_SPRITES.length];
      this.colorIndex++;
      const sprite = this.add.sprite(x + TILE_SIZE / 2, y + TILE_SIZE / 2, charKey, DIR_FRAME_START[0]);
      sprite.setOrigin(0.5, 0.8); // feet near bottom of tile
      this.avatars.set(spawn.entityId, {
        sprite,
        entityId: spawn.entityId,
        charKey,
        dir: 0,
        moving: false,
        targetX: x + TILE_SIZE / 2,
        targetY: y + TILE_SIZE / 2,
        predX: x / TILE_SIZE,
        predY: y / TILE_SIZE,
      });
      // Start idle animation immediately.
      sprite.play(`${charKey}_idle_down`, true);
      console.log(`spawned ${spawn.entityId} at (${x}, ${y})`);
    }

    // Update components
    for (const upd of batch.updates) {
      const avatar = this.avatars.get(upd.entityId);
      if (!avatar) continue;

      if (upd.componentId === 1) {
        // Position
        const pos = decodePosition(upd.data);
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
