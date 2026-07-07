// AvOverlay manages the DOM overlay for LiveKit video tiles. Video tiles
// are HTML <video> elements positioned above avatars each frame,
// billboarded (always upright) and scaled with camera zoom. Mic/camera HUD
// controls live in TopMenu, not here.
//
// The overlay is a single absolutely-positioned <div> on top of the Phaser
// canvas. Video elements are created/removed as participants join/leave.

import type { AvClient, AvParticipant } from "./AvClient";

interface VideoTile {
  video: HTMLVideoElement;
  entityId: string;
}

export class AvOverlay {
  private container: HTMLDivElement;
  private videos = new Map<string, VideoTile>();

  constructor(scene: Phaser.Scene, avClient: AvClient) {
    // Create overlay container on top of the canvas.
    this.container = document.createElement("div");
    this.container.style.cssText = "position:absolute;top:0;left:0;width:100%;height:100%;pointer-events:none;overflow:hidden;z-index:10;";
    // Attach to the same parent as the Phaser canvas.
    const canvas = scene.sys.game.canvas;
    canvas.parentElement?.appendChild(this.container);

    // Wire participant changes.
    avClient.setParticipantsHandler((participants) => this.syncParticipants(participants));
  }

  // syncParticipants creates/removes video elements to match the current
  // participant set. Attaches LiveKit camera tracks to <video> elements.
  private syncParticipants(participants: Map<string, AvParticipant>): void {
    // Remove videos for departed participants.
    for (const [entityId, tile] of this.videos) {
      if (!participants.has(entityId)) {
        tile.video.srcObject = null;
        tile.video.remove();
        this.videos.delete(entityId);
      }
    }
    // Add/update videos for new/existing participants.
    for (const [entityId, p] of participants) {
      let tile = this.videos.get(entityId);
      if (!tile && p.cameraTrack) {
        const video = document.createElement("video");
        video.autoplay = true;
        video.playsInline = true;
        video.muted = true; // don't play own audio from the video element
        video.style.cssText = "position:absolute;border:2px solid #0f0;border-radius:4px;background:#000;object-fit:cover;pointer-events:none;transform:translate(-50%,-50%);";
        this.container.appendChild(video);
        // Attach the LiveKit track to the video element.
        (p.cameraTrack as any).attach(video);
        tile = { video, entityId };
        this.videos.set(entityId, tile);
      }
      if (tile && !p.cameraTrack) {
        // Camera was turned off — remove the video element.
        tile.video.srcObject = null;
        tile.video.remove();
        this.videos.delete(entityId);
      }
    }
  }

  // updatePositions is called each frame to position video tiles above
  // their avatars. avatarScreenPos returns the screen-space {x, y} of an
  // avatar's head (for tile placement), or null if the avatar is offscreen.
  updatePositions(
    avatarScreenPos: (entityId: string) => { x: number; y: number; scale: number } | null,
  ): void {
    for (const [entityId, tile] of this.videos) {
      const pos = avatarScreenPos(entityId);
      if (!pos) {
        tile.video.style.display = "none";
        continue;
      }
      tile.video.style.display = "block";
      // Position above the avatar head.
      const w = 48 * pos.scale;
      const h = 36 * pos.scale;
      tile.video.style.left = `${pos.x - w / 2}px`;
      tile.video.style.top = `${pos.y - h - 8 * pos.scale}px`;
      tile.video.style.width = `${w}px`;
      tile.video.style.height = `${h}px`;
    }
  }

  destroy(): void {
    for (const tile of this.videos.values()) {
      tile.video.srcObject = null;
      tile.video.remove();
    }
    this.videos.clear();
    this.container.remove();
  }
}
