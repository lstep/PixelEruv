// DayNightOverlay is a purely cosmetic, client-side full-screen rectangle
// that tints the game world based on the browser's local clock. Color and
// alpha are interpolated between 8 time-of-day keyframes and recalculated
// once per minute. Alpha is capped at 0.44 so the map stays readable.
//
// The overlay is fixed to the screen (scrollFactor 0) and sits below the
// disconnect overlay (depth 9997 vs 9998). It can be toggled on/off via
// setEnabled(); the preference persists in localStorage and defaults to on.
//
// No backend involvement — this is 100% cosmetic.

import Phaser from "phaser";

const STORAGE_KEY = "daynight.enabled";
const MAX_ALPHA = 0.44;
const DEPTH = 9997;
const UPDATE_INTERVAL_MS = 60_000;

interface Keyframe {
  minutes: number; // minutes since midnight (0–1439)
  color: number; // 0xRRGGBB
  alpha: number; // 0–1 (before cap)
}

// 8 keyframes evenly spaced across the day. Hours in 24h local time.
const KEYFRAMES: Keyframe[] = [
  { minutes: 0, color: 0x0a0a2e, alpha: 0.38 }, // 00:00 deep night
  { minutes: 180, color: 0x0a0a2e, alpha: 0.38 }, // 03:00 night
  { minutes: 360, color: 0xff8c42, alpha: 0.20 }, // 06:00 dawn
  { minutes: 540, color: 0xfff4e6, alpha: 0.05 }, // 09:00 morning
  { minutes: 720, color: 0xffffff, alpha: 0.0 }, // 12:00 noon
  { minutes: 900, color: 0xfff4e6, alpha: 0.05 }, // 15:00 afternoon
  { minutes: 1080, color: 0xff6b35, alpha: 0.25 }, // 18:00 dusk
  { minutes: 1260, color: 0x1a1a4e, alpha: 0.35 }, // 21:00 evening
];

export class DayNightOverlay {
  private scene: Phaser.Scene;
  private rect: Phaser.GameObjects.Rectangle;
  private timer: Phaser.Time.TimerEvent;
  private enabled: boolean;

  constructor(scene: Phaser.Scene) {
    this.scene = scene;
    this.enabled = loadEnabled();

    this.rect = scene.add
      .rectangle(0, 0, scene.scale.width, scene.scale.height, 0x000000, 0)
      .setOrigin(0, 0)
      .setScrollFactor(0)
      .setDepth(DEPTH)
      .setVisible(this.enabled);

    this.apply();
    this.timer = scene.time.addEvent({
      delay: UPDATE_INTERVAL_MS,
      loop: true,
      callback: () => this.apply(),
    });
  }

  /** Recalculates color/alpha from the current local time and applies it. */
  private apply(): void {
    if (!this.enabled) return;
    const { color, alpha } = interpolate(new Date());
    this.rect.setFillStyle(color, Math.min(alpha, MAX_ALPHA));
  }

  /** Enables or disables the overlay. Persists the preference. */
  setEnabled(enabled: boolean): void {
    this.enabled = enabled;
    saveEnabled(enabled);
    this.rect.setVisible(enabled);
    if (enabled) this.apply();
  }

  isEnabled(): boolean {
    return this.enabled;
  }

  /** Resizes the rectangle to cover the viewport. Call on window resize. */
  resize(width: number, height: number): void {
    this.rect.setSize(width, height);
  }

  destroy(): void {
    this.timer.remove();
    this.rect.destroy();
  }
}

// --- Interpolation ---

function interpolate(now: Date): { color: number; alpha: number } {
  const minutes = now.getHours() * 60 + now.getMinutes();

  // Find the two surrounding keyframes (wrapping past midnight).
  let prev = KEYFRAMES[KEYFRAMES.length - 1];
  let next = KEYFRAMES[0];
  for (let i = 0; i < KEYFRAMES.length; i++) {
    const kf = KEYFRAMES[i];
    if (kf.minutes <= minutes) {
      prev = kf;
      next = KEYFRAMES[(i + 1) % KEYFRAMES.length];
    } else {
      break;
    }
  }

  // Span between the two keyframes, wrapping past midnight if needed.
  let span = next.minutes - prev.minutes;
  if (span <= 0) span += 24 * 60; // wrap

  // Position within the span, also wrapping.
  let pos = minutes - prev.minutes;
  if (pos < 0) pos += 24 * 60;

  const t = Phaser.Math.Clamp(pos / span, 0, 1);

  const r = Phaser.Math.Linear((prev.color >> 16) & 0xff, (next.color >> 16) & 0xff, t);
  const g = Phaser.Math.Linear((prev.color >> 8) & 0xff, (next.color >> 8) & 0xff, t);
  const b = Phaser.Math.Linear(prev.color & 0xff, next.color & 0xff, t);
  const a = Phaser.Math.Linear(prev.alpha, next.alpha, t);

  const color = (Math.round(r) << 16) | (Math.round(g) << 8) | Math.round(b);
  return { color, alpha: a };
}

// --- Persistence ---

function loadEnabled(): boolean {
  const v = localStorage.getItem(STORAGE_KEY);
  if (v === null) return true; // default: activated
  return v === "true";
}

function saveEnabled(enabled: boolean): void {
  localStorage.setItem(STORAGE_KEY, String(enabled));
}
