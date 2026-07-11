// ScreenShareOverlay manages ScreenViewer floating windows for all
// participants who are currently sharing their screen. It registers as a
// participant handler on AvClient and creates/destroys ScreenViewer
// instances as screen share tracks appear and disappear.

import type { AvClient, AvParticipant } from "../net/AvClient";
import { ScreenViewer } from "./ScreenViewer";

export interface ScreenShareOverlayOptions {
  avClient: AvClient;
  getName: (entityId: string) => string;
  getLocalEntityId: () => string | null;
}

interface ViewerEntry {
  viewer: ScreenViewer;
  entityId: string;
}

export class ScreenShareOverlay {
  private avClient: AvClient;
  private getName: (entityId: string) => string;
  private getLocalEntityId: () => string | null;
  private viewers = new Map<string, ViewerEntry>();

  constructor(opts: ScreenShareOverlayOptions) {
    this.avClient = opts.avClient;
    this.getName = opts.getName;
    this.getLocalEntityId = opts.getLocalEntityId;

    this.avClient.setParticipantsHandler((p) => this.syncParticipants(p));
  }

  private syncParticipants(participants: Map<string, AvParticipant>): void {
    const localId = this.getLocalEntityId();

    // Remove viewers for participants who stopped sharing.
    for (const [entityId, entry] of this.viewers) {
      const p = participants.get(entityId);
      if (!p || !p.hasScreenShare || !p.screenShareTrack) {
        entry.viewer.destroy();
        this.viewers.delete(entityId);
      }
    }

    // Add/update viewers for participants who are sharing.
    let index = 0;
    for (const [entityId, p] of participants) {
      if (!p.hasScreenShare || !p.screenShareTrack) continue;
      const label = p.isLocal
        ? `${this.getName(entityId) || "You"} (your screen)`
        : `${this.getName(entityId) || entityId} (screen share)`;

      let entry = this.viewers.get(entityId);
      if (!entry) {
        const viewer = new ScreenViewer(p.screenShareTrack, label, index);
        entry = { viewer, entityId };
        this.viewers.set(entityId, entry);
      }
      // Update muted state (source window hidden/minimized).
      entry.viewer.setScreenMuted(p.screenMuted);
      index++;
    }
  }

  destroy(): void {
    for (const entry of this.viewers.values()) {
      entry.viewer.destroy();
    }
    this.viewers.clear();
  }
}
