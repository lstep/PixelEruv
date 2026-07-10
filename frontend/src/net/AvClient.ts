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
  isLocal: boolean;
  hasCamera: boolean;
  hasMic: boolean;
  cameraTrack: Track | null;
}

export type AvParticipantsHandler = (participants: Map<string, AvParticipant>) => void;
export type AvAudioBlockedHandler = (blocked: boolean) => void;

const MIC_MUTED_KEY = "av.micMuted";
const CAM_ENABLED_KEY = "av.cameraEnabled";

export interface AvDeviceInfo {
  deviceId: string;
  label: string;
  kind: "audioinput" | "videoinput";
}

export class AvClient {
  private room: Room | null = null;
  private currentRoom: string | null = null;
  private micMuted = localStorage.getItem(MIC_MUTED_KEY) === "false" ? false : true;
  private cameraEnabled = localStorage.getItem(CAM_ENABLED_KEY) === "true";
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
  // Selected input device IDs, persisted across room connections so the
  // user's device choice survives disconnect/reconnect cycles.
  private selectedMicId: string | null = null;
  private selectedCamId: string | null = null;

  constructor() {
    console.log(`AvClient: init micMuted=${this.micMuted} cameraEnabled=${this.cameraEnabled}`);
    // Unlock audio on the very first click anywhere on the page. Safari
    // blocks audio autoplay unless a media element was played during a user
    // gesture. By playing a silent sample on the first click (before any
    // LiveKit room exists), we unlock audio for the entire page. When
    // LiveKit later subscribes remote audio tracks, they play automatically
    // — no second click needed. This is the standard WebRTC workaround.
    document.addEventListener("click", () => this.unlockAudio(), { once: true });
  }

