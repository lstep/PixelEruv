// SettingsModal — DOM-based in-game settings modal, opened from TopMenu.
// Mirrors the openPlayersModal pattern (backdrop + centered window, Esc +
// click-outside to close). Hosts the player-tunable options that don't fit
// the compact Menu dropdown, including a sprite-sheet image picker.
//
// Sections:
//   1. Character name        — localStorage + onName handler (WS setName)
//   2. Show my name tag      — merged into player options JSON (PB-persisted)
//   3. Spritesheet           — image grid from spriteBases; onSpriteBase (WS)
//   4. Notification sounds   — localStorage only (client-local pref)
//   5. Change password       — links to /forgot-password (hidden for guests)

import { getUsername, setUsername } from "../username";
import { parsePlayerOptions } from "./TopMenu";
import { isSoundEnabled, setSoundEnabled } from "../soundPrefs";
import type { SpriteBaseAsset } from "../spriteLoader";

// Reused visual constants — kept in sync with TopMenu's PILL_STYLE.
const PILL_STYLE =
  "padding:8px 16px;line-height:1.5;font-size:14px;font-family:sans-serif;font-weight:600;background:#2d2d3a;color:#fff;border:none;border-radius:20px;cursor:pointer;";
const INPUT_STYLE =
  "flex:1;padding:6px 8px;font-size:14px;background:#1a1a2e;color:#fff;border:1px solid #555;border-radius:6px;";
const SECTION_LABEL_STYLE = "color:#aaa;font-size:12px;margin-top:16px;margin-bottom:6px;";
const ACCENT = "#4c5cf0";

export interface SettingsModalContext {
  // Read the current player options JSON (server-confirmed) so the name-tag
  // checkbox reflects the real state on open.
  getPlayerOptions: () => string;
  // Write the full player options JSON (merged by the caller's merge helper).
  onPlayerOptions: (json: string) => void;
  // Sprite sheet picker.
  onSpriteBase: (id: string) => void;
  spriteBases: SpriteBaseAsset[];
  // Returns the local player's current sprite_base ID, or "" for the fallback.
  getLocalSpriteBase: () => string;
  // Name change handler (WS setName). The modal also calls setUsername for
  // localStorage persistence, matching the dropdown's behavior.
  onName: (name: string) => void;
  // Whether the user is logged in (gates the Change password section).
  isLoggedIn: () => boolean;
}

export class SettingsModal {
  private ctx: SettingsModalContext;
  private overlay: HTMLDivElement | null = null;
  private onKey: ((ev: KeyboardEvent) => void) | null = null;
  private selectedSprite: string = "";

  constructor(ctx: SettingsModalContext) {
    this.ctx = ctx;
  }

  isOpen(): boolean {
    return this.overlay !== null;
  }

  toggle(): void {
    if (this.overlay) this.close();
    else this.open();
  }

