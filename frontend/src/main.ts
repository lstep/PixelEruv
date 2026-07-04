import Phaser from "phaser";
import { GameScene } from "./scenes/GameScene";

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
