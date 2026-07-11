// ScreenViewer is a floating, draggable DOM window that displays a single
// screen share track. It supports three modes: windowed (draggable, ~70vw),
// fullscreen, and minimized (small thumbnail). The video element uses a
// callback-ref pattern so the LiveKit track follows the <video> across DOM
// remounts caused by mode switches.
//
// When screenMuted is true (the sharer minimized/hid the source window), a
// "paused" overlay is shown instead of the frozen last frame.

import type { Track } from "livekit-client";

type Mode = "windowed" | "fullscreen" | "minimized";

export class ScreenViewer {
  private element: HTMLDivElement;
  private videoEl: HTMLVideoElement | null = null;
  private mode: Mode = "windowed";
  private pos: { x: number; y: number };
  private size: { w: number; h: number };
  private dragWin: { ox: number; oy: number } | null = null;
  private pausedOverlay: HTMLDivElement | null = null;
  private track: Track;
  private label: string;
  private index: number;

  constructor(track: Track, label: string, index: number) {
    this.track = track;
    this.label = label;
    this.index = index;
    this.pos = { x: 80 + index * 32, y: 72 + index * 32 };
    this.size = { w: Math.round(window.innerWidth * 0.7), h: Math.round(window.innerHeight * 0.6) };

    this.element = document.createElement("div");
    this.element.style.cssText =
      "position:fixed;z-index:40;font-family:sans-serif;";
    document.body.appendChild(this.element);

    this.render();
  }

  // setScreenMuted toggles the "paused" overlay. Called by ScreenShareOverlay
  // when the participant's screenMuted state changes.
  setScreenMuted(muted: boolean): void {
    if (this.pausedOverlay) {
      this.pausedOverlay.style.display = muted ? "flex" : "none";
    }
  }

  // attachVideo is a callback-ref: it fires on every DOM mount/unmount of the
  // <video> element, ensuring the LiveKit track is attached to whichever
  // <video> is currently in the DOM (critical across mode switches which
  // remount the element).
  private attachVideo(el: HTMLVideoElement | null): void {
    if (this.videoEl && this.videoEl !== el) {
      try { (this.track as any).detach(this.videoEl); } catch { /* noop */ }
    }
    this.videoEl = el;
    if (el) {
      try { (this.track as any).attach(el); } catch { /* noop */ }
    }
  }

  private render(): void {
    this.element.innerHTML = "";
    this.element.onclick = null;
    this.element.style.zIndex = this.mode === "fullscreen" ? "50" : "40";

    if (this.mode === "minimized") {
      this.element.style.cssText =
        `position:fixed;bottom:4px;left:4px;z-index:40;width:200px;height:120px;` +
        `overflow:hidden;border-radius:8px;background:#000;` +
        `box-shadow:0 0 0 2px rgba(99,102,241,0.5),0 8px 30px rgba(0,0,0,0.5);` +
        `cursor:pointer;font-family:sans-serif;margin-bottom:${this.index * 130}px;`;
      this.element.onclick = () => { this.mode = "windowed"; this.render(); };
      this.buildVideoBody();
      this.buildLabel(this.element, "10px");
      return;
    }

    if (this.mode === "fullscreen") {
      this.element.style.cssText =
        "position:fixed;inset:0;z-index:50;display:flex;flex-direction:column;" +
        "background:#000;font-family:sans-serif;";
      this.buildTitleBar(this.element, true);
      const body = document.createElement("div");
      body.style.cssText = "position:relative;flex:1;min-height:0;";
      this.element.appendChild(body);
      this.buildVideoBody(body);
      return;
    }

    // windowed
    this.element.style.cssText =
      `position:fixed;left:${this.pos.x}px;top:${this.pos.y}px;z-index:40;` +
      `width:${this.size.w}px;display:flex;flex-direction:column;overflow:hidden;` +
      `border-radius:8px;background:#0f172a;` +
      `box-shadow:0 0 0 2px rgba(99,102,241,0.5),0 8px 30px rgba(0,0,0,0.5);` +
      `font-family:sans-serif;`;
    this.buildTitleBar(this.element, false);
    const body = document.createElement("div");
    body.style.cssText = `position:relative;height:${this.size.h}px;`;
    this.element.appendChild(body);
    this.buildVideoBody(body);
    this.buildResizeHandle();
  }

  private buildTitleBar(parent: HTMLElement, isFullscreen: boolean): void {
    const bar = document.createElement("div");
    bar.style.cssText =
      "display:flex;align-items:center;justify-content:space-between;gap:8px;" +
      "background:#1e293b;padding:8px 12px;color:#fff;cursor:" +
      (isFullscreen ? "default" : "move") + ";";

    if (!isFullscreen) {
      bar.addEventListener("mousedown", (e) => this.startWinDrag(e));
    }

    const left = document.createElement("div");
    left.style.cssText = "display:flex;align-items:center;gap:8px;overflow:hidden;";
    bar.appendChild(left);

    const badge = document.createElement("span");
    badge.textContent = "LIVE";
    badge.style.cssText =
      "display:flex;align-items:center;gap:4px;background:#dc2626;color:#fff;" +
      "padding:2px 8px;border-radius:999px;font-size:10px;font-weight:700;" +
      "text-transform:uppercase;letter-spacing:0.05em;";
    const dot = document.createElement("span");
    dot.style.cssText = "width:6px;height:6px;border-radius:50%;background:#fff;";
    badge.appendChild(dot);
    left.appendChild(badge);

    const labelEl = document.createElement("span");
    labelEl.textContent = this.label;
    labelEl.style.cssText = "font-size:13px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;";
    left.appendChild(labelEl);

    const right = document.createElement("div");
    right.style.cssText = "display:flex;align-items:center;gap:4px;";
    bar.appendChild(right);

    if (!isFullscreen) {
      const fsBtn = this.makeBtn("Fullscreen");
      fsBtn.onclick = () => { this.mode = "fullscreen"; this.render(); };
      right.appendChild(fsBtn);
    } else {
      const winBtn = this.makeBtn("Windowed");
      winBtn.onclick = () => { this.mode = "windowed"; this.render(); };
      right.appendChild(winBtn);
    }

    const minBtn = this.makeBtn("Minimize");
    minBtn.onclick = () => { this.mode = "minimized"; this.render(); };
    right.appendChild(minBtn);

    parent.appendChild(bar);
  }