  open(): void {
    if (this.overlay) return;
    const ctx = this.ctx;

    const overlay = document.createElement("div");
    overlay.style.cssText =
      "position:fixed;inset:0;z-index:40;background:rgba(0,0,0,0.5);display:flex;align-items:center;justify-content:center;font-family:sans-serif;";
    this.overlay = overlay;

    const win = document.createElement("div");
    win.style.cssText =
      "background:#2d2d3a;color:#fff;border-radius:12px;padding:20px;min-width:380px;max-width:560px;max-height:85vh;overflow:auto;box-shadow:0 8px 24px rgba(0,0,0,0.5);";
    win.addEventListener("click", (e) => e.stopPropagation());
    overlay.appendChild(win);

    // Header
    const header = document.createElement("div");
    header.style.cssText = "display:flex;justify-content:space-between;align-items:center;margin-bottom:8px;";
    const title = document.createElement("div");
    title.style.cssText = "font-size:16px;font-weight:700;";
    title.textContent = "Settings";
    header.appendChild(title);
    const closeBtn = document.createElement("button");
    closeBtn.textContent = "✕";
    closeBtn.style.cssText = PILL_STYLE + "padding:4px 10px;font-size:14px;background:#444;";
    closeBtn.addEventListener("click", () => this.close());
    header.appendChild(closeBtn);
    win.appendChild(header);

    // 1. Character name
    const nameLabel = document.createElement("div");
    nameLabel.style.cssText = SECTION_LABEL_STYLE;
    nameLabel.textContent = "Character name";
    win.appendChild(nameLabel);

    const nameRow = document.createElement("div");
    nameRow.style.cssText = "display:flex;gap:6px;";
    win.appendChild(nameRow);

    const nameInput = document.createElement("input");
    nameInput.type = "text";
    nameInput.value = getUsername() ?? "";
    nameInput.placeholder = "Enter a username";
    nameInput.style.cssText = INPUT_STYLE;
    nameRow.appendChild(nameInput);

    const nameSave = document.createElement("button");
    nameSave.textContent = "Save";
    nameSave.style.cssText = PILL_STYLE + "padding:6px 12px;";
    nameSave.addEventListener("click", () => {
      const name = nameInput.value.trim();
      setUsername(name);
      ctx.onName(name);
    });
    nameRow.appendChild(nameSave);

    // 2. Show my name tag
    const nameTagLabel = document.createElement("div");
    nameTagLabel.style.cssText = SECTION_LABEL_STYLE;
    nameTagLabel.textContent = "Player options";
    win.appendChild(nameTagLabel);

    const nameTagRow = document.createElement("div");
    nameTagRow.style.cssText = "display:flex;align-items:center;gap:6px;";
    win.appendChild(nameTagRow);

    const nameTagCheckbox = document.createElement("input");
    nameTagCheckbox.type = "checkbox";
    nameTagCheckbox.id = "settings-show-name-tag";
    nameTagCheckbox.style.cssText = "width:16px;height:16px;cursor:pointer;";
    nameTagCheckbox.checked = parsePlayerOptions(ctx.getPlayerOptions()).show_own_name_tag ?? true;
    nameTagRow.appendChild(nameTagCheckbox);

    const nameTagText = document.createElement("label");
    nameTagText.htmlFor = "settings-show-name-tag";
    nameTagText.textContent = "Show my name tag above my avatar";
    nameTagText.style.cssText = "color:#fff;font-size:14px;cursor:pointer;";
    nameTagRow.appendChild(nameTagText);

    nameTagCheckbox.addEventListener("change", () => {
      const opts = parsePlayerOptions(ctx.getPlayerOptions());
      opts.show_own_name_tag = nameTagCheckbox.checked;
      ctx.onPlayerOptions(JSON.stringify(opts));
    });

    // 3. Spritesheet picker
    const spriteLabel = document.createElement("div");
    spriteLabel.style.cssText = SECTION_LABEL_STYLE;
    spriteLabel.textContent = "Character sheet";
    win.appendChild(spriteLabel);

    let currentSprite = ctx.getLocalSpriteBase();
    this.selectedSprite = currentSprite;

    if (ctx.spriteBases.length === 0) {
      const empty = document.createElement("div");
      empty.style.cssText = "color:#aaa;font-size:13px;";
      empty.textContent = "No character sheets available.";
      win.appendChild(empty);
    } else {
      const grid = document.createElement("div");
      grid.style.cssText =
        "display:grid;grid-template-columns:repeat(auto-fill,minmax(72px,1fr));gap:8px;max-height:220px;overflow:auto;padding:4px;";
      win.appendChild(grid);

      // All selectable tiles (including the fallback "" entry) so a single
      // highlight function can update every border on selection change.
      const tiles: { id: string; border: HTMLDivElement }[] = [];
      const highlight = (id: string) => {
        for (const t of tiles) t.border.style.borderColor = t.id === id ? ACCENT : "transparent";
      };

      // Apply button — created before the tile loop so tile click handlers can
      // reference syncApplyBtn. Appended to the DOM after the grid.
      const applyBtn = document.createElement("button");
      applyBtn.textContent = "Apply";
      applyBtn.style.cssText = PILL_STYLE + "padding:6px 12px;";
      const syncApplyBtn = () => {
        applyBtn.disabled = this.selectedSprite === currentSprite;
        applyBtn.style.opacity = applyBtn.disabled ? "0.4" : "1";
      };

      for (const base of ctx.spriteBases) {
        const tile = document.createElement("div");
        tile.style.cssText =
          "display:flex;flex-direction:column;align-items:center;gap:4px;cursor:pointer;padding:4px;border-radius:8px;background:#1a1a2e;";
        tile.title = base.name;

        const border = document.createElement("div");
        border.style.cssText = "padding:2px;border-radius:6px;border:2px solid transparent;";
        const img = document.createElement("img");
        img.src = base.url;
        img.alt = base.name;
        // Show the sheet scaled down; image-rendering:pixelated keeps it crisp.
        img.style.cssText = "width:48px;height:96px;image-rendering:pixelated;display:block;object-fit:contain;";
        img.draggable = false;
        border.appendChild(img);
        tile.appendChild(border);

        const cap = document.createElement("div");
        cap.style.cssText = "font-size:10px;color:#aaa;max-width:72px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;";
        cap.textContent = base.name;
        tile.appendChild(cap);

        // Click only selects (highlights); the Apply button sends the change.
        tile.addEventListener("click", () => {
          this.selectedSprite = base.id;
          highlight(base.id);
          syncApplyBtn();
        });

        tiles.push({ id: base.id, border });
        grid.appendChild(tile);
      }

      // Fallback / clear option — reverts to the guest default sheet.
      const fallbackTile = document.createElement("div");
      fallbackTile.style.cssText =
        "display:flex;flex-direction:column;align-items:center;justify-content:center;gap:4px;cursor:pointer;padding:4px;border-radius:8px;background:#1a1a2e;min-height:120px;";
      fallbackTile.title = "Default (no sheet)";
      const fbBorder = document.createElement("div");
      fbBorder.style.cssText =
        "padding:2px;border-radius:6px;border:2px solid transparent;font-size:11px;color:#aaa;width:48px;height:96px;display:flex;align-items:center;justify-content:center;text-align:center;";
      fbBorder.textContent = "auto";
      fallbackTile.appendChild(fbBorder);
      const fbCap = document.createElement("div");
      fbCap.style.cssText = "font-size:10px;color:#aaa;";
      fbCap.textContent = "Default";
      fallbackTile.appendChild(fbCap);
      fallbackTile.addEventListener("click", () => {
        this.selectedSprite = "";
        highlight("");
        syncApplyBtn();
      });
      tiles.push({ id: "", border: fbBorder });
      grid.appendChild(fallbackTile);

      highlight(this.selectedSprite);

      applyBtn.addEventListener("click", () => {
        ctx.onSpriteBase(this.selectedSprite);
        currentSprite = this.selectedSprite;
        syncApplyBtn();
      });
      syncApplyBtn();

      const applyRow = document.createElement("div");
      applyRow.style.cssText = "display:flex;gap:6px;margin-top:8px;";
      applyRow.appendChild(applyBtn);
      win.appendChild(applyRow);
    }

    // 4. Notification sounds
    const soundLabel = document.createElement("div");
    soundLabel.style.cssText = SECTION_LABEL_STYLE;
    soundLabel.textContent = "Audio";
    win.appendChild(soundLabel);

    const soundRow = document.createElement("div");
    soundRow.style.cssText = "display:flex;align-items:center;gap:6px;";
    win.appendChild(soundRow);

    const soundCheckbox = document.createElement("input");
    soundCheckbox.type = "checkbox";
    soundCheckbox.id = "settings-sound";
    soundCheckbox.style.cssText = "width:16px;height:16px;cursor:pointer;";
    soundCheckbox.checked = isSoundEnabled();
    soundRow.appendChild(soundCheckbox);

    const soundText = document.createElement("label");
    soundText.htmlFor = "settings-sound";
    soundText.textContent = "Notification sounds (ping + interaction clicks)";
    soundText.style.cssText = "color:#fff;font-size:14px;cursor:pointer;";
    soundRow.appendChild(soundText);

    soundCheckbox.addEventListener("change", () => {
      setSoundEnabled(soundCheckbox.checked);
    });

    // 5. Change password (logged-in only)
    if (ctx.isLoggedIn()) {
      const pwLabel = document.createElement("div");
      pwLabel.style.cssText = SECTION_LABEL_STYLE;
      pwLabel.textContent = "Account";
      win.appendChild(pwLabel);

      const pwBtn = document.createElement("button");
      pwBtn.textContent = "Change password";
      pwBtn.style.cssText = PILL_STYLE + "padding:6px 12px;";
      pwBtn.addEventListener("click", () => {
        window.location.href = "/forgot-password";
      });
      win.appendChild(pwBtn);
    }

    overlay.addEventListener("click", () => this.close());
    this.onKey = (ev: KeyboardEvent) => {
      if (ev.key === "Escape") this.close();
    };
    document.addEventListener("keydown", this.onKey);

    document.body.appendChild(overlay);
    nameInput.focus();
  }

  close(): void {
    if (this.onKey) {
      document.removeEventListener("keydown", this.onKey);
      this.onKey = null;
    }
    if (this.overlay) {
      this.overlay.remove();
      this.overlay = null;
    }
  }
}
