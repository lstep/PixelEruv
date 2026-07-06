// AvClient wraps the LiveKit browser SDK. It manages room connection
// lifecycle (connect/disconnect on token frames from ext-av), publishes
// mic + camera tracks, and exposes subscribed participant tracks for the
// GameScene to render as billboarded video tiles.
//
// The LiveKit SDK is lazy-loaded (dynamic import) so it only adds to the
// bundle when A/V is actually used.

import type { Room, RemoteParticipant, LocalParticipant, Track, TrackPublication } from "livekit-client";

export interface AvParticipant {
  entityId: string;
  hasCamera: boolean;
  hasMic: boolean;
  cameraTrack: Track | null;
}

export type AvParticipantsHandler = (participants: Map<string, AvParticipant>) => void;

export class AvClient {
  private room: Room | null = null;
  private currentRoom: string | null = null;
  private micMuted = false;
  private cameraEnabled = false;
  // LiveKit SDK module, loaded on first use.
  private lkModule: typeof import("livekit-client") | null = null;
  // Participants indexed by entity_id (LiveKit participant identity).
  private participants = new Map<string, AvParticipant>();
  private onParticipantsChange: AvParticipantsHandler | null = null;

  setParticipantsHandler(handler: AvParticipantsHandler): void {
    this.onParticipantsChange = handler;
  }

  setMicMuted(muted: boolean): void {
    this.micMuted = muted;
    if (this.room) {
      this.room.localParticipant.setMicrophoneEnabled(!muted);
    }
  }

  setCameraEnabled(enabled: boolean): void {
    this.cameraEnabled = enabled;
    if (this.room) {
      this.room.localParticipant.setCameraEnabled(enabled);
    }
  }

  isMicMuted(): boolean {
    return this.micMuted;
  }

  isCameraEnabled(): boolean {
    return this.cameraEnabled;
  }

  // handleTokenFrame processes an AvTokenFrame from the server. On "join",
  // it disconnects from any current room and connects to the new one. On
  // "leave", it disconnects from the specified room.
  async handleTokenFrame(msg: {
    action: string;
    room: string;
    token: string;
    url: string;
    members: string[];
  }): Promise<void> {
    if (msg.action === "leave") {
      if (this.currentRoom === msg.room) {
        await this.disconnect();
      }
      return;
    }

    if (msg.action === "join") {
      // Disconnect from the current room before joining a new one.
      if (this.currentRoom && this.currentRoom !== msg.room) {
        await this.disconnect();
      }
      await this.connect(msg.url, msg.token, msg.room);
    }
  }

  private async ensureModule(): Promise<typeof import("livekit-client")> {
    if (!this.lkModule) {
      this.lkModule = await import("livekit-client");
    }
    return this.lkModule;
  }

  private async connect(url: string, token: string, roomName: string): Promise<void> {
    const lk = await this.ensureModule();
    this.room = new lk.Room({ adaptiveStream: true, dynacast: true });
    this.currentRoom = roomName;
    this.participants.clear();

    // Set up event handlers before connecting.
    this.room.on(lk.RoomEvent.ParticipantConnected, (participant: RemoteParticipant) => {
      this.updateParticipant(participant.identity, participant);
    });
    this.room.on(lk.RoomEvent.ParticipantDisconnected, (participant: RemoteParticipant) => {
      this.participants.delete(participant.identity);
      this.notifyChange();
    });
    this.room.on(lk.RoomEvent.TrackSubscribed, (_track: Track, pub: any, participant: RemoteParticipant) => {
      this.updateParticipant(participant.identity, participant);
    });
    this.room.on(lk.RoomEvent.TrackUnsubscribed, (_track: Track, _pub: TrackPublication, participant: RemoteParticipant) => {
      this.updateParticipant(participant.identity, participant);
    });

    await this.room.connect(url, token);
    console.log(`AvClient: connected to room ${roomName}`);

    // Publish mic + camera tracks based on current settings.
    await this.room.localParticipant.setMicrophoneEnabled(!this.micMuted);
    await this.room.localParticipant.setCameraEnabled(this.cameraEnabled);

    // Populate existing participants.
    for (const p of this.room.remoteParticipants.values()) {
      this.updateParticipant(p.identity, p);
    }
    this.notifyChange();
  }

  private async disconnect(): Promise<void> {
    if (this.room) {
      await this.room.disconnect();
      console.log(`AvClient: disconnected from room ${this.currentRoom}`);
      this.room = null;
      this.currentRoom = null;
      this.participants.clear();
      this.notifyChange();
    }
  }

  private updateParticipant(identity: string, participant: RemoteParticipant): void {
    const hasCamera = participant.isCameraEnabled;
    const hasMic = participant.isMicrophoneEnabled;
    let cameraTrack: Track | null = null;
    for (const pub of participant.getTrackPublications().values()) {
      if (pub.track && pub.source === "camera") {
        cameraTrack = pub.track;
        break;
      }
    }
    this.participants.set(identity, {
      entityId: identity,
      hasCamera,
      hasMic,
      cameraTrack,
    });
    this.notifyChange();
  }

  private notifyChange(): void {
    if (this.onParticipantsChange) {
      this.onParticipantsChange(new Map(this.participants));
    }
  }

  getParticipants(): Map<string, AvParticipant> {
    return new Map(this.participants);
  }

  // setParticipantVolume adjusts the audio volume of a remote participant
  // based on their distance from the local player. Called each tick by the
  // GameScene with the computed volume (0..1).
  setParticipantVolume(entityId: string, volume: number): void {
    if (!this.room) return;
    const participant = this.room.remoteParticipants.get(entityId);
    if (!participant) return;
    for (const pub of participant.getTrackPublications().values()) {
      if (pub.track && pub.kind === "audio") {
        const audioTrack = pub.track as any;
        if (audioTrack.setVolume) {
          audioTrack.setVolume(volume);
        }
      }
    }
  }

  async close(): Promise<void> {
    await this.disconnect();
  }
}
