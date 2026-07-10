// VideoTile is a single participant video tile for the VideoBar. It renders
// the participant's camera as an HTML <video> element with a name label at
// the bottom and a green border that appears while the participant speaks.
//
// The tile is self-contained: the VideoBar creates/destroys tiles as
// participants join/leave and calls setSpeaking to toggle the border. A
// controls overlay area is reserved (currently empty) so a per-user mute
// button can be added later without reworking the layout.

import type { Track } from "livekit-client";

export class VideoTile {
  readonly entityId: string;
  readonly isLocal: boolean;

  private root: HTMLDivElement;
  private video: HTMLVideoElement;
  private nameLabel: HTMLDivElement;
  private controlsOverlay: HTMLDivElement;
  // The track currently attached to the <video> element. Used to avoid
  // re-attaching the same track on every sync call, which would tear down
  // and rebuild the MediaStream and prevent video from ever rendering.
  private attachedTrack: Track | null = null;

  constructor(entityId: string, isLocal: boolean) {
    this.entityId = entityId;
    this.isLocal = isLocal;

    this.root = document.createElement("div");
    this.root.style.cssText =
      "position:relative;background:#000;border-radius:8px;overflow:hidden;flex:0 0 auto;border:2px solid transparent;box-sizing:border-box;";
    this.root.dataset.entityId = entityId;

    this.video = document.createElement("video");
    // Set both HTML attributes and DOM properties — Safari sometimes
    // requires the attributes to be present for inline autoplay to work.
    this.video.setAttribute("autoplay", "");
    this.video.setAttribute("playsinline", "");
    this.video.setAttribute("muted", "");
    this.video.autoplay = true;
    this.video.playsInline = true;
    this.video.muted = true; // don't play audio from the video element
    this.video.style.cssText = "width:100%;height:100%;object-fit:cover;display:block;";
    if (isLocal) {
      // Mirror the self-view (selfie convention).
      this.video.style.transform = "scaleX(-1)";
    }
    this.root.appendChild(this.video);

    // Name label at the bottom with a gradient scrim for readability.
    const scrim = document.createElement("div");
    scrim.style.cssText =
      "position:absolute;bottom:0;left:0;right:0;height:28px;background:linear-gradient(transparent,rgba(0,0,0,0.75));pointer-events:none;";
    this.root.appendChild(scrim);

    this.nameLabel = document.createElement("div");
    this.nameLabel.style.cssText =
      "position:absolute;bottom:4px;left:0;right:0;text-align:center;color:#fff;font-size:12px;font-family:sans-serif;font-weight:600;pointer-events:none;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;padding:0 4px;";
    this.root.appendChild(this.nameLabel);

    // Controls overlay (top-right) — reserved for a future per-user mute
    // button. Empty for now so the slot exists without visual clutter.
    this.controlsOverlay = document.createElement("div");
    this.controlsOverlay.style.cssText =
      "position:absolute;top:4px;right:4px;display:flex;gap:4px;pointer-events:auto;";
    this.root.appendChild(this.controlsOverlay);
  }

  get element(): HTMLDivElement {
    return this.root;
  }

  // attachTrack attaches a LiveKit camera track to the <video> element.
  // Only re-attaches when the track actually changes — calling attach()
  // repeatedly with the same track tears down and rebuilds the MediaStream,
  // preventing the <video> from ever rendering.
  attachTrack(track: Track | null): void {
    if (track === this.attachedTrack) return;
    if (this.attachedTrack) {
      try { (this.attachedTrack as any).detach(this.video); } catch { /* noop */ }
    }
    if (track) {
      (track as any).attach(this.video);
      // Explicitly call play() — Safari sometimes doesn't start playback
      // even with autoplay=true, especially after attach().
      this.video.play().catch(() => { /* autoplay may reject silently */ });
    } else {
      this.video.srcObject = null;
    }
    this.attachedTrack = track;
  }

  setName(name: string): void {
    this.nameLabel.textContent = name;
  }

  // setSize sets the tile dimensions. Called by the VideoBar when the user
  // drags the resize handle.
  setSize(w: number, h: number): void {
    this.root.style.width = `${w}px`;
    this.root.style.height = `${h}px`;
  }

  setSpeaking(speaking: boolean): void {
    this.root.style.borderColor = speaking ? "#22c55e" : "transparent";
    this.root.style.boxShadow = speaking ? "0 0 6px rgba(34,197,94,0.6)" : "none";
  }

  // addControl appends a control element (e.g. a mute button) to the
  // top-right controls overlay. Reserved for future use.
  addControl(el: HTMLElement): void {
    this.controlsOverlay.appendChild(el);
  }

  destroy(): void {
    if (this.attachedTrack) {
      try { (this.attachedTrack as any).detach(this.video); } catch { /* noop */ }
      this.attachedTrack = null;
    }
    this.video.srcObject = null;
    this.video.remove();
    this.root.remove();
  }
}
