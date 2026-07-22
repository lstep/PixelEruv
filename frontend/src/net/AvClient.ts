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
  hasScreenShare: boolean;
  screenShareTrack: Track | null;
  // True when the remote's screen-share publication is muted — happens when
  // the sharer minimizes/hides the source window. Propagated via the
  // visibility relay (see attachScreenSourceVisibilityRelay).
  screenMuted: boolean;
}

export type AvParticipantsHandler = (participants: Map<string, AvParticipant>) => void;
export type AvAudioBlockedHandler = (blocked: boolean) => void;

const MIC_MUTED_KEY = "av.micMuted";
const CAM_ENABLED_KEY = "av.cameraEnabled";
const NOISE_CANCELLATION_KEY = "av.noiseCancellation";
const MIC_ID_KEY = "av.micId";
const CAM_ID_KEY = "av.camId";
const SPEAKER_ID_KEY = "av.speakerId";

export interface AvDeviceInfo {
  deviceId: string;
  label: string;
  kind: "audioinput" | "videoinput" | "audiooutput";
}

export class AvClient {
  private room: Room | null = null;
  private currentRoom: string | null = null;
  private micMuted = localStorage.getItem(MIC_MUTED_KEY) === "false" ? false : true;
  private cameraEnabled = localStorage.getItem(CAM_ENABLED_KEY) === "true";
  // Do Not Disturb: when true, the client refuses A/V joins and disconnects
  // any active room. Mirrors the server-side exclusion (worldsim skips DND
  // players in proximity clustering; ext-av skips zone token minting and
  // proactively ejects). Defense in depth — a modded client could bypass
  // this, but the server won't mint tokens for DND players.
  private dnd = false;
  // AFK overlay + tab-visibility auto-mute flags. When either is true, all
  // local tracks (mic, camera, screen) are muted to stop broadcasting, but
  // the player stays in the room (unlike DND, which disconnects). When both
  // clear, tracks restore to the user's manual preferences (micMuted /
  // cameraEnabled). These do NOT change the stored preferences — only the
  // actual track state. See documentation/plans/2026-07-22-afk-state-design.md.
  private afkMuted = false;
  private tabHidden = false;
  // WebRTC noise cancellation (noiseSuppression + echoCancellation +
  // autoGainControl). Defaults on — persisted so the user's choice survives
  // reconnects. Not yet wired to the TopMenu; toggle via setNoiseCancellation.
  private noiseCancellation = localStorage.getItem(NOISE_CANCELLATION_KEY) !== "false";
  // LiveKit SDK module, loaded on first use.
  private lkModule: typeof import("livekit-client") | null = null;
  // Participants indexed by entity_id (LiveKit participant identity).
  private participants = new Map<string, AvParticipant>();
  private participantsHandlers = new Set<AvParticipantsHandler>();
  // Increments on each join attempt so stale retry loops can bail out
  // when a newer token frame supersedes them.
  private connectGeneration = 0;
  // Serializes handleTokenFrame calls. Without this, two concurrent "join"
  // frames (e.g. player oscillating on a proximity edge after a disconnect)
  // can both pass the "already connected?" guard — this.currentRoom is set
  // inside connect() after an await, so the second frame sees it still null.
  // Both then call connect(), creating two Room objects that connect to the
  // same LiveKit room with the same identity, triggering a DUPLICATE_IDENTITY
  // server kick and leaving AvClient referencing the dead room.
  private frameQueue: Promise<void> = Promise.resolve();
  // True while disconnect() is in progress. Suppresses the Disconnected event
  // listener so it doesn't double-clean state that disconnect() already clears.
  private disconnecting = false;
  // Notifies the UI when browser autoplay policy blocks audio playback.
  private onAudioBlocked: AvAudioBlockedHandler | null = null;
  // Debounce timer for "leave" events — delays disconnect briefly so a
  // rapid re-join can cancel it. Reduced from 1.5s to 200ms after backend
  // hysteresis + movement gating eliminated boundary oscillation (#88).
  private leaveTimer: ReturnType<typeof setTimeout> | null = null;
  // Selected input device IDs, persisted across room connections so the
  // user's device choice survives disconnect/reconnect cycles.
  // Selected mic/cam/speaker device IDs, persisted across page reloads
  // and room connections. deviceId may be stale after a reload (browser
  // can reassign IDs), but switchDevice restores from these and the
  // TopMenu only restores the select value if the ID is still present.
  private selectedMicId = localStorage.getItem(MIC_ID_KEY);
  private selectedCamId = localStorage.getItem(CAM_ID_KEY);
  private selectedSpeakerId = localStorage.getItem(SPEAKER_ID_KEY);
  // Screen share state — transient (not persisted). Screen sharing only
  // starts on user click when a room is connected (getDisplayMedia requires
  // a user gesture).
  private screenShareEnabled = false;
  // Relay state for the local screen-share source visibility. Browsers fire
  // 'mute' on a getDisplayMedia MediaStreamTrack when the source window is
  // minimized/hidden. LiveKit doesn't propagate this to remotes, so we relay
  // it via track.mute()/unmute() (see attachScreenSourceVisibilityRelay).
  private screenRelayMst: MediaStreamTrack | null = null;
  private screenRelayOnMute: (() => void) | null = null;
  private screenRelayOnUnmute: (() => void) | null = null;
  private localScreenSourceHidden = false;

