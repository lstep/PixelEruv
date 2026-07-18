// TopMenu is a floating DOM bar fixed to the top of the browser window,
// independent of the Phaser canvas/scene lifecycle. It shows mic/camera
// A/V controls, a Login/Logout button, and a dropdown menu with a username
// field. Styled as dark rounded pills, matching AvOverlay's previous HUD
// look.

import { isLoggedIn, redirectToLogin, redirectToRegister, logout, pb } from "../auth";
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
  private screenBtn: HTMLButtonElement;
  private avClient: AvClient | null = null;
  private chatPanel: ChatPanel | null = null;
  private chatBtn: HTMLButtonElement;
  private audioBtn: HTMLButtonElement;
  private authBtn: HTMLButtonElement;
  private registerBtn: HTMLButtonElement;
  private setNameHandler: ((name: string) => void) | null = null;
  private setSpriteBaseHandler: ((spriteBase: string) => void) | null = null;
  private setPlayerOptionsHandler: ((options: string) => void) | null = null;
  private setStatusHandler: ((status: number) => void) | null = null;
  // applyStatusFn is set in the constructor where the status buttons are
  // built. syncStatusFromServer calls it to reflect the server-confirmed
  // status (e.g. on page reload after restoring from PocketBase) without
  // re-firing setStatusHandler (which would echo a SetStatusFrame back).
  private applyStatusFn: ((value: number) => void) | null = null;
  private playerOptions: string = "";
  private authStoreUnsub: (() => void) | null = null;
  private boundDocClick: () => void;

  constructor() {
    this.container = document.createElement("div");
    this.container.style.cssText =
      "position:fixed;top:12px;right:12px;display:flex;gap:8px;align-items:flex-start;z-index:20;font-family:sans-serif;";
    document.body.appendChild(this.container);

    // Welcome page link — official PixelEruv icon, fixed to the top-left corner
    // (separate from the right-aligned A/V bar). Opens /welcome/ in a new tab
    // so the player's game session is preserved.
    const welcomeLink = document.createElement("a");
    welcomeLink.href = "/welcome/";
    welcomeLink.target = "_blank";
    welcomeLink.rel = "noopener noreferrer";
    welcomeLink.title = "Welcome page";
    welcomeLink.setAttribute("aria-label", "Welcome page");
    welcomeLink.style.cssText =
      "position:fixed;top:12px;left:12px;z-index:20;display:flex;align-items:center;justify-content:center;" +
      "width:80px;height:80px;text-decoration:none;";
    const welcomeImg = document.createElement("img");
    welcomeImg.src = "/assets/pixel-eruv-icon.svg";
    welcomeImg.alt = "";
    welcomeImg.style.cssText = "width:80px;height:80px;display:block;pointer-events:none;";
    welcomeLink.appendChild(welcomeImg);
    document.body.appendChild(welcomeLink);

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

    // Screen share toggle — hidden until a scene attaches an AvClient.
    this.screenBtn = document.createElement("button");
    this.screenBtn.style.cssText = PILL_STYLE + "display:none;";
    this.screenBtn.addEventListener("click", async () => {
      if (this.avClient) {
        try {
          await this.avClient.setScreenShareEnabled(!this.avClient.isScreenShareEnabled());
        } catch (err) {
          console.warn("screenBtn: screen share failed:", err);
        }
      }
      this.updateAvLabels();
    });
    this.container.appendChild(this.screenBtn);

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

    this.authBtn = document.createElement("button");
    this.authBtn.addEventListener("click", () => {
      if (isLoggedIn()) logout();
      else redirectToLogin();
    });
    this.container.appendChild(this.authBtn);

    // Register button — always created, hidden when logged in. Visibility is
    // toggled by refreshAuth() so it appears/disappears when the auth store
    // changes at runtime (e.g. stale token cleared after a server DB purge).
    this.registerBtn = document.createElement("button");
    this.registerBtn.textContent = "Register";
    this.registerBtn.style.cssText = PILL_STYLE + "background:#2ecc71;";
    this.registerBtn.addEventListener("click", () => redirectToRegister());
    this.container.appendChild(this.registerBtn);

    // Keep the auth button in sync with the PocketBase auth store. The store
    // can change at runtime without a page reload — notably when WsClient
    // clears a stale token after the server DB is reset, falling back to
    // guest mode. Without this, the button would keep saying "Logout" until
    // the user manually reloads.
    this.authStoreUnsub = pb.authStore.onChange(() => this.refreshAuth());
    this.refreshAuth();

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

    // Speaker (audio output) selector — only shown when the browser supports
    // setSinkId (Chrome/Firefox/Edge). Safari and most mobile browsers don't.
    let speakerSelect: HTMLSelectElement | null = null;
    if ("setSinkId" in document.createElement("audio")) {
      const speakerLabel = document.createElement("div");
      speakerLabel.textContent = "Speakers";
      speakerLabel.style.cssText = "color:#aaa;font-size:12px;margin-top:12px;margin-bottom:6px;";
      this.dropdown.appendChild(speakerLabel);

      speakerSelect = document.createElement("select");
      speakerSelect.style.cssText =
        "width:100%;padding:6px 8px;font-size:14px;background:#1a1a2e;color:#fff;border:1px solid #555;border-radius:6px;";
      this.dropdown.appendChild(speakerSelect);
    }

    // Populate device lists when the dropdown opens. Without mic/camera
    // permission, Safari only enumerates a single "default" device per
    // kind with no labels, so the user can't see their real devices
    // (e.g. "Logitech Webcam"). The menu-button click is a user gesture,
    // so we can request permission here and re-enumerate with labels.
    // After rebuilding options, restore the currently-selected device so
    // the <select> doesn't silently reset to the first option.
    const refreshDevices = async () => {
      if (!this.avClient) return;
      // First enumeration may have empty labels (no permission yet).
      let mics = await this.avClient.getDevices("audioinput");
      let cams = await this.avClient.getDevices("videoinput");
      const needsPermission =
        mics.some((d) => !d.label || d.label === "Microphone") ||
        cams.some((d) => !d.label || d.label === "Camera");
      if (needsPermission) {
        // Request both audio + video permission. Either may be denied;
        // we re-enumerate regardless and show whatever is available.
        try {
          await this.avClient.requestPermission("audio");
        } catch (e) {
          console.warn("TopMenu: audio permission denied:", e);
        }
        try {
          await this.avClient.requestPermission("video");
        } catch (e) {
          console.warn("TopMenu: video permission denied:", e);
        }
        mics = await this.avClient.getDevices("audioinput");
        cams = await this.avClient.getDevices("videoinput");
      }

      const selectedMic = this.avClient.getSelectedDevice("audioinput");
      micSelect.innerHTML = "";
      for (const d of mics) {
        const opt = document.createElement("option");
        opt.value = d.deviceId;
        opt.textContent = d.label;
        micSelect.appendChild(opt);
      }
      if (selectedMic && [...micSelect.options].some((o) => o.value === selectedMic)) {
        micSelect.value = selectedMic;
      }
      const selectedCam = this.avClient.getSelectedDevice("videoinput");
      camSelect.innerHTML = "";
      for (const d of cams) {
        const opt = document.createElement("option");
        opt.value = d.deviceId;
        opt.textContent = d.label;
        camSelect.appendChild(opt);
      }
      if (selectedCam && [...camSelect.options].some((o) => o.value === selectedCam)) {
        camSelect.value = selectedCam;
      }
      if (speakerSelect) {
        const speakers = await this.avClient.getDevices("audiooutput");
        const selectedSpeaker = this.avClient.getSelectedDevice("audiooutput");
        speakerSelect.innerHTML = "";
        for (const d of speakers) {
          const opt = document.createElement("option");
          opt.value = d.deviceId;
          opt.textContent = d.label;
          speakerSelect.appendChild(opt);
        }
        if (selectedSpeaker && [...speakerSelect.options].some((o) => o.value === selectedSpeaker)) {
          speakerSelect.value = selectedSpeaker;
        }
      }
    };

    micSelect.addEventListener("change", () => {
      this.avClient?.switchDevice("audioinput", micSelect.value);
    });
    camSelect.addEventListener("change", () => {
      this.avClient?.switchDevice("videoinput", camSelect.value);
    });
    if (speakerSelect) {
      speakerSelect.addEventListener("change", () => {
        this.avClient?.switchDevice("audiooutput", speakerSelect!.value);
      });
    }

    // --- Player options: Show my name tag ---
    // Checkbox that toggles whether the local player's name tag is visible
    // above their own avatar. Sent to the server via SetPlayerOptionsFrame
    // and persisted to PocketBase for logged-in users.
    const nameTagLabel = document.createElement("div");
    nameTagLabel.style.cssText = "color:#aaa;font-size:12px;margin-top:12px;margin-bottom:6px;";
    nameTagLabel.textContent = "Player options";
    this.dropdown.appendChild(nameTagLabel);

    const nameTagRow = document.createElement("div");
    nameTagRow.style.cssText = "display:flex;align-items:center;gap:6px;";
    this.dropdown.appendChild(nameTagRow);

    const nameTagCheckbox = document.createElement("input");
    nameTagCheckbox.type = "checkbox";
    nameTagCheckbox.id = "topmenu-show-name-tag";
    nameTagCheckbox.style.cssText = "width:16px;height:16px;cursor:pointer;";
    nameTagRow.appendChild(nameTagCheckbox);

    const nameTagLabelText = document.createElement("label");
    nameTagLabelText.htmlFor = "topmenu-show-name-tag";
    nameTagLabelText.textContent = "Show my name tag";
    nameTagLabelText.style.cssText = "color:#fff;font-size:14px;cursor:pointer;";
    nameTagRow.appendChild(nameTagLabelText);

    nameTagCheckbox.addEventListener("change", () => {
      const opts = parsePlayerOptions(this.playerOptions);
      opts.show_own_name_tag = nameTagCheckbox.checked;
      const json = JSON.stringify(opts);
      this.playerOptions = json;
      this.setPlayerOptionsHandler?.(json);
    });

    // --- Presence status selector (Available / Busy / Do Not Disturb) ---
    // Sends a SetStatusFrame via ws.setStatus. DND fully excludes the
    // player from A/V (server-side: worldsim skips proximity clustering,
    // ext-av skips zone token minting; client-side: AvClient disconnects
    // and refuses joins). Mic/camera/screen buttons are disabled while
    // DND is active as visual feedback.
    const statusLabel = document.createElement("div");
    statusLabel.style.cssText = "color:#aaa;font-size:12px;margin-top:12px;margin-bottom:6px;";
    statusLabel.textContent = "Status";
    this.dropdown.appendChild(statusLabel);

    const statusRow = document.createElement("div");
    statusRow.style.cssText = "display:flex;gap:6px;";
    this.dropdown.appendChild(statusRow);

    const statusOptions = [
      { label: "Available", value: 0, color: "#22c55e" },
      { label: "Busy", value: 1, color: "#eab308" },
      { label: "DND", value: 2, color: "#ef4444" },
    ];
    let currentStatus = 0;
    const statusButtons: HTMLButtonElement[] = [];
    const applyStatus = (value: number) => {
      currentStatus = value;
      for (const sb of statusButtons) {
        const v = Number(sb.dataset.value);
        sb.style.outline = v === value ? "2px solid #fff" : "none";
      }
      // Disable A/V controls while DND; re-enable otherwise.
      const disabled = value === 2;
      this.micBtn.disabled = disabled;
      this.camBtn.disabled = disabled;
      this.screenBtn.disabled = disabled;
      if (disabled) {
        this.micBtn.style.opacity = "0.4";
        this.camBtn.style.opacity = "0.4";
        this.screenBtn.style.opacity = "0.4";
      } else {
        this.updateAvLabels();
      }
    };
    this.applyStatusFn = applyStatus;
    for (const opt of statusOptions) {
      const btn = document.createElement("button");
      btn.textContent = opt.label;
      btn.dataset.value = String(opt.value);
      btn.style.cssText =
        PILL_STYLE + `padding:6px 10px;font-size:12px;background:${opt.color};`;
      btn.addEventListener("click", () => {
        applyStatus(opt.value);
        this.setStatusHandler?.(opt.value);
      });
      statusRow.appendChild(btn);
      statusButtons.push(btn);
    }
    applyStatus(0);

    menuBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      this.dropdown.style.display = this.dropdown.style.display === "none" ? "block" : "none";
      if (this.dropdown.style.display === "block") {
        refreshDevices();
        // Sync the name tag checkbox with the current server-side value.
        nameTagCheckbox.checked = parsePlayerOptions(this.playerOptions).show_own_name_tag ?? true;
      }
    });
    this.boundDocClick = () => {
      this.dropdown.style.display = "none";
    };
    document.addEventListener("click", this.boundDocClick);
    this.dropdown.addEventListener("click", (e) => e.stopPropagation());
  }

  // attachAvControls shows the mic/camera buttons and wires them to the
  // given AvClient. Called by GameScene once its AvClient is created.
  attachAvControls(avClient: AvClient): void {
    this.avClient = avClient;
    this.micBtn.style.display = "block";
    this.camBtn.style.display = "block";
    this.screenBtn.style.display = "block";
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
    this.screenBtn.style.display = "none";
    this.audioBtn.style.display = "none";
  }

  // setChatPanel wires the chat sidebar and shows the Chat toggle button.
  // Called by main.ts after both TopMenu and ChatPanel are created.
  setChatPanel(panel: ChatPanel): void {
    this.chatPanel = panel;
    this.chatBtn.style.display = "block";
  }

  // refreshAuth updates the Login/Logout button and Register button to match
  // the current PocketBase auth store state. Called on construction and
  // whenever pb.authStore changes (e.g. stale token cleared after a server
  // DB purge, which falls back to guest mode without a page reload).
  private refreshAuth(): void {
    const loggedIn = isLoggedIn();
    this.authBtn.textContent = loggedIn ? "Logout" : "Login";
    this.authBtn.style.cssText = PILL_STYLE + (loggedIn ? "" : "background:#4c5cf0;");
    this.registerBtn.style.display = loggedIn ? "none" : "block";
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

  // setSetPlayerOptionsHandler wires the callback invoked when the user toggles
  // a player option in the dropdown. GameScene passes ws.setPlayerOptions so
  // the change is sent to the server (which persists to PocketBase).
  setSetPlayerOptionsHandler(fn: (options: string) => void): void {
    this.setPlayerOptionsHandler = fn;
  }

  // setSetStatusHandler wires the callback invoked when the user picks a
  // presence status in the dropdown. GameScene passes ws.setStatus so the
  // change is sent to the server (which validates, replicates, and broadcasts
  // for A/V exclusion enforcement).
  setSetStatusHandler(fn: (status: number) => void): void {
    this.setStatusHandler = fn;
  }

  // syncStatusFromServer reflects a server-confirmed presence status in the
  // dropdown UI (button highlight + A/V control enablement) WITHOUT re-firing
  // setStatusHandler. Called by GameScene when the local player's DisplayName
  // component arrives from the server — e.g. on initial spawn after a page
  // reload, where the server restored the persisted status from PocketBase.
  syncStatusFromServer(value: number): void {
    this.applyStatusFn?.(value);
  }

  // setPlayerOptions updates the stored player options from the server's auth
  // result. Called by GameScene on ready so the checkbox reflects the current
  // server-side value when the dropdown is opened.
  setPlayerOptions(options: string): void {
    this.playerOptions = options;
  }

  private updateAvLabels(): void {
    if (!this.avClient) return;
    this.micBtn.textContent = this.avClient.isMicMuted() ? "🎤 Muted" : "🎤 Mic";
    this.micBtn.style.opacity = this.avClient.isMicMuted() ? "0.5" : "1";
    this.camBtn.textContent = this.avClient.isCameraEnabled() ? "📷 On" : "📷 Cam";
    this.camBtn.style.opacity = this.avClient.isCameraEnabled() ? "1" : "0.5";
    this.screenBtn.textContent = this.avClient.isScreenShareEnabled() ? "🖥️ Stop" : "🖥️ Screen";
    this.screenBtn.style.opacity = this.avClient.isScreenShareEnabled() ? "1" : "0.5";
  }

  // destroy tears down event listeners and removes the DOM container. Mirrors
  // the cleanup pattern used by VideoBar/VirtualJoystick. TopMenu is currently
  // created once per page load, but this keeps it safe to reuse/recreate.
  destroy(): void {
    this.authStoreUnsub?.();
    this.authStoreUnsub = null;
    document.removeEventListener("click", this.boundDocClick);
    this.container.remove();
  }
}

// parsePlayerOptions safely parses the player options JSON string, returning
// an object with known keys. Malformed JSON, null, or any non-object value
// yields an empty object — never throws. Shared by TopMenu and GameScene so
// both zoom persistence and the name-tag checkbox treat bad input the same.
export function parsePlayerOptions(raw: string): { show_own_name_tag?: boolean; zoom?: number } {
  if (!raw) return {};
  try {
    const parsed = JSON.parse(raw);
    if (typeof parsed === "object" && parsed !== null) return parsed;
  } catch {
    // malformed JSON — return empty
  }
  return {};
}
