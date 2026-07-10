// TopMenu is a floating DOM bar fixed to the top of the browser window,
// independent of the Phaser canvas/scene lifecycle. It shows mic/camera
// A/V controls, a Login/Logout button, and a dropdown menu with a username
// field. Styled as dark rounded pills, matching AvOverlay's previous HUD
// look.

import { isLoggedIn, redirectToLogin, logout } from "../auth";
import { getUsername, setUsername } from "../username";
import type { AvClient } from "../net/AvClient";
import type { ChatPanel } from "./ChatPanel";

const PILL_STYLE =
  "padding:8px 16px;font-size:14px;font-family:sans-serif;font-weight:600;background:#2d2d3a;color:#fff;border:none;border-radius:20px;cursor:pointer;";

export class TopMenu {
  private container: HTMLDivElement;
  private dropdown: HTMLDivElement;
  private micBtn: HTMLButtonElement;
  private camBtn: HTMLButtonElement;
  private avClient: AvClient | null = null;
  private chatPanel: ChatPanel | null = null;
  private chatBtn: HTMLButtonElement;
  private audioBtn: HTMLButtonElement;
  private setNameHandler: ((name: string) => void) | null = null;
  private setSpriteBaseHandler: ((spriteBase: string) => void) | null = null;

  constructor() {
    this.container = document.createElement("div");
    this.container.style.cssText =
      "position:fixed;top:12px;right:12px;display:flex;gap:8px;align-items:flex-start;z-index:20;font-family:sans-serif;";
    document.body.appendChild(this.container);

    // A/V controls, hidden until a scene attaches an AvClient.
    this.micBtn = document.createElement("button");
    this.micBtn.style.cssText = PILL_STYLE + "display:none;";
    this.micBtn.addEventListener("click", async () => {
      console.log("micBtn click, avClient=", !!this.avClient);
      if (this.avClient) await this.avClient.setMicMuted(!this.avClient.isMicMuted());
      this.updateAvLabels();
    });
    this.container.appendChild(this.micBtn);

    this.camBtn = document.createElement("button");
    this.camBtn.style.cssText = PILL_STYLE + "display:none;";
    this.camBtn.addEventListener("click", async () => {
      console.log("camBtn click, avClient=", !!this.avClient);
      if (this.avClient) await this.avClient.setCameraEnabled(!this.avClient.isCameraEnabled());
      this.updateAvLabels();
    });
    this.container.appendChild(this.camBtn);

    // "Enable Audio" button — shown when the browser's autoplay policy
    // blocks remote audio playback. Clicking calls room.startAudio()
    // within the user gesture, satisfying the browser requirement.
    this.audioBtn = document.createElement("button");
    this.audioBtn.textContent = "🔇 Enable Audio";
    this.audioBtn.title = "Click to enable remote audio playback";
    this.audioBtn.style.cssText = PILL_STYLE + "display:none;background:#c0392b;";
    this.audioBtn.addEventListener("click", async () => {
      if (this.avClient) await this.avClient.startAudio();
    });
    this.container.appendChild(this.audioBtn);

    // Chat toggle button, hidden until setChatPanel is called.
    this.chatBtn = document.createElement("button");
    this.chatBtn.textContent = "💬 Chat";
    this.chatBtn.style.cssText = PILL_STYLE + "display:none;";
    this.chatBtn.addEventListener("click", () => {
      this.chatPanel?.toggle();
    });
    this.container.appendChild(this.chatBtn);

    const authBtn = document.createElement("button");
    const loggedIn = isLoggedIn();
    authBtn.textContent = loggedIn ? "Logout" : "Login";
    authBtn.style.cssText = PILL_STYLE + (loggedIn ? "" : "background:#4c5cf0;");
    authBtn.addEventListener("click", () => {
      if (loggedIn) logout();
      else redirectToLogin();
    });
    this.container.appendChild(authBtn);

    const menuWrap = document.createElement("div");
    menuWrap.style.cssText = "position:relative;";
    this.container.appendChild(menuWrap);

    const menuBtn = document.createElement("button");
    menuBtn.textContent = "Menu \u25be";
    menuBtn.style.cssText = PILL_STYLE;
    menuWrap.appendChild(menuBtn);

    this.dropdown = document.createElement("div");
    this.dropdown.style.cssText =
      "display:none;position:absolute;top:calc(100% + 8px);right:0;background:#2d2d3a;border-radius:12px;padding:12px;min-width:220px;box-shadow:0 4px 12px rgba(0,0,0,0.4);";
    menuWrap.appendChild(this.dropdown);

    const label = document.createElement("div");
    label.textContent = "Your name";
    label.style.cssText = "color:#aaa;font-size:12px;margin-bottom:6px;";
    this.dropdown.appendChild(label);

    const row = document.createElement("div");
    row.style.cssText = "display:flex;gap:6px;";
    this.dropdown.appendChild(row);

    const input = document.createElement("input");
    input.type = "text";
    input.value = getUsername() ?? "";
    input.placeholder = "Enter a username";
    input.style.cssText =
      "flex:1;padding:6px 8px;font-size:14px;background:#1a1a2e;color:#fff;border:1px solid #555;border-radius:6px;";
    row.appendChild(input);

    const saveBtn = document.createElement("button");
    saveBtn.textContent = "Save";
    saveBtn.style.cssText = PILL_STYLE + "padding:6px 12px;";
    saveBtn.addEventListener("click", () => {
      const name = input.value.trim();
      setUsername(name);
      this.setNameHandler?.(name);
      this.dropdown.style.display = "none";
    });
    row.appendChild(saveBtn);

    // Character sheet picker — opens a prompt to enter a sprite_bases ID.
    // In phase 1 this is a simple text input; a richer UI is phase 2.
    const charLabel = document.createElement("div");
    charLabel.textContent = "Character sheet";
    charLabel.style.cssText = "color:#aaa;font-size:12px;margin-top:12px;margin-bottom:6px;";
    this.dropdown.appendChild(charLabel);

    const charRow = document.createElement("div");
    charRow.style.cssText = "display:flex;gap:6px;";
    this.dropdown.appendChild(charRow);

    const charInput = document.createElement("input");
    charInput.type = "text";
    charInput.placeholder = "Sprite ID (blank = fallback)";
    charInput.style.cssText =
      "flex:1;padding:6px 8px;font-size:14px;background:#1a1a2e;color:#fff;border:1px solid #555;border-radius:6px;";
    charRow.appendChild(charInput);

    const charBtn = document.createElement("button");
    charBtn.textContent = "Set";
    charBtn.style.cssText = PILL_STYLE + "padding:6px 12px;";
    charBtn.addEventListener("click", () => {
      this.setSpriteBaseHandler?.(charInput.value.trim());
      this.dropdown.style.display = "none";
    });
    charRow.appendChild(charBtn);

    // --- Device selectors (mic + camera) ---
    // Added to the dropdown so users can pick the right input device.
    // Labels are empty until mic/camera permission is granted, so we
    // populate the lists when the dropdown is opened.
    const micLabel = document.createElement("div");
    micLabel.textContent = "Microphone";
    micLabel.style.cssText = "color:#aaa;font-size:12px;margin-top:12px;margin-bottom:6px;";
    this.dropdown.appendChild(micLabel);

    const micSelect = document.createElement("select");
    micSelect.style.cssText =
      "width:100%;padding:6px 8px;font-size:14px;background:#1a1a2e;color:#fff;border:1px solid #555;border-radius:6px;";
    this.dropdown.appendChild(micSelect);

    const camLabel = document.createElement("div");
    camLabel.textContent = "Camera";
    camLabel.style.cssText = "color:#aaa;font-size:12px;margin-top:12px;margin-bottom:6px;";
    this.dropdown.appendChild(camLabel);

    const camSelect = document.createElement("select");
    camSelect.style.cssText =
      "width:100%;padding:6px 8px;font-size:14px;background:#1a1a2e;color:#fff;border:1px solid #555;border-radius:6px;";
    this.dropdown.appendChild(camSelect);

    // Populate device lists when the dropdown opens (labels may have
    // become available after the first permission grant).
    const refreshDevices = async () => {
      if (!this.avClient) return;
      const mics = await this.avClient.getDevices("audioinput");
      micSelect.innerHTML = "";
      for (const d of mics) {
        const opt = document.createElement("option");
        opt.value = d.deviceId;
        opt.textContent = d.label;
        micSelect.appendChild(opt);
      }
      const cams = await this.avClient.getDevices("videoinput");
      camSelect.innerHTML = "";
      for (const d of cams) {
        const opt = document.createElement("option");
        opt.value = d.deviceId;
        opt.textContent = d.label;
        camSelect.appendChild(opt);
      }
    };

    micSelect.addEventListener("change", () => {
      this.avClient?.switchDevice("audioinput", micSelect.value);
    });
    camSelect.addEventListener("change", () => {
      this.avClient?.switchDevice("videoinput", camSelect.value);
    });

    menuBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      this.dropdown.style.display = this.dropdown.style.display === "none" ? "block" : "none";
      if (this.dropdown.style.display === "block") {
        refreshDevices();
      }
    });
    document.addEventListener("click", () => {
      this.dropdown.style.display = "none";
    });
    this.dropdown.addEventListener("click", (e) => e.stopPropagation());
  }

  // attachAvControls shows the mic/camera buttons and wires them to the
  // given AvClient. Called by GameScene once its AvClient is created.
  attachAvControls(avClient: AvClient): void {
    this.avClient = avClient;
    this.micBtn.style.display = "block";
    this.camBtn.style.display = "block";
    // Wire the audio-blocked callback so the "Enable Audio" button
    // appears when the browser blocks autoplay.
    avClient.setAudioBlockedHandler((blocked) => {
      this.audioBtn.style.display = blocked ? "block" : "none";
    });
    this.updateAvLabels();
  }

  // detachAvControls hides the mic/camera buttons. Called on scene shutdown,
  // since the AvClient it was wired to no longer exists.
  detachAvControls(): void {
    this.avClient = null;
    this.micBtn.style.display = "none";
    this.camBtn.style.display = "none";
    this.audioBtn.style.display = "none";
  }

  // setChatPanel wires the chat sidebar and shows the Chat toggle button.
  // Called by main.ts after both TopMenu and ChatPanel are created.
  setChatPanel(panel: ChatPanel): void {
    this.chatPanel = panel;
    this.chatBtn.style.display = "block";
  }

  // setSetNameHandler wires the callback invoked when the user saves their
  // name in the dropdown. GameScene passes ws.setName so the name is sent
  // to the server (which sanitizes, replicates, and persists it).
  setSetNameHandler(fn: (name: string) => void): void {
    this.setNameHandler = fn;
  }

  // setSetSpriteBaseHandler wires the callback invoked when the user enters a
  // sprite_bases ID in the dropdown. GameScene passes ws.setSpriteBase so the
  // change is sent to the server (which validates, replicates, and persists).
  setSetSpriteBaseHandler(fn: (spriteBase: string) => void): void {
    this.setSpriteBaseHandler = fn;
  }

  private updateAvLabels(): void {
    if (!this.avClient) return;
    this.micBtn.textContent = this.avClient.isMicMuted() ? "🎤 Muted" : "🎤 Mic";
    this.micBtn.style.opacity = this.avClient.isMicMuted() ? "0.5" : "1";
    this.camBtn.textContent = this.avClient.isCameraEnabled() ? "📷 On" : "📷 Cam";
    this.camBtn.style.opacity = this.avClient.isCameraEnabled() ? "1" : "0.5";
  }
}
