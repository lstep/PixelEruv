// VirtualJoystick — a floating on-screen joystick for touch devices.
//
// Touch-drag anywhere in the left portion of the screen (MOVEMENT_ZONE_FRACTION)
// spawns the joystick base at the touch point and a draggable thumb. The thumb
// vector is thresholded with a deadzone into the same up/down/left/right
// booleans the keyboard path uses, so no backend or prediction changes are
// needed — the joystick is just another InputState source.
//
// Only activates on touch pointers (pointer.wasTouch), so desktop mouse input
// is unaffected. Both the base and thumb are screen-fixed (scrollFactor 0) and
// hidden until touched.

import Phaser from "phaser";

// Fraction of the screen width (from the left edge) that activates the
// joystick. The right side is reserved for UI and future buttons.
const MOVEMENT_ZONE_FRACTION = 0.6;
// Thumb travel radius in pixels. Beyond this the vector is clamped to the rim.
const THUMB_RADIUS = 60;
// Stick deflection below which no direction is asserted. Prevents jitter and
// accidental drift when the thumb rests near the center.
const DEADZONE = 0.35;
const BASE_RADIUS = 70;
const DEPTH = 9990;

export interface JoystickInput {
  up: boolean;
  down: boolean;
  left: boolean;
  right: boolean;
}

export class VirtualJoystick {
  private scene: Phaser.Scene;
  private base: Phaser.GameObjects.Arc;
  private thumb: Phaser.GameObjects.Arc;
  private activePointerId: number | null = null;
  private originX = 0;
  private originY = 0;
  private onInput: (input: JoystickInput) => void;

  constructor(scene: Phaser.Scene, onInput: (input: JoystickInput) => void) {
    this.scene = scene;
    this.onInput = onInput;

    this.base = scene.add
      .circle(0, 0, BASE_RADIUS, 0xffffff, 0.15)
      .setStrokeStyle(2, 0xffffff, 0.4)
      .setScrollFactor(0)
      .setDepth(DEPTH)
      .setVisible(false);

    this.thumb = scene.add
      .circle(0, 0, 28, 0xffffff, 0.35)
      .setStrokeStyle(2, 0xffffff, 0.6)
      .setScrollFactor(0)
      .setDepth(DEPTH + 1)
      .setVisible(false);

    scene.input.on("pointerdown", this.handleDown, this);
    scene.input.on("pointermove", this.handleMove, this);
    scene.input.on("pointerup", this.handleUp, this);
    scene.input.on("pointercancel", this.handleUp, this);
  }

  private inMovementZone(pointer: Phaser.Input.Pointer): boolean {
    return pointer.wasTouch && pointer.x <= this.scene.scale.width * MOVEMENT_ZONE_FRACTION;
  }

  private handleDown(pointer: Phaser.Input.Pointer): void {
    if (!this.inMovementZone(pointer)) return;
    if (this.activePointerId !== null) return; // already tracking a finger
    this.activePointerId = pointer.id;
    this.originX = pointer.x;
    this.originY = pointer.y;
    this.base.setPosition(pointer.x, pointer.y).setVisible(true);
    this.thumb.setPosition(pointer.x, pointer.y).setVisible(true);
  }

  private handleMove(pointer: Phaser.Input.Pointer): void {
    if (pointer.id !== this.activePointerId) return;
    const dx = pointer.x - this.originX;
    const dy = pointer.y - this.originY;
    const dist = Math.hypot(dx, dy);
    const clamped = Math.min(dist, THUMB_RADIUS);
    const angle = Math.atan2(dy, dx);
    const tx = this.originX + Math.cos(angle) * clamped;
    const ty = this.originY + Math.sin(angle) * clamped;
    this.thumb.setPosition(tx, ty);

    // Normalized vector in [-1, 1].
    const nx = dist > 0 ? dx / dist : 0;
    const ny = dist > 0 ? dy / dist : 0;
    const mag = Math.min(dist / THUMB_RADIUS, 1);

    // Threshold axes independently for 8-directional support, matching the
    // diagonal normalization in GameScene.applyMovement.
    this.onInput({
      left: nx < -DEADZONE,
      right: nx > DEADZONE,
      up: ny < -DEADZONE,
      down: ny > DEADZONE,
    });
    void mag;
  }

  private handleUp(pointer: Phaser.Input.Pointer): void {
    if (pointer.id !== this.activePointerId) return;
    this.activePointerId = null;
    this.base.setVisible(false);
    this.thumb.setVisible(false);
    this.onInput({ up: false, down: false, left: false, right: false });
  }

  destroy(): void {
    this.scene.input.off("pointerdown", this.handleDown, this);
    this.scene.input.off("pointermove", this.handleMove, this);
    this.scene.input.off("pointerup", this.handleUp, this);
    this.scene.input.off("pointercancel", this.handleUp, this);
    this.base.destroy();
    this.thumb.destroy();
  }
}
