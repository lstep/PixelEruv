// AfkDetector monitors user activity and tab visibility to drive the AFK
// overlay state. It uses a two-tier timer:
//
//   - Tab hidden / window blurred → AFK after AFK_TAB_HIDDEN_MS (2 min).
//   - Tab visible but input idle   → AFK after AFK_INPUT_IDLE_MS (10 min).
//
// A "meeting exemption" skips AFK activation while the player is in an A/V
// room with at least one other participant (isInMeeting callback), so a user
// in a long video meeting who doesn't move the mouse is not marked AFK.
//
// The tab-hidden state (document.hidden OR window blurred) is tracked
// internally with a debounce (TAB_VISIBILITY_DEBOUNCE_MS, 3s) in both
// directions and drives the shorter AFK_TAB_HIDDEN_MS idle timeout. It is no
// longer reported to the AvClient — A/V tracks keep broadcasting while the
// tab is hidden/unfocused.
//
// On an AFK transition (false→true or true→false), the detector calls
// onAfkChange and sends a SetAfkFrame via wsClient.setAfk. The server is
// authoritative for replication; the client is responsible for re-evaluating
// and re-sending on reconnect (self-healing).
//
// See documentation/plans/2026-07-22-afk-state-design.md.

import type { WsClient } from "./WsClient";

const AFK_TAB_HIDDEN_MS = 2 * 60 * 1000; // 2 min
const AFK_INPUT_IDLE_MS = 10 * 60 * 1000; // 10 min
const TAB_VISIBILITY_DEBOUNCE_MS = 3 * 1000; // 3s both directions
const CHECK_INTERVAL_MS = 5 * 1000; // 5s

export interface AfkDetectorCallbacks {
  onAfkChange: (afk: boolean) => void;
  isInMeeting: () => boolean;
}

export class AfkDetector {
  private wsClient: WsClient;
  private callbacks: AfkDetectorCallbacks;
  private lastActivityMs = Date.now();
  private tabHidden = false; // document.hidden OR window blurred
  private afk = false;
  private checkTimer: ReturnType<typeof setInterval> | null = null;
  // Debounce timer for tab-visibility transitions. A single timer serves
  // both directions: starting it for a new transition cancels any pending
  // transition in the opposite direction.
  private tabDebounceTimer: ReturnType<typeof setTimeout> | null = null;
  // The pending tab-hidden state that will be reported after the debounce.
  // If the tab returns before the timer fires, this is overwritten and the
  // timer restarts for the opposite direction.
  private pendingTabHidden: boolean | null = null;

  // Bound handlers (so removeEventListener works in destroy()).
  private boundActivity: () => void;
  private boundVisibilityChange: () => void;
  private boundBlur: () => void;
  private boundFocus: () => void;

  constructor(wsClient: WsClient, callbacks: AfkDetectorCallbacks) {
    this.wsClient = wsClient;
    this.callbacks = callbacks;

    // Throttle activity updates — just stamp the time. Using a no-op throttle
    // since Date.now() is cheap and we only read it on the check interval.
    this.boundActivity = () => {
      this.lastActivityMs = Date.now();
      // If currently AFK, clear it immediately on any activity (don't wait
      // for the next check tick — faster feedback).
      if (this.afk) {
        this.clearAfk();
      }
    };
    this.boundVisibilityChange = () => this.onVisibilityChange();
    this.boundBlur = () => this.onVisibilityChange();
    this.boundFocus = () => this.onVisibilityChange();

    window.addEventListener("mousemove", this.boundActivity, { passive: true });
    window.addEventListener("mousedown", this.boundActivity, { passive: true });
    window.addEventListener("keydown", this.boundActivity, { passive: true });
    window.addEventListener("wheel", this.boundActivity, { passive: true });
    window.addEventListener("touchstart", this.boundActivity, { passive: true });
    document.addEventListener("visibilitychange", this.boundVisibilityChange);
    window.addEventListener("blur", this.boundBlur);
    window.addEventListener("focus", this.boundFocus);

    // Initialize tab-hidden state from current visibility (no debounce on
    // init — used internally for the AFK idle timeout).
    this.tabHidden = this.computeTabHidden();

    this.checkTimer = setInterval(() => this.check(), CHECK_INTERVAL_MS);
  }

  private computeTabHidden(): boolean {
    return document.hidden || !document.hasFocus();
  }

  private onVisibilityChange(): void {
    const hidden = this.computeTabHidden();
    if (hidden === this.tabHidden) return; // no change
    // Debounce: start (or restart) a timer for the new state. If the tab
    // returns before the timer fires, the next onVisibilityChange call
    // restarts it for the opposite direction, canceling this pending one.
    this.pendingTabHidden = hidden;
    if (this.tabDebounceTimer) clearTimeout(this.tabDebounceTimer);
    this.tabDebounceTimer = setTimeout(() => {
      this.tabDebounceTimer = null;
      if (this.pendingTabHidden === null) return;
      const next = this.pendingTabHidden;
      this.pendingTabHidden = null;
      if (next !== this.tabHidden) {
        this.tabHidden = next;
      }
    }, TAB_VISIBILITY_DEBOUNCE_MS);
  }

  private check(): void {
    // Meeting exemption: never go AFK while in a room with other participants.
    // Prevents activation only — does NOT clear existing AFK, so a proximity/
    // zone A/V room forming around an already-AFK player (someone walks up)
    // does not wake them. Existing AFK clears only on genuine user input.
    if (this.callbacks.isInMeeting()) {
      return;
    }
    const idleMs = Date.now() - this.lastActivityMs;
    const shouldbeAfk =
      (this.tabHidden && idleMs >= AFK_TAB_HIDDEN_MS) ||
      (!this.tabHidden && idleMs >= AFK_INPUT_IDLE_MS);
    if (shouldbeAfk && !this.afk) {
      this.setAfk();
    }
  }

  private setAfk(): void {
    this.afk = true;
    this.wsClient.setAfk(true);
    this.callbacks.onAfkChange(true);
  }

  private clearAfk(): void {
    this.afk = false;
    this.lastActivityMs = Date.now();
    this.wsClient.setAfk(false);
    this.callbacks.onAfkChange(false);
  }

  isAfk(): boolean {
    return this.afk;
  }

  destroy(): void {
    if (this.checkTimer) {
      clearInterval(this.checkTimer);
      this.checkTimer = null;
    }
    if (this.tabDebounceTimer) {
      clearTimeout(this.tabDebounceTimer);
      this.tabDebounceTimer = null;
    }
    window.removeEventListener("mousemove", this.boundActivity);
    window.removeEventListener("mousedown", this.boundActivity);
    window.removeEventListener("keydown", this.boundActivity);
    window.removeEventListener("wheel", this.boundActivity);
    window.removeEventListener("touchstart", this.boundActivity);
    document.removeEventListener("visibilitychange", this.boundVisibilityChange);
    window.removeEventListener("blur", this.boundBlur);
    window.removeEventListener("focus", this.boundFocus);
  }
}