  private makeBtn(text: string): HTMLButtonElement {
    const btn = document.createElement("button");
    btn.textContent = text;
    btn.style.cssText =
      "background:rgba(255,255,255,0.1);color:#fff;border:none;border-radius:4px;" +
      "padding:4px 10px;font-size:12px;font-weight:600;cursor:pointer;";
    btn.onmouseenter = () => { btn.style.background = "rgba(255,255,255,0.2)"; };
    btn.onmouseleave = () => { btn.style.background = "rgba(255,255,255,0.1)"; };
    return btn;
  }

  private buildVideoBody(parent?: HTMLElement): void {
    const container = parent ?? this.element;
    const wrapper = document.createElement("div");
    wrapper.style.cssText =
      "position:relative;width:100%;height:100%;overflow:hidden;background:#000;";

    const video = document.createElement("video");
    video.autoplay = true;
    video.playsInline = true;
    video.style.cssText = "width:100%;height:100%;object-fit:contain;";
    wrapper.appendChild(video);

    // Paused overlay — shown when the sharer's source window is hidden.
    this.pausedOverlay = document.createElement("div");
    this.pausedOverlay.style.cssText =
      "position:absolute;inset:0;display:none;flex-direction:column;" +
      "align-items:center;justify-content:center;background:rgba(0,0,0,0.85);" +
      "color:#94a3b8;font-size:14px;gap:8px;";
    const pausedIcon = document.createElement("div");
    pausedIcon.textContent = "⏸";
    pausedIcon.style.cssText = "font-size:32px;";
    this.pausedOverlay.appendChild(pausedIcon);
    const pausedText = document.createElement("div");
    pausedText.textContent = "Screen share paused";
    this.pausedOverlay.appendChild(pausedText);
    wrapper.appendChild(this.pausedOverlay);

    container.appendChild(wrapper);

    // Use callback-ref: attach the track to the video element now that it's
    // in the DOM.
    this.attachVideo(video);
  }

  private buildLabel(parent: HTMLElement, fontSize: string): void {
    const label = document.createElement("div");
    label.textContent = this.label;
    label.style.cssText =
      `position:absolute;bottom:0;left:0;right:0;background:rgba(0,0,0,0.7);` +
      `color:#fff;padding:2px 8px;font-size:${fontSize};overflow:hidden;` +
      `text-overflow:ellipsis;white-space:nowrap;`;
    parent.appendChild(label);
  }

  private startWinDrag(e: MouseEvent): void {
    if (this.mode !== "windowed") return;
    this.dragWin = { ox: e.clientX - this.pos.x, oy: e.clientY - this.pos.y };

    const onMove = (ev: MouseEvent) => {
      if (!this.dragWin) return;
      this.pos = { x: ev.clientX - this.dragWin.ox, y: ev.clientY - this.dragWin.oy };
      this.element.style.left = `${this.pos.x}px`;
      this.element.style.top = `${this.pos.y}px`;
    };
    const onUp = () => {
      this.dragWin = null;
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }

  private buildResizeHandle(): void {
    const handle = document.createElement("div");
    handle.style.cssText =
      "position:absolute;right:0;bottom:0;width:16px;height:16px;cursor:nwse-resize;" +
      "background:linear-gradient(135deg,transparent 50%,rgba(255,255,255,0.4) 50%);" +
      "z-index:1;";
    handle.addEventListener("mousedown", (e) => this.startResize(e));
    this.element.appendChild(handle);
  }

  private startResize(e: MouseEvent): void {
    e.preventDefault();
    e.stopPropagation();
    const startW = this.size.w;
    const startH = this.size.h;
    const startX = e.clientX;
    const startY = e.clientY;

    const onMove = (ev: MouseEvent) => {
      this.size = {
        w: Math.max(240, startW + (ev.clientX - startX)),
        h: Math.max(160, startH + (ev.clientY - startY)),
      };
      this.element.style.width = `${this.size.w}px`;
      const body = this.element.querySelector(":scope > div:nth-child(2)") as HTMLElement | null;
      if (body) body.style.height = `${this.size.h}px`;
    };
    const onUp = () => {
      window.removeEventListener("mousemove", onMove);
      window.removeEventListener("mouseup", onUp);
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
  }

  destroy(): void {
    if (this.videoEl) {
      try { (this.track as any).detach(this.videoEl); } catch { /* noop */ }
    }
    this.videoEl = null;
    this.element.remove();
  }
}
