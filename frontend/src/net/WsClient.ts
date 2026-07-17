import { create, toBinary, fromBinary } from "@bufbuild/protobuf";
import { context, trace } from "@opentelemetry/api";
import { ClientFrameSchema, ServerFrameSchema, AuthFrameSchema, InputFrameSchema, InputStateSchema, ActionFrameSchema, ChatFrameSchema, SetNameFrameSchema, SetSpriteBaseFrameSchema, SetPlayerOptionsFrameSchema, SetStatusFrameSchema } from "../proto/frames_pb";
import { PositionSchema } from "../proto/components_pb";
import { tracer, traceparentFor } from "../otel";
import { getIdToken, clearIdToken, getDeviceId } from "../auth";

export type ReplicationHandler = (batch: ReplicationBatchView) => void;

export interface AvailableActionView {
  entityId: string;
  actionId: string;
  label: string;
  entityLabel: string;
}

export interface ActionResultView {
  seq: number;
  ok: boolean;
  reason: string;
  availableActions: AvailableActionView[];
}

// Connection state surfaced to the UI so it can show a "Reconnecting…"
// overlay. Transitions:
//   connecting -> open -> reconnecting -> open -> ... -> closed
// "closed" is terminal and only reached via an explicit close().
export type ConnectionState = "connecting" | "open" | "reconnecting" | "closed";

export interface ConnectHandlers {
  onReady: () => void;
  onReplication: ReplicationHandler;
  // Fired on every successful auth AFTER the first one (i.e. after a
  // reconnect). Carries the new clientId/entityId — these change on each
  // reconnect because the pusher mints a fresh session.
  onReconnect?: (clientId: string, entityId: string) => void;
  onStateChange?: (state: ConnectionState) => void;
  // Fired when an AvTokenFrame is received (LiveKit join/leave instruction
  // from ext-av via the pusher).
  onAvToken?: (msg: { action: string; room: string; token: string; url: string; members: string[] }) => void;
  // Fired when a ChatMessageFrame is received (global or proximity chat).
  // The server stamps display_name + timestamp; the client never authors
  // these directly. See documentation/plans/2026-07-07-chat-design.md.
  onChatMessage?: (msg: { channel: "global" | "proximity"; entityId: string; displayName: string; text: string; timestamp: number }) => void;
  // Fired when a MapTransitionFrame is received — the server moved the
  // player to a different map. The frontend should load the new tilemap
  // and clear entities from the old map.
  onMapTransition?: (msg: { mapId: string; spawnX: number; spawnY: number; mapOptions: string }) => void;
  // Fired when a MapOptionsUpdateFrame is received — the map's options were
  // edited in the PB admin GUI and hot-reloaded to connected clients.
  onMapOptionsUpdate?: (msg: { mapOptions: string }) => void;
  // Fired when an AdminInfoFrame is received (admin clients only). Carries
  // admin-only data (IP, guest status) about entities spawned near the
  // admin. Non-admin clients never receive this. See
  // documentation/plans/2026-07-11-admin-mode-design.md.
  onAdminInfo?: (msg: { entities: { entityId: string; ip: string; isGuest: boolean; userId: string; deviceId: string }[] }) => void;
  // Fired when the server rejects the connection due to an active ban.
  // The client will not attempt to reconnect. reason is human-readable;
  // banUntil is a unix timestamp (0 = permanent).
  onBanned?: (reason: string, banUntil: number) => void;
  // Fired when an ActionResultFrame is received. For popup-mode
  // interactions, availableActions carries the actions to display. For
  // immediate-mode interactions, availableActions is empty and the side
  // effects arrive via replication (state/sprite/animation updates).
  onActionResult?: (result: ActionResultView) => void;
}

export interface SpawnEntityView {
  entityId: string;
  components: { componentId: number; data: Uint8Array }[];
}

export interface UpdateComponentView {
  entityId: string;
  componentId: number;
  data: Uint8Array;
}

export interface DestroyEntityView {
  entityId: string;
}

