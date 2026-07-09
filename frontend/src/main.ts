import Phaser from "phaser";
import { GameScene } from "./scenes/GameScene";
import { CharacterSelectScene, shouldShowCharacterSelect } from "./scenes/CharacterSelectScene";
import { initOtel } from "./otel";
import { loadMapAssets } from "./mapLoader";
import { loadSpriteBases } from "./spriteLoader";
import { handleAuthCallback } from "./auth";
import { TopMenu } from "./ui/TopMenu";
import { ChatPanel } from "./ui/ChatPanel";

// Initialize OpenTelemetry before any instrumented code runs. No-op when
// VITE_OTEL_ENABLED != "true".
initOtel();

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
  // Handle OAuth callback from Dex.
  if (window.location.pathname === "/auth/callback") {
    handleAuthCallback();
    return; // handleAuthCallback redirects
  }

  const topMenu = new TopMenu();
  const chatPanel = new ChatPanel();
  topMenu.setChatPanel(chatPanel);

  const mapAssets = await loadMapAssets();
  console.log("loaded map from PocketBase");

  // Fetch the sprite catalog from PocketBase. Empty array means the
  // sprite_bases collection has not been seeded yet; the default char_0..char_4
  // sheets are always available as a baseline.
  const spriteBases = await loadSpriteBases();

  const game = new Phaser.Game(config);
  game.registry.set("mapAssets", mapAssets);
  game.registry.set("spriteBases", spriteBases);
  game.registry.set("topMenu", topMenu);
  game.registry.set("chatPanel", chatPanel);
}

bootstrap();
