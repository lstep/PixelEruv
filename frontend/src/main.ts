import Phaser from "phaser";
import { GameScene } from "./scenes/GameScene";
import { CharacterSelectScene, shouldShowCharacterSelect } from "./scenes/CharacterSelectScene";
import { initOtel, tracer } from "./otel";
import { loadMapAssets, type MapAssets } from "./mapLoader";
import { loadSpriteBases, type SpriteBaseAsset } from "./spriteLoader";
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
// preload() has the URLs ready. Falls back to static files in /maps/ if
// PocketBase is unavailable (e.g. not yet set up in dev).
async function bootstrap(): Promise<void> {
  // Handle OAuth callback from Dex.
  if (window.location.pathname === "/auth/callback") {
    handleAuthCallback();
    return; // handleAuthCallback redirects
  }

  const topMenu = new TopMenu();
  const chatPanel = new ChatPanel();
  topMenu.setChatPanel(chatPanel);

  let mapAssets: MapAssets | null = null;
  const span = tracer.startSpan("map.load", { attributes: { "map.source": "pocketbase" } });
  try {
    mapAssets = await loadMapAssets();
    span.setAttribute("map.fallback", false);
    console.log("loaded map from PocketBase");
  } catch (err) {
    span.setAttribute("map.fallback", true);
    span.recordException(err as Error);
    console.warn("PocketBase map load failed, falling back to static files:", err);
  } finally {
    span.end();
  }

  // Fetch the sprite catalog from PocketBase (parallel with map load would be
  // ideal, but keeping it sequential matches the existing pattern and the
  // catalog is small). Empty array = PB unavailable; GameScene falls back to
  // static char_0..char_4.
  const spriteBases = await loadSpriteBases();

  const game = new Phaser.Game(config);
  game.registry.set("mapAssets", mapAssets);
  game.registry.set("spriteBases", spriteBases);
  game.registry.set("topMenu", topMenu);
  game.registry.set("chatPanel", chatPanel);
}

bootstrap();