export interface PlayAnimationView {
  entityId: string;
  animationId: number;
}

export interface ReplicationBatchView {
  lastInputSeq: number;
  spawns: SpawnEntityView[];
  updates: UpdateComponentView[];
  destroys: DestroyEntityView[];
  animations: PlayAnimationView[];
}

export class WsClient {
  private ws: WebSocket | null = null;
  private url: string;
  private clientId: string | null = null;
  private entityId: string | null = null;
  private mapId: string | null = null;
  private mapOptions: string = "";
  private playerOptions: string = "";
  private admin = false;
  private seq = 0;
  private handlers!: ConnectHandlers;
  // True after the first successful auth; used to dispatch onReady vs
  // onReconnect on subsequent auths.
  private hadFirstAuth = false;
  // Set by close() to suppress the auto-reconnect loop.
  private closed = false;
  private reconnectAttempt = 0;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private state: ConnectionState = "connecting";

  constructor(url: string) {
    this.url = url;
  }

  connect(handlers: ConnectHandlers): void {
    this.handlers = handlers;
    this.closed = false;
    this.openSocket();
  }

  private setState(s: ConnectionState): void {
    this.state = s;
    this.handlers.onStateChange?.(s);
  }

  // openSocket creates a fresh WebSocket, sends the auth frame on open, and
  // wires onclose to schedule a reconnect (unless close() was called). Called
  // once from connect() and again on each reconnect attempt.
  private openSocket(): void {
    const isReconnect = this.hadFirstAuth;
    const connectSpan = tracer.startSpan(isReconnect ? "ws.reconnect" : "ws.connect");
    const ws = new WebSocket(this.url);
    this.ws = ws;
    ws.binaryType = "arraybuffer";

    ws.onopen = () => {
      const authSpan = tracer.startSpan("ws.send_auth");
      try {
        // Build the auth frame inside the span's active context so
        // traceparentFor() serializes this span's context for the backend.
        const auth = context.with(trace.setSpan(context.active(), authSpan), () =>
          create(AuthFrameSchema, { idToken: getIdToken() ?? "", traceparent: traceparentFor(), deviceId: getDeviceId() }),
        );
        const frame = create(ClientFrameSchema, { payload: { case: "auth", value: auth } });
        ws.send(toBinary(ClientFrameSchema, frame));
      } finally {
        authSpan.end();
      }
    };

    ws.onmessage = (event) => {
      // Ignore messages from a stale socket that has since been replaced.
      if (this.ws !== ws) return;
      const data = new Uint8Array(event.data as ArrayBuffer);
      const serverFrame = fromBinary(ServerFrameSchema, data);

      switch (serverFrame.payload.case) {
        case "authResult": {
          const ar = serverFrame.payload.value;
          if (ar.ok) {
            this.clientId = ar.clientId;
            this.entityId = ar.entityId || null;
            this.mapId = ar.mapId || null;
            this.mapOptions = ar.mapOptions || "";
            this.playerOptions = ar.playerOptions || "";
            this.admin = ar.isAdmin;
            this.reconnectAttempt = 0;
            connectSpan.setAttribute("client.id", ar.clientId);
            connectSpan.end();
            console.log(`authenticated: client=${this.clientId} entity=${this.entityId}`);
            this.setState("open");
            if (this.hadFirstAuth) {
              this.handlers.onReconnect?.(ar.clientId, this.entityId ?? "");
            } else {
              this.hadFirstAuth = true;
              this.handlers.onReady();
            }
          } else {
            connectSpan.recordException(new Error("auth failed"));
            connectSpan.setStatus({ code: 2, message: "auth failed" });
            connectSpan.end();
            // If the server sent a ban reason, the client is banned —
            // stop reconnecting and surface the ban to the UI.
            if (ar.banReason) {
              this.closed = true;
              this.setState("closed");
              console.error("banned:", ar.banReason);
              this.handlers.onBanned?.(ar.banReason, Number(ar.banUntil));
              break;
            }
            console.error("auth failed");
            // If auth failed with a stored token (e.g. PB DB was reset,
            // making the token stale), clear it so the next reconnect
            // falls back to guest mode instead of looping forever with
            // the same invalid token.
            if (getIdToken() !== null) {
              clearIdToken();
              this.reconnectAttempt = 0;
              console.log("cleared stale token, will retry as guest");
            }
          }
          break;
        }
        case "replication": {
          const batch = serverFrame.payload.value;
          const span = tracer.startSpan("ws.on_replication", {
            attributes: {
              "client.id": this.clientId ?? "",
              "batch.spawns": batch.spawns.length,
              "batch.updates": batch.updates.length,
              "batch.destroys": batch.destroys.length,
              "batch.last_input_seq": Number(batch.lastInputSeq),
            },
          });
          try {
            this.handlers.onReplication({
              lastInputSeq: Number(batch.lastInputSeq),
              spawns: batch.spawns.map((s: any) => ({
                entityId: s.entityId,
                components: s.components.map((c: any) => ({ componentId: c.componentId, data: c.data })),
              })),
              updates: batch.updates.map((u: any) => ({
                entityId: u.entityId,
                componentId: u.componentId,
                data: u.data,
              })),
              destroys: batch.destroys.map((d: any) => ({
                entityId: d.entityId,
              })),
              animations: (batch.animations ?? []).map((a: any) => ({
                entityId: a.entityId,
                animationId: Number(a.animationId),
              })),
            });
          } finally {
            span.end();
          }
          break;
        }
        case "pong":
          break;
        case "actionResult": {
          const ar = serverFrame.payload.value;
          this.handlers.onActionResult?.({
            seq: Number(ar.seq),
            ok: ar.ok,
            reason: ar.reason,
            availableActions: (ar.availableActions ?? []).map((a: any) => ({
              entityId: a.entityId,
              actionId: a.actionId,
              label: a.label,
              entityLabel: a.entityLabel,
            })),
          });
          break;
        }
        case "avToken": {
          const av = serverFrame.payload.value;
          this.handlers.onAvToken?.({
            action: av.action,
            room: av.room,
            token: av.token,
            url: av.url,
            members: av.members,
          });
          break;
        }
        case "chatMessage": {
          const cm = serverFrame.payload.value;
          this.handlers.onChatMessage?.({
            channel: cm.channel as "global" | "proximity",
            entityId: cm.entityId,
            displayName: cm.displayName,
            text: cm.text,
            timestamp: Number(cm.timestamp),
          });
          break;
        }
        case "error":
          console.error("server error:", serverFrame.payload.value);
          break;
        case "mapTransition": {
          const mt = serverFrame.payload.value;
          this.mapId = mt.mapId || null;
          this.mapOptions = mt.mapOptions || "";
          this.handlers.onMapTransition?.({
            mapId: mt.mapId,
            spawnX: Number(mt.spawnX),
            spawnY: Number(mt.spawnY),
            mapOptions: mt.mapOptions || "",
          });
          break;
        }
        case "mapOptionsUpdate": {
          const mo = serverFrame.payload.value;
          this.mapOptions = mo.mapOptions || "";
          this.handlers.onMapOptionsUpdate?.({
            mapOptions: mo.mapOptions || "",
          });
          break;
        }
        case "adminInfo": {
          const ai = serverFrame.payload.value;
          this.handlers.onAdminInfo?.({
            entities: ai.entities.map((e: any) => ({
              entityId: e.entityId,
              ip: e.ip,
              isGuest: e.isGuest,
              userId: e.userId,
              deviceId: e.deviceId,
            })),
          });
          break;
        }
      }
    };

    ws.onclose = () => {
      // Ignore close from a stale socket that has been replaced.
      if (this.ws !== ws) return;
      const span = tracer.startSpan("ws.close");
      span.setAttribute("client.id", this.clientId ?? "");
      span.end();
      console.log("websocket closed");
      if (this.closed) {
        this.setState("closed");
        return;
      }
      this.scheduleReconnect();
    };

    ws.onerror = (err) => {
      connectSpan.recordException(new Error("websocket error"));
      connectSpan.setStatus({ code: 2, message: "websocket error" });
      console.error("websocket error:", err);
    };
  }

