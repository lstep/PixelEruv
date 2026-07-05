import Phaser from "phaser";
import { GameScene } from "./scenes/GameScene";
import { initOtel, tracer } from "./otel";
import { loadMapAssets, type MapAssets } from "./mapLoader";
import { handleAuthCallback, getIdToken, redirectToLogin, isLoggedIn } from "./auth";

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
  scene: [GameScene],
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

  // Require login before starting the game.
  if (!isLoggedIn()) {
    await redirectToLogin();
    return;
  }

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

  const game = new Phaser.Game(config);
  game.registry.set("mapAssets", mapAssets);
}

bootstrap();