  constructor() {
    console.log(`AvClient: init micMuted=${this.micMuted} cameraEnabled=${this.cameraEnabled} noiseCancellation=${this.noiseCancellation}`);
    // Set the audio session type to "play-and-record" for video conferencing.
    // On Safari (macOS/iOS), this tells the system to use the VPIO (Voice
    // Processing I/O) unit, which enables hardware-level acoustic echo
    // cancellation. Without this, Safari may not properly set up the AEC
    // reference signal, causing remote participants to hear echo from the
    // Safari user's mic. The API is experimental and Safari-only; other
    // browsers ignore it. Must be set before any getUserMedia call.
    // See: https://developer.mozilla.org/en-US/docs/Web/API/Navigator/audioSession
    if ("audioSession" in navigator) {
      (navigator as any).audioSession.type = "play-and-record";
    }
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
    this.participantsHandlers.add(handler);
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
      const lk = await this.ensureModule();
      const pub = this.room.localParticipant.getTrackPublication(lk.Track.Source.Microphone);
      if (muted) {
        // Pause upstream instead of muting — calls sender.replaceTrack(null),
        // stopping all media flow (not just silence). The track stays
        // published and alive, so resume is instant with no getUserMedia.
        await pub?.pauseUpstream().catch((err) => console.warn("AvClient: mic pauseUpstream failed:", err));
      } else {
        if (pub?.isUpstreamPaused) {
          await pub.resumeUpstream().catch((err) => console.warn("AvClient: mic resumeUpstream failed:", err));
        } else if (!pub) {
          // No track published yet (mic was muted on connect) — publish now.
          this.room.localParticipant.setMicrophoneEnabled(true).catch((err) => console.warn("AvClient: mic publish failed:", err));
        }
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
      const lk = await this.ensureModule();
      const pub = this.room.localParticipant.getTrackPublication(lk.Track.Source.Camera);
      if (!enabled) {
        await pub?.pauseUpstream().catch((err) => console.warn("AvClient: camera pauseUpstream failed:", err));
      } else {
        if (pub?.isUpstreamPaused) {
          await pub.resumeUpstream().catch((err) => console.warn("AvClient: camera resumeUpstream failed:", err));
        } else if (!pub) {
          this.room.localParticipant.setCameraEnabled(true).catch((err) => console.warn("AvClient: camera publish failed:", err));
        }
        this.room.startAudio().catch(() => {});
      }
    }
  }

