import Phaser from "phaser";
import { WsClient, decodePosition, ReplicationBatchView } from "../net/WsClient";

const TILE_SIZE = 32;
const MAP_W = 20;
const MAP_H = 20;

// Avatar colors — cycle through for different players
const AVATAR_COLORS = [0xe74c3c, 0x3498db, 0x2ecc71, 0xf39c12, 0x9b59b6, 0x1abc9c];

interface Avatar {
  sprite: Phaser.GameObjects.Arc;
  entityId: string;
}

export class GameScene extends Phaser.Scene {
  private ws: WsClient | null = null;
  private avatars: Map<string, Avatar> = new Map();
  private myEntityId: string | null = null;
  private inputState = { up: false, down: false, left: false, right: false, run: false };
  private inputDirty = false;
  private colorIndex = 0;

  constructor() {
    super("GameScene");
  }

  preload(): void {
    this.load.tilemapTiledJSON("test-map", "/maps/test-map.json");
    this.load.image("tileset", "/maps/tileset.png");
  }

  create(): void {
    // Render the Tiled map
    const map = this.make.tilemap({ key: "test-map" });
    const tileset = map.addTilesetImage("tileset", "tileset");
    if (tileset) {
      const ground = map.createLayer(0, tileset, 0, 0);
      const walls = map.createLayer(1, tileset, 0, 0);
      if (ground) ground.setDepth(0);
      if (walls) walls.setDepth(1);
    }

    // Camera bounds
    this.cameras.main.setBounds(0, 0, MAP_W * TILE_SIZE, MAP_H * TILE_SIZE);

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
        console.log("ready, waiting for replication...");
      },
      (batch: ReplicationBatchView) => this.handleReplication(batch),
    );
  }

  update(): void {
    // Send input on change
    if (this.inputDirty && this.ws) {
      this.ws.sendInput(this.inputState);
      this.inputDirty = false;
    }
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

      const color = AVATAR_COLORS[this.colorIndex % AVATAR_COLORS.length];
      this.colorIndex++;
      const sprite = this.add.circle(x + TILE_SIZE / 2, y + TILE_SIZE / 2, 12, color, 1);
      sprite.setStrokeStyle(2, 0x000000, 0.5);
      this.avatars.set(spawn.entityId, { sprite, entityId: spawn.entityId });
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
        avatar.sprite.x = px;
        avatar.sprite.y = py;
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