  // unlockAudio plays a silent audio element to satisfy browser autoplay
  // policies. After this call, all future audio on the page (including
  // LiveKit remote tracks) plays without restriction.
  private unlockAudio(): void {
    const audio = document.createElement("audio");
    // 1-byte silent WAV — the smallest valid audio file.
    audio.src = "data:audio/wav;base64,UklGRiQAAABXQVZFZm10IBAAAAABAAEARKwAAIhYAQACABAAZGF0YQAAAAA=";
    audio.play().then(() => audio.remove()).catch(() => {});
  }

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
    console.log(`AvClient.setMicMuted(${muted}) room=${!!this.room}`);
    if (!muted && !this.room) {
      try {
        await this.requestPermission("audio");
      } catch {
        console.log("AvClient.setMicMuted: permission denied, not saving");
        return; // denied — keep previous mute state
      }
    }
    this.micMuted = muted;
    try {
      localStorage.setItem(MIC_MUTED_KEY, String(muted));
      console.log(`AvClient: saved ${MIC_MUTED_KEY}=${muted}`);
    } catch (e) {
      console.error("AvClient: localStorage.setItem failed:", e);
    }
    if (this.room) {
      this.room.localParticipant.setMicrophoneEnabled(!muted);
      if (!muted) {
        this.room.startAudio().catch(() => {});
      }
    }
  }

  // setCameraEnabled toggles the local camera. Same pre-prompt rationale as
  // setMicMuted: trigger the permission prompt at click time when alone, so
  // proximity-driven room joins don't surface an interrupting popup.
  async setCameraEnabled(enabled: boolean): Promise<void> {
    console.log(`AvClient.setCameraEnabled(${enabled}) room=${!!this.room}`);
    if (enabled && !this.room) {
      try {
        await this.requestPermission("video");
      } catch {
        console.log("AvClient.setCameraEnabled: permission denied, not saving");
        return; // denied — keep previous camera-off state
      }
    }
    this.cameraEnabled = enabled;
    try {
      localStorage.setItem(CAM_ENABLED_KEY, String(enabled));
      console.log(`AvClient: saved ${CAM_ENABLED_KEY}=${enabled}`);
    } catch (e) {
      console.error("AvClient: localStorage.setItem failed:", e);
    }
    if (this.room) {
      this.room.localParticipant.setCameraEnabled(enabled);
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

  // getDevices enumerates available audio/video input devices. Requires
  // microphone/camera permission to have been granted for labels to be
  // populated (browsers hide labels until permission is given).
  async getDevices(kind: "audioinput" | "videoinput"): Promise<AvDeviceInfo[]> {
    if (!navigator.mediaDevices?.enumerateDevices) return [];
    const devices = await navigator.mediaDevices.enumerateDevices();
    return devices
      .filter((d) => d.kind === kind)
      .map((d) => ({
        deviceId: d.deviceId,
        label: d.label || (kind === "audioinput" ? "Microphone" : "Camera"),
        kind,
      }));
  }

  // switchDevice changes the active mic or camera. If a room is connected,
  // uses room.switchActiveDevice to swap on the fly. Otherwise stores the
  // deviceId for use when the next room connects.
  async switchDevice(kind: "audioinput" | "videoinput", deviceId: string): Promise<void> {
    if (kind === "audioinput") this.selectedMicId = deviceId;
    else this.selectedCamId = deviceId;
    if (this.room) {
      try {
        await this.room.switchActiveDevice(kind, deviceId);
      } catch (err) {
        console.warn(`AvClient: switchDevice ${kind} failed:`, err);
      }
    }
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
    this.room = new lk.Room({
      adaptiveStream: true,
      dynacast: true,
      // Disable RED (Redundant Audio Data) — enabled by default in the
      // LiveKit SDK, but Safari cannot decode audio/red. Without this,
      // Chrome-published audio is silent on Safari: the track subscribes
      // but no audio data is decoded, so isSpeaking stays false and no
      // sound plays. Forcing audio/opus ensures cross-browser compatibility.
      publishDefaults: { red: false },
      audioCaptureDefaults: this.selectedMicId
        ? { deviceId: this.selectedMicId }
        : undefined,
      videoCaptureDefaults: this.selectedCamId
        ? { deviceId: this.selectedCamId }
        : undefined,
    });
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
      // LiveKit does NOT auto-attach remote audio tracks to audio elements.
      // Without attach(), no <audio> element is created, so setVolume() and
      // startAudio() are no-ops (they iterate attachedElements which is empty).
      // isSpeaking still works (analyzed from RTP packets) but no sound plays.
      // Fix: call attach() to create a hidden <audio> element with autoplay=true.
      if (pub.kind === "audio") {
        (track as any).attach();
        // startAudio plays all attached audio elements (needed if autoplay
        // was initially blocked — the attach() above sets autoplay=true but
        // the browser may have blocked it before the unlock gesture).
        if (this.room) {
          this.room.startAudio().catch(() => { /* may reject if still blocked */ });
        }
      }
      this.updateParticipant(participant.identity, participant);
    });
    this.room.on(lk.RoomEvent.TrackUnsubscribed, (_track: Track, _pub: TrackPublication, participant: RemoteParticipant) => {
      this.updateParticipant(participant.identity, participant);
    });
    // Local track events — keep the local participant entry in sync so the
    // VideoBar self-view appears/disappears when the camera is toggled.
    this.room.on(lk.RoomEvent.LocalTrackPublished, () => this.updateLocalParticipant());
    this.room.on(lk.RoomEvent.LocalTrackUnpublished, () => this.updateLocalParticipant());
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
    // Add the local participant so the VideoBar can show a self-view tile.
    this.updateLocalParticipant();
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
      isLocal: false,
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

  // updateLocalParticipant syncs the local participant entry in the map so
  // the VideoBar can render a self-view tile. The local camera track is read
  // from the local participant's track publications.
  private updateLocalParticipant(): void {
    if (!this.room) return;
    const lp = this.room.localParticipant;
    const identity = lp.identity;
    if (!identity) return;
    let cameraTrack: Track | null = null;
    for (const pub of lp.getTrackPublications().values()) {
      if (pub.source === "camera" && pub.track) {
        cameraTrack = pub.track;
        break;
      }
    }
    this.participants.set(identity, {
      entityId: identity,
      isLocal: true,
      hasCamera: lp.isCameraEnabled,
      hasMic: lp.isMicrophoneEnabled,
      cameraTrack,
    });
    this.notifyChange();
  }

  // getAudioLevel returns the current audio level (0..1) for a participant,
  // read live from the LiveKit SDK. Called each frame by the VideoBar to
  // drive the spectrogram. Returns 0 when no room is connected.
  getAudioLevel(entityId: string): number {
    if (!this.room) return 0;
    const lp = this.room.localParticipant;
    if (entityId === lp.identity) {
      return (lp as any).audioLevel ?? 0;
    }
    const p = this.room.remoteParticipants.get(entityId);
    return (p as any)?.audioLevel ?? 0;
  }

  // isSpeaking returns whether a participant is currently speaking, per
  // LiveKit's voice activity detection.
  isSpeaking(entityId: string): boolean {
    if (!this.room) return false;
    const lp = this.room.localParticipant;
    if (entityId === lp.identity) {
      return (lp as any).isSpeaking ?? false;
    }
    const p = this.room.remoteParticipants.get(entityId);
    return (p as any)?.isSpeaking ?? false;
  }

  // getLocalEntityId returns the LiveKit identity of the local participant,
  // or null when no room is connected.
  getLocalEntityId(): string | null {
    return this.room?.localParticipant.identity ?? null;
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
