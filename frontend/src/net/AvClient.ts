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
  // Increments on each join attempt so stale retry loops can bail out
  // when a newer token frame supersedes them.
  private connectGeneration = 0;

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
      // Retry on transient ICE failures (Docker Desktop UDP forwarding
      // can drop the first STUN binding request, causing peer connection
      // failure). A generation counter cancels stale retries when a newer
      // token frame arrives mid-retry.
      const gen = ++this.connectGeneration;
      const maxAttempts = 3;
      for (let attempt = 1; attempt <= maxAttempts; attempt++) {
        if (gen !== this.connectGeneration) return;
        try {
          await this.connect(msg.url, msg.token, msg.room);
          return;
        } catch (err) {
          if (gen !== this.connectGeneration) return;
          console.error(`AvClient: connect attempt ${attempt}/${maxAttempts} failed`, err);
          if (attempt < maxAttempts) {
            await new Promise((r) => setTimeout(r, 1000));
          }
        }
      }
      console.error("AvClient: exhausted connect retries");
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
      console.log(`[DEBUG-av] ParticipantConnected: identity=${participant.identity}`);
      this.updateParticipant(participant.identity, participant);
    });
    this.room.on(lk.RoomEvent.ParticipantDisconnected, (participant: RemoteParticipant) => {
      console.log(`[DEBUG-av] ParticipantDisconnected: identity=${participant.identity}`);
      this.participants.delete(participant.identity);
      this.notifyChange();
    });
    this.room.on(lk.RoomEvent.TrackSubscribed, (track: Track, pub: any, participant: RemoteParticipant) => {
      console.log(`[DEBUG-av] TrackSubscribed: identity=${participant.identity} source=${pub.source} kind=${track.kind} trackSid=${track.sid}`);
      this.updateParticipant(participant.identity, participant);
    });
    this.room.on(lk.RoomEvent.TrackUnsubscribed, (_track: Track, _pub: TrackPublication, participant: RemoteParticipant) => {
      console.log(`[DEBUG-av] TrackUnsubscribed: identity=${participant.identity}`);
      this.updateParticipant(participant.identity, participant);
    });
    this.room.on(lk.RoomEvent.TrackSubscriptionFailed, (trackSid: string, participant: RemoteParticipant, reason?: any) => {
      console.error(`[DEBUG-av] TrackSubscriptionFailed: identity=${participant.identity} trackSid=${trackSid} reason=${reason}`);
    });
    this.room.on(lk.RoomEvent.ConnectionQualityChanged, (quality: any, participant: any) => {
      console.log(`[DEBUG-av] ConnectionQuality: identity=${participant.identity} quality=${quality}`);
    });

    try {
      await this.room.connect(url, token);
    } catch (err) {
      // Clean up the failed room so retry creates a fresh one.
      try { await this.room.disconnect(); } catch { /* already torn down */ }
      this.room = null;
      this.currentRoom = null;
      throw err;
    }
    console.log(`AvClient: connected to room ${roomName}`);

    // Publish mic + camera tracks based on current settings. These can fail
    // independently (e.g. Safari NotAllowedError if the user denied camera
    // permission) — the room is still connected and can receive remote video.
    // We must NOT let a publishing failure tear down the room connection.
    try {
      await this.room.localParticipant.setMicrophoneEnabled(!this.micMuted);
    } catch (err) {
      console.warn("AvClient: microphone publish failed (room still connected):", err);
    }
    try {
      await this.room.localParticipant.setCameraEnabled(this.cameraEnabled);
    } catch (err) {
      console.warn("AvClient: camera publish failed (room still connected):", err);
    }

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
    const pubs = participant.getTrackPublications();
    console.log(`[DEBUG-av] updateParticipant: identity=${identity} isCameraEnabled=${hasCamera} isMicEnabled=${hasMic} pubCount=${pubs.length}`);
    for (const pub of pubs.values()) {
      console.log(`[DEBUG-av]   pub: source=${pub.source} kind=${pub.kind} hasTrack=${!!pub.track} trackSid=${pub.trackSid}`);
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
