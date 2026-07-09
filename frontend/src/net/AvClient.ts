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
export type AvAudioBlockedHandler = (blocked: boolean) => void;

export class AvClient {
  private room: Room | null = null;
  private currentRoom: string | null = null;
  private micMuted = true;
  private cameraEnabled = false;
  // LiveKit SDK module, loaded on first use.
  private lkModule: typeof import("livekit-client") | null = null;
  // Participants indexed by entity_id (LiveKit participant identity).
  private participants = new Map<string, AvParticipant>();
  private onParticipantsChange: AvParticipantsHandler | null = null;
  // Increments on each join attempt so stale retry loops can bail out
  // when a newer token frame supersedes them.
  private connectGeneration = 0;
  // Notifies the UI when browser autoplay policy blocks audio playback.
  private onAudioBlocked: AvAudioBlockedHandler | null = null;
  // Debounce timer for "leave" events — delays disconnect so momentary
  // proximity zone exits (2-tile radius) don't thrash the LiveKit room.
  private leaveTimer: ReturnType<typeof setTimeout> | null = null;
  // Tracks whether the document-level click listener for startAudio has
  // been installed. Done lazily on first room connect.
  private audioUnlockListenerInstalled = false;

  setParticipantsHandler(handler: AvParticipantsHandler): void {
    this.onParticipantsChange = handler;
  }

  setAudioBlockedHandler(handler: AvAudioBlockedHandler): void {
    this.onAudioBlocked = handler;
  }

  // startAudio resumes audio playback after the browser's autoplay policy
  // blocked it. Must be called within a user gesture handler (click/tap).
  async startAudio(): Promise<void> {
    if (this.room) {
      try {
        await this.room.startAudio();
      } catch (err) {
        console.warn("AvClient: startAudio failed:", err);
      }
    }
  }

  // setMicMuted toggles the local microphone. When no room is connected and
  // the user is unmuting, we pre-request mic permission via getUserMedia so
  // the browser prompt fires at click time (not later, when proximity joins
  // a LiveKit room and would interrupt the user). On denial the flag reverts.
  async setMicMuted(muted: boolean): Promise<void> {
    if (!muted && !this.room) {
      try {
        await this.requestPermission("audio");
      } catch {
        return; // denied — keep previous mute state
      }
    }
    this.micMuted = muted;
    if (this.room) {
      this.room.localParticipant.setMicrophoneEnabled(!muted);
      // Unmuting is a user gesture — proactively unlock audio playback
      // in case the browser's autoplay policy blocked remote audio.
      if (!muted) {
        this.room.startAudio().catch(() => {});
      }
    }
  }

  // setCameraEnabled toggles the local camera. Same pre-prompt rationale as
  // setMicMuted: trigger the permission prompt at click time when alone, so
  // proximity-driven room joins don't surface an interrupting popup.
  async setCameraEnabled(enabled: boolean): Promise<void> {
    if (enabled && !this.room) {
      try {
        await this.requestPermission("video");
      } catch {
        return; // denied — keep previous camera-off state
      }
    }
    this.cameraEnabled = enabled;
    if (this.room) {
      this.room.localParticipant.setCameraEnabled(enabled);
      // Camera toggle is a user gesture — proactively unlock audio.
      this.room.startAudio().catch(() => {});
    }
  }

  // requestPermission triggers the browser's mic/camera permission prompt by
  // acquiring the matching media stream and immediately stopping its tracks.
  // This caches the grant so later LiveKit publishing reuses it without a
  // prompt. Resolves silently when mediaDevices is unavailable (insecure
  // context), falling back to LiveKit's connect-time prompt.
  private async requestPermission(kind: "audio" | "video"): Promise<void> {
    if (!navigator.mediaDevices?.getUserMedia) return;
    const stream = await navigator.mediaDevices.getUserMedia({
      audio: kind === "audio",
      video: kind === "video",
    });
    for (const t of stream.getTracks()) t.stop();
  }

  isMicMuted(): boolean {
    return this.micMuted;
  }

  isCameraEnabled(): boolean {
    return this.cameraEnabled;
  }