  // setScreenShareEnabled toggles screen sharing. Requires an active room
  // connection — getDisplayMedia needs a user gesture, so screen sharing can
  // only start on button click when already in an A/V room. If no room is
  // connected, the call is a no-op.
  async setScreenShareEnabled(enabled: boolean): Promise<void> {
    if (!this.room && enabled) {
      console.log("AvClient.setScreenShareEnabled: no room connected, ignoring");
      return;
    }
    this.screenShareEnabled = enabled;
    if (!this.room) return;
    const lk = await this.ensureModule();
    try {
      // CRITICAL: detach the visibility relay BEFORE asking LiveKit to stop
      // the share. Otherwise, the MediaStreamTrack's 'mute' event (fired
      // during disposal) is caught by our relay and propagated as track.mute()
      // → remotes receive isMuted=true just before (or instead of) the
      // unpublish signal, leaving them stuck on a paused/black state.
      if (!enabled) {
        this.detachScreenSourceVisibilityRelay();
      }
      await this.room.localParticipant.setScreenShareEnabled(enabled, { audio: true });
      if (enabled) {
        // Locate the freshly-published screen track and hook our relay.
        for (const pub of this.room.localParticipant.trackPublications.values()) {
          if (pub.source === lk.Track.Source.ScreenShare && pub.track) {
            this.attachScreenSourceVisibilityRelay(pub.track as any);
            break;
          }
        }
      }
      this.updateLocalParticipant();
    } catch (err) {
      this.screenShareEnabled = !enabled; // revert on failure
      console.warn("AvClient: screen share failed:", err);
      throw err;
    }
  }

  isScreenShareEnabled(): boolean {
    return this.screenShareEnabled;
  }

  // Browsers fire 'mute' on a getDisplayMedia MediaStreamTrack when the OS
  // stops compositing the source (window minimized, app hidden). LiveKit's
  // built-in handler only pauses the upstream after a 5s debounce and does
  // NOT propagate isMuted to remote participants — so the other side keeps
  // displaying the last frozen frame with no indication.
  //
  // We relay the native mute/unmute through LiveKit's own mute()/unmute() so
  // RemoteTrackPublication.isMuted flips on every other client and the UI
  // can show a "paused" placeholder.
  private attachScreenSourceVisibilityRelay(track: import("livekit-client").LocalVideoTrack): void {
    this.detachScreenSourceVisibilityRelay();
    const mst = track.mediaStreamTrack;
    if (!mst) return;
    const onMute = () => {
      this.localScreenSourceHidden = true;
      void track.mute().catch(() => undefined);
      this.updateLocalParticipant();
    };
    const onUnmute = () => {
      this.localScreenSourceHidden = false;
      void track.unmute().catch(() => undefined);
      this.updateLocalParticipant();
    };
    mst.addEventListener("mute", onMute);
    mst.addEventListener("unmute", onUnmute);
    this.screenRelayMst = mst;
    this.screenRelayOnMute = onMute;
    this.screenRelayOnUnmute = onUnmute;
    if (mst.muted) onMute();
  }

  private detachScreenSourceVisibilityRelay(): void {
    if (this.screenRelayMst && this.screenRelayOnMute) {
      this.screenRelayMst.removeEventListener("mute", this.screenRelayOnMute);
    }
    if (this.screenRelayMst && this.screenRelayOnUnmute) {
      this.screenRelayMst.removeEventListener("unmute", this.screenRelayOnUnmute);
    }
    this.screenRelayMst = null;
    this.screenRelayOnMute = null;
    this.screenRelayOnUnmute = null;
    this.localScreenSourceHidden = false;
  }

