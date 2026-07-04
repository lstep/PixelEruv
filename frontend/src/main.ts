import Phaser from "phaser";
import { GameScene } from "./scenes/GameScene";
import { initOtel } from "./otel";

// Initialize OpenTelemetry before any instrumented code runs. No-op when
// VITE_OTEL_ENABLED != "true".
initOtel();

const config: Phaser.Types.Core.GameConfig = {
  type: Phaser.AUTO,
  parent: "game",
  width: 640,
  height: 640,
  pixelArt: true,
  backgroundColor: "#1a1a2e",
  scene: [GameScene],
};

new Phaser.Game(config);
