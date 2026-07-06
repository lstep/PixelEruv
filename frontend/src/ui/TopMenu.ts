// TopMenu is a floating DOM bar fixed to the top of the browser window,
// independent of the Phaser canvas/scene lifecycle. It shows a Login/Logout
// button and a dropdown menu with a username field. Styled as dark rounded
// pills, matching the AvOverlay HUD controls.

import { isLoggedIn, redirectToLogin, logout } from "../auth";
import { getUsername, setUsername } from "../username";

const PILL_STYLE =
  "padding:8px 16px;font-size:14px;font-family:sans-serif;font-weight:600;background:#2d2d3a;color:#fff;border:none;border-radius:20px;cursor:pointer;";

export class TopMenu {
  private container: HTMLDivElement;
  private dropdown: HTMLDivElement;

  constructor() {
    this.container = document.createElement("div");
    this.container.style.cssText =
      "position:fixed;top:12px;right:12px;display:flex;gap:8px;align-items:flex-start;z-index:20;font-family:sans-serif;";
    document.body.appendChild(this.container);

    const authBtn = document.createElement("button");
    authBtn.textContent = isLoggedIn() ? "Logout" : "Login";
    authBtn.style.cssText = PILL_STYLE + (isLoggedIn() ? "" : "background:#4c5cf0;");
    authBtn.addEventListener("click", () => {
      if (isLoggedIn()) logout();
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
      setUsername(input.value.trim());
      this.dropdown.style.display = "none";
    });
    row.appendChild(saveBtn);

    menuBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      this.dropdown.style.display = this.dropdown.style.display === "none" ? "block" : "none";
    });
    document.addEventListener("click", () => {
      this.dropdown.style.display = "none";
    });
    this.dropdown.addEventListener("click", (e) => e.stopPropagation());
  }
}
