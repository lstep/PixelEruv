// VirtualJoystick — a floating on-screen joystick for touch devices.
//
// Touch-drag anywhere in the left portion of the screen (MOVEMENT_ZONE_FRACTION)
// spawns the joystick base at the touch point and a draggable thumb. The thumb
// vector is thresholded with a deadzone into the same up/down/left/right
// booleans the keyboard path uses, so no backend or prediction changes are
// needed — the joystick is just another InputState source.
//
// Uses DOM elements (not Phaser GameObjects) so positioning is always in CSS
// pixels and unaffected by camera zoom or scroll. Only activates on touch
// pointers (pointerType === "touch"), so desktop mouse input is unaffected.

// Fraction of the screen width (from the left edge) that activates the
// joystick. The right side is reserved for UI and future buttons.
const MOVEMENT_ZONE_FRACTION = 0.6;
// Thumb travel radius in pixels. Beyond this the vector is clamped to the rim.
const THUMB_RADIUS = 60;
// Stick deflection below which no direction is asserted. Prevents jitter and
// accidental drift when the thumb rests near the center.
const DEADZONE = 0.35;
const BASE_RADIUS = 70;
const THUMB_RADIUS_VISUAL = 28;

export interface JoystickInput {
  up: boolean;
  down: boolean;
  left: boolean;
  right: boolean;
}

export class VirtualJoystick {
  private onInput: (input: JoystickInput) => void;
  private base: HTMLDivElement;
  private thumb: HTMLDivElement;
  private activePointerId: number | null = null;
  private originX = 0;
  private originY = 0;
  private boundDown: (e: PointerEvent) => void;
  private boundMove: (e: PointerEvent) => void;
  private boundUp: (e: PointerEvent) => void;

  constructor(onInput: (input: JoystickInput) => void) {
    this.onInput = onInput;

    this.base = document.createElement("div");
    this.base.style.cssText =
      `position:fixed;width:${BASE_RADIUS * 2}px;height:${BASE_RADIUS * 2}px;` +
      `margin-left:-${BASE_RADIUS}px;margin-top:-${BASE_RADIUS}px;` +
      `border-radius:50%;background:rgba(255,255,255,0.15);` +
      `border:2px solid rgba(255,255,255,0.4);pointer-events:none;` +
      `z-index:100;display:none;`;

    this.thumb = document.createElement("div");
    this.thumb.style.cssText =
      `position:fixed;width:${THUMB_RADIUS_VISUAL * 2}px;height:${THUMB_RADIUS_VISUAL * 2}px;` +
      `margin-left:-${THUMB_RADIUS_VISUAL}px;margin-top:-${THUMB_RADIUS_VISUAL}px;` +
      `border-radius:50%;background:rgba(255,255,255,0.35);` +
      `border:2px solid rgba(255,255,255,0.6);pointer-events:none;` +
      `z-index:101;display:none;`;

    document.body.appendChild(this.base);
    document.body.appendChild(this.thumb);

    this.boundDown = this.handleDown.bind(this);
    this.boundMove = this.handleMove.bind(this);
    this.boundUp = this.handleUp.bind(this);

    // Listen on window so Phaser's canvas can't intercept/stopPropagation
    // before we see the event. The target check skips DOM UI overlays
    // (TopMenu, ChatPanel, VideoBar) so they keep working normally.
    window.addEventListener("pointerdown", this.boundDown);
    window.addEventListener("pointermove", this.boundMove);
    window.addEventListener("pointerup", this.boundUp);
    window.addEventListener("pointercancel", this.boundUp);
  }

  private inMovementZone(e: PointerEvent): boolean {
    if (e.pointerType !== "touch") return false;
    // Only activate when touching the game canvas, not DOM UI overlays.
    const game = document.getElementById("game");
    if (e.target !== game && !(game?.contains(e.target as Node) ?? false)) return false;
    return e.clientX <= window.innerWidth * MOVEMENT_ZONE_FRACTION;
  }

  private handleDown(e: PointerEvent): void {
    if (!this.inMovementZone(e)) return;
    if (this.activePointerId !== null) return; // already tracking a finger
    this.activePointerId = e.pointerId;
    this.originX = e.clientX;
    this.originY = e.clientY;
    this.showAt(e.clientX, e.clientY);
  }

  private handleMove(e: PointerEvent): void {
    if (e.pointerId !== this.activePointerId) return;
    const dx = e.clientX - this.originX;
    const dy = e.clientY - this.originY;
    const dist = Math.hypot(dx, dy);
    const clamped = Math.min(dist, THUMB_RADIUS);
    const angle = Math.atan2(dy, dx);
    const tx = this.originX + Math.cos(angle) * clamped;
    const ty = this.originY + Math.sin(angle) * clamped;
    this.showThumbAt(tx, ty);

    // Normalized vector in [-1, 1].
    const nx = dist > 0 ? dx / dist : 0;
    const ny = dist > 0 ? dy / dist : 0;

    // Threshold axes independently for 8-directional support, matching the
    // diagonal normalization in GameScene.applyMovement.
    this.onInput({
      left: nx < -DEADZONE,
      right: nx > DEADZONE,
      up: ny < -DEADZONE,
      down: ny > DEADZONE,
    });
  }

  private handleUp(e: PointerEvent): void {
    if (e.pointerId !== this.activePointerId) return;
    this.activePointerId = null;
    this.hide();
    this.onInput({ up: false, down: false, left: false, right: false });
  }

  private showAt(x: number, y: number): void {
    this.base.style.left = `${x}px`;
    this.base.style.top = `${y}px`;
    this.base.style.display = "block";
    this.thumb.style.left = `${x}px`;
    this.thumb.style.top = `${y}px`;
    this.thumb.style.display = "block";
  }

  private showThumbAt(x: number, y: number): void {
    this.thumb.style.left = `${x}px`;
    this.thumb.style.top = `${y}px`;
  }

  private hide(): void {
    this.base.style.display = "none";
    this.thumb.style.display = "none";
  }

  /** Hides visuals and resets active state. Call on blur/visibilitychange. */
  reset(): void {
    this.activePointerId = null;
    this.hide();
  }

  destroy(): void {
    window.removeEventListener("pointerdown", this.boundDown);
    window.removeEventListener("pointermove", this.boundMove);
    window.removeEventListener("pointerup", this.boundUp);
    window.removeEventListener("pointercancel", this.boundUp);
    this.base.remove();
    this.thumb.remove();
  }
}
