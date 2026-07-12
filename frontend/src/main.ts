import Phaser from "phaser";
import { GameScene } from "./scenes/GameScene";
import { CharacterSelectScene, shouldShowCharacterSelect } from "./scenes/CharacterSelectScene";
import { initOtel } from "./otel";
import { loadMapAssets } from "./mapLoader";
import { loadSpriteBases } from "./spriteLoader";
import { TopMenu } from "./ui/TopMenu";
import { ChatPanel } from "./ui/ChatPanel";
import { renderRegisterPage, renderLoginPage, renderVerifyEmailPage } from "./ui/AuthPage";

// Initialize OpenTelemetry before any instrumented code runs. No-op when
// VITE_OTEL_ENABLED != "true".
initOtel();

// Poll /healthz every 10 seconds and display the kernel version in a small
// bottom-left badge. Fire-and-forget — failures leave the badge as-is.
function pollVersion(): void {
  const badge = document.getElementById("version-badge");
  if (!badge) return;
  fetch("/healthz")
    .then((res) => res.json())
    .then((data) => {
      const kernel = (data.services as { service: string; version: string }[])
        ?.find((s) => s.service === "kernel");
      if (kernel?.version) badge.textContent = kernel.version;
    })
    .catch(() => {}); // ignore — badge stays as-is on failure
}
pollVersion();
setInterval(pollVersion, 10_000);

const config: Phaser.Types.Core.GameConfig = {
  type: Phaser.AUTO,
  parent: "game",
  width: "100%",
  height: "100%",
  pixelArt: true,
  backgroundColor: "#1a1a2e",
  scale: {
    mode: Phaser.Scale.RESIZE,
    autoCenter: Phaser.Scale.CENTER_BOTH,
  },
  scene: [CharacterSelectScene, GameScene],
};

// Fetch map assets from PocketBase before starting Phaser so that GameScene's
// preload() has the URLs ready. PocketBase must be available; there is no
// static fallback.
async function bootstrap(): Promise<void> {
  // Handle auth pages — render DOM forms instead of starting Phaser.
  const path = window.location.pathname;
  if (path === "/register") {
    renderRegisterPage();
    return;
  }
  if (path === "/login") {
    renderLoginPage();
    return;
  }
  if (path === "/verify-email") {
    renderVerifyEmailPage();
    return;
  }

  const topMenu = new TopMenu();
  const chatPanel = new ChatPanel();
  topMenu.setChatPanel(chatPanel);

  // Wait for the rounded UI font so Phaser Text objects render with it
  // instead of falling back to sans-serif.
  try {
    await document.fonts.load("700 13px Nunito");
  } catch {
    // Font load failure is non-fatal — text falls back to sans-serif.
  }

  const mapAssets = await loadMapAssets();
  console.log("loaded map from PocketBase");

  // Fetch the sprite catalog from PocketBase. Empty array means the
  // sprite_bases collection has not been seeded yet; the default char_0..char_3
  // sheets are always available as a baseline.
  const spriteBases = await loadSpriteBases();

  const game = new Phaser.Game(config);
  game.registry.set("mapAssets", mapAssets);
  game.registry.set("spriteBases", spriteBases);
  game.registry.set("topMenu", topMenu);
  game.registry.set("chatPanel", chatPanel);
  // Track which map was initially loaded so onReady can detect if the
  // server wants the player on a different map (saved map_id in PB).
  game.registry.set("loadedMapName", import.meta.env.VITE_MAP_NAME || "main");
}

bootstrap();