  // handleTokenFrame processes an AvTokenFrame from the server. On "join",
  // it connects to the new room (skipping if already connected to the same
  // room). On "leave", it disconnects after a short debounce so momentary
  // proximity zone exits don't thrash the LiveKit connection.
  async handleTokenFrame(msg: {
    action: string;
    room: string;
    token: string;
    url: string;
    members: string[];
  }): Promise<void> {
    if (msg.action === "leave") {
      if (this.currentRoom === msg.room) {
        // Debounce: delay disconnect by 1.5s so a rapid re-join for the
        // same (or a new) room can cancel it. With a 2-tile proximity
        // radius, players briefly step out of range and back in within a
        // few ticks — immediate disconnect would tear down the LiveKit
        // room and require a fresh user gesture to re-enable audio.
        if (this.leaveTimer) clearTimeout(this.leaveTimer);
        const leavingRoom = msg.room;
        this.leaveTimer = setTimeout(async () => {
          this.leaveTimer = null;
          if (this.currentRoom === leavingRoom) {
            await this.disconnect();
          }
        }, 1500);
      }
      return;
    }

    if (msg.action === "join") {
      // Cancel any pending leave — we're rejoining (possibly the same room).
      if (this.leaveTimer) {
        clearTimeout(this.leaveTimer);
        this.leaveTimer = null;
      }
      // Skip if already connected to this room — avoids creating a new
      // Room object and orphaning the old one's WebRTC/audio elements.
      if (this.currentRoom === msg.room && this.room) {
        return;
      }
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
      this.updateParticipant(participant.identity, participant);
    });
    this.room.on(lk.RoomEvent.ParticipantDisconnected, (participant: RemoteParticipant) => {
      this.participants.delete(participant.identity);
      this.notifyChange();
    });
    this.room.on(lk.RoomEvent.TrackSubscribed, (track: Track, pub: any, participant: RemoteParticipant) => {
      this.updateParticipant(participant.identity, participant);
    });
    this.room.on(lk.RoomEvent.TrackUnsubscribed, (_track: Track, _pub: TrackPublication, participant: RemoteParticipant) => {
      this.updateParticipant(participant.identity, participant);
    });
    // Browsers block audio playback not initiated by a user gesture. When
    // LiveKit's auto-play is blocked, notify the UI so it can show an
    // "Enable Audio" button (whose click calls room.startAudio()).
    this.room.on(lk.RoomEvent.AudioPlaybackStatusChanged, () => {
      const blocked = !this.room?.canPlaybackAudio;
      this.onAudioBlocked?.(blocked);
    });

    // Upgrade the LiveKit signaling URL to a secure scheme when the page is
    // served over HTTPS. Browsers block insecure ws:// from an HTTPS page
    // (mixed content), so a server-provided ws:// URL would fail. The LiveKit
    // SDK derives both the signaling WebSocket and the /rtc/v1/validate fetch
    // from this URL's scheme, so upgrading here fixes both.
    if (window.location.protocol === "https:" && url.startsWith("ws://")) {
      url = "wss://" + url.slice("ws://".length);
    } else if (window.location.protocol === "https:" && url.startsWith("http://")) {
      url = "https://" + url.slice("http://".length);
    }

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

    // Install a document-level click listener to unlock audio playback.
    // Browsers block audio autoplay unless startAudio() is called within a
    // user gesture. The user typically clicks mic/camera BEFORE the room
    // connects (proximity join happens later), so the startAudio() calls
    // in setMicMuted/setCameraEnabled miss the room. This listener catches
    // ANY click after the room connects, unlocking audio on the next
    // interaction (movement key, menu click, etc.). Installed once.
    if (!this.audioUnlockListenerInstalled) {
      this.audioUnlockListenerInstalled = true;
      document.addEventListener("click", () => {
        if (this.room && !this.room.canPlaybackAudio) {
          this.room.startAudio().catch(() => {});
          this.onAudioBlocked?.(false);
        }
      });
    }

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
    if (this.leaveTimer) {
      clearTimeout(this.leaveTimer);
      this.leaveTimer = null;
    }
    if (this.room) {
      await this.room.disconnect();
      console.log(`AvClient: disconnected from room ${this.currentRoom}`);
      this.room = null;
      this.currentRoom = null;
      this.participants.clear();
      this.onAudioBlocked?.(false);
      this.notifyChange();
    }
  }

  private updateParticipant(identity: string, participant: RemoteParticipant): void {
    const hasCamera = participant.isCameraEnabled;
    const hasMic = participant.isMicrophoneEnabled;
    let cameraTrack: Track | null = null;
    const pubs = participant.getTrackPublications();
    for (const pub of pubs.values()) {
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