  // scheduleReconnect backs off exponentially (1s, 2s, 4s, ... capped at 30s)
  // and re-dials. Reset to 0 on successful auth.
  private scheduleReconnect(): void {
    this.setState("reconnecting");
    const delay = Math.min(1000 * 2 ** this.reconnectAttempt, 30000);
    this.reconnectAttempt++;
    console.log(`reconnecting in ${delay}ms (attempt ${this.reconnectAttempt})`);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      if (this.closed) return;
      this.openSocket();
    }, delay);
  }

  sendInput(state: { up: boolean; down: boolean; left: boolean; right: boolean; run: boolean }): number {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return 0;
    this.seq++;
    const span = tracer.startSpan("ws.send_input", {
      attributes: {
        "client.id": this.clientId ?? "",
        "input.seq": this.seq,
        "input.up": state.up,
        "input.down": state.down,
        "input.left": state.left,
        "input.right": state.right,
        "input.run": state.run,
      },
    });
    try {
      // Serialize this span's context into the frame so the backend's
      // input-handling spans parent to ws.send_input.
      const inputState = create(InputStateSchema, state);
      const input = context.with(trace.setSpan(context.active(), span), () =>
        create(InputFrameSchema, {
          seq: this.seq,
          clientTick: 0,
          state: inputState,
          traceparent: traceparentFor(),
        }),
      );
      const frame = create(ClientFrameSchema, { payload: { case: "input", value: input } });
      this.ws.send(toBinary(ClientFrameSchema, frame));
    } finally {
      span.end();
    }
    return this.seq;
  }

  // sendAction sends a discrete input trigger (e.g. "key:E") as an
  // ActionFrame. Unlike sendInput (continuous movement), this is a single
  // event; the server dispatches it to extensions registered for the input
  // type and replies with an ActionResultFrame (handled in onmessage).
  // For the two-phase interaction flow, pass input="action:execute" with
  // entityId and actionId set to the chosen popup action.
  sendAction(input: string, entityId?: string, actionId?: string): number {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return 0;
    this.seq++;
    const span = tracer.startSpan("ws.send_action", {
      attributes: {
        "client.id": this.clientId ?? "",
        "input.seq": this.seq,
        "action.input": input,
      },
    });
    try {
      const action = context.with(trace.setSpan(context.active(), span), () =>
        create(ActionFrameSchema, {
          seq: this.seq,
          input,
          entityId: entityId ?? "",
          actionId: actionId ?? "",
          traceparent: traceparentFor(),
        }),
      );
      const frame = create(ClientFrameSchema, { payload: { case: "action", value: action } });
      this.ws.send(toBinary(ClientFrameSchema, frame));
    } finally {
      span.end();
    }
    return this.seq;
  }

  // sendChat sends a chat message on the given channel ("global" or
  // "proximity"). Fire-and-forget — the server echoes the stamped message
  // back (via chat.broadcast for global, or client.<id>.chat_inbox for
  // proximity) which triggers onChatMessage, confirming delivery.
  sendChat(channel: "global" | "proximity", text: string): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    const span = tracer.startSpan("ws.send_chat", {
      attributes: {
        "client.id": this.clientId ?? "",
        "chat.channel": channel,
      },
    });
    try {
      const chat = context.with(trace.setSpan(context.active(), span), () =>
        create(ChatFrameSchema, { channel, text, traceparent: traceparentFor() }),
      );
      const frame = create(ClientFrameSchema, { payload: { case: "chat", value: chat } });
      this.ws.send(toBinary(ClientFrameSchema, frame));
    } finally {
      span.end();
    }
  }

  // setName sends a SetNameFrame to change the player's display name. The
  // server sanitizes (ASCII printable, max 20 chars), updates the entity,
  // replicates to all clients, and persists to PocketBase for logged-in
  // users. Fire-and-forget — the name tag update arrives via replication.
  setName(name: string): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    const span = tracer.startSpan("ws.send_set_name", {
      attributes: { "client.id": this.clientId ?? "" },
    });
    try {
      const setName = context.with(trace.setSpan(context.active(), span), () =>
        create(SetNameFrameSchema, { name, traceparent: traceparentFor() }),
      );
      const frame = create(ClientFrameSchema, { payload: { case: "setName", value: setName } });
      this.ws.send(toBinary(ClientFrameSchema, frame));
    } finally {
      span.end();
    }
  }

  // setSpriteBase sends a SetSpriteBaseFrame to change the player's character
  // sheet. The server validates the ID, updates the entity, replicates to all
  // clients, and persists to PocketBase for logged-in users. Fire-and-forget
  // — the avatar hot-swap arrives via replication.
  setSpriteBase(spriteBase: string): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    const span = tracer.startSpan("ws.send_set_sprite_base", {
      attributes: { "client.id": this.clientId ?? "" },
    });
    try {
      const ssb = context.with(trace.setSpan(context.active(), span), () =>
        create(SetSpriteBaseFrameSchema, { spriteBase, traceparent: traceparentFor() }),
      );
      const frame = create(ClientFrameSchema, { payload: { case: "setSpriteBase", value: ssb } });
      this.ws.send(toBinary(ClientFrameSchema, frame));
    } finally {
      span.end();
    }
  }

  // setPlayerOptions sends a SetPlayerOptionsFrame to update the player's
  // options JSON (e.g. show_own_name_tag). The server persists to PocketBase
  // for logged-in users; guests are session-only. Fire-and-forget.
  setPlayerOptions(options: string): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    const span = tracer.startSpan("ws.send_set_player_options", {
      attributes: { "client.id": this.clientId ?? "" },
    });
    try {
      const spo = context.with(trace.setSpan(context.active(), span), () =>
        create(SetPlayerOptionsFrameSchema, { options, traceparent: traceparentFor() }),
      );
      const frame = create(ClientFrameSchema, { payload: { case: "setPlayerOptions", value: spo } });
      this.ws.send(toBinary(ClientFrameSchema, frame));
      this.playerOptions = options;
    } finally {
      span.end();
    }
  }

  // setStatus sends a SetStatusFrame to change the player's presence status
  // (0=Available, 1=Busy, 2=Do Not Disturb). The server validates the range,
  // updates the entity, replicates to all clients via the DisplayName
  // component, and broadcasts on NATS so ext-av enforces DND A/V exclusion.
  // Session-only — not persisted to PocketBase. Fire-and-forget.
  setStatus(status: number): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    const span = tracer.startSpan("ws.send_set_status", {
      attributes: { "client.id": this.clientId ?? "", "status": status },
    });
    try {
      const ss = context.with(trace.setSpan(context.active(), span), () =>
        create(SetStatusFrameSchema, { status, traceparent: traceparentFor() }),
      );
      const frame = create(ClientFrameSchema, { payload: { case: "setStatus", value: ss } });
      this.ws.send(toBinary(ClientFrameSchema, frame));
    } finally {
      span.end();
    }
  }

  getClientId(): string | null {
    return this.clientId;
  }

  getEntityId(): string | null {
    return this.entityId;
  }

  getMapId(): string | null {
    return this.mapId;
  }

  getMapOptions(): string {
    return this.mapOptions;
  }

  getPlayerOptions(): string {
    return this.playerOptions;
  }

  isAdmin(): boolean {
    return this.admin;
  }

  close(): void {
    this.closed = true;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.ws?.close();
  }
}

export function decodePosition(data: Uint8Array): { x: number; y: number; mapId: string; dir: number } {
  const pos = fromBinary(PositionSchema, data);
  return { x: pos.x, y: pos.y, mapId: pos.mapId, dir: pos.dir };
}
