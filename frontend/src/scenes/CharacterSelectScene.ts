// CharacterSelectScene — pre-join character sheet picker.
//
// Shown for logged-in users who haven't picked a sprite yet (determined by a
// localStorage flag). Displays the PB-backed sprite catalog as clickable
// thumbnails; on confirm, stashes the chosen ID in the game registry and
// transitions to GameScene, which sends the SetSpriteBaseFrame after
// connecting. Guests skip this scene entirely.

import Phaser from "phaser";
import type { SpriteBaseAsset } from "../spriteLoader";
import { isLoggedIn } from "../auth";

const FRAME_W = 32;
const FRAME_H = 64;
const TILE_SIZE = 32;
const WALK_ROW = 2;
const COLS_PER_ROW = 24;
const FRAMES_PER_DIR = 6;
const DIR_NAMES = ["down", "left", "right", "up"] as const;
const DIR_FRAME_START = [
  (WALK_ROW * COLS_PER_ROW) + 18, // down
  (WALK_ROW * COLS_PER_ROW) + 12, // left
  (WALK_ROW * COLS_PER_ROW) + 0,  // right
  (WALK_ROW * COLS_PER_ROW) + 6,  // up
];

const STORAGE_KEY = "sprite_chosen";

// shouldShow returns true if the pre-join chooser should be shown: only for
// logged-in users who haven't picked a sprite yet (localStorage flag).
export function shouldShowCharacterSelect(): boolean {
  return isLoggedIn() && !localStorage.getItem(STORAGE_KEY);
}

export class CharacterSelectScene extends Phaser.Scene {
  private selected: string = "";
  private confirmBtn: Phaser.GameObjects.Text | null = null;
  private thumbnails: { id: string; sprite: Phaser.GameObjects.Sprite; border: Phaser.GameObjects.Rectangle }[] = [];

  constructor() {
    super("CharacterSelect");
  }

  preload(): void {
    const spriteBases = this.registry.get("spriteBases") as SpriteBaseAsset[] | null;
    if (spriteBases) {
      for (const base of spriteBases) {
        this.load.spritesheet(base.id, base.url, {
          frameWidth: FRAME_W,
          frameHeight: FRAME_H,
        });
      }
    }
  }

  create(): void {
    // Skip the chooser for guests or returning users who already picked.
    if (!shouldShowCharacterSelect()) {
      this.scene.start("GameScene");
      return;
    }

    const spriteBases = this.registry.get("spriteBases") as SpriteBaseAsset[] | null;
    if (!spriteBases || spriteBases.length === 0) {
      // No catalog available — skip straight to the game.
      this.scene.start("GameScene");
      return;
    }

    this.cameras.main.setBackgroundColor("#1a1a2e");

    const title = this.add.text(this.scale.width / 2, 60, "Choose your character", {
      fontSize: "24px",
      color: "#fff",
      fontFamily: "sans-serif",
    }).setOrigin(0.5);

    // Layout: row of thumbnails centered horizontally. Each thumbnail shows
    // the down-facing walk frame. Clicking selects it (highlights border).
    const thumbW = 64;
    const thumbH = 96;
    const gap = 16;
    const totalW = spriteBases.length * (thumbW + gap) - gap;
    const startX = (this.scale.width - totalW) / 2 + thumbW / 2;
    const y = this.scale.height / 2;

    for (let i = 0; i < spriteBases.length; i++) {
      const base = spriteBases[i];
      const x = startX + i * (thumbW + gap);

      const border = this.add.rectangle(x, y, thumbW + 8, thumbH + 8, 0x4c5cf0, 0)
        .setStrokeStyle(0, 0x4c5cf0);

      const sprite = this.add.sprite(x, y, base.id, DIR_FRAME_START[0]);
      sprite.setOrigin(0.5, 0.75);
      // Scale up the 32px-wide sprite for easier clicking.
      const scale = 2;
      sprite.setScale(scale);
      sprite.setInteractive({ useHandCursor: true });

      sprite.on("pointerdown", () => {
        this.select(base.id);
      });

      this.thumbnails.push({ id: base.id, sprite, border });
    }

    // Confirm button (disabled until a selection is made).
    this.confirmBtn = this.add.text(this.scale.width / 2, this.scale.height - 80, "Confirm", {
      fontSize: "20px",
      color: "#666",
      fontFamily: "sans-serif",
      backgroundColor: "#2d2d3a",
      padding: { x: 24, y: 12 },
    }).setOrigin(0.5);

    this.confirmBtn.setInteractive({ useHandCursor: true });
    this.confirmBtn.on("pointerdown", () => {
      if (this.selected) {
        this.registry.set("pendingSpriteBase", this.selected);
        localStorage.setItem(STORAGE_KEY, "1");
        this.scene.start("GameScene");
      }
    });

    // Skip button (for users who want to keep the fallback).
    this.add.text(this.scale.width / 2, this.scale.height - 30, "Skip", {
      fontSize: "14px",
      color: "#888",
      fontFamily: "sans-serif",
    })
      .setOrigin(0.5)
      .setInteractive({ useHandCursor: true })
      .on("pointerdown", () => {
        localStorage.setItem(STORAGE_KEY, "1");
        this.scene.start("GameScene");
      });
  }

  private select(id: string): void {
    this.selected = id;
    for (const t of this.thumbnails) {
      if (t.id === id) {
        t.border.setStrokeStyle(3, 0x4c5cf0);
      } else {
        t.border.setStrokeStyle(0, 0x4c5cf0);
      }
    }
    if (this.confirmBtn) {
      this.confirmBtn.setStyle({ color: "#fff", backgroundColor: "#4c5cf0" });
    }
  }
}
