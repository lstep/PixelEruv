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
const STORAGE_KEY_KEYFRAMES = "daynight.keyframes";
const MAX_ALPHA = 0.44;
const DEPTH = 9997;
const UPDATE_INTERVAL_MS = 60_000;

export interface Keyframe {
  minutes: number; // minutes since midnight (0–1439)
  color: number; // 0xRRGGBB
  alpha: number; // 0–1 (before cap)
}

// 8 default keyframes evenly spaced across the day. Hours in 24h local time.
export const DEFAULT_KEYFRAMES: Keyframe[] = [
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
  private keyframes: Keyframe[];

  constructor(scene: Phaser.Scene) {
    this.scene = scene;
    this.enabled = loadEnabled() ?? true; // default: activated
    this.keyframes = loadKeyframes();

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
    const { color, alpha } = interpolate(new Date(), this.keyframes);
    this.rect.setFillStyle(color, Math.min(alpha, MAX_ALPHA));
  }

  /** Enables or disables the overlay. Persists the preference. */
  setEnabled(enabled: boolean): void {
    this.enabled = enabled;
    saveEnabled(enabled);
    this.rect.setVisible(enabled);
    if (enabled) this.apply();
  }

  /**
   * Applies a map-level default for day/night. Only takes effect if the
   * player has no explicit localStorage preference — the player can always
   * override by toggling via setEnabled(). Called when map options arrive
   * (auth, map transition, hot-reload).
   */
  applyDefault(defaultEnabled: boolean): void {
    const stored = loadEnabled();
    if (stored !== null) return; // player has an explicit preference
    this.enabled = defaultEnabled;
    this.rect.setVisible(defaultEnabled);
    if (defaultEnabled) this.apply();
  }

  isEnabled(): boolean {
    return this.enabled;
  }

  /** Returns the current keyframes (a copy). */
  getKeyframes(): Keyframe[] {
    return this.keyframes.map((kf) => ({ ...kf }));
  }

  /**
   * Replaces the keyframes. Must be sorted ascending by minutes, with at
   * least 2 entries covering the day. Persists to localStorage and
   * re-applies immediately. Pass null/undefined to restore defaults.
   */
  setKeyframes(keyframes: Keyframe[] | null): void {
    this.keyframes = keyframes && keyframes.length >= 2 ? keyframes : DEFAULT_KEYFRAMES;
    saveKeyframes(this.keyframes);
    if (this.enabled) this.apply();
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

function interpolate(now: Date, keyframes: Keyframe[]): { color: number; alpha: number } {
  const minutes = now.getHours() * 60 + now.getMinutes();

  // Find the two surrounding keyframes (wrapping past midnight).
  let prev = keyframes[keyframes.length - 1];
  let next = keyframes[0];
  for (let i = 0; i < keyframes.length; i++) {
    const kf = keyframes[i];
    if (kf.minutes <= minutes) {
      prev = kf;
      next = keyframes[(i + 1) % keyframes.length];
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

function loadEnabled(): boolean | null {
  const v = localStorage.getItem(STORAGE_KEY);
  if (v === null) return null; // no explicit preference
  return v === "true";
}

function saveEnabled(enabled: boolean): void {
  localStorage.setItem(STORAGE_KEY, String(enabled));
}

function loadKeyframes(): Keyframe[] {
  const raw = localStorage.getItem(STORAGE_KEY_KEYFRAMES);
  if (!raw) return DEFAULT_KEYFRAMES;
  try {
    const parsed = JSON.parse(raw) as Keyframe[];
    if (!Array.isArray(parsed) || parsed.length < 2) return DEFAULT_KEYFRAMES;
    return parsed;
  } catch {
    return DEFAULT_KEYFRAMES;
  }
}

function saveKeyframes(keyframes: Keyframe[]): void {
  localStorage.setItem(STORAGE_KEY_KEYFRAMES, JSON.stringify(keyframes));
}