  // requestPermission triggers the browser's mic/camera permission prompt by
  // acquiring the matching media stream and immediately stopping its tracks.
  // This caches the grant so later LiveKit publishing reuses it without a
  // prompt. Resolves silently when mediaDevices is unavailable (insecure
  // context), falling back to LiveKit's connect-time prompt.
  // Public so the TopMenu can request permission when the user opens the
  // device dropdown outside an A/V room — without permission, Safari only
  // enumerates a single "default" device per kind with no labels, so the
  // user can't see their real devices (e.g. "Logitech Webcam").
  async requestPermission(kind: "audio" | "video"): Promise<void> {
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

  // currentRoomName returns the LiveKit room name the client is currently
  // connected to, or null if not in a room. Used by the TopMenu record button
  // to know which room to record.
  currentRoomName(): string | null {
    return this.currentRoom;
  }

  isNoiseCancellationEnabled(): boolean {
    return this.noiseCancellation;
  }

  // setNoiseCancellation toggles WebRTC noise suppression, echo cancellation,
  // and auto gain control on the local microphone. Persists the choice so it
  // survives reconnects. If a room is connected and the mic is published,
  // restarts the track with the new constraints so the change takes effect
  // immediately (no need to reconnect). When the mic is muted/disabled, the
  // new setting applies on the next connect or unmute.
  async setNoiseCancellation(enabled: boolean): Promise<void> {
    console.log(`AvClient.setNoiseCancellation(${enabled}) room=${!!this.room}`);
    this.noiseCancellation = enabled;
    try {
      localStorage.setItem(NOISE_CANCELLATION_KEY, String(enabled));
    } catch (e) {
      console.error("AvClient: localStorage.setItem failed:", e);
    }
    if (this.room) {
      const pub = this.room.localParticipant.getTrackPublication(
        (await this.ensureModule()).Track.Source.Microphone,
      );
      const track = pub?.track;
      if (track && !this.micMuted) {
        try {
          await (track as any).restartTrack(this.buildAudioCaptureOptions());
        } catch (err) {
          console.warn("AvClient: restartTrack for noise cancellation failed:", err);
        }
      }
    }
  }

  // getDevices enumerates available audio/video input devices. Requires
  // microphone/camera permission to have been granted for labels to be
  // populated (browsers hide labels until permission is given).
  async getDevices(kind: "audioinput" | "videoinput" | "audiooutput"): Promise<AvDeviceInfo[]> {
    if (!navigator.mediaDevices?.enumerateDevices) return [];
    const devices = await navigator.mediaDevices.enumerateDevices();
    const fallback = kind === "audioinput" ? "Microphone" : kind === "videoinput" ? "Camera" : "Speakers";
    return devices
      .filter((d) => d.kind === kind)
      .map((d) => ({
        deviceId: d.deviceId,
        label: d.label || fallback,
        kind,
      }));
  }

  // switchDevice changes the active mic, camera, or speaker. If a room is
  // connected, uses room.switchActiveDevice to swap on the fly. Otherwise
  // stores the deviceId for use when the next room connects. Persists all
  // three selections to localStorage so they survive page reloads.
  async switchDevice(kind: "audioinput" | "videoinput" | "audiooutput", deviceId: string): Promise<void> {
    const key = kind === "audioinput" ? MIC_ID_KEY
      : kind === "videoinput" ? CAM_ID_KEY
      : SPEAKER_ID_KEY;
    if (kind === "audioinput") this.selectedMicId = deviceId;
    else if (kind === "videoinput") this.selectedCamId = deviceId;
    else this.selectedSpeakerId = deviceId;
    try {
      localStorage.setItem(key, deviceId);
    } catch (e) {
      console.error("AvClient: localStorage.setItem failed:", e);
    }
    if (this.room) {
      try {
        await this.room.switchActiveDevice(kind, deviceId);
      } catch (err) {
        console.warn(`AvClient: switchDevice ${kind} failed:`, err);
      }
    }
  }

  // getSelectedDevice returns the deviceId currently chosen for the given
  // kind, or null if none has been explicitly selected. Used by the UI to
  // restore a <select>'s value after rebuilding its options.
  getSelectedDevice(kind: "audioinput" | "videoinput" | "audiooutput"): string | null {
    if (kind === "audioinput") return this.selectedMicId;
    if (kind === "videoinput") return this.selectedCamId;
    return this.selectedSpeakerId;
  }

  // supportsAudioOutputSelection checks whether the browser supports choosing
  // an audio output device (setSinkId). Safari and most mobile browsers don't.
  // Mirrors LiveKit's supportsAudioOutputSelection() without eagerly importing
  // the SDK module.
  supportsAudioOutputSelection(): boolean {
    return "setSinkId" in document.createElement("audio");
  }

  // handleTokenFrame processes an AvTokenFrame from the server. On "join",
  // it connects to the new room (skipping if already connected to the same
  // room). On "leave", it disconnects after a short debounce so momentary
  // proximity zone exits don't thrash the LiveKit connection.
  //
  // Calls are serialized via frameQueue: each frame waits for the previous
  // one to finish before processing. Without serialization, two concurrent
  // "join" frames (player oscillating on a proximity edge after a disconnect)
  // can both pass the "already connected?" guard — this.currentRoom is set
  // inside connect() after an await, so the second frame sees it still null.
  // Both create Room objects connecting to the same LiveKit room with the
  // same identity, triggering a DUPLICATE_IDENTITY server kick.
  handleTokenFrame(msg: {
    action: string;
    room: string;
    token: string;
    url: string;
    members: string[];
  }): Promise<void> {
    const run = () => this.processTokenFrame(msg);
    this.frameQueue = this.frameQueue.then(run, run);
    return this.frameQueue;
  }

  // setStatus updates the local DND flag. Entering DND (status 2) disconnects
  // any active A/V room immediately; while DND is active, join token frames
  // are refused. Leaving DND lets normal proximity/zone joins resume. This is
  // the client-side half of the defense-in-depth DND enforcement; the server
  // also excludes DND players from A/V (worldsim proximity clustering + ext-av
  // zone token minting).
  setStatus(status: number): void {
    const wasDnd = this.dnd;
    this.dnd = status === 2;
    if (this.dnd && !wasDnd) {
      // Disconnect immediately — fire-and-forget; processTokenFrame's leave
      // debounce doesn't apply here since there's no pending rejoin.
      void this.disconnect();
    }
  }

  // isInMeeting returns true when the player is connected to an A/V room
  // with at least one other participant. Used by AfkDetector to exempt
  // players in active meetings from the AFK idle timer (a user in a long
  // video meeting doesn't move the mouse but is actively engaged). A solo
  // user in a room (no remote participants) is NOT exempt.
  isInMeeting(): boolean {
    return this.room !== null && this.room.remoteParticipants.size > 0;
  }

  // setAfkMuted toggles the AFK overlay auto-mute. When AFK is on, all local
  // tracks are muted (mic, camera, screen) but the player stays in the room
  // (unlike DND, which disconnects). When AFK clears, tracks restore to the
  // user's manual preferences if the tab is also visible. Does NOT change the
  // stored micMuted/cameraEnabled preferences — only the actual track state.
  setAfkMuted(afk: boolean): void {
    this.afkMuted = afk;
    this.applyAutoMute();
  }

  // setTabHidden toggles tab-visibility auto-mute. When the tab is hidden or
  // the window is blurred, all local tracks are muted to stop broadcasting
  // (the player isn't looking). When the tab returns, tracks restore to the
  // user's manual preferences if AFK is also clear. Does NOT change the
  // stored micMuted/cameraEnabled preferences.
  setTabHidden(hidden: boolean): void {
    this.tabHidden = hidden;
    this.applyAutoMute();
  }

  // applyAutoMute mutes or restores all local tracks based on the
  // afkMuted/tabHidden flags. When either is true, mic/camera/screen are
  // muted. When both are false, tracks restore to the user's manual
  // preferences (micMuted / cameraEnabled). Fire-and-forget — LiveKit track
  // operations are async and safe to call without awaiting. No-op when no
  // room is connected.
  private applyAutoMute(): void {
    if (!this.room) return;
    const mute = this.afkMuted || this.tabHidden;
    if (mute) {
      this.room.localParticipant.setMicrophoneEnabled(false).catch((err) => console.warn("AvClient: auto-mute mic failed:", err));
      this.room.localParticipant.setCameraEnabled(false).catch((err) => console.warn("AvClient: auto-mute cam failed:", err));
      this.room.localParticipant.setScreenShareEnabled(false).catch((err) => console.warn("AvClient: auto-mute screen failed:", err));
    } else {
      // Restore to manual preferences.
      this.room.localParticipant.setMicrophoneEnabled(!this.micMuted).catch((err) => console.warn("AvClient: auto-mute restore mic failed:", err));
      this.room.localParticipant.setCameraEnabled(this.cameraEnabled).catch((err) => console.warn("AvClient: auto-mute restore cam failed:", err));
    }
  }

  private async processTokenFrame(msg: {
    action: string;
    room: string;
    token: string;
    url: string;
    members: string[];
  }): Promise<void> {
    if (msg.action === "leave") {
      if (this.currentRoom === msg.room) {
        // Debounce: delay disconnect briefly so a rapid re-join for the
        // same (or a new) room can cancel it. Hysteresis on the proximity
        // radius (enter at 2 tiles, exit at 3 tiles) and movement-gated
        // joins on the backend eliminate the boundary oscillation that
        // previously required a long debounce. 200ms is now just a safety
        // net for edge cases (e.g. teleportation). See issue #88.
        if (this.leaveTimer) clearTimeout(this.leaveTimer);
        const leavingRoom = msg.room;
        this.leaveTimer = setTimeout(async () => {
          this.leaveTimer = null;
          if (this.currentRoom === leavingRoom) {
            await this.disconnect();
          }
        }, 200);
      }
      return;
    }

    if (msg.action === "join") {
      // DND: refuse all A/V joins. The server also won't mint tokens for
      // DND players, so this is a safety net for any in-flight frame.
      if (this.dnd) {
        return;
      }
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

  // preloadModule loads and parses the LiveKit SDK module before the player
  // enters an A/V zone. The module is ~530KB; parsing/evaluating it on first
  // connect blocks the main thread for a significant chunk of the total A/V
  // connect freeze. By pre-loading it after the game scene is ready, the
  // parse cost is paid during idle time, not during the critical connect path.
  preloadModule(): void {
    this.ensureModule().catch((err) =>
      console.warn("AvClient: preloadModule failed:", err)
    );
  }

  // buildAudioCaptureOptions returns the AudioCaptureOptions for the local
  // microphone, merging the selected device ID (if any) with the WebRTC noise
  // cancellation flags. When noiseCancellation is enabled, noiseSuppression,
  // echoCancellation, and autoGainControl are set to true. When disabled, they
  // are explicitly set to false to override the LiveKit SDK's built-in defaults
  // (which enable all three).
  //
  // voiceIsolation is explicitly set to false to override the SDK default
  // (true). It is an experimental W3C constraint that may interfere with
  // Safari's acoustic echo cancellation — on macOS/iOS, voiceIsolation can
  // change the CoreAudio processing path (VPIO unit) in ways that degrade AEC.
  // See WebKit bug 213723: Safari's AEC is already weaker than Chrome's, and
  // voiceIsolation appears to make it worse. Disabling it has no downside on
  // Chrome since noiseSuppression (which we already set) covers the same use
  // case.
  private buildAudioCaptureOptions(): import("livekit-client").AudioCaptureOptions {
    const opts: import("livekit-client").AudioCaptureOptions = {
      noiseSuppression: this.noiseCancellation,
      echoCancellation: this.noiseCancellation,
      autoGainControl: this.noiseCancellation,
      voiceIsolation: false,
    };
    if (this.selectedMicId) {
      opts.deviceId = this.selectedMicId;
    }
    return opts;
  }

  private async connect(url: string, token: string, roomName: string): Promise<void> {
    const lk = await this.ensureModule();
    this.room = new lk.Room({
      adaptiveStream: true,
      dynacast: true,
      // Keep local MediaStreamTracks alive when unpublished. Combined with
      // pauseUpstream/resumeUpstream (used in setMicMuted/setCameraEnabled),
      // this avoids re-acquiring getUserMedia on mute/unmute cycles — the
      // track stays alive and can be resumed instantly.
      stopLocalTrackOnUnpublish: false,
      // Disable RED (Redundant Audio Data) — enabled by default in the
      // LiveKit SDK, but Safari cannot decode audio/red. Without this,
      // Chrome-published audio is silent on Safari: the track subscribes
      // but no audio data is decoded, so isSpeaking stays false and no
      // sound plays. Forcing audio/opus ensures cross-browser compatibility.
      publishDefaults: { red: false },
      audioCaptureDefaults: this.buildAudioCaptureOptions(),
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
    // TrackPublished/TrackUnpublished are different from Subscribed/Unsubscribed:
    // a track can be unpublished (sender stops) without an immediate
    // TrackUnsubscribed. Without these listeners, the participant state goes
    // stale until another event fires (e.g. mic mute), causing a delayed UI
    // update when a screen share stops.
    this.room.on(lk.RoomEvent.TrackPublished, (_pub: TrackPublication, participant: RemoteParticipant) => {
      this.updateParticipant(participant.identity, participant);
    });
    this.room.on(lk.RoomEvent.TrackUnpublished, (_pub: TrackPublication, participant: RemoteParticipant) => {
      this.updateParticipant(participant.identity, participant);
    });
    // Local track events — keep the local participant entry in sync so the
    // VideoBar self-view appears/disappears when the camera is toggled.
    this.room.on(lk.RoomEvent.LocalTrackPublished, () => this.updateLocalParticipant());
    this.room.on(lk.RoomEvent.LocalTrackUnpublished, (pub: TrackPublication) => {
      // If the user stopped screen sharing via the browser's native UI
      // (not through our toggle), the source MediaStreamTrack is gone —
      // drop our relay listeners and sync state.
      if (pub.source === lk.Track.Source.ScreenShare) {
        this.detachScreenSourceVisibilityRelay();
        this.screenShareEnabled = false;
      }
      this.updateLocalParticipant();
    });
    // Browsers block audio playback not initiated by a user gesture. When
    // LiveKit's auto-play is blocked, notify the UI so it can show an
    // "Enable Audio" button (whose click calls room.startAudio()).
    this.room.on(lk.RoomEvent.AudioPlaybackStatusChanged, () => {
      const blocked = !this.room?.canPlaybackAudio;
      this.onAudioBlocked?.(blocked);
    });
    // Clean up state when the room disconnects unexpectedly (server kick,
    // network drop, ICE failure). Without this, this.room and this.currentRoom
    // stay set after the room dies, so all future "join" frames for the same
    // room are skipped by the guard in processTokenFrame — the player is stuck
    // with no A/V until a page reload. The disconnecting flag suppresses this
    // handler during a client-initiated disconnect() (which already cleans up).
    this.room.on(lk.RoomEvent.Disconnected, () => {
      if (this.disconnecting) return;
      if (!this.room) return;
      console.warn(`AvClient: room ${this.currentRoom} disconnected unexpectedly`);
      this.detachScreenSourceVisibilityRelay();
      this.screenShareEnabled = false;
      this.room = null;
      this.currentRoom = null;
      this.participants.clear();
      this.onAudioBlocked?.(false);
      this.notifyChange();
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

    // Yield before each heavy operation (room.connect, getUserMedia) so the
    // game loop can run between them. Each operation may do synchronous
    // main-thread work (WebRTC setup, hardware access); yielding between them
    // breaks the total block into smaller chunks.
    try {
      await new Promise<void>((r) => setTimeout(r, 0));
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
      await new Promise<void>((r) => setTimeout(r, 0));
      await this.room.localParticipant.setMicrophoneEnabled(!this.micMuted);
    } catch (err) {
      console.warn("AvClient: microphone publish failed (room still connected):", err);
    }
    try {
      await new Promise<void>((r) => setTimeout(r, 0));
      await this.room.localParticipant.setCameraEnabled(this.cameraEnabled);
    } catch (err) {
      console.warn("AvClient: camera publish failed (room still connected):", err);
    }

    // Apply the persisted speaker device if one was selected. Unlike mic/camera
    // (which are set via Room constructor defaults), audio output can only be
    // changed via switchActiveDevice after the room exists.
    if (this.selectedSpeakerId && this.supportsAudioOutputSelection()) {
      this.room.switchActiveDevice("audiooutput", this.selectedSpeakerId).catch((err) =>
        console.warn("AvClient: speaker switchActiveDevice failed:", err),
      );
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
      this.disconnecting = true;
      this.detachScreenSourceVisibilityRelay();
      this.screenShareEnabled = false;
      // Clear participants and notify the UI to destroy DOM tiles BEFORE the
      // heavy WebRTC teardown. This lets the game loop run between the fast
      // DOM cleanup and the blocking room.disconnect() call. Without this,
      // room.disconnect() blocks the main thread (WebRTC peer connection
      // teardown, track stop) while the video tiles are still in the DOM,
      // freezing the game loop and making the local player unable to move.
      this.participants.clear();
      this.onAudioBlocked?.(false);
      this.notifyChange();
      // Yield to the event loop so the game loop can run one frame before
      // the blocking WebRTC teardown.
      await new Promise<void>((resolve) => setTimeout(resolve, 0));
      try {
        await this.room.disconnect();
        console.log(`AvClient: disconnected from room ${this.currentRoom}`);
      } finally {
        this.room = null;
        this.currentRoom = null;
        this.disconnecting = false;
        // Clear participants again and notify. room.disconnect() fires
        // TrackUnsubscribed events during teardown. RemoteTrackPublication.setTrack
        // emits the event BEFORE nullifying pub.track, so our handler reads the
        // dead track and re-adds the participant with a non-null cameraTrack.
        // Without this final clear, those zombie participants create black tiles
        // that are never destroyed.
        this.participants.clear();
        this.onAudioBlocked?.(false);
        this.notifyChange();
      }
    }
  }

  private updateParticipant(identity: string, participant: RemoteParticipant): void {
    const hasCamera = participant.isCameraEnabled;
    const hasMic = participant.isMicrophoneEnabled;
    let cameraTrack: Track | null = null;
    let screenShareTrack: Track | null = null;
    let screenMuted = false;
    const pubs = participant.getTrackPublications();
    for (const pub of pubs.values()) {
      if (pub.track && pub.source === "camera") {
        cameraTrack = pub.track;
      } else if (pub.source === "screen_share") {
        screenMuted = pub.isMuted;
        if (pub.track) screenShareTrack = pub.track;
      }
    }
    this.participants.set(identity, {
      entityId: identity,
      isLocal: false,
      hasCamera,
      hasMic,
      cameraTrack,
      hasScreenShare: !!screenShareTrack,
      screenShareTrack,
      screenMuted,
    });
    this.notifyChange();
  }

  private notifyChange(): void {
    const snapshot = new Map(this.participants);
    for (const handler of this.participantsHandlers) {
      handler(snapshot);
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
    let screenShareTrack: Track | null = null;
    for (const pub of lp.getTrackPublications().values()) {
      if (pub.source === "camera" && pub.track) {
        cameraTrack = pub.track;
      } else if (pub.source === "screen_share" && pub.track) {
        screenShareTrack = pub.track;
      }
    }
    this.participants.set(identity, {
      entityId: identity,
      isLocal: true,
      hasCamera: lp.isCameraEnabled,
      hasMic: lp.isMicrophoneEnabled,
      cameraTrack,
      hasScreenShare: !!screenShareTrack,
      screenShareTrack,
      screenMuted: this.localScreenSourceHidden,
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
